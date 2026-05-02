package webclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/webclient/internal/broker"
	"github.com/chenhg5/cc-connect/webclient/internal/store"
)

// Options configures the webclient backend server.
//
// Host/Port define the listen address. Token protects all API endpoints when non-empty
// (Authorization: Bearer <token>, or ?token=<token>). DataDir is required and is used
// for persistent message and attachment storage. PublicURL, when provided, is used
// to build absolute attachment URLs.
type Options struct {
	Host      string
	Port      int
	Token     string
	DataDir   string
	PublicURL string
	// Platform/DataNamespace apply only to legacy single-app mode (Apps empty).
	// They let the long-lived backend adapter register a custom Bridge platform
	// while preserving the old DataDir/webclient storage root.
	Platform      string
	DataNamespace string

	// Apps enables single-service multi-app mode. When empty, the server runs in
	// legacy single-app mode using root DataDir/webclient and Bridge platform
	// "webclient".
	//
	// When Apps is non-empty, each enabled app gets its own isolated store rooted
	// at DataDir/webclient/apps/{data_namespace} and its own external adapter
	// connection registered as app.Platform.
	Apps       []AppOptions
	DefaultApp string // optional app_id to use for legacy (non-/apps/) routes

	// ManagementBaseURL points at the management web/API server that this
	// webclient shell should call through its backend facade.
	ManagementBaseURL string
	ManagementToken   string
}

type AppOptions struct {
	ID            string
	Platform      string
	DataNamespace string
	Enabled       *bool
	Default       bool
}

type appRuntime struct {
	appID         string
	platform      string
	dataNamespace string
	root          string

	token     string
	publicURL string

	store   *store.Store
	events  *broker.Hub
	runEvts *broker.RunHub

	adapter *adapterClient

	// Prevent concurrent outbox recovery loops from double-delivering due items.
	outboxRecoverMu sync.Mutex
}

type Server struct {
	opts Options

	// Legacy/default app handles legacy routes (/api/*, /attachments/*) and backs
	// the copied admin frontend. In multi-app mode, app-specific routes are
	// dispatched through apps/defaultApp.
	store   *store.Store
	events  *broker.Hub
	runEvts *broker.RunHub
	handler http.Handler
	root    string

	apps         map[string]*appRuntime
	defaultAppID string
	defaultApp   *appRuntime

	mu               sync.RWMutex
	applyMu          sync.Mutex // serializes ApplyApps/Start/Stop to avoid interleaving runtime mutations
	started          bool
	httpServer       *http.Server
	listener         net.Listener
	projectHandlers  map[string]core.MessageHandler
	projectPlatforms map[string]*platform

	// static serves the webclient UI when configured or embedded.
	static http.Handler

	managementProxy *httputil.ReverseProxy

	// adapter is kept for legacy tests and single-app mode; it aliases the
	// default app runtime adapter when present.
	adapter *adapterClient
}

// ApplyAppsResult summarizes changes applied by Server.ApplyApps.
// It is intended for logging/diagnostics only.
type ApplyAppsResult struct {
	Added            []string
	Removed          []string
	AdapterRestarted []string // app IDs whose external adapter was restarted due to platform change
	DefaultAppID     string
}

func NewServer(opts Options) (*Server, error) {
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, fmt.Errorf("webclient: DataDir is required")
	}
	if opts.Port < 0 {
		return nil, fmt.Errorf("webclient: invalid port %d", opts.Port)
	}
	opts.Host = strings.TrimSpace(opts.Host) // empty means listen on all interfaces
	if opts.Port == 0 {
		opts.Port = 9840
	}
	opts.PublicURL = strings.TrimRight(strings.TrimSpace(opts.PublicURL), "/")
	opts.ManagementBaseURL = strings.TrimRight(strings.TrimSpace(opts.ManagementBaseURL), "/")

	s := &Server{
		opts:            opts,
		projectHandlers: make(map[string]core.MessageHandler),
		apps:            make(map[string]*appRuntime),
	}

	// Initialize runtimes (legacy single-app when opts.Apps is empty).
	if err := s.initApps(); err != nil {
		return nil, err
	}

	s.static = s.defaultStaticHandler()
	if opts.ManagementBaseURL != "" {
		if u, err := url.Parse(opts.ManagementBaseURL); err == nil && u.Scheme != "" && u.Host != "" {
			s.managementProxy = s.newManagementProxy(u)
		} else {
			return nil, fmt.Errorf("webclient: invalid management_base_url %q", opts.ManagementBaseURL)
		}
	}
	s.handler = s.newMux()
	return s, nil
}

