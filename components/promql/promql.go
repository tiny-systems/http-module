// Package promql queries any Prometheus-compatible metrics API.
//
// Deliberately addressed by URL rather than by assuming a deployment: the same
// component serves an in-cluster Prometheus, Grafana Cloud, Amazon Managed
// Prometheus, Thanos, Mimir or VictoriaMetrics, because all of them speak the
// same query API. A component that assumed a pod in the cluster would be
// useless to everyone running metrics as a service.
package promql

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
	ComponentName = "promql_query"
	RequestPort   = "request"
	ResultPort    = "result"
	ErrorPort     = "error"
)

const (
	// maxSeries bounds the reply. A careless query can match tens of thousands
	// of series, which would arrive as one unreadable message.
	maxSeries = 500
	// maxPoints bounds a range query's samples per series for the same reason.
	maxPoints = 500
)

type Context any

type Settings struct {
	BaseURL string `json:"baseURL" required:"true" title:"Base URL" description:"Root of the metrics API, e.g. http://prometheus-server.monitoring.svc.cluster.local or https://prometheus-prod-01.grafana.net/api/prom. The /api/v1 path is appended."`
	// OrgID is not optional in practice for the hosted backends: Mimir and
	// Grafana Cloud reject a query with no tenant.
	OrgID           string       `json:"orgID,omitempty" title:"Tenant (X-Scope-OrgID)" description:"Tenant header for multi-tenant backends (Mimir, Grafana Cloud, Cortex). Leave empty for a single-tenant Prometheus."`
	Headers         []etc.Header `json:"headers,omitempty" title:"Extra Headers" description:"Sent with every request, for backends that authenticate with a custom header."`
	TimeoutSeconds  int          `json:"timeoutSeconds" required:"true" title:"Timeout (seconds)"`
	EnableErrorPort bool         `json:"enableErrorPort" title:"Enable Error Port" description:"Emit failures on the error port instead of failing the flow."`
}

type Request struct {
	Context Context `json:"context,omitempty" configurable:"true" title:"Context" description:"Passed through to the result unchanged."`
	Query   string  `json:"query" required:"true" title:"PromQL" format:"textarea" description:"PromQL expression, e.g. sum(rate(container_cpu_usage_seconds_total[5m])) by (pod)."`
	// RangeMinutes is what turns one number into a trend — the difference
	// between "CPU is high" and "CPU has been climbing for an hour".
	RangeMinutes int    `json:"rangeMinutes,omitempty" title:"Range (minutes)" description:"Query over a window instead of at an instant. Empty or 0 evaluates once, now."`
	StepSeconds  int    `json:"stepSeconds,omitempty" title:"Step (seconds)" description:"Resolution of a range query. Defaults to a step that keeps the series under 500 points."`
	Token        string `json:"token,omitempty" format:"password" title:"Bearer Token" description:"Carried per-request from the trigger widget the user fills, so the credential is not stored in the flow."`
	Username     string `json:"username,omitempty" title:"Username" description:"For backends using basic auth (Grafana Cloud uses the instance id here, with the token as the password)."`
}

// Series is one metric stream. Labels identify it — an agent reads them to say
// WHICH pod or service the number belongs to.
type Series struct {
	Labels map[string]string `json:"labels" title:"Labels"`
	Value  float64           `json:"value" title:"Value" description:"Latest sample. For a range query this is the most recent point."`
	// Points carry the shape over time; empty for an instant query.
	Points []Point `json:"points,omitempty" title:"Points" description:"Samples over the window, oldest first. Empty for an instant query."`
}

type Point struct {
	At    string  `json:"at" title:"At" description:"RFC3339."`
	Value float64 `json:"value" title:"Value"`
}

type Result struct {
	Context Context  `json:"context,omitempty" title:"Context"`
	Series  []Series `json:"series" title:"Series" description:"Matching metric streams."`
	Count   int      `json:"count" title:"Count" description:"Series returned."`
	// Truncated keeps a capped answer from reading as a complete one.
	Truncated bool `json:"truncated" title:"Truncated" description:"True when the query matched more series than were returned."`
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
		Description: "PromQL Query",
		Info: "Runs a PromQL query against any Prometheus-compatible API and returns the matching series with their labels. Set rangeMinutes to query a window instead of an instant — that is what shows a trend rather than a single number. " +
			"Addressed by URL, so it works with an in-cluster Prometheus as well as Grafana Cloud, Amazon Managed Prometheus, Thanos, Mimir or VictoriaMetrics; set the tenant header for the multi-tenant ones. " +
			"Give the credential per request from the trigger widget rather than storing it in the flow. A malformed query is permanent and will not be retried; an unreachable or overloaded backend is retryable.",
		Tags: []string{"Observability", "Metrics", "Agent"},
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

	endpoint, form := buildQuery(set, in)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return c.handleError(ctx, handler, in.Context, module.Permanent(err))
	}
	// POST rather than GET: a real PromQL expression easily exceeds what some
	// proxies allow in a URL, and the API accepts either.
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	applyAuth(req, set, in)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return c.handleError(ctx, handler, in.Context, module.Retryable(fmt.Errorf("query %s: %w", endpoint, err)))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.handleError(ctx, handler, in.Context, module.Retryable(fmt.Errorf("read response: %w", err)))
	}

	if resp.StatusCode >= 400 {
		return c.handleError(ctx, handler, in.Context, classify(resp.StatusCode, body))
	}

	series, truncated, err := parse(body)
	if err != nil {
		return c.handleError(ctx, handler, in.Context, module.Permanent(err))
	}

	return handler(ctx, ResultPort, Result{
		Context:   in.Context,
		Series:    series,
		Count:     len(series),
		Truncated: truncated,
	})
}

