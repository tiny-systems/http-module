package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
	"github.com/tiny-systems/http-module/components/etc"
	"github.com/tiny-systems/http-module/components/server/portmanager"
	"github.com/tiny-systems/http-module/pkg/utils"
	"github.com/tiny-systems/module/api/v1alpha1"
	"github.com/tiny-systems/module/module"
	moduleutils "github.com/tiny-systems/module/pkg/utils"
	"github.com/tiny-systems/module/registry"
)

const (
	ComponentName string = "http_server"
	ResponsePort         = "response"
	RequestPort          = "request"
	StartPort            = "start"
	StatusPort           = "status"

	// Metadata keys for multi-pod state persistence
	metadataKeyStart = "http-start" // Start config JSON for all pods to read
	metadataKeyPort  = "port"       // Listen port for all pods to bind to
)

type Component struct {
	settings     Settings
	settingsLock *sync.Mutex

	startSettings Start

	publicListenAddrLock *sync.RWMutex
	publicListenAddr     []string

	cancelFunc     context.CancelFunc
	cancelFuncLock *sync.Mutex

	startStopLock *sync.Mutex

	listenPortLock *sync.RWMutex
	listenPort     int

	nodeName   string
	sourceNode string // node that triggered Start (for ownership)
	handler    module.Handler

	// port manager for exposing/disclosing ports via K8s Service/Ingress
	portMgr *portmanager.Manager
}

