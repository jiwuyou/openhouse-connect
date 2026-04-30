package core

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// BridgeServer — global WebSocket server shared across all engines
// ---------------------------------------------------------------------------

// BridgeServer exposes a WebSocket endpoint for external platform adapters.
// A single instance is created globally; each project engine receives a
// lightweight BridgePlatform handle that delegates to this server.
type BridgeServer struct {
	port        int
	host        string
	token       string
	path        string
	corsOrigins []string
	server      *http.Server

	mu       sync.RWMutex
	adapters map[string]*bridgeAdapter // platform name → external adapter

	frontendClientSeq uint64
	frontendServices  map[string]*bridgeFrontendService // service platform/slot → frontend service

	enginesMu sync.RWMutex
	engines   map[string]*bridgeEngineRef // project name → engine ref
}

type bridgeEngineRef struct {
	engine   *Engine
	platform *BridgePlatform
}

type bridgeAdapter struct {
	platform     string
	capabilities map[string]bool
	metadata     map[string]any
	conn         *websocket.Conn
	writeMu      sync.Mutex
	server       *BridgeServer

	previewMu       sync.Mutex
	previewRequests map[string]chan string // ref_id → channel receiving preview_handle
}

type bridgeFrontendService struct {
	platform     string
	app          string
	slot         string
	project      string
	capabilities map[string]bool
	metadata     map[string]any
	adapter      *bridgeAdapter
	clients      map[string]*bridgeFrontendClient
	registeredAt time.Time
	updatedAt    time.Time
}

type bridgeFrontendClient struct {
	id                  string
	platform            string
	app                 string
	slot                string
	project             string
	route               string
	sessionKey          string
	transportSessionKey string
	conn                *websocket.Conn
	writeMu             sync.Mutex
	connectedAt         time.Time
	lastSeen            time.Time
}

// BridgeFrontendServiceRegistration declares a backend-managed frontend service
// identity. Browser tabs connect as clients of this service; they are not
// bridge adapters and therefore do not replace adapter connections.
type BridgeFrontendServiceRegistration struct {
	Platform     string
	App          string
	Slot         string
	Project      string
	Capabilities []string
	Metadata     map[string]any
}

