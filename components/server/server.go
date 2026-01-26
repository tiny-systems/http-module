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

	metadataKeyStart = "http-start"
	metadataKeyPort  = "port"
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

	// lastExposedPort tracks the port from metadata to clean up if port changes on restart
	lastExposedPortLock *sync.RWMutex
	lastExposedPort     int

	nodeName   string
	sourceNode string
	handler    module.Handler

	portMgr *portmanager.Manager

	// serverDone is closed when the server stops, allowing waiters to unblock
	serverDone     chan struct{}
	serverDoneLock *sync.Mutex
}

func (h *Component) Instance() module.Component {
	return &Component{
		publicListenAddr:     []string{},
		publicListenAddrLock: &sync.RWMutex{},
		cancelFuncLock:       &sync.Mutex{},
		startStopLock:        &sync.Mutex{},
		listenPortLock:       &sync.RWMutex{},
		lastExposedPortLock:  &sync.RWMutex{},
		settingsLock:         &sync.Mutex{},
		serverDoneLock:       &sync.Mutex{},
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

// State management

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

func (h *Component) setCancelFunc(fn context.CancelFunc) {
	h.cancelFuncLock.Lock()
	defer h.cancelFuncLock.Unlock()
	h.cancelFunc = fn
}

func (h *Component) isRunning() bool {
	h.cancelFuncLock.Lock()
	defer h.cancelFuncLock.Unlock()
	return h.cancelFunc != nil
}

func (h *Component) setPublicListenAddr(addr []string) {
	h.publicListenAddrLock.Lock()
	defer h.publicListenAddrLock.Unlock()
	h.publicListenAddr = addr
}

func (h *Component) getPublicListenAddr() []string {
	h.publicListenAddrLock.RLock()
	defer h.publicListenAddrLock.RUnlock()
	return h.publicListenAddr
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

func (h *Component) getServerDone() chan struct{} {
	h.serverDoneLock.Lock()
	defer h.serverDoneLock.Unlock()
	return h.serverDone
}

func (h *Component) setServerDone(done chan struct{}) {
	h.serverDoneLock.Lock()
	defer h.serverDoneLock.Unlock()
	h.serverDone = done
}

// Handle dispatches to port-specific handlers

func (h *Component) Handle(ctx context.Context, handler module.Handler, port string, msg interface{}) any {
	h.handler = handler

	switch port {
	case v1alpha1.ReconcilePort:
		h.handleReconcile(msg, handler)
		return nil
	case v1alpha1.ClientPort:
		return h.handleClient(msg)
	case v1alpha1.SettingsPort:
		return h.handleSettings(msg)
	case StartPort:
		return h.handleStart(ctx, handler, msg)
	case ResponsePort:
		return h.handleResponse(msg)
	default:
		return fmt.Errorf("port %s is not supported", port)
	}
}

func (h *Component) handleClient(msg interface{}) error {
	log.Info().Str("msgType", fmt.Sprintf("%T", msg)).Msg("http-server: handleClient called")

	k8sProvider, ok := msg.(module.K8sClient)
	if !ok {
		log.Warn().Str("msgType", fmt.Sprintf("%T", msg)).Msg("http-server: msg does not implement K8sClient")
		return nil
	}

	h.portMgr = portmanager.New(k8sProvider.GetK8sClient(), k8sProvider.GetNamespace())
	log.Info().Str("namespace", k8sProvider.GetNamespace()).Msg("http-server: portMgr initialized")
	return nil
}

func (h *Component) handleSettings(msg interface{}) error {
	in, ok := msg.(Settings)
	if !ok {
		return fmt.Errorf("invalid settings message")
	}
	h.settingsLock.Lock()
	defer h.settingsLock.Unlock()
	h.settings = in
	return nil
}

func (h *Component) handleResponse(msg interface{}) any {
	log.Info().
		Str("type", fmt.Sprintf("%T", msg)).
		Bool("isNil", msg == nil).
		Msg("http_server: handleResponse received")

	in, ok := msg.(Response)
	if !ok {
		log.Error().
			Interface("msg", msg).
			Str("type", fmt.Sprintf("%T", msg)).
			Msg("http_server: handleResponse - msg is not Response type")
		return fmt.Errorf("invalid response message: got %T", msg)
	}

	if in.StatusCode == 0 && in.Body == "" && in.ContentType == "" {
		log.Warn().Msg("http_server: handleResponse - received empty Response (all zero values)")
	}

	log.Info().
		Int("statusCode", in.StatusCode).
		Int("bodyLen", len(in.Body)).
		Msg("http_server: handleResponse returning")

	return in
}

func (h *Component) handleReconcile(msg interface{}, handler module.Handler) {
	node, ok := msg.(v1alpha1.TinyNode)
	if !ok {
		return
	}

	h.nodeName = node.Name

	if node.Status.Metadata == nil {
		return
	}

	metadataPort := h.readPortFromMetadata(node.Status.Metadata)
	if metadataPort == 0 {
		return
	}

	if h.isRunning() {
		return
	}

	startCfg, ok := h.readStartFromMetadata(node.Status.Metadata)
	if !ok {
		return
	}

	h.startSettings = startCfg
	log.Info().Interface("start", startCfg).Int("port", metadataPort).Msg("http_server: restoring from metadata")

	go h.startFromMetadata(handler, metadataPort)
}

func (h *Component) readPortFromMetadata(metadata map[string]string) int {
	portStr, ok := metadata[metadataKeyPort]
	if !ok {
		return 0
	}

	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 {
		return 0
	}

	h.setListenPort(p)
	h.setLastExposedPort(p)
	return p
}

func (h *Component) setLastExposedPort(port int) {
	h.lastExposedPortLock.Lock()
	defer h.lastExposedPortLock.Unlock()
	h.lastExposedPort = port
}

func (h *Component) getLastExposedPort() int {
	h.lastExposedPortLock.RLock()
	defer h.lastExposedPortLock.RUnlock()
	return h.lastExposedPort
}

func (h *Component) readStartFromMetadata(metadata map[string]string) (Start, bool) {
	startStr, ok := metadata[metadataKeyStart]
	if !ok || startStr == "" {
		return Start{}, false
	}

	var cfg Start
	if err := json.Unmarshal([]byte(startStr), &cfg); err != nil {
		return Start{}, false
	}

	if cfg.ReadTimeout == 0 && cfg.WriteTimeout == 0 {
		return Start{}, false
	}

	return cfg, true
}

func (h *Component) startFromMetadata(handler module.Handler, port int) {
	h.startStopLock.Lock()
	defer h.startStopLock.Unlock()

	if h.isRunning() {
		return
	}

	log.Info().Int("port", port).Msg("http_server: starting server from metadata")
	if err := h.runServer(context.Background(), handler); err != nil {
		log.Error().Err(err).Msg("http_server: server stopped after metadata restoration")
	}
}

func (h *Component) handleStart(ctx context.Context, handler module.Handler, msg interface{}) error {
	if msg == nil {
		log.Info().Msg("http_server: StartPort received nil (state deleted), stopping")
		_ = h.stop()
		return nil
	}

	in := h.parseStartConfig(msg)

	h.sourceNode = moduleutils.GetSourceNode(ctx)
	h.startSettings = in

	log.Info().
		Str("sourceNode", h.sourceNode).
		Bool("isLeader", moduleutils.IsLeader(ctx)).
		Bool("isRunning", h.isRunning()).
		Msg("http_server: StartPort received")

	h.persistStartConfig(handler, in)

	// If server is already running, wait for it to stop (blocking behavior for ticker)
	if done := h.getServerDone(); done != nil {
		log.Info().Msg("http_server: already running, waiting for server to stop")
		<-done
		log.Info().Msg("http_server: server stopped, returning from Start")
		return nil
	}

	log.Info().Msg("http_server: starting server from StartPort")
	err := h.runServer(ctx, handler)

	log.Info().Err(err).Msg("http_server: server stopped")
	_ = handler(context.Background(), v1alpha1.ReconcilePort, nil)

	return err
}

func (h *Component) parseStartConfig(msg interface{}) Start {
	if start, ok := msg.(Start); ok {
		return start
	}

	in := h.startSettings
	in.Context = msg
	return in
}

func (h *Component) persistStartConfig(handler module.Handler, cfg Start) {
	startBytes, _ := json.Marshal(cfg)
	_ = handler(context.Background(), v1alpha1.ReconcilePort, func(n *v1alpha1.TinyNode) error {
		if n.Status.Metadata == nil {
			n.Status.Metadata = make(map[string]string)
		}
		n.Status.Metadata[metadataKeyStart] = string(startBytes)
		return nil
	})
}

// Server lifecycle

func (h *Component) runServer(ctx context.Context, handler module.Handler) error {
	if h.portMgr == nil {
		return fmt.Errorf("unable to start, no port manager available")
	}

	log.Info().Msg("http-server: entering")

	// Create serverDone channel so other callers can wait for server to stop
	done := make(chan struct{})
	h.setServerDone(done)
	defer func() {
		h.setServerDone(nil)
		close(done)
	}()

	e := h.createEchoServer(handler)
	serverCtx, serverCancel := context.WithCancel(ctx)
	defer serverCancel()

	h.setCancelFunc(serverCancel)
	h.watchParentContext(ctx, serverCancel)

	listenAddr := h.determineListenAddr()

	go h.startEchoServer(e, listenAddr, serverCancel)

	time.Sleep(time.Millisecond * 1500)

	actualPort, err := h.handleServerStarted(ctx, e, handler)
	if err != nil {
		return err
	}

	log.Info().Msg("http-server: waiting on serverCtx.Done()")
	<-serverCtx.Done()

	h.shutdownServer(e, actualPort)
	return serverCtx.Err()
}

func (h *Component) createEchoServer(handler module.Handler) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = false

	e.Any("*", func(c echo.Context) error {
		return h.handleHTTPRequest(c, handler)
	})

	e.Server.ReadTimeout = time.Duration(h.startSettings.ReadTimeout) * time.Second
	e.Server.WriteTimeout = time.Duration(h.startSettings.WriteTimeout) * time.Second

	return e
}

func (h *Component) handleHTTPRequest(c echo.Context, handler module.Handler) error {
	req := h.buildRequest(c)

	log.Info().Str("uri", req.RequestURI).Str("method", req.Method).Msg("http_server: handling request")

	resp := handler(c.Request().Context(), RequestPort, req)

	log.Info().
		Str("type", fmt.Sprintf("%T", resp)).
		Bool("isNil", resp == nil).
		Msg("http_server: handler returned")

	if err := moduleutils.CheckForError(resp); err != nil {
		log.Error().Err(err).Msg("http_server: handler returned error")
		return err
	}

	respObj, ok := resp.(Response)
	if !ok {
		log.Error().
			Interface("resp", resp).
			Str("type", fmt.Sprintf("%T", resp)).
			Msg("http_server: response is not Response type")
		return fmt.Errorf("invalid response: got %T", resp)
	}

	if respObj.StatusCode == 0 && respObj.Body == "" && respObj.ContentType == "" {
		log.Warn().
			Int("statusCode", respObj.StatusCode).
			Str("body", respObj.Body).
			Str("contentType", string(respObj.ContentType)).
			Msg("http_server: response appears empty (zero values)")
	}

	log.Info().
		Int("statusCode", respObj.StatusCode).
		Int("bodyLen", len(respObj.Body)).
		Msg("http_server: writing response")

	h.writeResponse(c, respObj)
	return nil
}

func (h *Component) buildRequest(c echo.Context) Request {
	req := Request{
		Context:       h.startSettings.Context,
		Host:          c.Request().Host,
		Method:        c.Request().Method,
		RequestURI:    c.Request().RequestURI,
		RequestParams: c.QueryParams(),
		RealIP:        c.RealIP(),
		Scheme:        c.Scheme(),
		Headers:       h.extractHeaders(c.Request()),
		PodName:       os.Getenv("HOSTNAME"),
	}

	body, err := io.ReadAll(c.Request().Body)
	if err == nil {
		req.Body = utils.BytesToString(body)
	}

	return req
}

func (h *Component) extractHeaders(r *http.Request) []etc.Header {
	headers := make([]etc.Header, 0)

	keys := make([]string, 0, len(r.Header))
	for k := range r.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		for _, v := range r.Header[k] {
			headers = append(headers, etc.Header{Key: k, Value: v})
		}
	}

	return headers
}

func (h *Component) writeResponse(c echo.Context, resp Response) {
	for _, header := range resp.Headers {
		c.Response().Header().Set(header.Key, header.Value)
	}

	if resp.ContentType != "" {
		c.Response().Header().Set(etc.HeaderContentType, string(resp.ContentType))
	}

	statusCode := resp.StatusCode
	if statusCode == 0 {
		statusCode = 200
	}

	_ = c.String(statusCode, fmt.Sprintf("%v", resp.Body))
}

func (h *Component) watchParentContext(ctx context.Context, cancel context.CancelFunc) {
	go func() {
		<-ctx.Done()
		log.Info().Msg("http-server: parent context cancelled, stopping server")
		cancel()
	}()
}

func (h *Component) determineListenAddr() string {
	listenPort := h.getListenPort()
	if listenPort > 0 {
		log.Info().Int("port", listenPort).Msg("http_server: using port from metadata")
		return fmt.Sprintf(":%d", listenPort)
	}
	return ":0"
}

func (h *Component) startEchoServer(e *echo.Echo, addr string, cancel context.CancelFunc) {
	err := e.Start(addr)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return
	}
	log.Error().Err(err).Str("addr", addr).Msg("failed to start HTTP server")
	cancel()
}