// ApplyApps hot-reloads the multi-app runtime configuration without restarting the HTTP server.
//
// Supported updates (multi-app mode only):
// - add an enabled app
// - remove/disable an app
// - switch default app
// - change an app platform name (restarts that app's external adapter)
//
// Not supported (returns error and leaves runtime unchanged):
// - host/port/data_dir changes
// - switching between legacy single-app mode (Apps empty) and multi-app mode (Apps non-empty)
// - changing an existing app's data_namespace
func (s *Server) ApplyApps(opts Options) (*ApplyAppsResult, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	// Normalize input like NewServer.
	opts.Host = strings.TrimSpace(opts.Host)
	opts.DataDir = strings.TrimSpace(opts.DataDir)
	if opts.Port == 0 {
		opts.Port = 9840
	}

	if opts.DataDir == "" {
		return nil, fmt.Errorf("webclient: DataDir is required")
	}
	if opts.Port < 0 {
		return nil, fmt.Errorf("webclient: invalid port %d", opts.Port)
	}

	// Snapshot current state under lock (authoritative).
	s.mu.Lock()
	curHost := strings.TrimSpace(s.opts.Host)
	curPort := s.opts.Port
	if curPort == 0 {
		curPort = 9840
	}
	curDataDir := strings.TrimSpace(s.opts.DataDir)
	curModeLegacy := len(s.opts.Apps) == 0
	started := s.started
	curApps := make(map[string]*appRuntime, len(s.apps))
	for id, rt := range s.apps {
		curApps[id] = rt
	}

	// Reject immutable runtime fields.
	if opts.Host != curHost {
		s.mu.Unlock()
		return nil, fmt.Errorf("webclient: hot update of host is not supported")
	}
	if opts.Port != curPort {
		s.mu.Unlock()
		return nil, fmt.Errorf("webclient: hot update of port is not supported")
	}
	if filepath.Clean(opts.DataDir) != filepath.Clean(curDataDir) {
		s.mu.Unlock()
		return nil, fmt.Errorf("webclient: hot update of data_dir is not supported")
	}

	// Reject mode switches. (We key legacy-vs-multi on the original opts set at construction.)
	nextModeLegacy := len(opts.Apps) == 0
	if curModeLegacy != nextModeLegacy {
		s.mu.Unlock()
		return nil, fmt.Errorf("webclient: switching between legacy and multi-app modes is not supported")
	}

	// Legacy mode: no app hot reload semantics (nothing to do).
	if curModeLegacy {
		s.mu.Unlock()
		return &ApplyAppsResult{DefaultAppID: "webclient"}, nil
	}

	enabledByID, defaultID, err := normalizeEnabledApps(opts.Apps, opts.DefaultApp)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}

	// Disallow data_namespace changes for existing apps.
	for id, next := range enabledByID {
		if cur := curApps[id]; cur != nil {
			if strings.TrimSpace(next.DataNamespace) != strings.TrimSpace(cur.dataNamespace) {
				s.mu.Unlock()
				return nil, fmt.Errorf("webclient: app %q data_namespace change is not supported", id)
			}
		}
	}

	// Plan changes.
	var (
		addIDs     []string
		removeIDs  []string
		restartIDs []string
		addRoots   = make(map[string]string) // app_id -> root directory
	)
	for id := range enabledByID {
		if curApps[id] == nil {
			addIDs = append(addIDs, id)
		}
	}
	for id, cur := range curApps {
		if cur == nil {
			continue
		}
		if enabledByID[id] == nil {
			removeIDs = append(removeIDs, id)
			continue
		}
		if strings.TrimSpace(enabledByID[id].Platform) != strings.TrimSpace(cur.platform) {
			restartIDs = append(restartIDs, id)
		}
	}

	// Prepare roots for added apps; store.New happens outside the server lock.
	base := filepath.Join(curDataDir, "webclient")
	for _, id := range addIDs {
		a := enabledByID[id]
		root := filepath.Join(base, "apps", strings.TrimSpace(a.DataNamespace))
		addRoots[id] = root
	}

	// Stage adapter restarts: swap runtime first, then stop old adapter and start new adapter.
	type restartPlan struct {
		id       string
		old      *appRuntime
		newRT    *appRuntime
		oldAdapt *adapterClient
	}
	var restarts []restartPlan
	for _, id := range restartIDs {
		cur := curApps[id]
		if cur == nil {
			continue
		}
		next := enabledByID[id]
		if next == nil {
			continue
		}
		// New runtime reuses the same store + hubs to avoid disrupting SSE and to
		// avoid having multiple store instances targeting the same directory.
		newRT := &appRuntime{
			appID:         cur.appID,
			platform:      strings.TrimSpace(next.Platform),
			dataNamespace: cur.dataNamespace,
			root:          cur.root,
			token:         cur.token,
			publicURL:     cur.publicURL,
			store:         cur.store,
			events:        cur.events,
			runEvts:       cur.runEvts,
		}
		if strings.TrimSpace(s.opts.ManagementBaseURL) != "" {
			newRT.adapter = newAdapterClient(s, newRT)
		}
		restarts = append(restarts, restartPlan{
			id:       id,
			old:      cur,
			newRT:    newRT,
			oldAdapt: cur.adapter,
		})
	}

	// We have an authoritative plan and haven't performed any adapter/hub side
	// effects yet. Unlock to build any new stores (slow I/O).
	s.mu.Unlock()

	// Build new runtimes for added apps (slow I/O outside server lock).
	addRuntimes := make(map[string]*appRuntime, len(addRoots))
	for _, id := range addIDs {
		root := addRoots[id]
		a := enabledByID[id]
		st, err := store.New(root, s.opts.PublicURL)
		if err != nil {
			return nil, err
		}
		rt := &appRuntime{
			appID:         strings.TrimSpace(a.ID),
			platform:      strings.TrimSpace(a.Platform),
			dataNamespace: strings.TrimSpace(a.DataNamespace),
			root:          root,
			token:         strings.TrimSpace(s.opts.Token),
			publicURL:     strings.TrimRight(strings.TrimSpace(s.opts.PublicURL), "/"),
			store:         st,
			events:        broker.NewHub(),
			runEvts:       broker.NewRunHub(),
		}
		if strings.TrimSpace(s.opts.ManagementBaseURL) != "" {
			rt.adapter = newAdapterClient(s, rt)
		}
		addRuntimes[id] = rt
	}

	// Commit swap (short critical section). No external side effects occur before this point.
	var (
		removedRuntimes []*appRuntime
		oldRestarted    []*adapterClient
		startRuntimes   []*appRuntime
	)

	s.mu.Lock()
	// Re-check invariants under lock (Start/Stop may have run, but should not change these).
	if strings.TrimSpace(s.opts.Host) != curHost || s.opts.Port != curPort || filepath.Clean(strings.TrimSpace(s.opts.DataDir)) != filepath.Clean(curDataDir) {
		s.mu.Unlock()
		return nil, fmt.Errorf("webclient: server options changed during ApplyApps; retry")
	}
	if len(s.opts.Apps) == 0 {
		s.mu.Unlock()
		return nil, fmt.Errorf("webclient: server is not in multi-app mode")
	}

	// Remove apps first so new requests 404 immediately.
	for _, id := range removeIDs {
		if cur := s.apps[id]; cur != nil {
			delete(s.apps, id)
			removedRuntimes = append(removedRuntimes, cur)
		}
	}

	// Apply adapter restarts by swapping the runtime pointer.
	for _, rp := range restarts {
		if rp.newRT == nil {
			continue
		}
		s.apps[rp.id] = rp.newRT
		if rp.oldAdapt != nil {
			oldRestarted = append(oldRestarted, rp.oldAdapt)
		}
		// Update default pointer if this app was default.
		if s.defaultAppID == rp.id {
			s.defaultApp = rp.newRT
		}
		// Start new adapter after unlock if server is started.
		if started && rp.newRT.adapter != nil {
			startRuntimes = append(startRuntimes, rp.newRT)
		}
	}

	// Add new runtimes.
	for id, rt := range addRuntimes {
		s.apps[id] = rt
		if started && rt != nil && rt.adapter != nil {
			startRuntimes = append(startRuntimes, rt)
		}
	}

	// Select and apply new default.
	if strings.TrimSpace(defaultID) == "" {
		s.mu.Unlock()
		return nil, fmt.Errorf("webclient: default_app is empty")
	}
	nextDefault := s.apps[defaultID]
	if nextDefault == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("webclient: default_app %q is not configured/enabled", defaultID)
	}
	s.defaultAppID = defaultID
	s.defaultApp = nextDefault

	// Persist the latest app list for subsequent ApplyApps calls.
	s.opts.Apps = opts.Apps
	s.opts.DefaultApp = opts.DefaultApp

	// Legacy-compatible aliases point at the default app store/hubs.
	s.root = filepath.Join(curDataDir, "webclient")
	s.store = nextDefault.store
	s.events = nextDefault.events
	s.runEvts = nextDefault.runEvts
	s.adapter = nextDefault.adapter

	// Refresh started flag snapshot.
	started = s.started
	s.mu.Unlock()

	// Side effects happen after the swap so errors cannot leave us in a partially
	// applied state. Stop can block, so we do it after the map mutation so new
	// requests observe 404 immediately.
	for _, rt := range removedRuntimes {
		if rt == nil {
			continue
		}
		if rt.adapter != nil {
			rt.adapter.Stop()
		}
		rt.events.Close()
		rt.runEvts.Close()
	}

	for _, a := range oldRestarted {
		if a != nil {
			a.Stop()
		}
	}

	// Start adapters for added + restarted apps when server is started.
	if started {
		for _, rt := range startRuntimes {
			if rt == nil || rt.adapter == nil {
				continue
			}
			rt.adapter.Start()
			go func(rtt *appRuntime) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				_ = rtt.adapter.WaitReady(ctx)
				if n, err := rtt.recoverOutboxOnce(ctx, 50); err != nil {
					slog.Warn("webclient: outbox recovery failed", "app", rtt.appID, "error", err)
				} else if n > 0 {
					slog.Info("webclient: outbox recovery attempted", "app", rtt.appID, "count", n)
				}
			}(rt)
		}
	}

	return &ApplyAppsResult{
		Added:            addIDs,
		Removed:          removeIDs,
		AdapterRestarted: restartIDs,
		DefaultAppID:     defaultID,
	}, nil
}

