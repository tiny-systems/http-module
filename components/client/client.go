package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tiny-systems/http-module/components/etc"
	"github.com/tiny-systems/module/api/v1alpha1"
	"github.com/tiny-systems/module/module"
	"github.com/tiny-systems/module/registry"
)

const (
	ComponentName = "http_request"
	RequestPort   = "request"
	ResponsePort  = "response"
	ErrorPort     = "error"
)

// defaultMaxResponseBytes bounds the response read. An unbounded ReadAll of a
// hostile or misconfigured endpoint would OOM the pod.
const defaultMaxResponseBytes = 10 * 1024 * 1024

type Context any

type Settings struct {
	EnableErrorPort  bool `json:"enableErrorPort" required:"true" title:"Enable Error Port" description:"If request fail, error port will emit an error message. HTTP responses with status code >= 400 will emit an error message."`
	MaxResponseBytes int  `json:"maxResponseBytes" title:"Max Response Bytes" description:"Cap on response body size; larger responses fail rather than OOM the pod. Defaults to 10485760 (10MB)."`
}

type Request struct {
	Context           Context         `json:"context,omitempty" configurable:"true" title:"Context" description:"Message to be sent further"`
	Method            string          `json:"method" required:"true" title:"Method" enum:"GET,POST,PATCH,PUT,DELETE" enumTitles:"GET,POST,PATCH,PUT,DELETE" colSpan:"col-span-6"`
	Timeout           int             `json:"timeout" required:"true" title:"Request Timeout" colSpan:"col-span-6"`
	URL               string          `json:"url" required:"true" title:"URL" format:"uri"`
	BearerToken       string          `json:"bearerToken,omitempty" format:"password" title:"Bearer Token" description:"Sets the Authorization: Bearer header. Wins over basic auth if both are set; an explicit Authorization header below overrides either."`
	BasicAuthUser     string          `json:"basicAuthUser,omitempty" title:"Basic Auth User" colSpan:"col-span-6"`
	BasicAuthPassword string          `json:"basicAuthPassword,omitempty" format:"password" title:"Basic Auth Password" colSpan:"col-span-6"`
	Headers           []etc.Header    `json:"headers,omitempty" title:"Headers"`
	ContentType       etc.ContentType `json:"contentType" title:"Request Content Type" required:"true"`
	Body              string          `json:"body" title:"Request Body" format:"textarea"`
}

type Response struct {
	Context  Context          `json:"context" configurable:"true" required:"true" title:"Context" description:"Message to be sent further"`
	Response ResponseResponse `json:"response" title:"Response" required:"true" description:"HTTP Response"`
}

type TLSInfo struct {
	NotAfter        string   `json:"notAfter" title:"Not After" description:"Certificate expiry date in RFC3339 format"`
	NotBefore       string   `json:"notBefore" title:"Not Before" description:"Certificate start date in RFC3339 format"`
	Issuer          string   `json:"issuer" title:"Issuer"`
	Subject         string   `json:"subject" title:"Subject"`
	DNSNames        []string `json:"dnsNames" title:"DNS Names"`
	DaysUntilExpiry int      `json:"daysUntilExpiry" title:"Days Until Expiry"`
}

type ResponseResponse struct {
	Headers    []etc.Header `json:"headers" required:"true" title:"Headers"`
	Status     string       `json:"status"`
	StatusCode int          `json:"statusCode"`
	Body       string       `json:"body" configurable:"false" title:"Body"`
	TLS        *TLSInfo     `json:"tls,omitempty" title:"TLS" description:"TLS certificate information (HTTPS only)"`
}

type Error struct {
	Context   Context          `json:"context" configurable:"true" required:"true" title:"Context" description:"Message to be sent further"`
	Error     string           `json:"error" required:"true"`
	Retryable bool             `json:"retryable" title:"Retryable" description:"True for network failures, 429 (rate limit), and 5xx responses — wire the error port into the retry component to retry these with backoff. False for 4xx (client errors won't get better on retry)."`
	Response  ResponseResponse `json:"response"`
}

type Component struct {
	settings Settings
}

func (h *Component) Instance() module.Component {
	return &Component{
		Settings{MaxResponseBytes: defaultMaxResponseBytes},
	}
}

func (h *Component) GetInfo() module.ComponentInfo {
	return module.ComponentInfo{
		Name:        ComponentName,
		Description: "HTTP Client",
		Info:        "Outbound HTTP request maker. Request port receives: context, method, timeout, URL, bearerToken, basicAuthUser/basicAuthPassword, headers, contentType, body. Auth fields set the Authorization header (bearer wins over basic; an explicit Authorization header overrides both). Blocks until HTTP response received. Response bodies are capped by maxResponseBytes in settings (default 10MB); larger responses fail rather than OOM the pod. On success (status < 400): emits context + response on Response port. On failure or status >= 400: returns error, or if enableErrorPort=true in settings, emits on Error port instead.",
		Tags:        []string{"HTTP", "Client"},
	}
}

// OnSettings receives Settings from the SettingsPort.
func (h *Component) OnSettings(_ context.Context, msg any) error {
	in, ok := msg.(Settings)
	if !ok {
		return fmt.Errorf("invalid settings")
	}
	h.settings = in
	return nil
}

