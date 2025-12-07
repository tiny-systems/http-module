package client

import (
	"bytes"
	"context"
	"fmt"
	"github.com/tiny-systems/http-module/components/etc"
	"github.com/tiny-systems/module/api/v1alpha1"
	"github.com/tiny-systems/module/module"
	"github.com/tiny-systems/module/registry"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	ComponentName = "http_client"
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

type ResponseResponse struct {
	Headers    []etc.Header `json:"headers" required:"true" title:"Headers"`
	Status     string       `json:"status"`
	StatusCode int          `json:"statusCode"`
	Body       string       `json:"body" configurable:"false" title:"Body"`
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
		Info:        "Performs HTTP requests.",
		Tags:        []string{"HTTP", "Component"},
	}
}

func (h *Component) Handle(ctx context.Context, handler module.Handler, port string, msg interface{}) any {

	switch port {
	case v1alpha1.SettingsPort:
		// compile template
		in, ok := msg.(Settings)
		if !ok {
			return fmt.Errorf("invalid settings")
		}
		h.settings = in

		return nil

	case RequestPort:
		in, ok := msg.(Request)
		if !ok {
			return fmt.Errorf("invalid message")
		}

		ctx, cancel := context.WithTimeout(ctx, time.Second*time.Duration(in.Timeout))
		defer cancel()

		var requestBody []byte

		req, err := http.NewRequestWithContext(ctx, in.Method, in.URL, bytes.NewReader(requestBody))
		if err != nil {
			return err
		}

		for _, header := range in.Headers {
			req.Header.Set(header.Key, header.Value)
		}

		c := http.Client{}
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		defer func() {
			_ = resp.Body.Close()
		}()

		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		builder := strings.Builder{}
		builder.Write(b)
		result := builder.String()

		var headers []etc.Header
		for k, v := range resp.Header {
			for _, vv := range v {
				headers = append(headers, etc.Header{
					Key:   k,
					Value: vv,
				})
			}
		}

		if resp.StatusCode >= 400 && h.settings.EnableErrorPort {
			// error range
			// send to error port
			return handler(ctx, ErrorPort, Error{
				Context: in.Context,
				Error:   fmt.Sprint(result),
				Response: ResponseResponse{
					Body:       result,
					Headers:    headers,
					Status:     resp.Status,
					StatusCode: resp.StatusCode,
				},
			})

		}

		return handler(ctx, ResponsePort, Response{
			Response: ResponseResponse{
				Body:       result,
				Headers:    headers,
				Status:     resp.Status,
				StatusCode: resp.StatusCode,
			},
			Context: in.Context,
		})

	default:
		return fmt.Errorf("port %s is not supoprted", port)
	}
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

var _ module.Component = (*Component)(nil)

func init() {
	registry.Register(&Component{})
}