// buildQuery picks the instant or range endpoint and its parameters.
func buildQuery(set Settings, in Request) (string, url.Values) {
	base := strings.TrimSuffix(set.BaseURL, "/")
	form := url.Values{}
	form.Set("query", in.Query)

	if in.RangeMinutes <= 0 {
		return base + "/api/v1/query", form
	}

	end := time.Now()
	start := end.Add(-time.Duration(in.RangeMinutes) * time.Minute)
	step := in.StepSeconds
	if step <= 0 {
		// Choose a step that keeps a series under the point cap, so a wide
		// window does not silently return a truncated line.
		step = (in.RangeMinutes * 60) / maxPoints
		if step < 15 {
			step = 15
		}
	}
	form.Set("start", strconv.FormatInt(start.Unix(), 10))
	form.Set("end", strconv.FormatInt(end.Unix(), 10))
	form.Set("step", strconv.Itoa(step))
	return base + "/api/v1/query_range", form
}

func applyAuth(req *http.Request, set Settings, in Request) {
	switch {
	case in.Username != "" && in.Token != "":
		// Grafana Cloud's shape: instance id as user, token as password.
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

// classify separates a failure the flow can retry from one it cannot. A bad
// query returns 400 forever, so retrying only repeats it; a 429 or 5xx is the
// backend asking for time.
func classify(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 400 {
		msg = msg[:400]
	}
	err := fmt.Errorf("metrics API %d: %s", status, msg)
	if status == http.StatusTooManyRequests || status >= 500 {
		return module.Retryable(err)
	}
	return module.Permanent(err)
}

// promResponse is the envelope both query endpoints share.
type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			// A vector carries one sample, a matrix a list; the API uses
			// different keys for them.
			Value  []interface{}   `json:"value"`
			Values [][]interface{} `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func parse(body []byte) ([]Series, bool, error) {
	var resp promResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, false, fmt.Errorf("metrics API returned a body that is not JSON: %w", err)
	}
	// The API answers 200 with status:"error" for some failures, so the HTTP
	// code alone is not enough to trust the payload.
	if resp.Status != "" && resp.Status != "success" {
		return nil, false, fmt.Errorf("metrics API error: %s", resp.Error)
	}

	truncated := len(resp.Data.Result) > maxSeries
	series := make([]Series, 0, len(resp.Data.Result))

	for i, r := range resp.Data.Result {
		if i >= maxSeries {
			break
		}
		s := Series{Labels: r.Metric}
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}

		if len(r.Value) == 2 {
			s.Value = sampleValue(r.Value[1])
		}
		for _, v := range r.Values {
			if len(v) != 2 {
				continue
			}
			if len(s.Points) >= maxPoints {
				truncated = true
				break
			}
			s.Points = append(s.Points, Point{
				At:    sampleTime(v[0]),
				Value: sampleValue(v[1]),
			})
		}
		// For a range query the newest point is the current reading, which is
		// what a threshold check wants without walking the list.
		if len(s.Points) > 0 {
			s.Value = s.Points[len(s.Points)-1].Value
		}
		series = append(series, s)
	}
	return series, truncated, nil
}

// sampleValue reads Prometheus' numeric-as-string encoding, which also carries
// NaN and Inf that a plain float parse would reject.
func sampleValue(v interface{}) float64 {
	s, ok := v.(string)
	if !ok {
		if f, isFloat := v.(float64); isFloat {
			return f
		}
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

func sampleTime(v interface{}) string {
	switch t := v.(type) {
	case float64:
		return time.Unix(int64(t), 0).UTC().Format(time.RFC3339)
	case string:
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return time.Unix(int64(f), 0).UTC().Format(time.RFC3339)
		}
	}
	return ""
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
			// A concrete example so an edge reading $.series[0].value is
			// checkable when the flow is built.
			Configuration: Result{
				Series: []Series{{
					Labels: map[string]string{"pod": "example-pod"},
					Value:  0.42,
					Points: []Point{{At: "2026-07-30T00:00:00Z", Value: 0.42}},
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
