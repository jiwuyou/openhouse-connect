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

	// ManagementBaseURL points at the management web/API server that this
	// webclient shell should call through its backend facade.
	ManagementBaseURL string
	ManagementToken   string
}

type Server struct {
	opts Options

	store   *store.Store
	events  *broker.Hub
	handler http.Handler
	root    string

	mu               sync.RWMutex
	started          bool
	httpServer       *http.Server
	listener         net.Listener
	projectHandlers  map[string]core.MessageHandler
	projectPlatforms map[string]*platform

	// static serves the webclient UI when configured or embedded.
	static http.Handler

	managementProxy *httputil.ReverseProxy
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

	root := filepath.Join(opts.DataDir, "webclient")
	st, err := store.New(root, opts.PublicURL)
	if err != nil {
		return nil, err
	}

	s := &Server{
		opts:            opts,
		store:           st,
		events:          broker.NewHub(),
		root:            root,
		projectHandlers: make(map[string]core.MessageHandler),
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

func (s *Server) Platform(project string) core.Platform {
	return newPlatform(s, project)
}

func (s *Server) Start() error {
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
	s.mu.Unlock()

	go func() {
		err := srv.Serve(ln)
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		slog.Error("webclient: http server exited unexpectedly", "error", err)
	}()

	// Best-effort recovery: attempt due outbox items once at startup.
	// This is intentionally minimal (no persistent goroutine); it provides
	// a real recovery path after restart.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n, err := s.recoverOutboxOnce(ctx, 50); err != nil {
		slog.Warn("webclient: outbox recovery failed", "error", err)
	} else if n > 0 {
		slog.Info("webclient: outbox recovery attempted", "count", n)
	}

	return nil
}

// recoverOutboxOnce scans due outbox items and attempts delivery.
// Tests may call this directly; Start also invokes it once.
func (s *Server) recoverOutboxOnce(ctx context.Context, limit int) (int, error) {
	items, err := s.store.ListOutboxDue(time.Now().UTC(), limit)
	if err != nil {
		return 0, err
	}
	attempted := 0
	for _, item := range items {
		attempted++
		if err := s.deliverOutboxItem(ctx, item); err != nil {
			// Minimal recovery: keep it immediately due so the next Start() can retry.
			_, _ = s.store.MarkOutboxFailed(item.Project, item.ID, err.Error(), time.Now().UTC())
			continue
		}
		_, _ = s.store.MarkOutboxSent(item.Project, item.ID)
	}
	return attempted, nil
}

func (s *Server) Stop(ctx context.Context) error {
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
	s.mu.Unlock()

	s.events.Close()
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

	// Client API facade for the copied management frontend served on 9840.
	mux.HandleFunc("GET /api/v1/projects/{project}/sessions", s.wrap(s.handleV1ListSessions))
	mux.HandleFunc("POST /api/v1/projects/{project}/sessions", s.wrap(s.handleV1CreateSession))
	mux.HandleFunc("GET /api/v1/projects/{project}/sessions/{id}", s.wrap(s.handleV1GetSession))
	mux.HandleFunc("PATCH /api/v1/projects/{project}/sessions/{id}", s.wrap(s.handleV1PatchSession))
	mux.HandleFunc("DELETE /api/v1/projects/{project}/sessions/{id}", s.wrap(s.handleV1DeleteSession))
	mux.HandleFunc("POST /api/v1/projects/{project}/sessions/switch", s.wrap(s.handleV1SwitchSession))
	mux.HandleFunc("POST /api/v1/projects/{project}/send", s.wrap(s.handleV1Send))

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
	// /bridge/ws is the primary chat path used by the copied admin frontend.
	// We intercept WebSocket upgrades so 9840 can persist chat history locally.
	// Non-WebSocket requests (e.g. health checks) still proxy upstream.
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
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
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
	project := r.PathValue("project")
	if err := store.ValidateSegment("project", project); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	list, err := s.store.ListSessions(project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(list)
}

func (s *Server) handleGetMessages(w http.ResponseWriter, r *http.Request) {
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
	msgs, err := s.store.ReadMessages(project, session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(msgs)
}

type postMessageRequest struct {
	Content string `json:"content"`
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
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
	stored, err := s.store.AppendMessage(project, session, msg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.events.Publish(project, session, stored)

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

	ch, cancel := s.events.Subscribe(project, session)
	defer cancel()

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
		case msg, ok := <-ch:
			if !ok {
				return
			}
			_, _ = w.Write([]byte("event: message\n"))
			_, _ = w.Write([]byte("data: "))
			if err := enc.Encode(msg); err != nil {
				return
			}
			_, _ = w.Write([]byte("\n"))
			flusher.Flush()
		}
	}
}

func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	meta, path, err := s.store.OpenAttachment(id)
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
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	base := "/attachments/" + id
	if s.opts.PublicURL != "" {
		base = s.opts.PublicURL + base
	}
	if strings.TrimSpace(s.opts.Token) == "" {
		return base
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "token=" + url.QueryEscape(s.opts.Token)
}
