package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/clbanning/mxj/v2"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/tiny-systems/http-module/components/etc"
	"github.com/tiny-systems/http-module/pkg/ttlmap"
	"github.com/tiny-systems/http-module/pkg/utils"
	"github.com/tiny-systems/module/api/v1alpha1"
	"github.com/tiny-systems/module/module"
	"github.com/tiny-systems/module/registry"
	"go.uber.org/atomic"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ComponentName string = "http_server"
	ResponsePort         = "response"
	RequestPort          = "request"
	StartPort            = "start"
	StopPort             = "stop"
	StatusPort           = "status"
)

type Component struct {
	//e            *echo.Echo
	settings     Settings
	settingsLock *sync.Mutex
	//
	startSettings Start
	//
	contexts *ttlmap.TTLMap

	publicListenAddrLock *sync.Mutex
	publicListenAddr     []string
	//listenPort           int

	cancelFunc     context.CancelFunc
	cancelFuncLock *sync.Mutex

	runLock *sync.Mutex

	startErr *atomic.Error
	//
	node v1alpha1.TinyNode

	// k8s client wrapper
	client module.Client
}

func (h *Component) Instance() module.Component {

	return &Component{
		//	e:                    echo.New(),
		publicListenAddr:     []string{},
		publicListenAddrLock: &sync.Mutex{},
		cancelFuncLock:       &sync.Mutex{},
		runLock:              &sync.Mutex{},
		//
		settingsLock: &sync.Mutex{},
		//
		startErr: &atomic.Error{},
		startSettings: Start{
			WriteTimeout: 10,
			ReadTimeout:  60,
			AutoHostName: true,
		},
		settings: Settings{
			EnableStatusPort: false,
			EnableStopPort:   false,
		},
	}
}

type Settings struct {
	EnableStatusPort bool `json:"enableStatusPort" required:"true" title:"Enable status port" description:"Status port notifies when server is up or down"`
	EnableStopPort   bool `json:"enableStopPort" required:"true" title:"Enable stop port" description:"Stop port allows you to stop server"`
	EnableStartPort  bool `json:"enableStartPort" required:"true" title:"Enable start port" description:"Start port allows you to start server"`
}

type StartContext any

type Start struct {
	Context      StartContext `json:"context,omitempty" configurable:"true" title:"Context" description:"Start context"`
	AutoHostName bool         `json:"autoHostName" title:"Automatically generate hostname" description:"Use cluster auto subdomain setup if any."`
	Hostnames    []string     `json:"hostnames,omitempty" title:"Hostnames"  description:"List of virtual host this server should be bound to."` //requiredWhen:"['kind', 'equal', 'enum 1']"
	ReadTimeout  int          `json:"readTimeout" required:"true" title:"Read Timeout" description:"Read timeout is the maximum duration for reading the entire request, including the body. A zero or negative value means there will be no timeout."`
	WriteTimeout int          `json:"writeTimeout" required:"true" title:"Write Timeout" description:"Write timeout is the maximum duration before timing out writes of the response. It is reset whenever a new request's header is read."`
}

type Request struct {
	Context       StartContext `json:"context"`
	RequestID     string       `json:"requestID" required:"true"`
	RequestURI    string       `json:"requestURI" required:"true"`
	RequestParams url.Values   `json:"requestParams" required:"true"`
	Host          string       `json:"host" required:"true"`
	Method        string       `json:"method" required:"true" title:"Method" enum:"GET,POST,PATCH,PUT,DELETE" enumTitles:"GET,POST,PATCH,PUT,DELETE"`
	RealIP        string       `json:"realIP"`
	Headers       []etc.Header `json:"headers,omitempty"`
	Body          any          `json:"body"`
	Scheme        string       `json:"scheme"`
}

type StartControl struct {
	Status string `json:"status" title:"Status" readonly:"true"`
	Start  bool   `json:"start" format:"button" title:"Start" required:"true" description:"Start HTTP server"`
}

type StopControl struct {
	Stop       bool     `json:"stop" format:"button" title:"Stop" required:"true" description:"Stop HTTP server"`
	Status     string   `json:"status" title:"Status" readonly:"true"`
	ListenAddr []string `json:"listenAddr" title:"Listen Address" readonly:"true"`
}