func (s *Server) Platform(project string) core.Platform {
	return newPlatform(s, project)
}

func (s *Server) UsesExternalAdapter() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.defaultApp != nil && s.defaultApp.adapter != nil
}

func (s *Server) Start() error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}

	addr := net.JoinHostPort(s.opts.Host, strconv.Itoa(s.opts.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("webclient: listen %s: %w", addr, err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.httpServer = srv
	s.listener = ln
	s.started = true
	runtimes := make([]*appRuntime, 0, len(s.apps))
	for _, rt := range s.apps {
		runtimes = append(runtimes, rt)
	}
	s.mu.Unlock()

	go func() {
		err := srv.Serve(ln)
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		slog.Error("webclient: http server exited unexpectedly", "error", err)
	}()

	// Start external adapters per app (if configured).
	for _, rt := range runtimes {
		if rt.adapter == nil {
			continue
		}
		rt.adapter.Start()
		// Best-effort recovery: wait briefly for adapter readiness (bridge/mgmt
		// start after webclient in the single-binary setup), then attempt due
		// outbox items once.
		go func(rtt *appRuntime) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_ = rtt.adapter.WaitReady(ctx)
			if n, err := rtt.recoverOutboxOnce(ctx, 50); err != nil {
				slog.Warn("webclient: outbox recovery failed", "app", rtt.appID, "error", err)
			} else if n > 0 {
				slog.Info("webclient: outbox recovery attempted", "app", rtt.appID, "count", n)
			}
		}(rt)
	}

	return nil
}

// recoverOutboxOnce scans due outbox items for the default app and attempts delivery.
// Tests may call this directly; Start also invokes it once for each app.
func (s *Server) recoverOutboxOnce(ctx context.Context, limit int) (int, error) {
	rt := s.defaultRuntime()
	if rt == nil {
		return 0, fmt.Errorf("webclient: default app is not initialized")
	}
	return rt.recoverOutboxOnce(ctx, limit)
}

func (s *Server) Stop(ctx context.Context) error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = false
	srv := s.httpServer
	s.httpServer = nil
	ln := s.listener
	s.listener = nil
	runtimes := make([]*appRuntime, 0, len(s.apps))
	for _, rt := range s.apps {
		runtimes = append(runtimes, rt)
	}
	s.mu.Unlock()

	for _, rt := range runtimes {
		if rt.adapter != nil {
			rt.adapter.Stop()
		}
		rt.events.Close()
		rt.runEvts.Close()
	}
	if ln != nil {
		_ = ln.Close()
	}
	if srv == nil {
		return nil
	}
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("webclient: shutdown: %w", err)
	}
	return nil
}

