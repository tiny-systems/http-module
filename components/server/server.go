package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
	"github.com/tiny-systems/http-module/components/etc"
	"github.com/tiny-systems/http-module/pkg/utils"
	"github.com/tiny-systems/module/api/v1alpha1"
	"github.com/tiny-systems/module/module"
	utils2 "github.com/tiny-systems/module/pkg/utils"
	"github.com/tiny-systems/module/registry"
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
	StatusPort           = "status"
)

const (
	PortMetadata = "http-server-port"
)

type Component struct {
	//e            *echo.Echo
	settings     Settings
	settingsLock *sync.Mutex
	//
	startSettings Start

	publicListenAddrLock *sync.Mutex
	publicListenAddr     []string
	//listenPort           int

	cancelFunc     context.CancelFunc
	cancelFuncLock *sync.Mutex

	startStopLock *sync.Mutex // Protects start/stop sequence to prevent races

	listenPortLock *sync.Mutex
	listenPort     int

	nodeName string

	// k8s client wrapper
	client module.Client
}

func (h *Component) Instance() module.Component {

	return &Component{
		//	e:                    echo.New(),
		publicListenAddr:     []string{},
		publicListenAddrLock: &sync.Mutex{},
		cancelFuncLock:       &sync.Mutex{},
		startStopLock:        &sync.Mutex{},
		listenPortLock:       &sync.Mutex{},
		//
		settingsLock: &sync.Mutex{},
		//

		startSettings: Start{
			WriteTimeout: 10,
			ReadTimeout:  60,
			AutoHostName: true,
		},
		settings: Settings{
			EnableStatusPort: false,
		},
	}
}

type Settings struct {
	EnableStatusPort bool `json:"enableStatusPort" required:"true" title:"Enable status port" description:"Status port notifies when server is up or down"`
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
	Body          string       `json:"body"`
	Scheme        string       `json:"scheme"`
}

type Control struct {
	Status     string   `json:"status" title:"Status" readonly:"true"`
	ListenAddr []string `json:"listenAddr" title:"Listen Address" readonly:"true"`
}

type Status struct {
	Context    StartContext `json:"context" title:"Context"`
	ListenAddr []string     `json:"listenAddr" title:"Listen Address"`
	IsRunning  bool         `json:"isRunning" title:"Is running"`
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
	// stop with no error
	log.Info().Msg("stopping HTTP server")
	h.cancelFunc()
	return nil
}

func (h *Component) setCancelFunc(causeFunc context.CancelFunc) {
	h.cancelFuncLock.Lock()
	defer h.cancelFuncLock.Unlock()
	h.cancelFunc = causeFunc
}

func (h *Component) isRunning() bool {
	h.cancelFuncLock.Lock()
	defer h.cancelFuncLock.Unlock()

	return h.cancelFunc != nil
}