type Stop struct {
}

type Status struct {
	Context    StartContext `json:"context" title:"Context"`
	ListenAddr []string     `json:"listenAddr" title:"Listen Address" readonly:"true"`
	IsRunning  bool         `json:"isRunning" title:"Is running" readonly:"true"`
}

type Response struct {
	RequestID   string          `json:"requestID" required:"true" title:"Request ID" minLength:"1" description:"To match response with request pass request ID to response port"`
	StatusCode  int             `json:"statusCode" required:"true" title:"Status Code" description:"HTTP status code for response" minimum:"100" default:"200" maximum:"599"`
	ContentType etc.ContentType `json:"contentType" required:"true"`
	Headers     []etc.Header    `json:"headers,omitempty"  title:"Response headers"`
	Body        string          `json:"body" title:"Response body" format:"textarea"`
}

func (h *Component) GetInfo() module.ComponentInfo {
	return module.ComponentInfo{
		Name:        ComponentName,
		Description: "HTTP Server",
		Info:        "Serves HTTP requests. Each HTTP requests creates its representing message on a Request port. To display HTTP response incoming message should find its way to the Response port. Other way HTTP request timeout error will be shown.",
		Tags:        []string{"HTTP", "Server"},
	}
}

func (h *Component) stop() error {
	h.cancelFuncLock.Lock()
	defer h.cancelFuncLock.Unlock()
	if h.cancelFunc == nil {
		return nil
	}
	h.cancelFunc()

	return nil
}

func (h *Component) setCancelFunc(f func()) {
	h.cancelFuncLock.Lock()
	defer h.cancelFuncLock.Unlock()
	h.cancelFunc = f
}

func (h *Component) isRunning() bool {
	h.cancelFuncLock.Lock()
	defer h.cancelFuncLock.Unlock()

	return h.cancelFunc != nil
}