// BridgeFrontendServiceInfo is a runtime snapshot for a frontend service.
type BridgeFrontendServiceInfo struct {
	Platform         string         `json:"platform"`
	App              string         `json:"app,omitempty"`
	Slot             string         `json:"slot,omitempty"`
	Project          string         `json:"project,omitempty"`
	Capabilities     []string       `json:"capabilities"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	ConnectedClients int            `json:"connected_clients"`
	RegisteredAt     time.Time      `json:"registered_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

// bridgeReplyCtx carries the information needed to route replies back to the adapter.
type bridgeReplyCtx struct {
	Platform            string `json:"platform"`
	SessionKey          string `json:"session_key"`
	SessionID           string `json:"session_id,omitempty"`
	ReplyCtx            string `json:"reply_ctx"`
	TransportSessionKey string `json:"transport_session_key,omitempty"`
	Route               string `json:"route,omitempty"`
	ClientID            string `json:"client_id,omitempty"`

	progressStyle               string `json:"-"`
	supportsProgressCardPayload bool   `json:"-"`
}

func (rc *bridgeReplyCtx) progressStyleHint() string {
	if rc == nil {
		return progressStyleLegacy
	}
	return rc.progressStyle
}

func (rc *bridgeReplyCtx) supportsProgressCardPayloadHint() bool {
	if rc == nil {
		return false
	}
	return rc.supportsProgressCardPayload
}

const bridgeReconstructReplyCtxKind = "bridge_reconstruct"

// bridgeReconstructReplyCtxPayload is a forward-compatible reply envelope for
// reconstruct_reply adapters. Receivers should ignore unknown fields.
type bridgeReconstructReplyCtxPayload struct {
	Kind                string `json:"kind"`
	Version             int    `json:"v"`
	SenderProject       string `json:"sender_project"`
	TransportChatID     string `json:"transport_chat_id"`
	TransportSessionKey string `json:"transport_session_key,omitempty"`
}

// --- Wire protocol messages ---

type bridgeMsg struct {
	Type string `json:"type"`
}

type bridgeRegister struct {
	Type         string         `json:"type"`
	Platform     string         `json:"platform"`
	Capabilities []string       `json:"capabilities"`
	Project      string         `json:"project,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

type bridgeFrontendConnect struct {
	Type                string         `json:"type"`
	Platform            string         `json:"platform,omitempty"`
	App                 string         `json:"app,omitempty"`
	Slot                string         `json:"slot,omitempty"`
	Route               string         `json:"route,omitempty"`
	ClientID            string         `json:"client_id,omitempty"`
	SessionKey          string         `json:"session_key,omitempty"`
	TransportSessionKey string         `json:"transport_session_key,omitempty"`
	Capabilities        []string       `json:"capabilities,omitempty"`
	Project             string         `json:"project,omitempty"`
	Metadata            map[string]any `json:"metadata,omitempty"`
}

type bridgeMessage struct {
	Type                string            `json:"type"`
	MsgID               string            `json:"msg_id"`
	SessionKey          string            `json:"session_key"`
	SessionID           string            `json:"session_id,omitempty"`
	UserID              string            `json:"user_id"`
	UserName            string            `json:"user_name,omitempty"`
	Content             string            `json:"content"`
	ReplyCtx            string            `json:"reply_ctx"`
	Project             string            `json:"project,omitempty"`
	TransportSessionKey string            `json:"transport_session_key,omitempty"`
	Route               string            `json:"route,omitempty"`
	Images              []bridgeImageData `json:"images,omitempty"`
	Files               []bridgeFileData  `json:"files,omitempty"`
	Audio               *bridgeAudioData  `json:"audio,omitempty"`
}

type bridgeCardAction struct {
	Type                string `json:"type"`
	SessionKey          string `json:"session_key"`
	SessionID           string `json:"session_id,omitempty"`
	Action              string `json:"action"`
	ReplyCtx            string `json:"reply_ctx"`
	Project             string `json:"project,omitempty"`
	TransportSessionKey string `json:"transport_session_key,omitempty"`
	Route               string `json:"route,omitempty"`
}

type bridgePreviewAck struct {
	Type          string `json:"type"`
	RefID         string `json:"ref_id"`
	PreviewHandle string `json:"preview_handle"`
}

type bridgeImageData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64
	FileName string `json:"file_name,omitempty"`
	Size     int    `json:"size,omitempty"`
}

type bridgeFileData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64
	FileName string `json:"file_name"`
}

type bridgeAudioData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64
	Format   string `json:"format"`
	Duration int    `json:"duration,omitempty"`
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func NewBridgeServer(port int, token, path string, corsOrigins []string) *BridgeServer {
	if port <= 0 {
		port = 9810
	}
	if path == "" {
		path = "/bridge/ws"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return &BridgeServer{
		port:             port,
		token:            token,
		path:             path,
		corsOrigins:      corsOrigins,
		adapters:         make(map[string]*bridgeAdapter),
		frontendServices: make(map[string]*bridgeFrontendService),
		engines:          make(map[string]*bridgeEngineRef),
	}
}

func (bs *BridgeServer) SetHost(host string) {
	bs.host = strings.TrimSpace(host)
}

// NewPlatform creates a BridgePlatform for a specific project engine.
func (bs *BridgeServer) NewPlatform(projectName string) *BridgePlatform {
	return &BridgePlatform{server: bs, project: projectName}
}

// RegisterEngine associates a project engine with its BridgePlatform.
func (bs *BridgeServer) RegisterEngine(projectName string, engine *Engine, bp *BridgePlatform) {
	bs.enginesMu.Lock()
	defer bs.enginesMu.Unlock()
	if err := bp.Start(engine.handleMessage); err != nil {
		slog.Warn("bridge: platform start failed", "project", projectName, "error", err)
	}
	bp.SetCardNavigationHandler(engine.handleCardNav)
	bs.engines[projectName] = &bridgeEngineRef{engine: engine, platform: bp}
}

// Start launches the HTTP/WebSocket server.
func (bs *BridgeServer) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc(bs.path, bs.handleWS)

	// Session management REST endpoints (with CORS support)
	mux.HandleFunc("/bridge/sessions", bs.corsHTTP(bs.authHTTP(bs.handleSessions)))
	mux.HandleFunc("/bridge/sessions/", bs.corsHTTP(bs.authHTTP(bs.handleSessionRoutes)))

	addr := tcpListenAddr(bs.host, bs.port)
	bs.server = &http.Server{Addr: addr, Handler: mux}

	go func() {
		slog.Info("bridge: server started", "addr", addr, "path", bs.path)
		if err := bs.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("bridge: server error", "error", err)
		}
	}()
}

// corsHTTP wraps a handler with CORS headers. OPTIONS preflight is handled directly.
func (bs *BridgeServer) corsHTTP(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bs.setCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		handler(w, r)
	}
}

// setCORS sets Access-Control-* headers when the request origin matches cors_origins.
func (bs *BridgeServer) setCORS(w http.ResponseWriter, r *http.Request) {
	if len(bs.corsOrigins) == 0 {
		return
	}
	origin := r.Header.Get("Origin")
	for _, o := range bs.corsOrigins {
		if o == "*" || o == origin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			break
		}
	}
}

// Stop shuts down the server and closes all adapter connections.
func (bs *BridgeServer) Stop() {
	bs.mu.Lock()
	for _, a := range bs.adapters {
		a.conn.Close()
	}
	for _, svc := range bs.frontendServices {
		for _, c := range svc.clients {
			c.conn.Close()
		}
	}
	bs.adapters = make(map[string]*bridgeAdapter)
	bs.frontendServices = make(map[string]*bridgeFrontendService)
	bs.mu.Unlock()

	if bs.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := bs.server.Shutdown(ctx); err != nil && err != http.ErrServerClosed {
			slog.Debug("bridge: server shutdown failed", "error", err)
		}
	}
}

// ConnectedAdapters returns the names of currently connected adapters.
func (bs *BridgeServer) ConnectedAdapters() []string {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	names := make([]string, 0, len(bs.adapters))
	for name := range bs.adapters {
		names = append(names, name)
	}
	return names
}

// RegisterFrontendService declares a backend-managed frontend slot/service.
// The service can receive browser frontend clients without those clients
// registering as bridge adapters.
func (bs *BridgeServer) RegisterFrontendService(reg BridgeFrontendServiceRegistration) (BridgeFrontendServiceInfo, error) {
	platform := bridgeFrontendServicePlatform(reg.Platform, reg.Slot, "", "", "")
	if platform == "" {
		return BridgeFrontendServiceInfo{}, fmt.Errorf("bridge: frontend service platform is required")
	}
	caps := bridgeCapabilitiesMap(reg.Capabilities)
	if len(reg.Capabilities) == 0 {
		caps = bridgeDefaultFrontendCapabilities()
	}

	bs.mu.Lock()
	defer bs.mu.Unlock()
	now := time.Now().UTC()
	svc := bs.ensureFrontendServiceLocked(platform, reg.App, reg.Slot, reg.Project, caps, reg.Metadata, now)
	return bridgeFrontendServiceSnapshotLocked(svc), nil
}

// ConnectedFrontendServices returns runtime frontend service snapshots.
func (bs *BridgeServer) ConnectedFrontendServices() []BridgeFrontendServiceInfo {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	services := make([]BridgeFrontendServiceInfo, 0, len(bs.frontendServices))
	for _, svc := range bs.frontendServices {
		services = append(services, bridgeFrontendServiceSnapshotLocked(svc))
	}
	return services
}

// ---------------------------------------------------------------------------
// BridgePlatform — per-engine Platform that delegates to BridgeServer
// ---------------------------------------------------------------------------

// BridgePlatform implements core.Platform for a single project.
// It is a lightweight handle; the actual WebSocket server lives in BridgeServer.
type BridgePlatform struct {
	server                  *BridgeServer
	project                 string
	handler                 MessageHandler
	navHandler              CardNavigationHandler
	navHandlerWithSessionID CardNavigationHandlerWithSessionID
}

// Compile-time interface checks.
var (
	_ Platform                   = (*BridgePlatform)(nil)
	_ CardSender                 = (*BridgePlatform)(nil)
	_ InlineButtonSender         = (*BridgePlatform)(nil)
	_ MessageUpdater             = (*BridgePlatform)(nil)
	_ PreviewStarter             = (*BridgePlatform)(nil)
	_ PreviewCleaner             = (*BridgePlatform)(nil)
	_ TypingIndicator            = (*BridgePlatform)(nil)
	_ ImageSender                = (*BridgePlatform)(nil)
	_ AudioSender                = (*BridgePlatform)(nil)
	_ CardNavigable              = (*BridgePlatform)(nil)
	_ CardNavigableWithSessionID = (*BridgePlatform)(nil)
	_ ReplyContextReconstructor  = (*BridgePlatform)(nil)
)

func (bp *BridgePlatform) Name() string { return "bridge" }

func (bp *BridgePlatform) Start(handler MessageHandler) error {
	bp.handler = handler
	return nil
}

func (bp *BridgePlatform) Stop() error { return nil }

func (bp *BridgePlatform) Reply(ctx context.Context, replyCtx any, content string) error {
	rc, ok := replyCtx.(*bridgeReplyCtx)
	if !ok {
		return fmt.Errorf("bridge: invalid reply context type %T", replyCtx)
	}
	payload := bridgeOutboundBase("reply", rc)
	payload["reply_ctx"] = rc.ReplyCtx
	payload["content"] = content
	payload["format"] = "text"
	return bp.server.sendToReplyTarget(rc, payload)
}

func (bp *BridgePlatform) Send(ctx context.Context, replyCtx any, content string) error {
	return bp.Reply(ctx, replyCtx, content)
}

func (bp *BridgePlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	platform := bp.server.platformFromSessionKey(sessionKey)
	if platform == "" {
		return nil, fmt.Errorf("bridge: cannot determine adapter from session key %q", sessionKey)
	}
	a := bp.server.getAdapter(platform)
	if a == nil {
		return nil, fmt.Errorf("bridge: adapter %q not connected", platform)
	}
	if !a.capabilities["reconstruct_reply"] {
		return nil, fmt.Errorf("bridge: adapter %q does not support reconstruct_reply", platform)
	}
	replyCtx, err := buildBridgeReconstructReplyCtx(bp.project, sessionKey)
	if err != nil {
		return nil, err
	}
	return newBridgeReplyCtx(a, sessionKey, replyCtx), nil
}

func newBridgeReplyCtx(a *bridgeAdapter, sessionKey, replyCtx string) *bridgeReplyCtx {
	return newBridgeReplyCtxWithSessionID(a, sessionKey, "", replyCtx)
}

func newBridgeReplyCtxWithSessionID(a *bridgeAdapter, sessionKey, sessionID, replyCtx string) *bridgeReplyCtx {
	rc := &bridgeReplyCtx{
		SessionKey: sessionKey,
		SessionID:  strings.TrimSpace(sessionID),
		ReplyCtx:   replyCtx,
	}
	if a == nil {
		return rc
	}
	rc.Platform = a.platform
	rc.progressStyle = bridgeProgressStyleForAdapter(a)
	rc.supportsProgressCardPayload = bridgeSupportsProgressCardPayloadForAdapter(a)
	return rc
}

func newBridgeReplyCtxWithFrontendClient(a *bridgeAdapter, sessionKey, sessionID, replyCtx, transportSessionKey, route string, client *bridgeFrontendClient) *bridgeReplyCtx {
	rc := newBridgeReplyCtxWithSessionID(a, sessionKey, sessionID, replyCtx)
	if client != nil {
		rc.ClientID = client.id
		if transportSessionKey == "" {
			transportSessionKey = client.transportSessionKey
		}
		if route == "" {
			route = client.route
		}
	}
	rc.TransportSessionKey = strings.TrimSpace(transportSessionKey)
	rc.Route = strings.TrimSpace(route)
	return rc
}

func bridgeOutboundBase(msgType string, rc *bridgeReplyCtx) map[string]any {
	payload := map[string]any{
		"type":        msgType,
		"session_key": rc.SessionKey,
	}
	if sessionID := strings.TrimSpace(rc.SessionID); sessionID != "" {
		payload["session_id"] = sessionID
	}
	if transportSessionKey := strings.TrimSpace(rc.TransportSessionKey); transportSessionKey != "" {
		payload["transport_session_key"] = transportSessionKey
	}
	if route := strings.TrimSpace(rc.Route); route != "" {
		payload["route"] = route
	}
	return payload
}

func bridgeAddSessionID(payload map[string]any, sessionID string) map[string]any {
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		payload["session_id"] = sessionID
	}
	return payload
}

func bridgeProgressStyleForAdapter(a *bridgeAdapter) string {
	if a == nil {
		return progressStyleLegacy
	}
	if style, ok := bridgeMetadataString(a.metadata, "progress_style"); ok {
		return normalizeProgressStyle(style)
	}
	if a.capabilities["preview"] && a.capabilities["update_message"] {
		if a.capabilities["card"] {
			return progressStyleCard
		}
		return progressStyleCompact
	}
	return progressStyleLegacy
}

func bridgeSupportsProgressCardPayloadForAdapter(a *bridgeAdapter) bool {
	if a == nil {
		return false
	}
	if supported, ok := bridgeMetadataBool(a.metadata, "supports_progress_card_payload"); ok {
		return supported
	}
	adapterName, _ := bridgeMetadataString(a.metadata, "adapter")
	return adapterName == "bot-gateway" && a.capabilities["preview"] && a.capabilities["update_message"]
}

func bridgeMetadataString(metadata map[string]any, key string) (string, bool) {
	if metadata == nil {
		return "", false
	}
	raw, ok := metadata[key]
	if !ok {
		return "", false
	}
	value, ok := raw.(string)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return value, true
}

func bridgeMetadataBool(metadata map[string]any, key string) (bool, bool) {
	if metadata == nil {
		return false, false
	}
	raw, ok := metadata[key]
	if !ok {
		return false, false
	}
	value, ok := raw.(bool)
	if !ok {
		return false, false
	}
	return value, true
}

func buildBridgeReconstructReplyCtx(project, sessionKey string) (string, error) {
	chatID, err := bridgeTransportChatID(sessionKey)
	if err != nil {
		return "", err
	}
	payload := bridgeReconstructReplyCtxPayload{
		Kind:                bridgeReconstructReplyCtxKind,
		Version:             1,
		SenderProject:       project,
		TransportChatID:     chatID,
		TransportSessionKey: sessionKey,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("bridge: marshal reconstruct reply ctx: %w", err)
	}
	return string(data), nil
}

func bridgeTransportChatID(sessionKey string) (string, error) {
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) < 2 || parts[1] == "" {
		return "", fmt.Errorf("bridge: invalid session key %q", sessionKey)
	}
	return parts[1], nil
}

func (bp *BridgePlatform) SendCard(ctx context.Context, replyCtx any, card *Card) error {
	rc, ok := replyCtx.(*bridgeReplyCtx)
	if !ok {
		return fmt.Errorf("bridge: invalid reply context")
	}
	a := bp.server.getAdapter(rc.Platform)
	if a == nil || !a.capabilities["card"] {
		return bp.Reply(ctx, replyCtx, card.RenderText())
	}
	payload := bridgeOutboundBase("card", rc)
	payload["reply_ctx"] = rc.ReplyCtx
	payload["card"] = serializeCard(card)
	return bp.server.sendToReplyTarget(rc, payload)
}

func (bp *BridgePlatform) ReplyCard(ctx context.Context, replyCtx any, card *Card) error {
	return bp.SendCard(ctx, replyCtx, card)
}

func (bp *BridgePlatform) SendWithButtons(ctx context.Context, replyCtx any, content string, buttons [][]ButtonOption) error {
	rc, ok := replyCtx.(*bridgeReplyCtx)
	if !ok {
		return fmt.Errorf("bridge: invalid reply context")
	}
	a := bp.server.getAdapter(rc.Platform)
	if a == nil || !a.capabilities["buttons"] {
		return bp.Reply(ctx, replyCtx, content)
	}
	payload := bridgeOutboundBase("buttons", rc)
	payload["reply_ctx"] = rc.ReplyCtx
	payload["content"] = content
	payload["buttons"] = buttons
	return bp.server.sendToReplyTarget(rc, payload)
}

func (bp *BridgePlatform) UpdateMessage(ctx context.Context, replyCtx any, content string) error {
	rc, ok := replyCtx.(*bridgeReplyCtx)
	if !ok {
		return fmt.Errorf("bridge: invalid reply context")
	}
	a := bp.server.getAdapter(rc.Platform)
	if a == nil || !a.capabilities["update_message"] {
		return ErrNotSupported
	}
	payload := bridgeOutboundBase("update_message", rc)
	payload["preview_handle"] = rc.ReplyCtx
	payload["content"] = content
	return bp.server.sendToReplyTarget(rc, payload)
}

func (bp *BridgePlatform) SendPreviewStart(ctx context.Context, replyCtx any, content string) (previewHandle any, err error) {
	rc, ok := replyCtx.(*bridgeReplyCtx)
	if !ok {
		return nil, fmt.Errorf("bridge: invalid reply context")
	}
	a := bp.server.getAdapter(rc.Platform)
	if a == nil || !a.capabilities["preview"] {
		return nil, ErrNotSupported
	}

	refID := fmt.Sprintf("prev-%d", time.Now().UnixNano())
	ch := make(chan string, 1)

	a.previewMu.Lock()
	a.previewRequests[refID] = ch
	a.previewMu.Unlock()

	payload := bridgeOutboundBase("preview_start", rc)
	payload["ref_id"] = refID
	payload["reply_ctx"] = rc.ReplyCtx
	payload["content"] = content
	if err := bp.server.sendToReplyTarget(rc, payload); err != nil {
		a.previewMu.Lock()
		delete(a.previewRequests, refID)
		a.previewMu.Unlock()
		return nil, err
	}

	select {
	case handle := <-ch:
		previewRC := newBridgeReplyCtxWithSessionID(a, rc.SessionKey, rc.SessionID, handle)
		previewRC.TransportSessionKey = rc.TransportSessionKey
		previewRC.Route = rc.Route
		previewRC.ClientID = rc.ClientID
		return previewRC, nil
	case <-time.After(10 * time.Second):
		a.previewMu.Lock()
		delete(a.previewRequests, refID)
		a.previewMu.Unlock()
		return nil, fmt.Errorf("bridge: preview_ack timeout")
	case <-ctx.Done():
		a.previewMu.Lock()
		delete(a.previewRequests, refID)
		a.previewMu.Unlock()
		return nil, ctx.Err()
	}
}

func (bp *BridgePlatform) DeletePreviewMessage(ctx context.Context, previewHandle any) error {
	rc, ok := previewHandle.(*bridgeReplyCtx)
	if !ok {
		return fmt.Errorf("bridge: invalid preview handle")
	}
	a := bp.server.getAdapter(rc.Platform)
	if a == nil || !a.capabilities["delete_message"] {
		return ErrNotSupported
	}
	payload := bridgeOutboundBase("delete_message", rc)
	payload["preview_handle"] = rc.ReplyCtx
	return bp.server.sendToReplyTarget(rc, payload)
}

func (bp *BridgePlatform) StartTyping(ctx context.Context, replyCtx any) (stop func()) {
	rc, ok := replyCtx.(*bridgeReplyCtx)
	if !ok {
		return func() {}
	}
	a := bp.server.getAdapter(rc.Platform)
	if a == nil || !a.capabilities["typing"] {
		return func() {}
	}
	startPayload := bridgeOutboundBase("typing_start", rc)
	startPayload["reply_ctx"] = rc.ReplyCtx
	_ = bp.server.sendToReplyTarget(rc, startPayload)
	return func() {
		stopPayload := bridgeOutboundBase("typing_stop", rc)
		stopPayload["reply_ctx"] = rc.ReplyCtx
		_ = bp.server.sendToReplyTarget(rc, stopPayload)
	}
}

func (bp *BridgePlatform) SendAudio(ctx context.Context, replyCtx any, audio []byte, format string) error {
	rc, ok := replyCtx.(*bridgeReplyCtx)
	if !ok {
		return fmt.Errorf("bridge: invalid reply context")
	}
	a := bp.server.getAdapter(rc.Platform)
	if a == nil || !a.capabilities["audio"] {
		return ErrNotSupported
	}
	payload := bridgeOutboundBase("audio", rc)
	payload["reply_ctx"] = rc.ReplyCtx
	payload["data"] = base64.StdEncoding.EncodeToString(audio)
	payload["format"] = format
	return bp.server.sendToReplyTarget(rc, payload)
}

func (bp *BridgePlatform) SendImage(ctx context.Context, replyCtx any, img ImageAttachment) error {
	rc, ok := replyCtx.(*bridgeReplyCtx)
	if !ok {
		return fmt.Errorf("bridge: invalid reply context")
	}
	a := bp.server.getAdapter(rc.Platform)
	if a == nil || !a.capabilities["image"] {
		return ErrNotSupported
	}
	images := HistoryImagesFromAttachments([]ImageAttachment{img})
	if len(images) == 0 {
		return fmt.Errorf("bridge: image data is required")
	}
	payload := bridgeOutboundBase("image", rc)
	payload["reply_ctx"] = rc.ReplyCtx
	payload["image"] = images[0]
	payload["images"] = images
	return bp.server.sendToReplyTarget(rc, payload)
}

func (bp *BridgePlatform) SetCardNavigationHandler(h CardNavigationHandler) {
	bp.navHandler = h
}

func (bp *BridgePlatform) SetCardNavigationHandlerWithSessionID(h CardNavigationHandlerWithSessionID) {
	bp.navHandlerWithSessionID = h
}

// ---------------------------------------------------------------------------
// WebSocket connection handling (on BridgeServer)
// ---------------------------------------------------------------------------

func (bs *BridgeServer) handleWS(w http.ResponseWriter, r *http.Request) {
	if !bs.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("bridge: websocket upgrade failed", "error", err)
		return
	}

	slog.Info("bridge: new connection", "remote", conn.RemoteAddr())
	bs.handleConnection(conn)
}

func (bs *BridgeServer) handleConnection(conn *websocket.Conn) {
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(90 * time.Second)); err != nil {
		slog.Debug("bridge: set read deadline failed", "error", err)
		return
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	})

	// First message must identify either an external adapter ("register") or a
	// browser frontend client ("frontend_connect").
	_, raw, err := conn.ReadMessage()
	if err != nil {
		slog.Error("bridge: read register failed", "error", err)
		return
	}

	var base bridgeMsg
	if err := json.Unmarshal(raw, &base); err != nil {
		if err := writeJSON(conn, nil, map[string]any{"type": "register_ack", "ok": false, "error": "first message must be register or frontend_connect"}); err != nil {
			slog.Debug("bridge: write register ack failed", "error", err)
		}
		return
	}

	if base.Type == "frontend_connect" {
		var fc bridgeFrontendConnect
		if err := json.Unmarshal(raw, &fc); err != nil {
			if err := writeJSON(conn, nil, map[string]any{"type": "register_ack", "ok": false, "error": "invalid frontend_connect payload"}); err != nil {
				slog.Debug("bridge: write frontend ack failed", "error", err)
			}
			return
		}
		bs.handleFrontendConnection(conn, fc)
		return
	}

	var reg bridgeRegister
	if err := json.Unmarshal(raw, &reg); err != nil || reg.Type != "register" {
		if err := writeJSON(conn, nil, map[string]any{"type": "register_ack", "ok": false, "error": "first message must be register or frontend_connect"}); err != nil {
			slog.Debug("bridge: write register ack failed", "error", err)
		}
		return
	}

	if bridgeRegisterLooksLikeFrontendClient(reg) {
		bs.handleFrontendConnection(conn, bridgeFrontendConnectFromRegister(reg))
		return
	}

	if reg.Platform == "" {
		if err := writeJSON(conn, nil, map[string]any{"type": "register_ack", "ok": false, "error": "platform name is required"}); err != nil {
			slog.Debug("bridge: write register ack failed", "error", err)
		}
		return
	}

	caps := bridgeCapabilitiesMap(reg.Capabilities)

	adapter := &bridgeAdapter{
		platform:        reg.Platform,
		capabilities:    caps,
		metadata:        reg.Metadata,
		conn:            conn,
		server:          bs,
		previewRequests: make(map[string]chan string),
	}

	bs.mu.Lock()
	if old, exists := bs.adapters[reg.Platform]; exists {
		old.conn.Close()
		slog.Info("bridge: replaced existing adapter", "platform", reg.Platform)
	}
	bs.adapters[reg.Platform] = adapter
	bs.mu.Unlock()

	if err := writeJSON(conn, &adapter.writeMu, map[string]any{"type": "register_ack", "ok": true}); err != nil {
		slog.Debug("bridge: write register ack failed", "error", err)
		return
	}

	if bridgeMetadataStringListContains(reg.Metadata, "control_plane", bridgeCapabilitiesSnapshotProto) {
		if err := writeJSON(conn, &adapter.writeMu, bs.buildCapabilitiesSnapshot()); err != nil {
			slog.Debug("bridge: write capabilities snapshot failed", "platform", reg.Platform, "error", err)
			return
		}
	}

	slog.Info("bridge: adapter registered", "platform", reg.Platform, "capabilities", reg.Capabilities)

	defer func() {
		bs.mu.Lock()
		if bs.adapters[reg.Platform] == adapter {
			delete(bs.adapters, reg.Platform)
		}
		bs.mu.Unlock()
		slog.Info("bridge: adapter disconnected", "platform", reg.Platform)
	}()

	for {
		if err := conn.SetReadDeadline(time.Now().Add(90 * time.Second)); err != nil {
			slog.Debug("bridge: set read deadline failed", "platform", reg.Platform, "error", err)
			return
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Debug("bridge: read error", "platform", reg.Platform, "error", err)
			}
			return
		}

		var base bridgeMsg
		if err := json.Unmarshal(raw, &base); err != nil {
			slog.Debug("bridge: invalid JSON", "platform", reg.Platform, "error", err)
			continue
		}

		switch base.Type {
		case "message":
			adapter.handleMessage(raw)
		case "card_action":
			adapter.handleCardAction(raw)
		case "preview_ack":
			adapter.handlePreviewAck(raw)
		case "ping":
			if err := writeJSON(conn, &adapter.writeMu, map[string]any{"type": "pong", "ts": time.Now().UnixMilli()}); err != nil {
				slog.Debug("bridge: write pong failed", "platform", reg.Platform, "error", err)
				return
			}
		default:
			slog.Debug("bridge: unknown message type", "platform", reg.Platform, "type", base.Type)
		}
	}
}

func (bs *BridgeServer) handleFrontendConnection(conn *websocket.Conn, fc bridgeFrontendConnect) {
	client, svc, err := bs.registerFrontendClient(conn, fc)
	if err != nil {
		if writeErr := writeJSON(conn, nil, map[string]any{"type": "register_ack", "ok": false, "error": err.Error()}); writeErr != nil {
			slog.Debug("bridge: write frontend ack failed", "error", writeErr)
		}
		return
	}

	if err := writeJSON(conn, &client.writeMu, map[string]any{
		"type":      "register_ack",
		"ok":        true,
		"frontend":  true,
		"platform":  svc.platform,
		"slot":      svc.slot,
		"client_id": client.id,
	}); err != nil {
		slog.Debug("bridge: write frontend ack failed", "platform", svc.platform, "client_id", client.id, "error", err)
		return
	}

	slog.Info("bridge: frontend client connected",
		"platform", svc.platform, "slot", svc.slot, "route", client.route,
		"project", client.project, "client_id", client.id,
	)

	defer func() {
		bs.unregisterFrontendClient(svc.platform, client.id)
		slog.Info("bridge: frontend client disconnected", "platform", svc.platform, "client_id", client.id)
	}()

	for {
		if err := conn.SetReadDeadline(time.Now().Add(90 * time.Second)); err != nil {
			slog.Debug("bridge: set read deadline failed", "platform", svc.platform, "client_id", client.id, "error", err)
			return
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Debug("bridge: frontend read error", "platform", svc.platform, "client_id", client.id, "error", err)
			}
			return
		}

		bs.markFrontendClientSeen(svc.platform, client.id)

		var base bridgeMsg
		if err := json.Unmarshal(raw, &base); err != nil {
			slog.Debug("bridge: invalid frontend JSON", "platform", svc.platform, "client_id", client.id, "error", err)
			continue
		}

		switch base.Type {
		case "message":
			svc.adapter.handleMessageFromClient(raw, client)
		case "card_action":
			svc.adapter.handleCardActionFromClient(raw, client)
		case "preview_ack":
			svc.adapter.handlePreviewAck(raw)
		case "ping":
			if err := writeJSON(conn, &client.writeMu, map[string]any{"type": "pong", "ts": time.Now().UnixMilli()}); err != nil {
				slog.Debug("bridge: write frontend pong failed", "platform", svc.platform, "client_id", client.id, "error", err)
				return
			}
		default:
			slog.Debug("bridge: unknown frontend message type", "platform", svc.platform, "client_id", client.id, "type", base.Type)
		}
	}
}

func (bs *BridgeServer) registerFrontendClient(conn *websocket.Conn, fc bridgeFrontendConnect) (*bridgeFrontendClient, *bridgeFrontendService, error) {
	platform := bridgeFrontendServicePlatform(fc.Platform, fc.Slot, fc.Route, fc.TransportSessionKey, fc.SessionKey)
	if platform == "" {
		return nil, nil, fmt.Errorf("frontend service platform is required")
	}
	caps := bridgeCapabilitiesMap(fc.Capabilities)
	if len(fc.Capabilities) == 0 {
		caps = bridgeDefaultFrontendCapabilities()
	}
	now := time.Now().UTC()
	clientID := strings.TrimSpace(fc.ClientID)
	if clientID == "" {
		clientID = fmt.Sprintf("frontend-%d-%d", now.UnixNano(), atomic.AddUint64(&bs.frontendClientSeq, 1))
	}
	client := &bridgeFrontendClient{
		id:                  clientID,
		platform:            platform,
		app:                 strings.TrimSpace(fc.App),
		slot:                strings.TrimSpace(fc.Slot),
		project:             strings.TrimSpace(fc.Project),
		route:               strings.TrimSpace(fc.Route),
		sessionKey:          strings.TrimSpace(fc.SessionKey),
		transportSessionKey: strings.TrimSpace(fc.TransportSessionKey),
		conn:                conn,
		connectedAt:         now,
		lastSeen:            now,
	}
	if client.slot == "" {
		client.slot = platform
	}
	if client.route == "" {
		client.route = client.slot
	}

	bs.mu.Lock()
	defer bs.mu.Unlock()
	svc := bs.ensureFrontendServiceLocked(platform, client.app, client.slot, client.project, caps, fc.Metadata, now)
	svc.clients[client.id] = client
	return client, svc, nil
}

func (bs *BridgeServer) unregisterFrontendClient(platform, clientID string) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if svc, ok := bs.frontendServices[platform]; ok {
		delete(svc.clients, clientID)
		svc.updatedAt = time.Now().UTC()
	}
}

func (bs *BridgeServer) markFrontendClientSeen(platform, clientID string) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if svc, ok := bs.frontendServices[platform]; ok {
		if client, ok := svc.clients[clientID]; ok {
			client.lastSeen = time.Now().UTC()
		}
	}
}

// ---------------------------------------------------------------------------
// Adapter message handlers
// ---------------------------------------------------------------------------

func (a *bridgeAdapter) handleMessage(raw json.RawMessage) {
	a.handleMessageFromClient(raw, nil)
}

func (a *bridgeAdapter) handleMessageFromClient(raw json.RawMessage, client *bridgeFrontendClient) {
	var m bridgeMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		slog.Debug("bridge: invalid message payload", "error", err)
		return
	}

	if m.SessionKey == "" || m.UserID == "" {
		slog.Debug("bridge: message missing required fields", "platform", a.platform)
		return
	}

	project := m.Project
	if project == "" && client != nil {
		project = client.project
	}
	ref := a.server.resolveEngine(m.SessionKey, project)
	if ref == nil {
		slog.Warn("bridge: no engine for session", "platform", a.platform, "session_key", m.SessionKey, "project", project)
		return
	}

	rc := newBridgeReplyCtxWithFrontendClient(a, m.SessionKey, m.SessionID, m.ReplyCtx, m.TransportSessionKey, m.Route, client)
	msg := &Message{
		SessionKey: m.SessionKey,
		SessionID:  m.SessionID,
		Platform:   a.platform,
		MessageID:  m.MsgID,
		UserID:     m.UserID,
		UserName:   m.UserName,
		Content:    m.Content,
		ReplyCtx:   rc,
	}

	images, err := bridgeImagesToAttachments(m.Images)
	if err != nil {
		slog.Warn("bridge: rejecting message with invalid images", "platform", a.platform, "session_key", m.SessionKey, "error", err)
		a.sendError("invalid_images", err.Error(), rc)
		return
	}
	msg.Images = images

	for _, f := range m.Files {
		data, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			slog.Debug("bridge: invalid file base64", "error", err)
			continue
		}
		msg.Files = append(msg.Files, FileAttachment{
			MimeType: f.MimeType, Data: data, FileName: f.FileName,
		})
	}

	if m.Audio != nil {
		if data, err := base64.StdEncoding.DecodeString(m.Audio.Data); err == nil {
			msg.Audio = &AudioAttachment{
				MimeType: m.Audio.MimeType, Data: data,
				Format: m.Audio.Format, Duration: m.Audio.Duration,
			}
		}
	}

	slog.Info("bridge: message received",
		"platform", a.platform, "session_key", m.SessionKey,
		"session_id", m.SessionID, "user", m.UserID, "content_len", len(m.Content),
	)

	if ref.platform.handler != nil {
		ref.platform.handler(ref.platform, msg)
	}
}

func bridgeImagesToAttachments(images []bridgeImageData) ([]ImageAttachment, error) {
	if len(images) == 0 {
		return nil, nil
	}
	if err := validateImageAttachmentCount(len(images)); err != nil {
		return nil, err
	}
	out := make([]ImageAttachment, 0, len(images))
	for i, img := range images {
		attachment, err := imageAttachmentFromBase64(img.MimeType, img.Data, img.FileName)
		if err != nil {
			return nil, fmt.Errorf("image %d: %w", i+1, err)
		}
		out = append(out, attachment)
	}
	return out, nil
}

func (a *bridgeAdapter) sendError(code, message string, rc *bridgeReplyCtx) {
	payload := map[string]any{
		"type":    "error",
		"code":    code,
		"message": message,
	}
	if rc != nil && strings.TrimSpace(rc.SessionKey) != "" {
		payload["session_key"] = rc.SessionKey
	}
	if rc != nil && strings.TrimSpace(rc.SessionID) != "" {
		payload["session_id"] = rc.SessionID
	}
	if err := a.server.sendToReplyTarget(rc, payload); err != nil {
		slog.Debug("bridge: write error payload failed", "platform", a.platform, "error", err)
	}
}

func (a *bridgeAdapter) handleCardAction(raw json.RawMessage) {
	a.handleCardActionFromClient(raw, nil)
}

func (a *bridgeAdapter) handleCardActionFromClient(raw json.RawMessage, client *bridgeFrontendClient) {
	var ca bridgeCardAction
	if err := json.Unmarshal(raw, &ca); err != nil {
		slog.Debug("bridge: invalid card_action payload", "error", err)
		return
	}

	project := ca.Project
	if project == "" && client != nil {
		project = client.project
	}
	slog.Debug("bridge: card_action", "platform", a.platform, "action", ca.Action, "session_key", ca.SessionKey, "session_id", ca.SessionID, "project", project)

	ref := a.server.resolveEngine(ca.SessionKey, project)
	if ref == nil {
		return
	}

	// perm: — permission response; convert to a regular message for the engine
	if strings.HasPrefix(ca.Action, "perm:") {
		var responseText string
		switch ca.Action {
		case "perm:allow":
			responseText = "allow"
		case "perm:deny":
			responseText = "deny"
		case "perm:allow_all":
			responseText = "allow all"
		default:
			return
		}
		a.dispatchAsMessage(ref, ca.SessionKey, ca.SessionID, ca.ReplyCtx, responseText, ca.TransportSessionKey, ca.Route, client)
		return
	}

	// askq: — AskUserQuestion answer; forward as a regular message
	if strings.HasPrefix(ca.Action, "askq:") {
		a.dispatchAsMessage(ref, ca.SessionKey, ca.SessionID, ca.ReplyCtx, ca.Action, ca.TransportSessionKey, ca.Route, client)
		return
	}

	// cmd: — command shortcut from a card button; forward as a message
	if strings.HasPrefix(ca.Action, "cmd:") {
		cmdText := strings.TrimPrefix(ca.Action, "cmd:")
		a.dispatchAsMessage(ref, ca.SessionKey, ca.SessionID, ca.ReplyCtx, cmdText, ca.TransportSessionKey, ca.Route, client)
		return
	}

	// nav: / act: — card navigation and in-place updates
	var card *Card
	if ref.platform.navHandlerWithSessionID != nil {
		card = ref.platform.navHandlerWithSessionID(ca.Action, ca.SessionKey, ca.SessionID)
	} else if ref.platform.navHandler != nil {
		card = ref.platform.navHandler(ca.Action, ca.SessionKey)
	} else {
		return
	}
	if card == nil {
		return
	}

	rc := newBridgeReplyCtxWithFrontendClient(a, ca.SessionKey, ca.SessionID, ca.ReplyCtx, ca.TransportSessionKey, ca.Route, client)
	if a.capabilities["card"] {
		payload := bridgeOutboundBase("card", rc)
		payload["reply_ctx"] = ca.ReplyCtx
		payload["card"] = serializeCard(card)
		_ = a.server.sendToReplyTarget(rc, payload)
	} else {
		_ = ref.platform.Reply(context.Background(), rc, card.RenderText())
	}
}

// dispatchAsMessage converts a card action into a regular user message
// and dispatches it to the engine's message handler.
func (a *bridgeAdapter) dispatchAsMessage(ref *bridgeEngineRef, sessionKey, sessionID, replyCtx, content, transportSessionKey, route string, client *bridgeFrontendClient) {
	if ref.platform.handler == nil {
		return
	}
	msg := &Message{
		SessionKey: sessionKey,
		SessionID:  sessionID,
		Platform:   a.platform,
		UserID:     "web-admin",
		UserName:   "Web Admin",
		Content:    content,
		ReplyCtx:   newBridgeReplyCtxWithFrontendClient(a, sessionKey, sessionID, replyCtx, transportSessionKey, route, client),
	}
	go ref.platform.handler(ref.platform, msg)
}

func (a *bridgeAdapter) handlePreviewAck(raw json.RawMessage) {
	var ack bridgePreviewAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		return
	}

	a.previewMu.Lock()
	ch, ok := a.previewRequests[ack.RefID]
	if ok {
		delete(a.previewRequests, ack.RefID)
	}
	a.previewMu.Unlock()

	if ok {
		ch <- ack.PreviewHandle
	}
}

// ---------------------------------------------------------------------------
// Session management REST API (on BridgeServer)
// ---------------------------------------------------------------------------

// authHTTP wraps an HTTP handler with token authentication.
func (bs *BridgeServer) authHTTP(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !bs.authenticate(r) {
			bridgeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		handler(w, r)
	}
}

func bridgeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data}); err != nil {
		slog.Debug("bridge: write JSON failed", "error", err)
	}
}

func bridgeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg}); err != nil {
		slog.Debug("bridge: write JSON failed", "error", err)
	}
}

func bridgeSessionForKey(sm *SessionManager, sessionKey, sessionID string) (*Session, bool) {
	if sm == nil || sessionKey == "" || sessionID == "" {
		return nil, false
	}
	for _, s := range sm.ListSessions(sessionKey) {
		if s != nil && s.ID == sessionID {
			return s, true
		}
	}
	return nil, false
}

func bridgeSessionSummary(sessionKey, activeID string, s *Session) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"id":            s.ID,
		"session_key":   sessionKey,
		"name":          s.Name,
		"active":        s.ID == activeID,
		"history_count": len(s.History),
		"created_at":    s.CreatedAt,
		"updated_at":    s.UpdatedAt,
	}
}

// resolveEngineForSessionKey returns the engine ref for a given session key and optional project.
func (bs *BridgeServer) resolveEngineForSessionKey(sessionKey, project string) *bridgeEngineRef {
	return bs.resolveEngine(sessionKey, project)
}

// handleSessions handles GET /bridge/sessions and POST /bridge/sessions.
func (bs *BridgeServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sessionKey := r.URL.Query().Get("session_key")
		if sessionKey == "" {
			bridgeError(w, http.StatusBadRequest, "session_key query parameter is required")
			return
		}
		project := r.URL.Query().Get("project")
		ref := bs.resolveEngineForSessionKey(sessionKey, project)
		if ref == nil {
			bridgeError(w, http.StatusNotFound, "no engine found for session key")
			return
		}

		sessions := ref.engine.sessions.ListSessions(sessionKey)
		activeID := ref.engine.sessions.ActiveSessionID(sessionKey)

		list := make([]map[string]any, 0, len(sessions))
		for _, s := range sessions {
			list = append(list, bridgeSessionSummary(sessionKey, activeID, s))
		}

		bridgeJSON(w, http.StatusOK, map[string]any{
			"sessions":          list,
			"active_session_id": activeID,
		})

	case http.MethodPost:
		var body struct {
			SessionKey string `json:"session_key"`
			Name       string `json:"name"`
			Project    string `json:"project,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			bridgeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.SessionKey == "" {
			bridgeError(w, http.StatusBadRequest, "session_key is required")
			return
		}
		ref := bs.resolveEngineForSessionKey(body.SessionKey, body.Project)
		if ref == nil {
			bridgeError(w, http.StatusNotFound, "no engine found for session key")
			return
		}
		name := body.Name
		if name == "" {
			name = "default"
		}
		s := ref.engine.sessions.NewSession(body.SessionKey, name)
		bridgeJSON(w, http.StatusOK, map[string]any{
			"id":          s.ID,
			"session_key": body.SessionKey,
			"name":        s.GetName(),
			"created_at":  s.CreatedAt,
			"updated_at":  s.UpdatedAt,
			"message":     "session created",
		})

	default:
		bridgeError(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

// handleSessionRoutes dispatches /bridge/sessions/{sub} routes.
func (bs *BridgeServer) handleSessionRoutes(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(r.URL.Path, "/bridge/sessions/")
	if sub == "" {
		bridgeError(w, http.StatusBadRequest, "session id required")
		return
	}

	// POST /bridge/sessions/switch
	if sub == "switch" {
		bs.handleSessionSwitch(w, r)
		return
	}

	// GET or DELETE /bridge/sessions/{id}
	sessionKey := r.URL.Query().Get("session_key")
	if sessionKey == "" {
		bridgeError(w, http.StatusBadRequest, "session_key query parameter is required")
		return
	}
	project := r.URL.Query().Get("project")
	ref := bs.resolveEngineForSessionKey(sessionKey, project)
	if ref == nil {
		bridgeError(w, http.StatusNotFound, "no engine found for session key")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s, ok := bridgeSessionForKey(ref.engine.sessions, sessionKey, sub)
		if !ok {
			bridgeError(w, http.StatusNotFound, "session not found")
			return
		}
		histLimit := 50
		if v := r.URL.Query().Get("history_limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				histLimit = n
			}
		}
		hist := s.GetHistory(histLimit)
		histJSON := make([]map[string]any, len(hist))
		for i, h := range hist {
			histJSON[i] = historyEntryMap(h, false)
		}
		bridgeJSON(w, http.StatusOK, map[string]any{
			"id":          s.ID,
			"session_key": sessionKey,
			"name":        s.GetName(),
			"created_at":  s.CreatedAt,
			"updated_at":  s.UpdatedAt,
			"history":     histJSON,
		})

	case http.MethodDelete:
		if _, ok := bridgeSessionForKey(ref.engine.sessions, sessionKey, sub); !ok {
			bridgeError(w, http.StatusNotFound, "session not found")
			return
		}
		if ref.engine.sessions.DeleteByID(sub) {
			bridgeJSON(w, http.StatusOK, map[string]string{"message": "session deleted"})
		} else {
			bridgeError(w, http.StatusNotFound, "session not found")
		}

	default:
		bridgeError(w, http.StatusMethodNotAllowed, "GET or DELETE only")
	}
}

// handleSessionSwitch handles POST /bridge/sessions/switch.
func (bs *BridgeServer) handleSessionSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		bridgeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		SessionKey string `json:"session_key"`
		SessionID  string `json:"session_id,omitempty"`
		Target     string `json:"target"`
		Project    string `json:"project,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		bridgeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	target := strings.TrimSpace(body.SessionID)
	if target == "" {
		target = strings.TrimSpace(body.Target)
	}
	if body.SessionKey == "" || target == "" {
		bridgeError(w, http.StatusBadRequest, "session_key and session_id are required")
		return
	}
	ref := bs.resolveEngineForSessionKey(body.SessionKey, body.Project)
	if ref == nil {
		bridgeError(w, http.StatusNotFound, "no engine found for session key")
		return
	}
	s, err := ref.engine.sessions.SwitchSession(body.SessionKey, target)
	if err != nil {
		bridgeError(w, http.StatusNotFound, err.Error())
		return
	}
	bridgeJSON(w, http.StatusOK, map[string]any{
		"message":           "session switched",
		"session_key":       body.SessionKey,
		"session_id":        s.ID,
		"active_session_id": s.ID,
	})
}

// ---------------------------------------------------------------------------
// Internal helpers (on BridgeServer)
// ---------------------------------------------------------------------------

func (bs *BridgeServer) authenticate(r *http.Request) bool {
	if bs.token == "" {
		return true
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		if strings.HasPrefix(auth, "Bearer ") {
			return subtle.ConstantTimeCompare([]byte(auth[7:]), []byte(bs.token)) == 1
		}
	}
	if tok := r.Header.Get("X-Bridge-Token"); tok != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(bs.token)) == 1
	}
	if tok := r.URL.Query().Get("token"); tok != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(bs.token)) == 1
	}
	return false
}

func (bs *BridgeServer) getAdapter(platform string) *bridgeAdapter {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	if a := bs.adapters[platform]; a != nil {
		return a
	}
	if svc := bs.frontendServices[platform]; svc != nil {
		return svc.adapter
	}
	return nil
}

func (bs *BridgeServer) sendToReplyTarget(rc *bridgeReplyCtx, msg map[string]any) error {
	if rc == nil {
		return fmt.Errorf("bridge: missing reply target")
	}
	if strings.TrimSpace(rc.ClientID) != "" {
		return bs.sendToFrontendTarget(rc.Platform, rc.ClientID, msg)
	}
	return bs.sendToAdapter(rc.Platform, msg)
}

func (bs *BridgeServer) sendToAdapter(platform string, msg map[string]any) error {
	bs.mu.RLock()
	a, ok := bs.adapters[platform]
	bs.mu.RUnlock()
	if !ok {
		return bs.sendToFrontendTarget(platform, "", msg)
	}
	return writeJSON(a.conn, &a.writeMu, msg)
}

func (bs *BridgeServer) sendToFrontendTarget(platform, clientID string, msg map[string]any) error {
	bs.mu.RLock()
	svc, ok := bs.frontendServices[platform]
	if !ok {
		bs.mu.RUnlock()
		return fmt.Errorf("bridge: adapter %q not connected", platform)
	}

	var clients []*bridgeFrontendClient
	if clientID != "" {
		if client := svc.clients[clientID]; client != nil {
			clients = append(clients, client)
		}
	} else {
		sessionKey, _ := msg["session_key"].(string)
		route, _ := msg["route"].(string)
		for _, client := range svc.clients {
			if !bridgeFrontendClientMatches(client, sessionKey, route) {
				continue
			}
			clients = append(clients, client)
		}
	}
	bs.mu.RUnlock()

	if len(clients) == 0 {
		return fmt.Errorf("bridge: frontend service %q has no connected clients", platform)
	}

	var firstErr error
	for _, client := range clients {
		if err := writeJSON(client.conn, &client.writeMu, msg); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return fmt.Errorf("bridge: send to frontend service %q: %w", platform, firstErr)
	}
	return nil
}

func bridgeFrontendClientMatches(client *bridgeFrontendClient, sessionKey, route string) bool {
	if client == nil {
		return false
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey != "" {
		if client.sessionKey != "" && client.sessionKey != sessionKey {
			return false
		}
		if client.sessionKey == "" && client.transportSessionKey != "" && client.transportSessionKey != sessionKey {
			return false
		}
	}
	route = strings.TrimSpace(route)
	if route != "" && client.route != "" && client.route != route {
		return false
	}
	return true
}

func (bs *BridgeServer) ensureFrontendServiceLocked(platform, app, slot, project string, caps map[string]bool, metadata map[string]any, now time.Time) *bridgeFrontendService {
	if bs.frontendServices == nil {
		bs.frontendServices = make(map[string]*bridgeFrontendService)
	}
	if caps == nil {
		caps = bridgeDefaultFrontendCapabilities()
	}
	svc := bs.frontendServices[platform]
	if svc == nil {
		adapter := &bridgeAdapter{
			platform:        platform,
			capabilities:    cloneBridgeCapabilities(caps),
			metadata:        cloneBridgeMetadata(metadata),
			server:          bs,
			previewRequests: make(map[string]chan string),
		}
		svc = &bridgeFrontendService{
			platform:     platform,
			app:          strings.TrimSpace(app),
			slot:         strings.TrimSpace(slot),
			project:      strings.TrimSpace(project),
			capabilities: adapter.capabilities,
			metadata:     adapter.metadata,
			adapter:      adapter,
			clients:      make(map[string]*bridgeFrontendClient),
			registeredAt: now,
			updatedAt:    now,
		}
		if svc.slot == "" {
			svc.slot = platform
		}
		bs.frontendServices[platform] = svc
		return svc
	}
	if strings.TrimSpace(app) != "" {
		svc.app = strings.TrimSpace(app)
	}
	if strings.TrimSpace(slot) != "" {
		svc.slot = strings.TrimSpace(slot)
	}
	if strings.TrimSpace(project) != "" {
		svc.project = strings.TrimSpace(project)
	}
	for capName, enabled := range caps {
		if enabled {
			svc.capabilities[capName] = true
		}
	}
	if metadata != nil {
		svc.metadata = cloneBridgeMetadata(metadata)
		svc.adapter.metadata = svc.metadata
	}
	svc.updatedAt = now
	return svc
}

func bridgeFrontendServiceSnapshotLocked(svc *bridgeFrontendService) BridgeFrontendServiceInfo {
	if svc == nil {
		return BridgeFrontendServiceInfo{}
	}
	caps := make([]string, 0, len(svc.capabilities))
	for c := range svc.capabilities {
		caps = append(caps, c)
	}
	return BridgeFrontendServiceInfo{
		Platform:         svc.platform,
		App:              svc.app,
		Slot:             svc.slot,
		Project:          svc.project,
		Capabilities:     caps,
		Metadata:         cloneBridgeMetadata(svc.metadata),
		ConnectedClients: len(svc.clients),
		RegisteredAt:     svc.registeredAt,
		UpdatedAt:        svc.updatedAt,
	}
}

func bridgeCapabilitiesMap(capabilities []string) map[string]bool {
	caps := make(map[string]bool, len(capabilities)+1)
	for _, c := range capabilities {
		c = strings.TrimSpace(c)
		if c != "" {
			caps[c] = true
		}
	}
	caps["text"] = true
	return caps
}

func bridgeDefaultFrontendCapabilities() map[string]bool {
	return bridgeCapabilitiesMap([]string{
		"text",
		"image",
		"card",
		"buttons",
		"typing",
		"update_message",
		"preview",
		"delete_message",
		"reconstruct_reply",
	})
}

func cloneBridgeCapabilities(input map[string]bool) map[string]bool {
	if input == nil {
		return nil
	}
	out := make(map[string]bool, len(input))
	for k, v := range input {
		out[k] = v
	}
	return out
}

func cloneBridgeMetadata(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	out := make(map[string]any, len(input))
	for k, v := range input {
		out[k] = v
	}
	return out
}

func bridgeRegisterLooksLikeFrontendClient(reg bridgeRegister) bool {
	if strings.Contains(strings.TrimSpace(reg.Platform), "-tab-") {
		return true
	}
	if reg.Metadata == nil {
		return false
	}
	_, hasRoute := bridgeMetadataString(reg.Metadata, "route")
	_, hasTransportSessionKey := bridgeMetadataString(reg.Metadata, "transport_session_key")
	return hasRoute && hasTransportSessionKey
}

func bridgeFrontendConnectFromRegister(reg bridgeRegister) bridgeFrontendConnect {
	route, _ := bridgeMetadataString(reg.Metadata, "route")
	transportSessionKey, _ := bridgeMetadataString(reg.Metadata, "transport_session_key")
	project, _ := bridgeMetadataString(reg.Metadata, "project")
	slot, _ := bridgeMetadataString(reg.Metadata, "slot")
	if slot == "" {
		slot, _ = bridgeMetadataString(reg.Metadata, "frontend_slot")
	}
	return bridgeFrontendConnect{
		Type:                "frontend_connect",
		Platform:            reg.Platform,
		Slot:                slot,
		Route:               route,
		TransportSessionKey: transportSessionKey,
		Capabilities:        reg.Capabilities,
		Project:             project,
		Metadata:            reg.Metadata,
	}
}

func bridgeFrontendServicePlatform(platform, slot, route, transportSessionKey, sessionKey string) string {
	for _, candidate := range []string{
		slot,
		platform,
		route,
		bridgePlatformFromSessionKey(transportSessionKey),
		bridgePlatformFromSessionKey(sessionKey),
	} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || strings.Contains(candidate, "-tab-") {
			continue
		}
		return candidate
	}
	return ""
}

func bridgePlatformFromSessionKey(sessionKey string) string {
	if idx := strings.Index(sessionKey, ":"); idx > 0 {
		return sessionKey[:idx]
	}
	return ""
}

func bridgeMetadataStringListContains(metadata map[string]any, key, want string) bool {
	if metadata == nil || key == "" || want == "" {
		return false
	}
	raw, ok := metadata[key]
	if !ok {
		return false
	}
	items, ok := raw.([]any)
	if !ok {
		if stringsList, ok := raw.([]string); ok {
			for _, item := range stringsList {
				if strings.TrimSpace(item) == want {
					return true
				}
			}
		}
		return false
	}
	for _, item := range items {
		if s, ok := item.(string); ok && strings.TrimSpace(s) == want {
			return true
		}
	}
	return false
}

func (bs *BridgeServer) platformFromSessionKey(sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if idx := strings.Index(sessionKey, ":"); idx > 0 {
		candidate := sessionKey[:idx]
		bs.mu.RLock()
		_, ok := bs.adapters[candidate]
		if !ok {
			_, ok = bs.frontendServices[candidate]
		}
		bs.mu.RUnlock()
		if ok {
			return candidate
		}
	}
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	return bridgeFrontendServicePlatformForSessionKeyLocked(bs.frontendServices, sessionKey)
}

func bridgeFrontendServicePlatformForSessionKeyLocked(services map[string]*bridgeFrontendService, sessionKey string) string {
	if sessionKey == "" {
		return ""
	}
	matchedPlatform := ""
	for platform, svc := range services {
		if svc == nil {
			continue
		}
		for _, client := range svc.clients {
			if client == nil {
				continue
			}
			if client.sessionKey != sessionKey && client.transportSessionKey != sessionKey {
				continue
			}
			if matchedPlatform != "" && matchedPlatform != platform {
				return ""
			}
			matchedPlatform = platform
			break
		}
	}
	return matchedPlatform
}

// resolveEngine finds the engine to handle a message.
// It first tries to match by project name, then by session_key ownership,
// and finally falls back to the single-engine case.
func (bs *BridgeServer) resolveEngine(sessionKey, project string) *bridgeEngineRef {
	bs.enginesMu.RLock()
	defer bs.enginesMu.RUnlock()

	if project != "" {
		if ref, ok := bs.engines[project]; ok {
			return ref
		}
	}

	if len(bs.engines) == 1 {
		for _, ref := range bs.engines {
			return ref
		}
	}

	// Try to find the engine that owns sessions for this key.
	for _, ref := range bs.engines {
		if sessions := ref.engine.sessions.ListSessions(sessionKey); len(sessions) > 0 {
			return ref
		}
	}

	return nil
}

func writeJSON(conn *websocket.Conn, mu *sync.Mutex, v any) error {
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	return conn.WriteJSON(v)
}

// serializeCard converts a Card into a JSON-friendly map for the bridge protocol.
func serializeCard(c *Card) map[string]any {
	result := make(map[string]any)
	if c.Header != nil {
		result["header"] = map[string]string{
			"title": c.Header.Title,
			"color": c.Header.Color,
		}
	}
	var elements []map[string]any
	for _, elem := range c.Elements {
		switch e := elem.(type) {
		case CardMarkdown:
			elements = append(elements, map[string]any{"type": "markdown", "content": e.Content})
		case CardDivider:
			elements = append(elements, map[string]any{"type": "divider"})
		case CardActions:
			var btns []map[string]any
			for _, b := range e.Buttons {
				btns = append(btns, map[string]any{
					"text": b.Text, "btn_type": b.Type, "value": b.Value,
				})
			}
			elements = append(elements, map[string]any{
				"type": "actions", "buttons": btns, "layout": string(e.Layout),
			})
		case CardNote:
			m := map[string]any{"type": "note", "text": e.Text}
			if e.Tag != "" {
				m["tag"] = e.Tag
			}
			elements = append(elements, m)
		case CardListItem:
			elements = append(elements, map[string]any{
				"type": "list_item", "text": e.Text,
				"btn_text": e.BtnText, "btn_type": e.BtnType, "btn_value": e.BtnValue,
			})
		case CardSelect:
			var opts []map[string]string
			for _, o := range e.Options {
				opts = append(opts, map[string]string{"text": o.Text, "value": o.Value})
			}
			elements = append(elements, map[string]any{
				"type": "select", "placeholder": e.Placeholder,
				"options": opts, "init_value": e.InitValue,
			})
		}
	}
	result["elements"] = elements
	return result
}