func (h *Component) handleServerStarted(ctx context.Context, e *echo.Echo, handler module.Handler) (int, error) {
	if e.Listener == nil {
		log.Error().Msg("HTTP server failed to bind - listener is nil")
		h.setListenPort(0)
		return 0, fmt.Errorf("server failed to bind")
	}

	tcpAddr, ok := e.Listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, nil
	}

	actualPort := tcpAddr.Port
	log.Info().Int("port", actualPort).Msg("HTTP server started successfully")

	time.Sleep(time.Second)
	h.setListenPort(actualPort)

	publicURLs := h.exposePort(ctx, tcpAddr.Port)
	h.setPublicListenAddr(publicURLs)

	h.persistPort(handler, actualPort)
	_ = handler(context.Background(), v1alpha1.ReconcilePort, nil)

	return actualPort, nil
}

func (h *Component) exposePort(ctx context.Context, port int) []string {
	log.Info().Int("port", port).Bool("hasPortMgr", h.portMgr != nil).Msg("http-server: exposePort called")

	if h.portMgr == nil {
		log.Error().Int("port", port).Msg("http-server: portMgr is nil, cannot expose port")
		return []string{fmt.Sprintf("http://localhost:%d", port)}
	}

	// Clean up old port if it's different from the new one (e.g., after pod restart)
	oldPort := h.getLastExposedPort()
	if oldPort > 0 && oldPort != port {
		log.Info().Int("oldPort", oldPort).Int("newPort", port).Msg("http-server: cleaning up old port before exposing new one")
		discloseCtx, discloseCancel := context.WithTimeout(ctx, time.Second*30)
		if err := h.portMgr.DisclosePort(discloseCtx, oldPort); err != nil {
			log.Error().Err(err).Int("port", oldPort).Msg("http-server: failed to disclose old port")
		}
		discloseCancel()
	}

	exposeCtx, cancel := context.WithTimeout(ctx, time.Second*30)
	defer cancel()

	var autoHostName string
	if h.startSettings.AutoHostName || len(h.startSettings.Hostnames) == 0 {
		parts := strings.Split(h.nodeName, ".")
		autoHostName = parts[len(parts)-1]
	}

	log.Info().
		Int("port", port).
		Str("autoHostName", autoHostName).
		Strs("hostnames", h.startSettings.Hostnames).
		Msg("http-server: calling portMgr.ExposePort")

	publicURLs, err := h.portMgr.ExposePort(exposeCtx, autoHostName, h.startSettings.Hostnames, port)
	if err != nil {
		log.Error().Err(err).Int("port", port).Msg("http-server: failed to expose port")
		return []string{fmt.Sprintf("http://localhost:%d", port)}
	}

	log.Info().Int("port", port).Strs("publicURLs", publicURLs).Msg("http-server: port exposed successfully")

	// Update last exposed port after successful expose
	h.setLastExposedPort(port)

	return publicURLs
}