func (h *Component) start(ctx context.Context, handler module.Handler) error {
	//
	if h.client == nil {
		return fmt.Errorf("unable to start, no client available")
	}

	h.runLock.Lock()
	defer h.runLock.Unlock()

	e := echo.New()
	e.HideBanner = false
	e.HidePort = false

	serverCtx, serverCancel := context.WithCancel(ctx)
	defer serverCancel()

	h.setCancelFunc(serverCancel)
	h.contexts = ttlmap.New(ctx, h.startSettings.ReadTimeout*2)

	e.Any("*", func(c echo.Context) error {
		id, err := uuid.NewUUID()
		if err != nil {
			return err
		}

		idStr := id.String()
		requestResult := Request{
			Context:       h.startSettings.Context,
			RequestID:     idStr,
			Host:          c.Request().Host,
			Method:        c.Request().Method,
			RequestURI:    c.Request().RequestURI,
			RequestParams: c.QueryParams(),
			RealIP:        c.RealIP(),
			Scheme:        c.Scheme(),
			Headers:       make([]etc.Header, 0),
		}
		req := c.Request()

		keys := make([]string, 0, len(req.Header))
		for k := range req.Header {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			for _, v := range req.Header[k] {
				requestResult.Headers = append(requestResult.Headers, etc.Header{
					Key:   k,
					Value: v,
				})
			}
		}

		cType := req.Header.Get(etc.HeaderContentType)
		switch {
		case strings.HasPrefix(cType, etc.MIMEApplicationJSON):
			if err = c.Echo().JSONSerializer.Deserialize(c, &requestResult.Body); err != nil {
				var HTTPError *echo.HTTPError
				switch {
				case errors.As(err, &HTTPError):
					return err
				default:
					return echo.NewHTTPError(http.StatusBadRequest, err.Error()).SetInternal(err)
				}
			}
		case strings.HasPrefix(cType, etc.MIMEApplicationXML), strings.HasPrefix(cType, etc.MIMETextXML):
			mxj.SetAttrPrefix("")
			m, err := mxj.NewMapXmlReader(req.Body, false)
			if err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error()).SetInternal(err)
			}
			requestResult.Body = m.Old()

		case strings.HasPrefix(cType, etc.MIMEApplicationForm), strings.HasPrefix(cType, etc.MIMEMultipartForm):
			params, err := c.FormParams()
			if err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error()).SetInternal(err)
			}
			requestResult.Body = params
		default:
			body, _ := io.ReadAll(req.Body)
			requestResult.Body = utils.BytesToString(body)
		}

		ch := make(chan Response)
		h.contexts.Put(idStr, ch)
		defer close(ch)

		doneCh := make(chan struct{})
		go func() {
			defer close(doneCh)

			for {
				select {
				case <-c.Request().Context().Done():
					return

				case <-ctx.Done():
					return

				case <-time.Tick(time.Duration(h.startSettings.ReadTimeout) * time.Second):
					c.Error(fmt.Errorf("read timeout"))
					return

				case resp := <-ch:
					for _, header := range resp.Headers {
						c.Response().Header().Set(header.Key, header.Value)
					}
					if resp.ContentType != "" {
						c.Response().Header().Set("Content-Type", string(resp.ContentType))
					}
					_ = c.String(resp.StatusCode, fmt.Sprintf("%v", resp.Body))
					return
				}
			}
		}()

		if err = handler(c.Request().Context(), RequestPort, requestResult); err != nil {
			return err
		}
		<-doneCh
		return nil
	})

	e.Server.ReadTimeout = time.Duration(h.startSettings.ReadTimeout) * time.Second
	e.Server.WriteTimeout = time.Duration(h.startSettings.WriteTimeout) * time.Second

	var (
		listenPort      int
		actualLocalPort int
	)

	if annotationPort, err := strconv.Atoi(h.node.Labels[v1alpha1.SuggestedHttpPortAnnotation]); err == nil {
		listenPort = annotationPort
	}

	if len(h.startSettings.Hostnames) == 1 {
		portParts := strings.Split(h.startSettings.Hostnames[0], ":")
		if len(portParts) == 2 {
			// we have single hostname defined with explicit port defined, try parse it and use it
			if port, err := strconv.Atoi(portParts[1]); err == nil {
				listenPort = port
			}
		}
	}

	var listenAddr = ":0"

	if listenPort > 0 {
		listenAddr = fmt.Sprintf(":%d", listenPort)
	}

	go func() {
		err := e.Start(listenAddr)
		if errors.Is(err, http.ErrServerClosed) {
			return
		}
		h.startErr.Store(err)
	}()

	time.Sleep(time.Millisecond * 1500)

	if e.Listener != nil {
		if tcpAddr, ok := e.Listener.Addr().(*net.TCPAddr); ok {
			//

			actualLocalPort = tcpAddr.Port
			//
			exposeCtx, exposeCancel := context.WithTimeout(ctx, time.Second*30)
			defer exposeCancel()

			// upgrade
			// hostname it's a last part of the node name
			var autoHostName string

			if h.startSettings.AutoHostName {
				autoHostNameParts := strings.Split(h.node.Name, ".")
				autoHostName = autoHostNameParts[len(autoHostNameParts)-1]
			}

			publicURLs, err := h.client.ExposePort(exposeCtx, autoHostName, h.startSettings.Hostnames, tcpAddr.Port)
			if err != nil {
				return err
			}
			h.setPublicListerAddr(publicURLs)
		}
	}

	// send status that we run
	_ = h.sendStatus(ctx, h.startSettings.Context, handler)
	// ask to reconcile (redraw the component)

	<-serverCtx.Done()

	shutdownCtx, shutDownCancel := context.WithTimeout(context.Background(), time.Second*30)
	defer shutDownCancel()

	_ = e.Shutdown(shutdownCtx)
	h.setCancelFunc(nil)

	//
	discloseCtx, discloseCancel := context.WithTimeout(context.Background(), time.Second*30)
	defer discloseCancel()

	_ = h.client.DisclosePort(discloseCtx, actualLocalPort)

	// send status when we stopped
	_ = h.sendStatus(discloseCtx, h.startSettings.Context, handler)
	// ask to reconcile (redraw the component)

	return h.startErr.Load()
}

func (h *Component) setPublicListerAddr(addr []string) {
	h.publicListenAddrLock.Lock()
	defer h.publicListenAddrLock.Unlock()
	h.publicListenAddr = addr
}

