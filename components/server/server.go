package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"os"
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
	PortMetadata   = "http-server-port"
	ConfigMetadata = "http-server-config"
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

	// metadataPort is synced from TinyNode metadata - source of truth for multi-pod status
	metadataPortLock *sync.Mutex
	metadataPort     int

	// restartInProgress indicates server is being stopped to restart with new context
	// When true, don't update metadata to port=0 on stop (to avoid triggering other pods to stop)
	restartInProgress     bool
	restartInProgressLock *sync.Mutex

	nodeName string

	// k8s client wrapper
	client module.Client
}

func (h *Component) Instance() module.Component {

	return &Component{
		//	e:                    echo.New(),
		publicListenAddr:      []string{},
		publicListenAddrLock:  &sync.Mutex{},
		cancelFuncLock:        &sync.Mutex{},
		startStopLock:         &sync.Mutex{},
		listenPortLock:        &sync.Mutex{},
		metadataPortLock:      &sync.Mutex{},
		restartInProgressLock: &sync.Mutex{},
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
		Info:        "HTTP request handler. Start port configures and starts the server (context, autoHostName, hostnames for virtual hosts, readTimeout, writeTimeout). Each incoming HTTP request emits on Request port (blocks until Response port receives reply). Wire Request to processing logic, then wire result to Response port with statusCode, contentType, headers, body. Request times out if Response not received within readTimeout.",
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

	log.Info().
		Int("listenPort", listenPort).
		Interface("ctxErrOnEntry", ctx.Err()).
		Msg("http-server start: entering")

	e := echo.New()
	e.HideBanner = true
	e.HidePort = false

	serverCtx, serverCancel := context.WithCancel(ctx)
	defer serverCancel()

	// Monitor parent context cancellation and propagate to serverCtx
	go func() {
		<-ctx.Done()
		log.Info().
			Interface("ctxErr", ctx.Err()).
			Msg("http-server start: parent context cancelled, stopping server")
		serverCancel() // Cancel serverCtx when parent is cancelled
	}()

	h.setCancelFunc(serverCancel)

	//
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

		body, _ := io.ReadAll(req.Body)
		requestResult.Body = utils.BytesToString(body)

		resp := handler(c.Request().Context(), RequestPort, requestResult)
		if err := utils2.CheckForError(resp); err != nil {
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

	if e.Listener == nil {
		// Server failed to start - clear listenPort
		log.Error().Msg("HTTP server failed to bind - listener is nil")
		h.setListenPort(0)
		return fmt.Errorf("server failed to bind")
	}

	if tcpAddr, ok := e.Listener.Addr().(*net.TCPAddr); ok {
		actualLocalPort = tcpAddr.Port

		log.Info().Int("port", actualLocalPort).Msg("HTTP server started successfully")

		// Clear restart flag now that new server is bound - safe for old server cleanup to run
		h.setRestartInProgress(false)

		time.Sleep(time.Second)
		h.setListenPort(actualLocalPort)

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

	// Always update metadata when server successfully binds
	// This ensures metadata is correct even if an OLD start() on another pod
	// corrupted it to port=0 during a race condition
	_ = h.sendStatus(ctx, h.startSettings.Context, handler)

	log.Info().
		Int("listenPort", listenPort).
		Msg("http-server start: waiting on serverCtx.Done()")

	// ask to reconcile (redraw the component)

	<-serverCtx.Done()

	log.Info().
		Int("listenPort", listenPort).
		Interface("serverCtxErr", serverCtx.Err()).
		Interface("parentCtxErr", ctx.Err()).
		Msg("http-server start: serverCtx done, shutting down")

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

	// Only leader (listenPort==0) should update metadata when stopping
	// Replicas (listenPort>0) just stop locally - they don't own the metadata
	// Skip metadata update if restart is in progress (StartPort will start a new server)
	if listenPort == 0 && !h.isRestartInProgress() {
		log.Info().
			Int("currentListenPort", h.getListenPort()).
			Msg("http-server start: leader sending stop status")

		if err := h.sendStopStatus(handler); err != nil {
			log.Error().Err(err).Msg("http-server start: failed to send stop status")
		}
	} else if listenPort == 0 && h.isRestartInProgress() {
		log.Info().Msg("http-server start: skipping stop status (restart in progress)")
	} else {
		// Replica just triggers a local status refresh (no metadata change)
		log.Info().
			Int("listenPortParam", listenPort).
			Msg("http-server start: replica stopped, triggering local status refresh")
		_ = handler(context.Background(), v1alpha1.ReconcilePort, nil)
	}

	log.Info().
		Int("listenPort", listenPort).
		Msg("http-server start: exiting")

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

func (h *Component) setMetadataPort(port int) {
	h.metadataPortLock.Lock()
	defer h.metadataPortLock.Unlock()
	h.metadataPort = port
}

func (h *Component) getMetadataPort() int {
	h.metadataPortLock.Lock()
	defer h.metadataPortLock.Unlock()
	return h.metadataPort
}

func (h *Component) setRestartInProgress(v bool) {
	h.restartInProgressLock.Lock()
	defer h.restartInProgressLock.Unlock()
	h.restartInProgress = v
}

func (h *Component) isRestartInProgress() bool {
	h.restartInProgressLock.Lock()
	defer h.restartInProgressLock.Unlock()
	return h.restartInProgress
}

func (h *Component) Handle(ctx context.Context, handler module.Handler, port string, msg interface{}) any {

	switch port {
	case v1alpha1.ReconcilePort:

		if node, ok := msg.(v1alpha1.TinyNode); ok {
			//
			// all replicas should get copy of same node
			h.nodeName = node.Name

			listenPort, _ := strconv.Atoi(node.Status.Metadata[PortMetadata])
			// Sync metadata port for all pods - this is the source of truth for status display
			h.setMetadataPort(listenPort)
			log.Info().Int("metadataPort", listenPort).Int("localPort", h.getListenPort()).Bool("isLeader", utils2.IsLeader(ctx)).Msg("http_server: ReconcilePort received")

			// Leader should wait for StartPort signal for initial start
			// BUT if port metadata is set (server was previously running), leader should
			// also start via ReconcilePort to recover after pod restart
			if utils2.IsLeader(ctx) && listenPort == 0 {
				// Check if we have a running server (e.g., after leadership change)
				// If so, update metadata with our local port
				localPort := h.getListenPort()
				if localPort > 0 {
					log.Info().Int("localPort", localPort).Msg("http_server: leader has running server but metadata=0, updating metadata")
					h.setMetadataPort(localPort)
					_ = handler(ctx, v1alpha1.ReconcilePort, func(n *v1alpha1.TinyNode) error {
						if n.Status.Metadata == nil {
							n.Status.Metadata = map[string]string{}
						}
						n.Status.Metadata[PortMetadata] = fmt.Sprintf("%d", localPort)
						// Also save config so replicas get same settings
						if configData, err := json.Marshal(h.startSettings); err == nil {
							n.Status.Metadata[ConfigMetadata] = string(configData)
						}
						return nil
					})
				}
				return nil
			}

			if listenPort == h.getListenPort() {
				// it is the same instance
				return nil
			}

			if listenPort == 0 {
				// Port is 0 - either not assigned yet, or server should stop
				if h.getListenPort() > 0 {
					// Server is running but metadata says stop - stop the server
					log.Info().Msg("http_server: ReconcilePort detected port=0, stopping server")
					_ = h.stop()
				}
				return nil
			}

			// Restore startSettings from metadata if available
			// This ensures replicas get the same config (including Context) as the leader
			if configData, ok := node.Status.Metadata[ConfigMetadata]; ok && configData != "" {
				var savedConfig Start
				if err := json.Unmarshal([]byte(configData), &savedConfig); err == nil {
					h.startSettings = savedConfig
				}
			}

			// Protect stop/start sequence to prevent races
			h.startStopLock.Lock()
			defer h.startStopLock.Unlock()

			// Double-check after acquiring lock (another goroutine might have started)
			if listenPort == h.getListenPort() {
				return nil
			}

			// Set restartInProgress BEFORE stopping - this prevents any OLD start() that's
			// still shutting down from setting metadata to port=0
			h.setRestartInProgress(true)

			// stop if we were running
			_ = h.stop()

			// Set listenPort immediately from metadata so Ports() shows correct status
			// before the goroutine actually starts the server
			h.setListenPort(listenPort)

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

		// Loop until server state is stable (either fully running or fully stopped)
		// This prevents a race where we see cancelFunc!=nil during shutdown
		// and incorrectly block on ctx.Done() instead of starting a new server
		for {
			h.startStopLock.Lock()

			h.cancelFuncLock.Lock()
			hasCancel := h.cancelFunc != nil
			h.cancelFuncLock.Unlock()
			listenPort := h.getListenPort()

			// Server is fully running - block until this signal's context is cancelled
			if listenPort > 0 && hasCancel {
				h.startStopLock.Unlock()
				log.Info().
					Int("listenPort", listenPort).
					Bool("hasCancel", hasCancel).
					Msg("http_server: StartPort server running, blocking until context done")

				<-ctx.Done()

				log.Info().
					Interface("ctxErr", ctx.Err()).
					Msg("http_server: StartPort context cancelled, returning")
				return ctx.Err()
			}

			// Server is fully stopped - proceed to start
			if !hasCancel && listenPort == 0 {
				h.startStopLock.Unlock()
				break
			}

			// Server is in transitional state (starting up or shutting down)
			// Release lock and wait before checking again
			h.startStopLock.Unlock()

			log.Info().
				Int("listenPort", listenPort).
				Bool("hasCancel", hasCancel).
				Msg("http_server: StartPort server in transitional state, waiting")

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
				// Check again
			}
		}

		log.Info().
			Msg("http_server: StartPort starting new server")

		// Server not running, start it
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
	// Use metadataPort as source of truth - synced from TinyNode metadata
	// This ensures consistent status display across all pods in multi-replica setup
	port := h.getMetadataPort()
	log.Info().Int("metadataPort", port).Int("localPort", h.getListenPort()).Msg("http_server: getControl called")
	if port > 0 {
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
		IsRunning:  h.getListenPort() > 0,
	}
}

// sendStopStatus explicitly updates metadata to port=0 and triggers a status refresh
// This ensures the TinyNode status is updated when the server stops
func (h *Component) sendStopStatus(handler module.Handler) error {
	log.Info().
		Int("listenPort", h.getListenPort()).
		Bool("restartInProgress", h.isRestartInProgress()).
		Msg("http_server: sendStopStatus called")

	// Double-check restartInProgress before updating metadata
	// This provides additional protection against race conditions where
	// a new server is being started while this old instance is shutting down
	if h.isRestartInProgress() {
		log.Info().Msg("http_server: sendStopStatus skipping metadata update (restart in progress)")
		return nil
	}

	// Update local state FIRST so getControl() returns correct status immediately
	h.setMetadataPort(0)

	result := handler(context.Background(), v1alpha1.ReconcilePort, func(n *v1alpha1.TinyNode) error {
		if n.Status.Metadata == nil {
			n.Status.Metadata = map[string]string{}
		}
		// Explicitly set port to 0 to indicate stopped
		n.Status.Metadata[PortMetadata] = "0"
		log.Info().
			Str("nodeName", n.Name).
			Msg("http_server: sendStopStatus updating metadata to port=0")
		return nil
	})

	if err, ok := result.(error); ok && err != nil {
		log.Error().Err(err).Msg("http_server: sendStopStatus ReconcilePort failed")
		return err
	}

	log.Info().Msg("http_server: sendStopStatus completed successfully")
	return nil
}

// sendStatus changes node and sends status if it's port enabled
func (h *Component) sendStatus(ctx context.Context, _ StartContext, handler module.Handler) any {
	// Update local metadataPort so getControl() returns correct status immediately
	h.setMetadataPort(h.getListenPort())

	_ = handler(ctx, v1alpha1.ReconcilePort, func(n *v1alpha1.TinyNode) error {

		if n.Status.Metadata == nil {
			n.Status.Metadata = map[string]string{}
		}

		n.Status.Metadata[PortMetadata] = fmt.Sprintf("%d", h.getListenPort())

		// Store serialized startSettings so replicas can restore full config
		if configData, err := json.Marshal(h.startSettings); err == nil {
			n.Status.Metadata[ConfigMetadata] = string(configData)
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
	registry.Register((&Component{}).Instance())
}