// Handle dispatches the RequestPort. System ports go through capabilities.
func (h *Component) Handle(ctx context.Context, handler module.Handler, port string, msg interface{}) module.Result {
	if port != RequestPort {
		return module.Fail(fmt.Errorf("port %s is not supported", port))
	}
	in, ok := msg.(Request)
	if !ok {
		return module.Fail(fmt.Errorf("invalid message"))
	}
	return h.doRequest(ctx, handler, in)
}

func (h *Component) doRequest(ctx context.Context, handler module.Handler, in Request) module.Result {
	ctx, cancel := context.WithTimeout(ctx, time.Second*time.Duration(in.Timeout))
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, in.Method, in.URL, bytes.NewReader([]byte(in.Body)))
	if err != nil {
		return h.handleError(ctx, handler, in.Context, err, ResponseResponse{})
	}

	if in.ContentType != "" {
		req.Header.Set("Content-Type", string(in.ContentType))
	}

	// Auth fields go first so an explicit Authorization header from the
	// headers array below still overrides them. Bearer wins if both are set.
	switch {
	case in.BearerToken != "":
		req.Header.Set("Authorization", "Bearer "+in.BearerToken)
	case in.BasicAuthUser != "" || in.BasicAuthPassword != "":
		req.SetBasicAuth(in.BasicAuthUser, in.BasicAuthPassword)
	}

	for _, header := range in.Headers {
		req.Header.Set(header.Key, header.Value)
	}

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		// The request never got a response (DNS/dial/timeout) — transient.
		return h.handleError(ctx, handler, in.Context, module.Retryable(err), ResponseResponse{})
	}
	defer resp.Body.Close()

	maxBytes := int64(h.settings.MaxResponseBytes)
	if maxBytes <= 0 {
		maxBytes = defaultMaxResponseBytes
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return h.handleError(ctx, handler, in.Context, module.Retryable(err), ResponseResponse{})
	}
	if int64(len(b)) > maxBytes {
		// Deliberately not retryable: the same response will be just as big
		// next time. Raise maxResponseBytes in settings if it is expected.
		return h.handleError(ctx, handler, in.Context,
			fmt.Errorf("response body exceeds the %d byte cap (maxResponseBytes setting)", maxBytes), ResponseResponse{})
	}

	body := string(b)

	var headers []etc.Header
	for k, v := range resp.Header {
		for _, vv := range v {
			headers = append(headers, etc.Header{
				Key:   k,
				Value: vv,
			})
		}
	}

	var tlsInfo *TLSInfo
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		cert := resp.TLS.PeerCertificates[0]
		tlsInfo = &TLSInfo{
			NotAfter:        cert.NotAfter.Format(time.RFC3339),
			NotBefore:       cert.NotBefore.Format(time.RFC3339),
			Issuer:          cert.Issuer.String(),
			Subject:         cert.Subject.String(),
			DNSNames:        cert.DNSNames,
			DaysUntilExpiry: int(time.Until(cert.NotAfter).Hours() / 24),
		}
	}

	respData := ResponseResponse{
		Body:       body,
		Headers:    headers,
		Status:     resp.Status,
		StatusCode: resp.StatusCode,
		TLS:        tlsInfo,
	}

	if resp.StatusCode >= 400 {
		statusErr := fmt.Errorf("%s", body)
		// 429 and 5xx are the server asking (or failing) in a way a backoff
		// retry can clear; 4xx is the caller's fault and won't improve.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			statusErr = module.Retryable(statusErr)
		}
		return h.handleError(ctx, handler, in.Context, statusErr, respData)
	}

	return handler(ctx, ResponsePort, Response{
		Response: respData,
		Context:  in.Context,
	})
}

func (h *Component) handleError(ctx context.Context, handler module.Handler, reqContext Context, err error, resp ResponseResponse) module.Result {
	if !h.settings.EnableErrorPort {
		// Bubble the error unchanged — its retryability (marked by the caller
		// with module.Retryable) rides along through Result.Err so an upstream
		// error port sees it.
		return module.Fail(err)
	}
	// Retryability is the error's own property now (module.Retryable at the
	// transient branches above); read it back with module.IsRetryable rather
	// than re-deriving from the status here. Error carries a full Response too,
	// so it keeps its own struct instead of module.ErrorMessage — but the
	// {context, error, retryable} shape matches the canonical contract.
	return handler(ctx, ErrorPort, Error{
		Context:   reqContext,
		Error:     err.Error(),
		Retryable: module.IsRetryable(err),
		Response:  resp,
	})
}

func (h *Component) Ports() []module.Port {
	ports := []module.Port{
		{
			Name:  RequestPort,
			Label: "Request",
			Configuration: Request{
				Method:      http.MethodGet,
				Headers:     make([]etc.Header, 0),
				URL:         "",
				Timeout:     10,
				ContentType: "application/json",
			},
			Position: module.Left,
		},

		{
			Name:          ResponsePort,
			Label:         "Response",
			Position:      module.Right,
			Source:        true,
			Configuration: Response{},
		},

		{
			Name:          v1alpha1.SettingsPort,
			Label:         "Settings",
			Configuration: h.settings,
		},
	}

	if !h.settings.EnableErrorPort {
		return ports
	}

	return append(ports, module.Port{
		Name:          ErrorPort,
		Label:         "Error",
		Source:        true,
		Position:      module.Bottom,
		Configuration: Error{},
	})
}

var (
	_ module.Component       = (*Component)(nil)
	_ module.SettingsHandler = (*Component)(nil)
)

func init() {
	registry.Register((&Component{}).Instance())
}
