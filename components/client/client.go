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

type Context any

type Settings struct {
	EnableErrorPort bool `json:"enableErrorPort" required:"true" title:"Enable Error Port" description:"If request fail, error port will emit an error message. HTTP responses with status code >= 400 will emit an error message."`
}

type Request struct {
	Context     Context         `json:"context,omitempty" configurable:"true" title:"Context" description:"Message to be sent further"`
	Method      string          `json:"method" required:"true" title:"Method" enum:"GET,POST,PATCH,PUT,DELETE" enumTitles:"GET,POST,PATCH,PUT,DELETE" colSpan:"col-span-6"`
	Timeout     int             `json:"timeout" required:"true" title:"Request Timeout" colSpan:"col-span-6"`
	URL         string          `json:"url" required:"true" title:"URL" format:"uri"`
	Headers     []etc.Header    `json:"headers,omitempty" title:"Headers"`
	ContentType etc.ContentType `json:"contentType" title:"Request Content Type" required:"true"`
	Body        string          `json:"body" title:"Request Body" format:"textarea"`
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
	Context  Context          `json:"context" configurable:"true" required:"true" title:"Context" description:"Message to be sent further"`
	Response ResponseResponse `json:"response"`
	Error    string           `json:"error" required:"true"`
}

type Component struct {
	settings Settings
}

func (h *Component) Instance() module.Component {
	return &Component{
		Settings{},
	}
}

func (h *Component) GetInfo() module.ComponentInfo {
	return module.ComponentInfo{
		Name:        ComponentName,
		Description: "HTTP Client",
		Info:        "Outbound HTTP request maker. Request port receives: context, method, timeout, URL, headers, contentType, body. Blocks until HTTP response received. On success (status < 400): emits context + response on Response port. On failure or status >= 400: returns error, or if enableErrorPort=true in settings, emits on Error port instead.",
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
func (h *Component) Handle(ctx context.Context, handler module.Handler, port string, msg interface{}) any {
	if port != RequestPort {
		return fmt.Errorf("port %s is not supported", port)
	}
	in, ok := msg.(Request)
	if !ok {
		return fmt.Errorf("invalid message")
	}
	return h.doRequest(ctx, handler, in)
}

func (h *Component) doRequest(ctx context.Context, handler module.Handler, in Request) any {
	ctx, cancel := context.WithTimeout(ctx, time.Second*time.Duration(in.Timeout))
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, in.Method, in.URL, bytes.NewReader([]byte(in.Body)))
	if err != nil {
		return h.handleError(ctx, handler, in.Context, err, ResponseResponse{})
	}

	if in.ContentType != "" {
		req.Header.Set("Content-Type", string(in.ContentType))
	}

	for _, header := range in.Headers {
		req.Header.Set(header.Key, header.Value)
	}

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return h.handleError(ctx, handler, in.Context, err, ResponseResponse{})
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return h.handleError(ctx, handler, in.Context, err, ResponseResponse{})
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
		return h.handleError(ctx, handler, in.Context, fmt.Errorf("%s", body), respData)
	}

	return handler(ctx, ResponsePort, Response{
		Response: respData,
		Context:  in.Context,
	})
}

func (h *Component) handleError(ctx context.Context, handler module.Handler, reqContext Context, err error, resp ResponseResponse) any {
	if !h.settings.EnableErrorPort {
		return err
	}
	return handler(ctx, ErrorPort, Error{
		Context:  reqContext,
		Error:    err.Error(),
		Response: resp,
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