func (s *Server) newMux() http.Handler {
	mux := http.NewServeMux()

	// API
	mux.HandleFunc("GET /healthz", s.wrap(s.handleHealthz))
	mux.HandleFunc("GET /api/projects/{project}/sessions", s.wrap(s.handleListSessions))
	mux.HandleFunc("GET /api/projects/{project}/sessions/{session}/messages", s.wrap(s.handleGetMessages))
	mux.HandleFunc("POST /api/projects/{project}/sessions/{session}/messages", s.wrap(s.handlePostMessage))
	mux.HandleFunc("GET /api/projects/{project}/sessions/{session}/events", s.wrap(s.handleEvents))
	mux.HandleFunc("GET /attachments/{id}", s.wrap(s.handleGetAttachment))

	// App-namespaced API
	mux.HandleFunc("GET /apps/{app_id}/api/projects/{project}/sessions", s.wrap(s.handleAppListSessions))
	mux.HandleFunc("GET /apps/{app_id}/api/projects/{project}/sessions/{session}/messages", s.wrap(s.handleAppGetMessages))
	mux.HandleFunc("POST /apps/{app_id}/api/projects/{project}/sessions/{session}/messages", s.wrap(s.handleAppPostMessage))
	mux.HandleFunc("GET /apps/{app_id}/api/projects/{project}/sessions/{session}/events", s.wrap(s.handleAppEvents))
	mux.HandleFunc("GET /apps/{app_id}/attachments/{id}", s.wrap(s.handleAppGetAttachment))

	// Client API facade for the copied management frontend served on 9840.
	mux.HandleFunc("GET /api/v1/projects/{project}/sessions", s.wrap(s.handleV1ListSessions))
	mux.HandleFunc("POST /api/v1/projects/{project}/sessions", s.wrap(s.handleV1CreateSession))
	mux.HandleFunc("GET /api/v1/projects/{project}/sessions/{id}", s.wrap(s.handleV1GetSession))
	mux.HandleFunc("PATCH /api/v1/projects/{project}/sessions/{id}", s.wrap(s.handleV1PatchSession))
	mux.HandleFunc("DELETE /api/v1/projects/{project}/sessions/{id}", s.wrap(s.handleV1DeleteSession))
	mux.HandleFunc("POST /api/v1/projects/{project}/sessions/switch", s.wrap(s.handleV1SwitchSession))
	mux.HandleFunc("POST /api/v1/projects/{project}/send", s.wrap(s.handleV1Send))
	mux.HandleFunc("GET /api/v1/settings", s.wrap(s.handleV1GetSettings))
	mux.HandleFunc("PATCH /api/v1/settings", s.wrap(s.handleV1PatchSettings))

	// App-namespaced v1 facade.
	mux.HandleFunc("GET /apps/{app_id}/api/v1/projects/{project}/sessions", s.wrap(s.handleAppV1ListSessions))
	mux.HandleFunc("POST /apps/{app_id}/api/v1/projects/{project}/sessions", s.wrap(s.handleAppV1CreateSession))
	mux.HandleFunc("GET /apps/{app_id}/api/v1/projects/{project}/sessions/{id}", s.wrap(s.handleAppV1GetSession))
	mux.HandleFunc("PATCH /apps/{app_id}/api/v1/projects/{project}/sessions/{id}", s.wrap(s.handleAppV1PatchSession))
	mux.HandleFunc("DELETE /apps/{app_id}/api/v1/projects/{project}/sessions/{id}", s.wrap(s.handleAppV1DeleteSession))
	mux.HandleFunc("POST /apps/{app_id}/api/v1/projects/{project}/sessions/switch", s.wrap(s.handleAppV1SwitchSession))
	mux.HandleFunc("POST /apps/{app_id}/api/v1/projects/{project}/send", s.wrap(s.handleAppV1Send))
	mux.HandleFunc("GET /apps/{app_id}/api/v1/settings", s.wrap(s.handleAppV1GetSettings))
	mux.HandleFunc("PATCH /apps/{app_id}/api/v1/settings", s.wrap(s.handleAppV1PatchSettings))

	// Management facade used by the copied web admin frontend served on 9840.
	for _, method := range []string{
		http.MethodGet,
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodOptions,
	} {
		mux.HandleFunc(method+" /api/v1/{path...}", s.wrap(s.handleManagementProxy))
	}
	// Legacy compatibility: older/copied shells used /bridge/ws (frontend_connect)
	// directly. The intended webclient flow is:
	// browser -> 9840 /api/v1/... -> 9840 adapter -> upstream bridge.
	// We keep /bridge/ws to avoid breaking old clients; new clients should not
	// rely on frontend_connect as the primary path.
	mux.HandleFunc("GET /bridge/ws", s.handleBridgeWS)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodOptions} {
		mux.HandleFunc(method+" /bridge/{path...}", s.handleBridgeProxy)
	}

	// UI assets are public so browsers can load CSS/JS without custom auth
	// headers. API and attachment routes above remain token-protected.
	mux.HandleFunc("/", s.handleUI)

	// CORS preflight
	mux.HandleFunc("OPTIONS /{path...}", s.wrap(s.handleOptions))

	return mux
}

func (s *Server) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.writeCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !s.authorize(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) authorize(r *http.Request) bool {
	token := strings.TrimSpace(s.opts.Token)
	if token == "" {
		return true
	}
	if q := strings.TrimSpace(r.URL.Query().Get("token")); q != "" {
		return q == token
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, prefix)) == token
}

func (s *Server) writeCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	// Minimal CORS: allow any origin for local UI usage. Token still protects APIs.
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Credentials", "true")
}

func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":   true,
		"name": "webclient",
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><html><head><meta charset=\"utf-8\"><title>openhouse-connect webclient</title></head><body><h1>openhouse-connect webclient</h1></body></html>"))
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	h := s.static
	s.mu.RUnlock()
	if h == nil {
		s.handleRoot(w, r)
		return
	}
	h.ServeHTTP(w, r)
}

func (s *Server) handleManagementProxy(w http.ResponseWriter, r *http.Request) {
	if s.managementProxy == nil {
		http.Error(w, "management proxy is not configured", http.StatusServiceUnavailable)
		return
	}
	s.managementProxy.ServeHTTP(w, r)
}