func (h *Component) persistPort(handler module.Handler, port int) {
	_ = handler(context.Background(), v1alpha1.ReconcilePort, func(n *v1alpha1.TinyNode) error {
		if n.Status.Metadata == nil {
			n.Status.Metadata = make(map[string]string)
		}
		n.Status.Metadata[metadataKeyPort] = fmt.Sprintf("%d", port)
		return nil
	})
}

func (h *Component) shutdownServer(e *echo.Echo, actualPort int) {
	log.Info().Msg("http-server: serverCtx done, shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	_ = e.Shutdown(shutdownCtx)

	h.setCancelFunc(nil)
	h.setListenPort(0)

	// NOTE: We intentionally do NOT call DisclosePort here.
	// During rolling updates, the new pod exposes ports before the old pod shuts down.
	// If we disclose here, we'd remove ports that the new pod just added.
	// Port cleanup only happens in OnDestroy() when the TinyNode CRD is deleted.

	h.setPublicListenAddr([]string{})
	log.Info().Msg("http-server: exiting")
}

// UI helpers

func (h *Component) getControl() Control {
	if h.isRunning() {
		return Control{
			Status:     "Running",
			ListenAddr: h.getPublicListenAddr(),
		}
	}
	return Control{Status: "Not running"}
}

func (h *Component) getStatus() Status {
	return Status{
		ListenAddr: h.getPublicListenAddr(),
		IsRunning:  h.isRunning(),
	}
}

func (h *Component) Ports() []module.Port {
	h.settingsLock.Lock()
	defer h.settingsLock.Unlock()

	ports := []module.Port{
		{Name: v1alpha1.ClientPort},
		{Name: v1alpha1.ReconcilePort},
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
			Name:          ResponsePort,
			Label:         "Response",
			Position:      module.Right,
			Configuration: Response{StatusCode: 200},
		},
		{
			Name:          v1alpha1.ControlPort,
			Label:         "Dashboard",
			Source:        true,
			Configuration: h.getControl(),
		},
		{
			Name:          StartPort,
			Label:         "Start",
			Position:      module.Left,
			Configuration: h.startSettings,
		},
	}

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

var _ module.Component = (*Component)(nil)
var _ module.Destroyer = (*Component)(nil)

// OnDestroy implements module.Destroyer interface.
// Called when the node is being deleted (via finalizer) to clean up exposed ports.
func (h *Component) OnDestroy(metadata map[string]string) {
	if h.portMgr == nil {
		return
	}

	portStr, ok := metadata[metadataKeyPort]
	if !ok || portStr == "" {
		return
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port == 0 {
		return
	}

	log.Info().Int("port", port).Msg("http-server: cleaning up exposed port on destroy")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	if err := h.portMgr.DisclosePort(ctx, port); err != nil {
		log.Error().Err(err).Int("port", port).Msg("http-server: failed to disclose port on destroy")
	}
}

func init() {
	registry.Register((&Component{}).Instance())
}