// start
// listenPort == master
// listenPort > 0 means it is slave
func (h *Component) start(ctx context.Context, listenPort int, handler module.Handler) error {
	//
	if h.client == nil {
		return fmt.Errorf("unable to start, no client available")
	}

	log.Info().Msgf("starting %d", listenPort)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = false

	serverCtx, serverCancel := context.WithCancel(ctx)
	defer serverCancel()

	h.setCancelFunc(serverCancel)

	//
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

		body, _ := io.ReadAll(req.Body)
		requestResult.Body = utils.BytesToString(body)

		resp := handler(c.Request().Context(), RequestPort, requestResult)
		if err = utils2.CheckForError(resp); err != nil {
			return err
		}

		respObj, ok := resp.(Response)
		if !ok {
			return fmt.Errorf("invalid response")
		}

		for _, header := range respObj.Headers {
			c.Response().Header().Set(header.Key, header.Value)
		}
		if respObj.ContentType != "" {
			c.Response().Header().Set(etc.HeaderContentType, string(respObj.ContentType))
		}
		_ = c.String(respObj.StatusCode, fmt.Sprintf("%v", respObj.Body))

		return nil
	})

	e.Server.ReadTimeout = time.Duration(h.startSettings.ReadTimeout) * time.Second
	e.Server.WriteTimeout = time.Duration(h.startSettings.WriteTimeout) * time.Second

	var (
		actualLocalPort int
	)

	if listenPort == 0 && len(h.startSettings.Hostnames) == 1 {
		// try to use user defined port as suggested
		portParts := strings.Split(h.startSettings.Hostnames[0], ":")
		if len(portParts) == 2 {
			// we have single hostname defined with explicit port defined, try parse it and use it
			if port, err := strconv.Atoi(portParts[1]); err == nil {
				listenPort = port
			}
		}
	}

	// auto suggested port
	var listenAddr = ":0"

	if listenPort > 0 {
		// if port suggested we listen to it
		listenAddr = fmt.Sprintf(":%d", listenPort)
	}

	go func() {
		err := e.Start(listenAddr)

		if err == nil {
			return
		}

		if errors.Is(err, http.ErrServerClosed) {
			return
		}

		log.Error().Err(err).Int("port", listenPort).Msg("failed to start HTTP server")
		serverCancel()
	}()

	time.Sleep(time.Millisecond * 1500)

	if e.Listener != nil {
		if tcpAddr, ok := e.Listener.Addr().(*net.TCPAddr); ok {
			actualLocalPort = tcpAddr.Port

			log.Info().Int("port", actualLocalPort).Msg("HTTP server started successfully")

			time.Sleep(time.Second)
			h.setListenPort(actualLocalPort)
			//
			exposeCtx, exposeCancel := context.WithTimeout(ctx, time.Second*30)
			defer exposeCancel()

			// upgrade
			// hostname it's a last part of the node name
			var autoHostName string

			if h.startSettings.AutoHostName {
				autoHostNameParts := strings.Split(h.nodeName, ".")
				autoHostName = autoHostNameParts[len(autoHostNameParts)-1]
			}

			publicURLs, err := h.client.ExposePort(exposeCtx, autoHostName, h.startSettings.Hostnames, tcpAddr.Port)
			if err != nil {
				log.Error().Err(err).Msg("failed to expose port")
				// failed to expose port
				publicURLs = []string{fmt.Sprintf("http://localhost:%d", tcpAddr.Port)}
			}

			h.setPublicListenAddr(publicURLs)
		}
	}

	// send status that we run is it is not slave
	if listenPort == 0 {
		_ = h.sendStatus(ctx, h.startSettings.Context, handler)
	}

	// ask to reconcile (redraw the component)

	<-serverCtx.Done()

	shutdownCtx, shutDownCancel := context.WithTimeout(context.Background(), time.Second*30)
	defer shutDownCancel()

	_ = e.Shutdown(shutdownCtx)

	h.setCancelFunc(nil)
	h.setListenPort(0)
	//

	if actualLocalPort > 0 {
		discloseCtx, discloseCancel := context.WithTimeout(context.Background(), time.Second*30)
		defer discloseCancel()

		_ = h.client.DisclosePort(discloseCtx, actualLocalPort)
	}

	h.setPublicListenAddr([]string{})

	// send status when we stopped
	if listenPort == 0 {
		_ = h.sendStatus(context.Background(), h.startSettings.Context, handler)
	}

	return serverCtx.Err()
}

func (h *Component) setPublicListenAddr(addr []string) {
	h.publicListenAddrLock.Lock()
	defer h.publicListenAddrLock.Unlock()
	h.publicListenAddr = addr
}

func (h *Component) setListenPort(port int) {
	h.listenPortLock.Lock()
	defer h.listenPortLock.Unlock()
	h.listenPort = port
}
func (h *Component) getListenPort() int {
	h.listenPortLock.Lock()
	defer h.listenPortLock.Unlock()
	return h.listenPort
}

func (h *Component) getPublicListerAddr() []string {
	h.publicListenAddrLock.Lock()
	defer h.publicListenAddrLock.Unlock()
	return h.publicListenAddr
}

