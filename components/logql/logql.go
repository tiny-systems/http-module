// Package logql queries Loki for log lines.
//
// Addressed by URL like its metrics sibling, so an in-cluster Loki and Grafana
// Cloud are the same component. It exists because reading logs any other way
// requires knowing a pod name: this searches across every pod at once, which is
// what "what broke in the last hour" actually needs.
package logql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tiny-systems/http-module/components/etc"
	"github.com/tiny-systems/module/api/v1alpha1"
	"github.com/tiny-systems/module/module"
	"github.com/tiny-systems/module/registry"
)

const (
	ComponentName = "logql_query"
	RequestPort   = "request"
	ResultPort    = "result"
	ErrorPort     = "error"
)

const (
	// maxEntries bounds the reply. Loki will happily return thousands of lines,
	// and an agent reading them pays for every one.
	maxEntries = 500
	// defaultRangeMinutes matches the question this is usually asked: what has
	// been happening recently.
	defaultRangeMinutes = 60
)

type Context any

type Settings struct {
	BaseURL         string       `json:"baseURL" required:"true" title:"Base URL" description:"Root of the Loki API, e.g. http://loki.monitoring.svc.cluster.local:3100 or https://logs-prod-01.grafana.net. The /loki/api/v1 path is appended."`
	OrgID           string       `json:"orgID,omitempty" title:"Tenant (X-Scope-OrgID)" description:"Tenant header for multi-tenant Loki. Leave empty for single-tenant."`
	Headers         []etc.Header `json:"headers,omitempty" title:"Extra Headers" description:"Sent with every request, for backends that authenticate with a custom header."`
	TimeoutSeconds  int          `json:"timeoutSeconds" required:"true" title:"Timeout (seconds)"`
	EnableErrorPort bool         `json:"enableErrorPort" title:"Enable Error Port" description:"Emit failures on the error port instead of failing the flow."`
}

type Request struct {
	Context      Context `json:"context,omitempty" configurable:"true" title:"Context" description:"Passed through to the result unchanged."`
	Query        string  `json:"query" required:"true" title:"LogQL" format:"textarea" description:"LogQL selector, e.g. {namespace=\"prod\"} |= \"error\". A stream selector in braces is mandatory — Loki rejects a bare search."`
	RangeMinutes int     `json:"rangeMinutes,omitempty" title:"Range (minutes)" description:"Window to search. Defaults to 60."`
	Limit        int     `json:"limit,omitempty" title:"Limit" description:"Maximum log lines. Capped at 500."`
	Token        string  `json:"token,omitempty" format:"password" title:"Bearer Token" description:"Carried per-request from the trigger widget the user fills, so the credential is not stored in the flow."`
	Username     string  `json:"username,omitempty" title:"Username" description:"For backends using basic auth (Grafana Cloud uses the instance id here, with the token as the password)."`
}

// Entry is one log line with the stream labels that say where it came from.
type Entry struct {
	At     string            `json:"at" title:"At" description:"RFC3339."`
	Line   string            `json:"line" title:"Line"`
	Labels map[string]string `json:"labels" title:"Labels" description:"Stream labels — namespace, pod, container."`
}

type Result struct {
	Context   Context `json:"context,omitempty" title:"Context"`
	Entries   []Entry `json:"entries" title:"Entries" description:"Matching lines, newest first."`
	Count     int     `json:"count" title:"Count"`
	Truncated bool    `json:"truncated" title:"Truncated" description:"True when the query matched more lines than were returned — narrow the window or the selector before concluding anything from the sample."`
}

type Component struct {
	settings     Settings
	settingsLock sync.RWMutex
}

func (c *Component) Instance() module.Component {
	return &Component{settings: Settings{TimeoutSeconds: 30}}
}

func (c *Component) GetInfo() module.ComponentInfo {
	return module.ComponentInfo{
		Name:        ComponentName,
		Description: "LogQL Query",
		Info: "Searches Loki for log lines across every pod at once, which is what pod_logs_get cannot do — it needs an exact pod name. Returns each line with its stream labels, so an agent can say which pod and namespace produced it. " +
			"The query needs a stream selector in braces: {namespace=\"prod\"} |= \"error\". Addressed by URL, so an in-cluster Loki and Grafana Cloud work the same way; set the tenant header for multi-tenant installs. " +
			"Give the credential per request from the trigger widget rather than storing it in the flow. Check truncated before concluding anything — a capped sample is not the whole picture.",
		Tags: []string{"Observability", "Logs", "Agent"},
	}
}

func (c *Component) OnSettings(_ context.Context, msg any) error {
	in, ok := msg.(Settings)
	if !ok {
		return fmt.Errorf("invalid settings")
	}
	base := strings.TrimSpace(in.BaseURL)
	if base == "" {
		return fmt.Errorf("base URL is required")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return fmt.Errorf("base URL must be http:// or https://, got %q", base)
	}
	c.settingsLock.Lock()
	c.settings = in
	c.settingsLock.Unlock()
	return nil
}

func (c *Component) getSettings() Settings {
	c.settingsLock.RLock()
	defer c.settingsLock.RUnlock()
	return c.settings
}

