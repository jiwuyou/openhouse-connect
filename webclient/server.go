package webclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
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
}

type Server struct {
	opts Options

	store   *store.Store
	events  *broker.Hub
	handler http.Handler

	mu              sync.RWMutex
	started         bool
	httpServer      *http.Server
	listener        net.Listener
	projectHandlers map[string]core.MessageHandler
	projectPlatforms map[string]*platform
}

func NewServer(opts Options) (*Server, error) {
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, fmt.Errorf("webclient: DataDir is required")
	}
	if opts.Port < 0 {
		return nil, fmt.Errorf("webclient: invalid port %d", opts.Port)
	}
	if strings.TrimSpace(opts.Host) == "" {
		opts.Host = "127.0.0.1"
	}
	if opts.Port == 0 {
		opts.Port = 9830
	}
	opts.PublicURL = strings.TrimRight(strings.TrimSpace(opts.PublicURL), "/")

	root := filepath.Join(opts.DataDir, "webclient")
	st, err := store.New(root, opts.PublicURL)
	if err != nil {
		return nil, err
	}

	s := &Server{
		opts:            opts,
		store:           st,
		events:          broker.NewHub(),
		projectHandlers: make(map[string]core.MessageHandler),
	}
	s.handler = s.newMux()
	return s, nil
}

func (s *Server) Platform(project string) core.Platform {
	return newPlatform(s, project)
}

func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}

	addr := net.JoinHostPort(s.opts.Host, strconv.Itoa(s.opts.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("webclient: listen %s: %w", addr, err)
	}

	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.listener = ln
	s.started = true

	go func() {
		err := s.httpServer.Serve(ln)
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		slog.Error("webclient: http server exited unexpectedly", "error", err)
	}()

	return nil
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

	// Root UI (minimal). If a local static directory exists, serve it.
	if dir := filepath.Join("webclient", "ui", "static"); dirExists(dir) {
		mux.Handle("GET /", http.FileServer(http.Dir(dir)))
		mux.Handle("GET /assets/", http.StripPrefix("/", http.FileServer(http.Dir(dir))))
	} else {
		mux.HandleFunc("GET /", s.wrap(s.handleRoot))
	}

	// CORS preflight
	mux.HandleFunc("OPTIONS /{path...}", s.wrap(s.handleOptions))

	return mux
}

func dirExists(dir string) bool {
	st, err := os.Stat(dir)
	return err == nil && st.IsDir()
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
