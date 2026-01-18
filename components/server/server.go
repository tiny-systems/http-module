package server

import (
	"context"
	"encoding/json"
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
)

// State represents HTTP server runtime state stored in TinyState
// Presence of TinyState means server should be running, absence means stopped
type State struct {
	Port       int    `json:"port,omitempty"`
	Config     Start  `json:"config,omitempty"`
	SourceNode string `json:"sourceNode,omitempty"` // node that triggered Start (for ownership)
}

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

	// State synced from TinyState - source of truth for multi-pod status
	stateLock *sync.RWMutex
	state     State
	hasState  bool // true when TinyState exists (server should be running)

	// Notification channel for state changes (used by StartPort waiter)
	stateChangeCh   chan struct{}
	stateChangeLock *sync.Mutex

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
		stateLock:            &sync.RWMutex{},
		settingsLock:         &sync.Mutex{},
		stateChangeLock:      &sync.Mutex{},
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

	if len(h.startSettings.Hostnames) == 1 {
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

		// Update state with actual port (keep same owner)
		h.writeState(handler, State{
			Port:       actualLocalPort,
			Config:     h.startSettings,
			SourceNode: h.sourceNode,
		}, h.sourceNode)
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

func (h *Component) getState() (State, bool) {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()
	return h.state, h.hasState
}

// notifyStateChange signals waiters that state has changed
func (h *Component) notifyStateChange() {
	h.stateChangeLock.Lock()
	defer h.stateChangeLock.Unlock()
	if h.stateChangeCh != nil {
		close(h.stateChangeCh)
		h.stateChangeCh = nil
	}
}

// waitForStateDeleted blocks until hasState becomes false (state deleted)
func (h *Component) waitForStateDeleted(ctx context.Context) {
	for {
		// Check if state is already deleted
		_, hasState := h.getState()
		if !hasState {
			return
		}

		// Create notification channel
		h.stateChangeLock.Lock()
		if h.stateChangeCh == nil {
			h.stateChangeCh = make(chan struct{})
		}
		ch := h.stateChangeCh
		h.stateChangeLock.Unlock()

		// Wait for state change or context cancellation
		select {
		case <-ch:
			// State changed, loop to check if deleted
		case <-ctx.Done():
			return
		}
	}
}

// runServerFromState is called by leader when state arrives via StatePort
// It starts the server and blocks until stopped, then deletes state
func (h *Component) runServerFromState(parentCtx context.Context, handler module.Handler) {
	log.Info().Msg("http_server: runServerFromState starting")

	// Start the server (blocking)
	err := h.start(parentCtx, handler)

	log.Info().
		Err(err).
		Msg("http_server: runServerFromState server stopped, deleting state")

	// Delete state when server stops
	h.deleteState(handler)

	// Trigger reconcile to update UI
	_ = handler(context.Background(), v1alpha1.ReconcilePort, nil)
}

func (h *Component) writeState(handler module.Handler, state State, ownerNode string) error {
	stateData, err := json.Marshal(state)
	if err != nil {
		return err
	}

	// Update local state immediately (optimistic update)
	// This ensures UI shows correct status before K8s round-trip completes
	h.stateLock.Lock()
	h.state = state
	h.hasState = true
	h.stateLock.Unlock()

	// Notify waiters that state changed
	h.notifyStateChange()

	result := handler(context.Background(), v1alpha1.ReconcilePort, v1alpha1.StateUpdate{
		Data:      stateData,
		OwnerNode: ownerNode,
	})

	return moduleutils.CheckForError(result)
}

// deleteState deletes the TinyState CRD (nil Data signals deletion)
func (h *Component) deleteState(handler module.Handler) error {
	// Update local state immediately (optimistic update)
	// This ensures UI shows correct status before K8s round-trip completes
	h.stateLock.Lock()
	h.state = State{}
	h.hasState = false
	h.stateLock.Unlock()

	// Notify waiters that state changed
	h.notifyStateChange()

	result := handler(context.Background(), v1alpha1.ReconcilePort, v1alpha1.StateUpdate{
		Data: nil,
	})
	return moduleutils.CheckForError(result)
}

func (h *Component) Handle(ctx context.Context, handler module.Handler, port string, msg interface{}) any {
	h.handler = handler

	switch port {
	case v1alpha1.ReconcilePort:
		if node, ok := msg.(v1alpha1.TinyNode); ok {
			h.nodeName = node.Name
		}
		return nil

	case v1alpha1.StatePort:
		// Receive state data as []byte
		// nil means state was deleted (stop)
		data, ok := msg.([]byte)
		if !ok {
			return fmt.Errorf("invalid state message: expected []byte")
		}

		// nil data means state was deleted - stop server
		if data == nil {
			log.Info().Msg("http_server: state deleted, stopping")

			// Reset local state
			h.stateLock.Lock()
			h.state = State{}
			h.hasState = false
			h.stateLock.Unlock()

			// Notify any waiters that state changed
			h.notifyStateChange()

			// All pods stop their server
			_ = h.stop()
			return nil
		}

		// Parse state data - presence means server should run
		var serverState State
		if err := json.Unmarshal(data, &serverState); err != nil {
			log.Error().Err(err).Msg("http_server: failed to unmarshal state")
			return err
		}

		log.Info().
			Int("statePort", serverState.Port).
			Str("sourceNode", serverState.SourceNode).
			Bool("isLeader", moduleutils.IsLeader(ctx)).
			Int("localPort", h.getListenPort()).
			Bool("isRunning", h.isRunning()).
			Msg("http_server: received state, server should run")

		// Update local state for UI display
		h.stateLock.Lock()
		h.state = serverState
		h.hasState = true
		h.stateLock.Unlock()

		// Restore config and source from state
		h.startSettings = serverState.Config
		h.sourceNode = serverState.SourceNode

		// All pods start the server for load balancing
		if !h.isRunning() {
			log.Info().Msg("http_server: starting server from StatePort")
			go h.runServerFromState(context.Background(), handler)
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
		in, ok := msg.(Start)
		if !ok {
			return fmt.Errorf("invalid start message")
		}

		// Get source node from context for ownership
		sourceNode := moduleutils.GetSourceNode(ctx)
		h.sourceNode = sourceNode
		h.startSettings = in

		isLeader := moduleutils.IsLeader(ctx)

		log.Info().
			Str("sourceNode", sourceNode).
			Bool("isLeader", isLeader).
			Msg("http_server: StartPort received, writing state")

		// Any pod can write state - this triggers leader to start server via StatePort
		if err := h.writeState(handler, State{
			Config:     in,
			SourceNode: sourceNode,
		}, sourceNode); err != nil {
			log.Error().Err(err).Msg("http_server: failed to write start state")
			return err
		}

		// Trigger reconcile to update UI
		_ = handler(context.Background(), v1alpha1.ReconcilePort, nil)

		// BLOCK until state is deleted (server stopped)
		// This works for both leader and non-leader pods
		log.Info().Msg("http_server: StartPort waiting for state deletion (server stop)")
		h.waitForStateDeleted(ctx)

		log.Info().Msg("http_server: StartPort state deleted, returning")
		return nil

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
	state, hasState := h.getState()
	if hasState && state.Port > 0 {
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
			Name:          v1alpha1.StatePort,
			Configuration: struct{}{}, // Hidden port - receives TinyState for state sync
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

	// Always show Start port - it blocks until server stops
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
	state, hasState := h.getState()
	return Status{
		ListenAddr: h.getPublicListerAddr(),
		IsRunning:  hasState && state.Port > 0,
	}
}

var _ module.Component = (*Component)(nil)

func init() {
	registry.Register((&Component{}).Instance())
}