func (c *Component) Handle(ctx context.Context, handler module.Handler, port string, msg any) module.Result {
	if port != RequestPort {
		return module.Fail(fmt.Errorf("port %s is not supported", port))
	}
	in, ok := msg.(Request)
	if !ok {
		return module.Fail(fmt.Errorf("invalid message"))
	}
	if strings.TrimSpace(in.Query) == "" {
		return c.handleError(ctx, handler, in.Context, module.Permanent(fmt.Errorf("query is required")))
	}

	set := c.getSettings()
	timeout := set.TimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	rangeMinutes := in.RangeMinutes
	if rangeMinutes <= 0 {
		rangeMinutes = defaultRangeMinutes
	}
	limit := in.Limit
	if limit <= 0 || limit > maxEntries {
		limit = maxEntries
	}

	end := time.Now()
	start := end.Add(-time.Duration(rangeMinutes) * time.Minute)

	q := url.Values{}
	q.Set("query", in.Query)
	q.Set("limit", strconv.Itoa(limit))
	// Loki takes nanosecond timestamps.
	q.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	q.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	// Newest first: an agent asking what just broke reads the top of the list.
	q.Set("direction", "backward")

	endpoint := strings.TrimSuffix(set.BaseURL, "/") + "/loki/api/v1/query_range?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return c.handleError(ctx, handler, in.Context, module.Permanent(err))
	}
	applyAuth(req, set, in)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return c.handleError(ctx, handler, in.Context, module.Retryable(fmt.Errorf("query loki: %w", err)))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.handleError(ctx, handler, in.Context, module.Retryable(fmt.Errorf("read response: %w", err)))
	}
	if resp.StatusCode >= 400 {
		return c.handleError(ctx, handler, in.Context, classify(resp.StatusCode, body))
	}

	entries, err := parse(body)
	if err != nil {
		return c.handleError(ctx, handler, in.Context, module.Permanent(err))
	}

	return handler(ctx, ResultPort, Result{
		Context: in.Context,
		Entries: entries,
		Count:   len(entries),
		// Hitting the limit means the window held at least one more line, so
		// the sample cannot be read as the complete picture.
		Truncated: len(entries) >= limit,
	})
}

func applyAuth(req *http.Request, set Settings, in Request) {
	switch {
	case in.Username != "" && in.Token != "":
		req.SetBasicAuth(in.Username, in.Token)
	case in.Token != "":
		req.Header.Set("Authorization", "Bearer "+in.Token)
	}
	if set.OrgID != "" {
		req.Header.Set("X-Scope-OrgID", set.OrgID)
	}
	for _, h := range set.Headers {
		if h.Key != "" {
			req.Header.Set(h.Key, h.Value)
		}
	}
}

// classify separates a failure worth retrying from one that will repeat. A
// malformed LogQL selector returns 400 every time.
func classify(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 400 {
		msg = msg[:400]
	}
	err := fmt.Errorf("loki %d: %s", status, msg)
	if status == http.StatusTooManyRequests || status >= 500 {
		return module.Retryable(err)
	}
	return module.Permanent(err)
}

type lokiResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			// Each value is [nanosecond timestamp as string, line].
			Values [][]string `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func parse(body []byte) ([]Entry, error) {
	var resp lokiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("loki returned a body that is not JSON: %w", err)
	}

	entries := make([]Entry, 0, maxEntries)
	for _, stream := range resp.Data.Result {
		for _, v := range stream.Values {
			if len(v) != 2 {
				continue
			}
			if len(entries) >= maxEntries {
				return entries, nil
			}
			entries = append(entries, Entry{
				At:     nanosToRFC3339(v[0]),
				Line:   v[1],
				Labels: stream.Stream,
			})
		}
	}
	return entries, nil
}

func nanosToRFC3339(s string) string {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return ""
	}
	return time.Unix(0, n).UTC().Format(time.RFC3339)
}

func (c *Component) handleError(ctx context.Context, handler module.Handler, reqContext Context, err error) module.Result {
	if !c.getSettings().EnableErrorPort {
		return module.Fail(err)
	}
	return handler(ctx, ErrorPort, module.NewError(reqContext, err))
}

func (c *Component) Ports() []module.Port {
	ports := []module.Port{
		{
			Name:          v1alpha1.SettingsPort,
			Label:         "Settings",
			Configuration: c.getSettings(),
		},
		{
			Name:          RequestPort,
			Label:         "Request",
			Position:      module.Left,
			Configuration: Request{},
		},
		{
			Name:     ResultPort,
			Label:    "Result",
			Source:   true,
			Position: module.Right,
			// Concrete example so an edge reading $.entries[0].line is
			// checkable when the flow is built.
			Configuration: Result{
				Entries: []Entry{{
					At:     "2026-07-30T00:00:00Z",
					Line:   "example log line",
					Labels: map[string]string{"namespace": "prod", "pod": "example-pod"},
				}},
				Count: 1,
			},
		},
	}
	if !c.getSettings().EnableErrorPort {
		return ports
	}
	return append(ports, module.Port{
		Name:          ErrorPort,
		Label:         "Error",
		Source:        true,
		Position:      module.Bottom,
		Configuration: module.ErrorMessage{},
	})
}

var (
	_ module.Component       = (*Component)(nil)
	_ module.SettingsHandler = (*Component)(nil)
)

func init() {
	registry.Register((&Component{}).Instance())
}