func (h *Component) getPublicListerAddr() []string {
	h.publicListenAddrLock.Lock()
	defer h.publicListenAddrLock.Unlock()
	return h.publicListenAddr
}

func (h *Component) Handle(ctx context.Context, handler module.Handler, port string, msg interface{}) error {

	switch port {
	case module.NodePort:
		h.node, _ = msg.(v1alpha1.TinyNode)

	case module.ClientPort:
		h.client, _ = msg.(module.Client)

	case module.ControlPort:
		if msg == nil {
			break
		}

		switch msg.(type) {
		case StartControl:
			return h.start(ctx, handler)

		case StopControl:
			return h.stop()
		}

	case module.SettingsPort:
		in, ok := msg.(Settings)
		if !ok {
			return fmt.Errorf("invalid settings message")
		}

		h.settingsLock.Lock()
		h.settings = in
		h.settingsLock.Unlock()

	case StartPort:
		in, ok := msg.(Start)
		if !ok {
			return fmt.Errorf("invalid start message")
		}

		h.startSettings = in
		return h.start(ctx, handler)

	case StopPort:
		return h.stop()

	case ResponsePort:
		in, ok := msg.(Response)
		if !ok {
			return fmt.Errorf("invalid response message")
		}

		if h.contexts == nil {
			return fmt.Errorf("unknown request ID %s", in.RequestID)
		}

		ch := h.contexts.Get(in.RequestID)
		if ch == nil {
			return fmt.Errorf("context '%s' not found", in.RequestID)
		}

		if respChannel, ok := ch.(chan Response); ok {
			h.contexts.Delete(in.RequestID)
			respChannel <- in
		}

	default:
		return fmt.Errorf("port %s is not supported", port)
	}

	return nil
}

func (h *Component) getControl() interface{} {
	if h.isRunning() {
		return StopControl{
			Status:     "Running",
			ListenAddr: h.getPublicListerAddr(),
		}
	}
	return StartControl{
		Status: "Not running",
	}
}

func (h *Component) Ports() []module.Port {

	h.settingsLock.Lock()
	defer h.settingsLock.Unlock()

	ports := []module.Port{
		{
			Name: module.NodePort, // to receive tiny node instance
		},
		{
			Name: module.ClientPort, // to receive k8s client
		},
		{
			Name:          module.SettingsPort,
			Label:         "Settings",
			Configuration: h.settings,
			Source:        true,
		},
		{
			Name:          RequestPort,
			Label:         "Request",
			Configuration: Request{},
			Position:      module.Right,
		},
		{
			Name:     ResponsePort,
			Label:    "Response",
			Source:   true,
			Position: module.Right,
			Configuration: Response{
				StatusCode: 200,
			},
		},
		{
			Name:          module.ControlPort,
			Label:         "Dashboard",
			Configuration: h.getControl(),
		},
	}

	if h.settings.EnableStartPort {

		ports = append(ports, module.Port{
			Name:          StartPort,
			Label:         "Start",
			Source:        true,
			Position:      module.Left,
			Configuration: h.startSettings,
		})
	}

	// programmatically stop server
	if h.settings.EnableStopPort {
		ports = append(ports, module.Port{
			Position:      module.Left,
			Name:          StopPort,
			Label:         "Stop",
			Source:        true,
			Configuration: Stop{},
		})
	}

	// programmatically use status in flows

	if h.settings.EnableStatusPort {
		ports = append(ports, module.Port{
			Position:      module.Bottom,
			Name:          StatusPort,
			Label:         "Status",
			Configuration: h.getStatus(),
		})
	}
	return ports
}

func (h *Component) getStatus() Status {
	return Status{
		ListenAddr: h.getPublicListerAddr(),
		IsRunning:  h.isRunning(),
	}
}

func (h *Component) sendStatus(ctx context.Context, start StartContext, handler module.Handler) error {
	_ = handler(ctx, module.ReconcilePort, nil)

	if !h.settings.EnableStatusPort {
		return nil
	}
	return handler(ctx, StatusPort, Status{
		Context:    start,
		ListenAddr: h.getPublicListerAddr(),
		IsRunning:  h.isRunning(),
	})
}

var _ module.Component = (*Component)(nil)

func init() {
	registry.Register(&Component{})
}