func (h *Component) Handle(ctx context.Context, handler module.Handler, port string, msg interface{}) any {

	switch port {
	case v1alpha1.ReconcilePort:

		if node, ok := msg.(v1alpha1.TinyNode); ok {
			//
			// all replicas should get copy of same node
			h.nodeName = node.Name
			// start server

			listenPort, _ := strconv.Atoi(node.Status.Metadata[PortMetadata])

			if listenPort == h.getListenPort() {
				// it is the same instance
				return nil
			}

			if listenPort == 0 {
				// No port assigned yet, non-leader replicas should wait
				return nil
			}

			// Protect stop/start sequence to prevent races
			h.startStopLock.Lock()
			defer h.startStopLock.Unlock()

			// Double-check after acquiring lock (another goroutine might have started)
			if listenPort == h.getListenPort() {
				return nil
			}

			// stop if we were running
			_ = h.stop()

			// start server with suggested port
			go func() {
				// @todo best effort to start server in background
				_ = h.start(context.Background(), listenPort, handler)
			}()

		}

	case v1alpha1.ClientPort:
		h.client, _ = msg.(module.Client)

	case v1alpha1.SettingsPort:
		in, ok := msg.(Settings)
		if !ok {
			return fmt.Errorf("invalid settings message")
		}

		h.settingsLock.Lock()
		defer h.settingsLock.Unlock()

		h.settings = in

	case StartPort:
		in, ok := msg.(Start)
		if !ok {
			return fmt.Errorf("invalid start message")
		}

		h.startSettings = in
		return h.start(ctx, 0, handler)

	case ResponsePort:
		in, ok := msg.(Response)
		if !ok {
			return fmt.Errorf("invalid response message")
		}
		// we return response as final destination reached
		return in

	default:
		return fmt.Errorf("port %s is not supported", port)
	}

	return nil
}

func (h *Component) getControl() Control {
	if h.isRunning() {
		return Control{
			Status:     "Running",
			ListenAddr: h.getPublicListerAddr(),
		}
	}
	return Control{
		Status: "Not running",
	}
}

func (h *Component) Ports() []module.Port {

	h.settingsLock.Lock()
	defer h.settingsLock.Unlock()

	ports := []module.Port{
		// receive client first
		{
			Name: v1alpha1.ClientPort, // to receive k8s client wrapper
		},

		{
			Name: v1alpha1.ReconcilePort, // to receive TinyNode instance
		},

		{
			Name:          v1alpha1.SettingsPort,
			Label:         "Settings",
			Configuration: h.settings,
		},
		{
			Name:                  RequestPort,
			Label:                 "Request",
			Source:                true,
			Configuration:         Request{},
			Position:              module.Right,
			ResponseConfiguration: Response{},
		},
		{
			Name:     ResponsePort,
			Label:    "Response",
			Position: module.Right,
			Configuration: Response{
				StatusCode: 200,
			},
		},
		{
			Name:          v1alpha1.ControlPort,
			Label:         "Dashboard",
			Source:        true,
			Configuration: h.getControl(),
		},
	}

	ports = append(ports, module.Port{
		Name:          StartPort,
		Label:         "Start",
		Position:      module.Left,
		Configuration: h.startSettings,
	})

	// programmatically use status in flows

	if h.settings.EnableStatusPort {
		ports = append(ports, module.Port{
			Position:      module.Bottom,
			Name:          StatusPort,
			Label:         "Status",
			Source:        true,
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

// sendStatus changes node and sends status if it's port enabled
func (h *Component) sendStatus(ctx context.Context, _ StartContext, handler module.Handler) any {

	_ = handler(ctx, v1alpha1.ReconcilePort, func(n *v1alpha1.TinyNode) error {

		if n.Status.Metadata == nil {
			n.Status.Metadata = map[string]string{}
		}

		n.Status.Metadata = map[string]string{
			PortMetadata: fmt.Sprintf("%d", h.getListenPort()),
		}
		return nil
	})

	if !h.settings.EnableStatusPort {
		return nil
	}
	return handler(ctx, StatusPort, h.getStatus())
}

var _ module.Component = (*Component)(nil)

func init() {
	registry.Register(&Component{})
}