func (h *Component) Instance() module.Component {
	return &Component{
		publicListenAddr:     []string{},
		publicListenAddrLock: &sync.RWMutex{},
		cancelFuncLock:       &sync.Mutex{},
		startStopLock:        &sync.Mutex{},
		listenPortLock:       &sync.RWMutex{},
		settingsLock:         &sync.Mutex{},
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
	Hostnames    []string     `json:"hostnames,omitempty" title:"Hostnames"  description:"List of virtual host this server should be bound to."`
	ReadTimeout  int          `json:"readTimeout" required:"true" title:"Read Timeout" description:"Read timeout is the maximum duration for reading the entire request in seconds, including the body. A zero or negative value means there will be no timeout."`
	WriteTimeout int          `json:"writeTimeout" required:"true" title:"Write Timeout" description:"Write timeout is the maximum duration before timing out writes of the response in seconds. It is reset whenever a new request's header is read."`
}

type Request struct {
	Context       StartContext `json:"context"`
	RequestURI    string       `json:"requestURI" required:"true"`
	RequestParams url.Values   `json:"requestParams" required:"true"`
	Host          string       `json:"host" required:"true"`
	Method        string       `json:"method" required:"true" title:"Method" enum:"GET,POST,PATCH,PUT,DELETE" enumTitles:"GET,POST,PATCH,PUT,DELETE"`
	RealIP        string       `json:"realIP"`
	Headers       []etc.Header `json:"headers,omitempty"`
	Body          string       `json:"body"`
	Scheme        string       `json:"scheme"`
	PodName       string       `json:"podName" title:"Pod Name" description:"Name of the pod handling this request"`
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
	StatusCode  int             `json:"statusCode" required:"true" title:"Status Code" description:"HTTP status code for response" minimum:"100" default:"200" maximum:"599"`
	ContentType etc.ContentType `json:"contentType" required:"true"`
	Headers     []etc.Header    `json:"headers,omitempty"  title:"Response headers"`
	Body        string          `json:"body" title:"Response body" format:"textarea"`
}

func (h *Component) GetInfo() module.ComponentInfo {
	return module.ComponentInfo{
		Name:        ComponentName,
		Description: "HTTP Server",
		Info:        "HTTP request handler. Start port receives configuration and starts the server (blocks until stopped). Each incoming HTTP request emits on Request port. Wire Request to processing logic, then wire result to Response port with statusCode, contentType, headers, body.",
		Tags:        []string{"HTTP", "Server"},
	}
}

func (h *Component) stop() error {
	h.cancelFuncLock.Lock()
	defer h.cancelFuncLock.Unlock()
	if h.cancelFunc == nil {
		return nil
	}
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

func (h *Component) start(ctx context.Context, handler module.Handler) error {
	if h.portMgr == nil {
		return fmt.Errorf("unable to start, no port manager available")
	}

	log.Info().Msg("http-server start: entering")

	e := echo.New()
	e.HideBanner = true
	e.HidePort = false

	serverCtx, serverCancel := context.WithCancel(ctx)
	defer serverCancel()

	go func() {
		<-ctx.Done()
		log.Info().Msg("http-server start: parent context cancelled, stopping server")
		serverCancel()
	}()

	h.setCancelFunc(serverCancel)

	e.Any("*", func(c echo.Context) error {
		requestResult := Request{
			Context:       h.startSettings.Context,
			Host:          c.Request().Host,
			Method:        c.Request().Method,
			RequestURI:    c.Request().RequestURI,
			RequestParams: c.QueryParams(),
			RealIP:        c.RealIP(),
			Scheme:        c.Scheme(),
			Headers:       make([]etc.Header, 0),
			PodName:       os.Getenv("HOSTNAME"),
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

		body, err := io.ReadAll(req.Body)
		if err != nil {
			return fmt.Errorf("read request body: %w", err)
		}
		requestResult.Body = utils.BytesToString(body)

		resp := handler(c.Request().Context(), RequestPort, requestResult)
		if err := moduleutils.CheckForError(resp); err != nil {
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

	var actualLocalPort int
	var listenPort int

	// Try to get port from node metadata (for multi-pod load balancing)
	// This is set by the first pod that starts and stored in Status.Metadata["port"]
	if len(h.startSettings.Hostnames) == 1 {
		// Parse port from hostname if specified
		portParts := strings.Split(h.startSettings.Hostnames[0], ":")
		if len(portParts) == 2 {
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
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		log.Error().Err(err).Int("port", listenPort).Msg("failed to start HTTP server")
		serverCancel()
	}()

	time.Sleep(time.Millisecond * 1500)

	if e.Listener == nil {
		log.Error().Msg("HTTP server failed to bind - listener is nil")
		h.setListenPort(0)
		return fmt.Errorf("server failed to bind")
	}

	if tcpAddr, ok := e.Listener.Addr().(*net.TCPAddr); ok {
		actualLocalPort = tcpAddr.Port
		log.Info().Int("port", actualLocalPort).Msg("HTTP server started successfully")

		time.Sleep(time.Second)
		h.setListenPort(actualLocalPort)

		exposeCtx, exposeCancel := context.WithTimeout(ctx, time.Second*30)
		defer exposeCancel()

		var autoHostName string
		if h.startSettings.AutoHostName || len(h.startSettings.Hostnames) == 0 {
			autoHostNameParts := strings.Split(h.nodeName, ".")
			autoHostName = autoHostNameParts[len(autoHostNameParts)-1]
		}

		publicURLs, err := h.portMgr.ExposePort(exposeCtx, autoHostName, h.startSettings.Hostnames, tcpAddr.Port)
		if err != nil {
			log.Error().Err(err).Msg("failed to expose port")
			publicURLs = []string{fmt.Sprintf("http://localhost:%d", tcpAddr.Port)}
		}

		h.setPublicListenAddr(publicURLs)

		// Update node metadata with actual port for other pods to use
		_ = handler(context.Background(), v1alpha1.ReconcilePort, func(n *v1alpha1.TinyNode) error {
			if n.Status.Metadata == nil {
				n.Status.Metadata = make(map[string]string)
			}
			n.Status.Metadata[metadataKeyPort] = fmt.Sprintf("%d", actualLocalPort)
			return nil
		})
	}

	_ = handler(context.Background(), v1alpha1.ReconcilePort, nil)

	log.Info().Msg("http-server start: waiting on serverCtx.Done()")

	<-serverCtx.Done()

	log.Info().Msg("http-server start: serverCtx done, shutting down")

	shutdownCtx, shutDownCancel := context.WithTimeout(context.Background(), time.Second*30)
	defer shutDownCancel()

	_ = e.Shutdown(shutdownCtx)

	h.setCancelFunc(nil)
	h.setListenPort(0)

	if actualLocalPort > 0 {
		discloseCtx, discloseCancel := context.WithTimeout(context.Background(), time.Second*30)
		defer discloseCancel()
		_ = h.portMgr.DisclosePort(discloseCtx, actualLocalPort)
	}

	h.setPublicListenAddr([]string{})

	log.Info().Msg("http-server start: exiting")

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
	h.listenPortLock.RLock()
	defer h.listenPortLock.RUnlock()
	return h.listenPort
}

func (h *Component) getPublicListerAddr() []string {
	h.publicListenAddrLock.RLock()
	defer h.publicListenAddrLock.RUnlock()
	return h.publicListenAddr
}

func (h *Component) Handle(ctx context.Context, handler module.Handler, port string, msg interface{}) any {
	h.handler = handler

	switch port {
	case v1alpha1.ReconcilePort:
		if node, ok := msg.(v1alpha1.TinyNode); ok {
			h.nodeName = node.Name

			// Read from metadata for multi-pod state restoration
			if node.Status.Metadata != nil {
				// Read port from metadata
				if portStr, ok := node.Status.Metadata[metadataKeyPort]; ok {
					if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
						h.listenPortLock.Lock()
						h.listenPort = p
						h.listenPortLock.Unlock()
					}
				}

				// If not running but Start config exists in metadata, start the server
				// This enables multi-pod load balancing - all pods read the same config
				if !h.isRunning() {
					if startStr, ok := node.Status.Metadata[metadataKeyStart]; ok && startStr != "" {
						var startCfg Start
						if err := json.Unmarshal([]byte(startStr), &startCfg); err == nil && (startCfg.ReadTimeout > 0 || startCfg.WriteTimeout > 0) {
							log.Info().Interface("start", startCfg).Msg("http_server: restoring from metadata")
							h.startSettings = startCfg

							// Start server in goroutine to not block reconcile
							go func() {
								h.startStopLock.Lock()
								defer h.startStopLock.Unlock()

								if h.isRunning() {
									return
								}

								log.Info().Msg("http_server: starting server from metadata restoration")
								err := h.start(context.Background(), handler)
								if err != nil {
									log.Error().Err(err).Msg("http_server: server stopped after metadata restoration")
								}
							}()
						}
					}
				}
			}
		}
		return nil

	case v1alpha1.ClientPort:
		if k8sProvider, ok := msg.(module.K8sClient); ok {
			h.portMgr = portmanager.New(k8sProvider.GetK8sClient(), k8sProvider.GetNamespace())
		}

	case v1alpha1.SettingsPort:
		in, ok := msg.(Settings)
		if !ok {
			return fmt.Errorf("invalid settings message")
		}
		h.settingsLock.Lock()
		defer h.settingsLock.Unlock()
		h.settings = in

	case StartPort:
		// StartPort can receive Start config directly or as []byte from blocking TinyState
		var in Start
		switch v := msg.(type) {
		case Start:
			in = v
		case []byte:
			// Received from blocking TinyState via controller
			if v == nil {
				// nil means blocking state was deleted - stop server and clear metadata
				log.Info().Msg("http_server: StartPort received nil (state deleted), stopping")
				_ = h.stop()

				// Clear start config from metadata so other pods don't restart
				_ = handler(context.Background(), v1alpha1.ReconcilePort, func(n *v1alpha1.TinyNode) error {
					if n.Status.Metadata != nil {
						delete(n.Status.Metadata, metadataKeyStart)
						delete(n.Status.Metadata, metadataKeyPort)
					}
					return nil
				})

				return nil
			}
			// Try to unmarshal as Start struct first
			if err := json.Unmarshal(v, &in); err == nil && (in.ReadTimeout > 0 || in.WriteTimeout > 0 || in.Context != nil) {
				// Successfully parsed as Start struct with meaningful values
				log.Info().Interface("start", in).Msg("http_server: parsed as Start struct")
			} else {
				// Data is raw context from Signal - wrap it in Start.Context
				log.Info().Msg("http_server: received raw context, wrapping in Start struct")
				in = h.startSettings
				var ctx StartContext
				if err := json.Unmarshal(v, &ctx); err == nil {
					in.Context = ctx
				}
				log.Info().Interface("start", in).Msg("http_server: wrapped context in Start")
			}
		default:
			return fmt.Errorf("invalid start message: expected Start or []byte, got %T", msg)
		}

		// Get source node from context for ownership
		sourceNode := moduleutils.GetSourceNode(ctx)
		h.sourceNode = sourceNode
		h.startSettings = in

		isLeader := moduleutils.IsLeader(ctx)

		log.Info().
			Str("sourceNode", sourceNode).
			Bool("isLeader", isLeader).
			Bool("isRunning", h.isRunning()).
			Msg("http_server: StartPort received")

		// Persist Start config to metadata so all pods can read it
		startBytes, _ := json.Marshal(in)
		_ = handler(context.Background(), v1alpha1.ReconcilePort, func(n *v1alpha1.TinyNode) error {
			if n.Status.Metadata == nil {
				n.Status.Metadata = make(map[string]string)
			}
			n.Status.Metadata[metadataKeyStart] = string(startBytes)
			return nil
		})

		// If already running, just return (continuous reconciliation)
		if h.isRunning() {
			log.Info().Msg("http_server: already running, skipping start")
			return nil
		}

		// Start the server (blocking)
		log.Info().Msg("http_server: starting server from StartPort")
		err := h.start(ctx, handler)

		log.Info().
			Err(err).
			Msg("http_server: server stopped")

		// Trigger reconcile to update UI
		_ = handler(context.Background(), v1alpha1.ReconcilePort, nil)

		return err

	case ResponsePort:
		in, ok := msg.(Response)
		if !ok {
			return fmt.Errorf("invalid response message")
		}
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
		{
			Name: v1alpha1.ClientPort,
		},
		{
			Name: v1alpha1.ReconcilePort,
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

	// Start port - receives Start config (from blocking TinyState via controller)
	ports = append(ports, module.Port{
		Name:          StartPort,
		Label:         "Start",
		Position:      module.Left,
		Configuration: h.startSettings,
	})

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

var _ module.Component = (*Component)(nil)

func init() {
	registry.Register((&Component{}).Instance())
}
