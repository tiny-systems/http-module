package client

import (
	"bytes"
	"context"
	"fmt"
	"github.com/clbanning/mxj/v2"
	"github.com/spyzhov/ajson"
	"github.com/tiny-systems/http-module/components/etc"
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
	EnableErrorPort bool `json:"enableErrorPort" required:"true" title:"Enable Error Port" description:"If request may fail, error port will emit an error message"`
}

type Request struct {
	Context Context        `json:"context,omitempty" configurable:"true" title:"Context" description:"Message to be sent further"`
	Request RequestRequest `json:"request" title:"Request" required:"true" description:"HTTP Request"`

	ResponseContentType etc.ContentType `json:"responseContentType,omitempty" title:"Response Content Type" description:"Override response content type"`
	ResponseBody        ResponseBody    `json:"responseBody" configurable:"true" title:"Response Body Example" description:"Define response body struct"`
}

type ResponseBody any

type RequestRequest struct {
	Method  string `json:"method" required:"true" title:"Method" enum:"GET,POST,PATCH,PUT,DELETE" enumTitles:"GET,POST,PATCH,PUT,DELETE" colSpan:"col-span-6"`
	Timeout int    `json:"timeout" required:"true" title:"Request Timeout" colSpan:"col-span-6"`

	URL         string          `json:"url" required:"true" title:"URL" format:"uri"`
	ContentType etc.ContentType `json:"contentType" title:"Request Content Type" required:"true"`
	Headers     []etc.Header    `json:"headers,omitempty" title:"Headers"`
	Body        any             `json:"body" configurable:"true" title:"Request Body"`
}

type Response struct {
	Context  Context          `json:"context" configurable:"true" required:"true" title:"Context" description:"Message to be sent further"`
	Response ResponseResponse `json:"response" title:"Response" required:"true" description:"HTTP Response"`
}

type ResponseResponse struct {
	Headers    []etc.Header `json:"headers" required:"true" title:"Headers"`
	Status     string       `json:"status"`
	StatusCode int          `json:"statusCode"`
	Body       ResponseBody `json:"body" configurable:"false" title:"Body"`
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
	return &Component{}
}

func (h *Component) GetInfo() module.ComponentInfo {
	return module.ComponentInfo{
		Name:        ComponentName,
		Description: "HTTP Client",
		Info:        "Performs HTTP requests.",
		Tags:        []string{"HTTP", "Component"},
	}
}

func (h *Component) Handle(ctx context.Context, handler module.Handler, port string, msg interface{}) error {

	switch port {
	case module.SettingsPort:
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

		ctx, cancel := context.WithTimeout(ctx, time.Second*time.Duration(in.Request.Timeout))
		defer cancel()

		var requestBody []byte

		switch in.Request.ContentType {
		case etc.MIMEApplicationXML:

		case etc.MIMEApplicationJSON:

		case etc.MIMETextHTML:

		case etc.MimeTextPlain:

		case etc.MIMEApplicationForm:

		case etc.MIMEMultipartForm:
		}

		req, err := http.NewRequestWithContext(ctx, in.Request.Method, in.Request.URL, bytes.NewReader(requestBody))
		if err != nil {
			return err
		}
		for _, header := range in.Request.Headers {
			req.Header.Set(header.Key, header.Value)
		}
		c := http.Client{}
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		cType := resp.Header.Get(etc.HeaderContentType)

		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		var result interface{}

		switch {
		case strings.HasPrefix(cType, etc.MIMEApplicationJSON) || in.ResponseContentType == etc.MIMEApplicationJSON:
			root, err := ajson.Unmarshal(b)

			if err != nil {
				if !h.settings.EnableErrorPort {
					return err
				}
				return handler(ctx, ErrorPort, Error{
					Error: err.Error(),
				})
			}

			result, err = root.Unpack()
			if err != nil {
				if !h.settings.EnableErrorPort {
					return err
				}
				return handler(ctx, ErrorPort, Error{
					Error: err.Error(),
				})
			}

		case strings.HasPrefix(cType, etc.MIMEApplicationXML) || strings.HasPrefix(cType, etc.MIMETextXML) || in.ResponseContentType == etc.MIMEApplicationXML:

			mxj.SetAttrPrefix("")
			m, err := mxj.NewMapXml(b, false)
			if err != nil {
				if !h.settings.EnableErrorPort {
					return err
				}
				return handler(ctx, ErrorPort, Error{
					Error: err.Error(),
				})
			}

			result = m.Old()

		default:
			builder := strings.Builder{}
			builder.Write(b)
			result = builder.String()
		}

		var headers []etc.Header
		for k, v := range resp.Header {
			for _, vv := range v {
				headers = append(headers, etc.Header{
					Key:   k,
					Value: vv,
				})
			}
		}

		if resp.StatusCode >= 400 {
			// error range
			// send to error port

			if !h.settings.EnableErrorPort {
				return fmt.Errorf(fmt.Sprint(result))
			}

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
			Name:   RequestPort,
			Label:  "Request",
			Source: true,
			Configuration: Request{
				Request: RequestRequest{
					Method:      http.MethodGet,
					Headers:     make([]etc.Header, 0),
					URL:         "",
					Timeout:     10,
					ContentType: "application/json",
				},
			},
			Position: module.Left,
		},

		{
			Name:          ResponsePort,
			Label:         "Response",
			Position:      module.Right,
			Configuration: Response{},
		},

		{
			Name:          module.SettingsPort,
			Label:         "Settings",
			Configuration: h.settings,
			Source:        true,
		},
	}

	if !h.settings.EnableErrorPort {
		return ports
	}

	return append(ports, module.Port{
		Name:          ErrorPort,
		Label:         "Error",
		Source:        false,
		Position:      module.Bottom,
		Configuration: Error{},
	})
}

var _ module.Component = (*Component)(nil)

func init() {
	registry.Register(&Component{})
}