func (s *Server) handleBridgeProxy(w http.ResponseWriter, r *http.Request) {
	if s.managementProxy == nil {
		http.Error(w, "management proxy is not configured", http.StatusServiceUnavailable)
		return
	}
	s.managementProxy.ServeHTTP(w, r)
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	rt := s.defaultRuntime()
	if rt == nil {
		http.Error(w, "default app is not configured", http.StatusServiceUnavailable)
		return
	}
	project := r.PathValue("project")
	if err := store.ValidateSegment("project", project); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	list, err := rt.store.ListSessions(project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(list)
}

func (s *Server) handleGetMessages(w http.ResponseWriter, r *http.Request) {
	rt := s.defaultRuntime()
	if rt == nil {
		http.Error(w, "default app is not configured", http.StatusServiceUnavailable)
		return
	}
	project := r.PathValue("project")
	session := r.PathValue("session")
	if err := store.ValidateSegment("project", project); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := store.ValidateSegment("session", session); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msgs, err := rt.store.ReadMessages(project, session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	msgs = rewriteMessageAttachments(msgs, rt.attachmentURLLegacy)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(msgs)
}

type postMessageRequest struct {
	Content string `json:"content"`
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	rt := s.defaultRuntime()
	if rt == nil {
		http.Error(w, "default app is not configured", http.StatusServiceUnavailable)
		return
	}
	project := r.PathValue("project")
	session := r.PathValue("session")
	if err := store.ValidateSegment("project", project); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := store.ValidateSegment("session", session); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var req postMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}

	msg := store.Message{
		Role:    store.RoleUser,
		Content: req.Content,
	}
	stored, err := rt.store.AppendMessage(project, session, msg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rt.events.Publish(project, session, stored)

	// Dispatch to engine asynchronously via the per-project MessageHandler.
	s.mu.RLock()
	h := s.projectHandlers[project]
	p := s.projectPlatforms[project]
	s.mu.RUnlock()
	if h != nil {
		if p == nil {
			p = newPlatform(s, project)
		}
		coreMsg := &core.Message{
			SessionKey: sessionKey(project, session),
			Platform:   "webclient",
			UserID:     "web",
			UserName:   "Web",
			Content:    req.Content,
			ReplyCtx:   replyContext{Project: project, Session: session},
		}
		go h(p, coreMsg)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":        stored.ID,
		"timestamp": stored.Timestamp.Format(time.RFC3339Nano),
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	rt := s.defaultRuntime()
	if rt == nil {
		http.Error(w, "default app is not configured", http.StatusServiceUnavailable)
		return
	}
	project := r.PathValue("project")
	session := r.PathValue("session")
	if err := store.ValidateSegment("project", project); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := store.ValidateSegment("session", session); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	msgCh, cancelMsg := rt.events.Subscribe(project, session)
	defer cancelMsg()
	runCh, cancelRun := rt.runEvts.Subscribe(project, session)
	defer cancelRun()

	// Initial ping to establish the stream.
	_, _ = w.Write([]byte(": ok\n\n"))
	flusher.Flush()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case msg, ok := <-msgCh:
			if !ok {
				return
			}
			if len(msg.Attachments) > 0 {
				msg.Attachments = rewriteAttachments(msg.Attachments, rt.attachmentURLLegacy)
			}
			_, _ = w.Write([]byte("event: message\n"))
			_, _ = w.Write([]byte("data: "))
			if err := enc.Encode(msg); err != nil {
				return
			}
			_, _ = w.Write([]byte("\n"))
			flusher.Flush()
		case ev, ok := <-runCh:
			if !ok {
				return
			}
			_, _ = w.Write([]byte("event: run_event\n"))
			_, _ = w.Write([]byte("data: "))
			if err := enc.Encode(ev); err != nil {
				return
			}
			_, _ = w.Write([]byte("\n"))
			flusher.Flush()
		}
	}
}

func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	rt := s.defaultRuntime()
	if rt == nil {
		http.Error(w, "default app is not configured", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	meta, path, err := rt.store.OpenAttachment(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if meta.MimeType != "" {
		w.Header().Set("Content-Type", meta.MimeType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if meta.FileName != "" {
		// Prefer inline for images.
		disp := "attachment"
		if strings.HasPrefix(strings.ToLower(meta.MimeType), "image/") {
			disp = "inline"
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disp, meta.FileName))
	}
	http.ServeFile(w, r, path)
}

func (s *Server) requireAppRuntime(w http.ResponseWriter, r *http.Request) *appRuntime {
	appID := strings.TrimSpace(r.PathValue("app_id"))
	rt, ok := s.appRuntimeByID(appID)
	if !ok || rt == nil {
		http.Error(w, "unknown app", http.StatusNotFound)
		return nil
	}
	return rt
}

func (s *Server) handleAppListSessions(w http.ResponseWriter, r *http.Request) {
	rt := s.requireAppRuntime(w, r)
	if rt == nil {
		return
	}
	project := r.PathValue("project")
	if err := store.ValidateSegment("project", project); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	list, err := rt.store.ListSessions(project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(list)
}

func (s *Server) handleAppGetMessages(w http.ResponseWriter, r *http.Request) {
	rt := s.requireAppRuntime(w, r)
	if rt == nil {
		return
	}
	project := r.PathValue("project")
	session := r.PathValue("session")
	if err := store.ValidateSegment("project", project); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := store.ValidateSegment("session", session); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msgs, err := rt.store.ReadMessages(project, session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	msgs = rewriteMessageAttachments(msgs, rt.attachmentURLApp)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(msgs)
}

func (s *Server) handleAppPostMessage(w http.ResponseWriter, r *http.Request) {
	rt := s.requireAppRuntime(w, r)
	if rt == nil {
		return
	}
	project := r.PathValue("project")
	session := r.PathValue("session")
	if err := store.ValidateSegment("project", project); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := store.ValidateSegment("session", session); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var req postMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	h := s.projectHandlers[project]
	s.mu.RUnlock()
	if h == nil && rt.adapter == nil {
		http.Error(w, "no delivery path configured for app", http.StatusServiceUnavailable)
		return
	}

	msg := store.Message{Role: store.RoleUser, Content: req.Content}
	stored, err := rt.store.AppendMessage(project, session, msg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rt.events.Publish(project, session, stored)

	// Dispatch to engine asynchronously via the per-project MessageHandler
	// (internal mode). If no handler is registered, fall back to best-effort
	// upstream delivery via the external adapter when configured.
	if h != nil {
		p := newAppPlatform(s, rt, project)
		coreMsg := &core.Message{
			SessionKey: sessionKey(project, session),
			Platform:   firstNonEmpty(strings.TrimSpace(rt.platform), "webclient"),
			UserID:     "web",
			UserName:   "Web",
			Content:    req.Content,
			ReplyCtx:   replyContext{Project: project, Session: session},
		}
		go h(p, coreMsg)
	} else {
		// Persist an outbox record so reconnects can retry delivery. Any failure
		// here is reported to the caller instead of returning 202 for a message
		// that has no delivery path.
		det, err := rt.store.GetClientSession(project, session, 0)
		if err != nil {
			http.Error(w, fmt.Sprintf("resolve session for delivery: %v", err), http.StatusInternalServerError)
			return
		}
		sk := strings.TrimSpace(det.SessionKey)
		if sk == "" {
			http.Error(w, "session_key is required for delivery", http.StatusBadRequest)
			return
		}
		payloadBytes, err := json.Marshal(outboxPayloadV1Send{
			Kind:       outboxPayloadKindV1Send,
			SessionKey: sk,
			SessionID:  session,
			Message:    req.Content,
		})
		if err != nil {
			http.Error(w, fmt.Sprintf("encode outbox payload: %v", err), http.StatusInternalServerError)
			return
		}
		ob, err := rt.store.CreateOutboxItem(store.CreateOutboxItemInput{
			ID:          stored.ID,
			Project:     project,
			SessionID:   session,
			SessionKey:  sk,
			Payload:     payloadBytes,
			NextRetryAt: time.Now().UTC(),
		})
		if err != nil {
			http.Error(w, fmt.Sprintf("create outbox item: %v", err), http.StatusInternalServerError)
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			if err := rt.deliverOutboxItem(ctx, ob); err != nil {
				_, _ = rt.store.MarkOutboxFailed(project, ob.ID, err.Error(), time.Now().UTC())
				return
			}
			_, _ = rt.store.MarkOutboxSent(project, ob.ID)
		}()
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":        stored.ID,
		"timestamp": stored.Timestamp.Format(time.RFC3339Nano),
	})
}

func (s *Server) handleAppEvents(w http.ResponseWriter, r *http.Request) {
	rt := s.requireAppRuntime(w, r)
	if rt == nil {
		return
	}
	project := r.PathValue("project")
	session := r.PathValue("session")
	if err := store.ValidateSegment("project", project); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := store.ValidateSegment("session", session); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	msgCh, cancelMsg := rt.events.Subscribe(project, session)
	defer cancelMsg()
	runCh, cancelRun := rt.runEvts.Subscribe(project, session)
	defer cancelRun()

	_, _ = w.Write([]byte(": ok\n\n"))
	flusher.Flush()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case msg, ok := <-msgCh:
			if !ok {
				return
			}
			if len(msg.Attachments) > 0 {
				msg.Attachments = rewriteAttachments(msg.Attachments, rt.attachmentURLApp)
			}
			_, _ = w.Write([]byte("event: message\n"))
			_, _ = w.Write([]byte("data: "))
			if err := enc.Encode(msg); err != nil {
				return
			}
			_, _ = w.Write([]byte("\n"))
			flusher.Flush()
		case ev, ok := <-runCh:
			if !ok {
				return
			}
			_, _ = w.Write([]byte("event: run_event\n"))
			_, _ = w.Write([]byte("data: "))
			if err := enc.Encode(ev); err != nil {
				return
			}
			_, _ = w.Write([]byte("\n"))
			flusher.Flush()
		}
	}
}

func (s *Server) handleAppGetAttachment(w http.ResponseWriter, r *http.Request) {
	rt := s.requireAppRuntime(w, r)
	if rt == nil {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	meta, path, err := rt.store.OpenAttachment(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if meta.MimeType != "" {
		w.Header().Set("Content-Type", meta.MimeType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if meta.FileName != "" {
		disp := "attachment"
		if strings.HasPrefix(strings.ToLower(meta.MimeType), "image/") {
			disp = "inline"
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disp, meta.FileName))
	}
	http.ServeFile(w, r, path)
}

// SetStaticFS configures a static filesystem to be served at "/" (UI).
// root is the subdirectory within fsys that should be treated as the web root.
// If root is empty, fsys is treated as already rooted.
func (s *Server) SetStaticFS(fsys fs.FS, root string) error {
	h, err := staticHandlerFromFS(fsys, root)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.static = h
	s.mu.Unlock()
	return nil
}

func (s *Server) newManagementProxy(target *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host
		if strings.HasPrefix(req.URL.Path, "/api/v1/") && strings.TrimSpace(s.opts.ManagementToken) != "" {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.opts.ManagementToken))
			q := req.URL.Query()
			q.Del("token")
			req.URL.RawQuery = q.Encode()
		} else if strings.HasPrefix(req.URL.Path, "/api/v1/") {
			req.Header.Del("Authorization")
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Warn("webclient: management proxy failed", "path", r.URL.Path, "error", err)
		http.Error(w, "management proxy failed", http.StatusBadGateway)
	}
	return proxy
}

func (s *Server) defaultStaticHandler() http.Handler {
	fsys, root := embeddedStaticFS()
	if fsys == nil {
		return nil
	}
	h, err := staticHandlerFromFS(fsys, root)
	if err != nil {
		return nil
	}
	return h
}

func staticHandlerFromFS(fsys fs.FS, root string) (http.Handler, error) {
	if fsys == nil {
		return nil, nil
	}
	if strings.TrimSpace(root) != "" {
		sub, err := fs.Sub(fsys, root)
		if err != nil {
			return nil, fmt.Errorf("webclient: static fs.Sub(%q): %w", root, err)
		}
		fsys = sub
	}
	if _, err := fs.Stat(fsys, "index.html"); err != nil {
		return nil, fmt.Errorf("webclient: static index.html: %w", err)
	}
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		urlPath := strings.TrimPrefix(r.URL.Path, "/")
		if urlPath == "" {
			urlPath = "index.html"
		}
		if f, err := fsys.Open(urlPath); err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		indexData, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method != http.MethodHead {
			_, _ = w.Write(indexData)
		}
	}), nil
}

func (s *Server) attachmentURL(id string) string {
	rt := s.defaultRuntime()
	if rt == nil {
		return ""
	}
	return rt.attachmentURLLegacy(id)
}

func (s *Server) bestEffortFindProjectForSession(sessionKey, sessionID string) (string, bool) {
	rt := s.defaultRuntime()
	if rt == nil || rt.store == nil {
		return "", false
	}
	project, ok, err := rt.store.FindProjectForClientSession(sessionKey, sessionID)
	if err != nil || !ok {
		return "", false
	}
	return strings.TrimSpace(project), true
}

func (s *Server) bestEffortLastUserRun(project, sessionID string) (runRef, bool) {
	rt := s.defaultRuntime()
	if rt == nil || rt.store == nil {
		return runRef{}, false
	}
	msgs, err := rt.store.ReadMessages(project, sessionID)
	if err != nil || len(msgs) == 0 {
		return runRef{}, false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if strings.TrimSpace(m.Role) != store.RoleUser {
			continue
		}
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		runID := strings.TrimSpace(m.RunID)
		if runID == "" {
			runID = id
		}
		userID := strings.TrimSpace(m.UserMessageID)
		if userID == "" {
			userID = id
		}
		return runRef{RunID: runID, UserMessageID: userID, UpdatedAt: m.Timestamp}, true
	}
	return runRef{}, false
}

func (s *Server) initApps() error {
	// Build legacy single-app runtime when Apps is empty.
	if len(s.opts.Apps) == 0 {
		root := filepath.Join(s.opts.DataDir, "webclient")
		st, err := store.New(root, s.opts.PublicURL)
		if err != nil {
			return err
		}
		platform := strings.TrimSpace(s.opts.Platform)
		if platform == "" {
			platform = "webclient"
		}
		dataNamespace := strings.TrimSpace(s.opts.DataNamespace)
		if dataNamespace == "" {
			dataNamespace = "__legacy__"
		}
		rt := &appRuntime{
			appID:         "webclient",
			platform:      platform,
			dataNamespace: dataNamespace,
			root:          root,
			token:         strings.TrimSpace(s.opts.Token),
			publicURL:     strings.TrimRight(strings.TrimSpace(s.opts.PublicURL), "/"),
			store:         st,
			events:        broker.NewHub(),
			runEvts:       broker.NewRunHub(),
		}
		if s.opts.ManagementBaseURL != "" {
			rt.adapter = newAdapterClient(s, rt)
		}
		s.apps[rt.appID] = rt
		s.defaultAppID = rt.appID
		s.defaultApp = rt
		s.root = root
		s.store = rt.store
		s.events = rt.events
		s.runEvts = rt.runEvts
		s.adapter = rt.adapter
		return nil
	}

	// Multi-app mode.
	reNS := regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	seenID := make(map[string]bool)
	seenPlatform := make(map[string]bool)
	seenNS := make(map[string]bool)

	var enabled []*AppOptions
	for i := range s.opts.Apps {
		a := &s.opts.Apps[i]
		enabledFlag := true
		if a.Enabled != nil {
			enabledFlag = *a.Enabled
		}
		a.ID = strings.TrimSpace(a.ID)
		a.Platform = strings.TrimSpace(a.Platform)
		a.DataNamespace = strings.TrimSpace(a.DataNamespace)

		// Disabled apps are ignored for validation/dup checks so deployments can
		// keep placeholders in config without blocking startup.
		if !enabledFlag {
			if a.Default {
				if a.ID == "" {
					return fmt.Errorf("webclient: disabled app cannot be default")
				}
				return fmt.Errorf("webclient: app %q cannot be default when disabled", a.ID)
			}
			continue
		}

		// Enabled apps must be well-formed and unique.
		if a.ID == "" || a.Platform == "" || a.DataNamespace == "" {
			return fmt.Errorf("webclient: app requires id/platform/data_namespace")
		}
		if !reNS.MatchString(a.ID) {
			return fmt.Errorf("webclient: invalid app id %q", a.ID)
		}
		if !reNS.MatchString(a.Platform) {
			return fmt.Errorf("webclient: invalid app platform %q", a.Platform)
		}
		if !reNS.MatchString(a.DataNamespace) {
			return fmt.Errorf("webclient: invalid data_namespace %q", a.DataNamespace)
		}
		if seenID[a.ID] {
			return fmt.Errorf("webclient: duplicate app id %q", a.ID)
		}
		seenID[a.ID] = true
		if seenPlatform[a.Platform] {
			return fmt.Errorf("webclient: duplicate app platform %q", a.Platform)
		}
		seenPlatform[a.Platform] = true
		if seenNS[a.DataNamespace] {
			return fmt.Errorf("webclient: duplicate app data_namespace %q", a.DataNamespace)
		}
		seenNS[a.DataNamespace] = true
		enabled = append(enabled, a)
	}
	if len(enabled) == 0 {
		return fmt.Errorf("webclient: no enabled apps configured")
	}

	// Choose default app.
	defaultID := strings.TrimSpace(s.opts.DefaultApp)
	if defaultID == "" {
		for _, a := range enabled {
			if a.Default {
				defaultID = a.ID
				break
			}
		}
	}
	if defaultID == "" {
		defaultID = enabled[0].ID
	}

	base := filepath.Join(s.opts.DataDir, "webclient")
	for _, a := range enabled {
		root := filepath.Join(base, "apps", a.DataNamespace)
		st, err := store.New(root, s.opts.PublicURL)
		if err != nil {
			return err
		}
		rt := &appRuntime{
			appID:         a.ID,
			platform:      a.Platform,
			dataNamespace: a.DataNamespace,
			root:          root,
			token:         strings.TrimSpace(s.opts.Token),
			publicURL:     strings.TrimRight(strings.TrimSpace(s.opts.PublicURL), "/"),
			store:         st,
			events:        broker.NewHub(),
			runEvts:       broker.NewRunHub(),
		}
		if s.opts.ManagementBaseURL != "" {
			rt.adapter = newAdapterClient(s, rt)
		}
		s.apps[rt.appID] = rt
		if rt.appID == defaultID {
			s.defaultAppID = rt.appID
			s.defaultApp = rt
		}
	}
	if s.defaultApp == nil {
		return fmt.Errorf("webclient: default_app %q is not configured/enabled", defaultID)
	}

	// Legacy-compatible aliases point at the default app store/hubs.
	s.root = filepath.Join(s.opts.DataDir, "webclient")
	s.store = s.defaultApp.store
	s.events = s.defaultApp.events
	s.runEvts = s.defaultApp.runEvts
	s.adapter = s.defaultApp.adapter
	return nil
}

func (s *Server) defaultRuntime() *appRuntime {
	s.mu.RLock()
	rt := s.defaultApp
	s.mu.RUnlock()
	return rt
}

func (s *Server) appRuntimeByID(appID string) (*appRuntime, bool) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return nil, false
	}
	s.mu.RLock()
	rt, ok := s.apps[appID]
	s.mu.RUnlock()
	return rt, ok
}

type normalizedApp struct {
	ID            string
	Platform      string
	DataNamespace string
}

func normalizeEnabledApps(apps []AppOptions, defaultApp string) (map[string]*normalizedApp, string, error) {
	reNS := regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	seenID := make(map[string]bool)
	seenPlatform := make(map[string]bool)
	seenNS := make(map[string]bool)

	enabled := make(map[string]*normalizedApp)
	var enabledOrder []string
	var defaultFlagged string

	for i := range apps {
		a := apps[i]
		enabledFlag := true
		if a.Enabled != nil {
			enabledFlag = *a.Enabled
		}
		id := strings.TrimSpace(a.ID)
		platform := strings.TrimSpace(a.Platform)
		ns := strings.TrimSpace(a.DataNamespace)
		if !enabledFlag {
			if a.Default {
				if id == "" {
					return nil, "", fmt.Errorf("webclient: disabled app cannot be default")
				}
				return nil, "", fmt.Errorf("webclient: app %q cannot be default when disabled", id)
			}
			continue
		}
		if id == "" || platform == "" || ns == "" {
			return nil, "", fmt.Errorf("webclient: app requires id/platform/data_namespace")
		}
		if !reNS.MatchString(id) {
			return nil, "", fmt.Errorf("webclient: invalid app id %q", id)
		}
		if !reNS.MatchString(platform) {
			return nil, "", fmt.Errorf("webclient: invalid app platform %q", platform)
		}
		if !reNS.MatchString(ns) {
			return nil, "", fmt.Errorf("webclient: invalid data_namespace %q", ns)
		}
		if seenID[id] {
			return nil, "", fmt.Errorf("webclient: duplicate app id %q", id)
		}
		seenID[id] = true
		if seenPlatform[platform] {
			return nil, "", fmt.Errorf("webclient: duplicate app platform %q", platform)
		}
		seenPlatform[platform] = true
		if seenNS[ns] {
			return nil, "", fmt.Errorf("webclient: duplicate app data_namespace %q", ns)
		}
		seenNS[ns] = true

		enabled[id] = &normalizedApp{ID: id, Platform: platform, DataNamespace: ns}
		enabledOrder = append(enabledOrder, id)
		if a.Default {
			defaultFlagged = id
		}
	}
	if len(enabled) == 0 {
		return nil, "", fmt.Errorf("webclient: no enabled apps configured")
	}

	defaultID := strings.TrimSpace(defaultApp)
	if defaultID == "" {
		defaultID = strings.TrimSpace(defaultFlagged)
	}
	if defaultID == "" {
		defaultID = enabledOrder[0]
	}
	if enabled[defaultID] == nil {
		return nil, "", fmt.Errorf("webclient: default_app %q is not configured/enabled", defaultID)
	}
	return enabled, defaultID, nil
}

func (rt *appRuntime) attachmentURLLegacy(id string) string {
	return rt.attachmentURLWithPrefix("/attachments", id)
}

func (rt *appRuntime) attachmentURLApp(id string) string {
	return rt.attachmentURLWithPrefix("/apps/"+url.PathEscape(rt.appID)+"/attachments", id)
}

func (rt *appRuntime) attachmentURLWithPrefix(prefix, id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(prefix), "/") + "/" + id
	if strings.TrimSpace(rt.publicURL) != "" {
		base = strings.TrimRight(strings.TrimSpace(rt.publicURL), "/") + base
	}
	if strings.TrimSpace(rt.token) == "" {
		return base
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "token=" + url.QueryEscape(strings.TrimSpace(rt.token))
}

func (rt *appRuntime) recoverOutboxOnce(ctx context.Context, limit int) (int, error) {
	rt.outboxRecoverMu.Lock()
	defer rt.outboxRecoverMu.Unlock()

	items, err := rt.store.ListOutboxDue(time.Now().UTC(), limit)
	if err != nil {
		return 0, err
	}
	attempted := 0
	for _, item := range items {
		attempted++
		if err := rt.deliverOutboxItem(ctx, item); err != nil {
			// Minimal recovery: keep it immediately due so the next Start() can retry.
			_, _ = rt.store.MarkOutboxFailed(item.Project, item.ID, err.Error(), time.Now().UTC())
			continue
		}
		_, _ = rt.store.MarkOutboxSent(item.Project, item.ID)
	}
	return attempted, nil
}

func rewriteMessageAttachments(msgs []store.Message, urlFn func(id string) string) []store.Message {
	if len(msgs) == 0 || urlFn == nil {
		return msgs
	}
	out := make([]store.Message, 0, len(msgs))
	for _, m := range msgs {
		if len(m.Attachments) == 0 {
			out = append(out, m)
			continue
		}
		cp := m
		cp.Attachments = rewriteAttachments(cp.Attachments, urlFn)
		out = append(out, cp)
	}
	return out
}

func rewriteAttachments(atts []store.Attachment, urlFn func(id string) string) []store.Attachment {
	if len(atts) == 0 || urlFn == nil {
		return atts
	}
	out := make([]store.Attachment, 0, len(atts))
	for _, a := range atts {
		cp := a
		if strings.TrimSpace(cp.ID) != "" {
			cp.URL = urlFn(cp.ID)
		}
		out = append(out, cp)
	}
	return out
}
