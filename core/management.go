package core

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProjectSettingsUpdate is passed to SetSaveProjectSettings to persist management API PATCH fields.
// The implementation (typically in cmd/cc-connect) maps this to config.ProjectSettingsUpdate.
type ProjectSettingsUpdate struct {
	DisplayName          *string
	Language             *string
	AdminFrom            *string
	DisabledCommands     []string
	WorkDir              *string
	Mode                 *string
	ShowContextIndicator *bool
	ReplyFooter          *bool
	PlatformAllowFrom    map[string]string
}

func syncProjectAliasesForEngine(e *Engine) {
	if e == nil || e.sessions == nil {
		return
	}
	idToKey, _ := e.sessions.SessionKeyMap()
	seen := make(map[string]struct{})
	for _, sessionKey := range idToKey {
		if sessionKey == "" {
			continue
		}
		if _, ok := seen[sessionKey]; ok {
			continue
		}
		seen[sessionKey] = struct{}{}
		e.syncSessionAliases(e.sessions, sessionKey)
	}
}

// ManagementServer provides an HTTP REST API for external management tools
// (web dashboards, TUI clients, GUI desktop apps, Mac tray apps, etc.).
type ManagementServer struct {
	port        int
	host        string
	token       string
	corsOrigins []string
	server      *http.Server
	startedAt   time.Time

	mu      sync.RWMutex
	engines map[string]*Engine // project name → engine

	cronScheduler      *CronScheduler
	heartbeatScheduler *HeartbeatScheduler
	bridgeServer       *BridgeServer
	frontendRegistry   *FrontendAppRegistry

	setupFeishuSave           func(req FeishuSetupSaveRequest) error
	setupWeixinSave           func(req WeixinSetupSaveRequest) error
	addPlatformToProject      func(projectName, platType string, opts map[string]any, workDir, agentType string) error
	removePlatformFromProject func(projectName, selector string) error
	createProject             func(projectName, displayName, workDir, agentType string) (string, bool, error)
	removeProject             func(projectName string) error
	saveProjectSettings       func(projectName string, update ProjectSettingsUpdate) error
	getProjectConfig          func(projectName string) map[string]any
	saveProviderRefs          func(projectName string, refs []string) error
	configFilePath            string
	getGlobalSettings         func() map[string]any
	saveGlobalSettings        func(map[string]any) error

	// Global provider callbacks (set by cmd/cc-connect)
	listGlobalProviders  func() ([]GlobalProviderInfo, error)
	addGlobalProvider    func(GlobalProviderInfo) error
	updateGlobalProvider func(name string, info GlobalProviderInfo) error
	removeGlobalProvider func(name string) error
	fetchPresets         func() (*ProviderPresetsResponse, error)
}

// NewManagementServer creates a new management API server.
func NewManagementServer(port int, token string, corsOrigins []string) *ManagementServer {
	return &ManagementServer{
		port:        port,
		token:       token,
		corsOrigins: corsOrigins,
		engines:     make(map[string]*Engine),
		startedAt:   time.Now(),
	}
}

func (m *ManagementServer) RegisterEngine(name string, e *Engine) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.engines[name] = e
}

func (m *ManagementServer) SetCronScheduler(cs *CronScheduler)           { m.cronScheduler = cs }
func (m *ManagementServer) SetHeartbeatScheduler(hs *HeartbeatScheduler) { m.heartbeatScheduler = hs }
func (m *ManagementServer) SetBridgeServer(bs *BridgeServer)             { m.bridgeServer = bs }
func (m *ManagementServer) SetHost(host string)                          { m.host = strings.TrimSpace(host) }
func (m *ManagementServer) SetFrontendAppRegistry(registry *FrontendAppRegistry) {
	m.frontendRegistry = registry
}
func (m *ManagementServer) SetSetupFeishuSave(fn func(FeishuSetupSaveRequest) error) {
	m.setupFeishuSave = fn
}
func (m *ManagementServer) SetSetupWeixinSave(fn func(WeixinSetupSaveRequest) error) {
	m.setupWeixinSave = fn
}

func (m *ManagementServer) SetAddPlatformToProject(fn func(string, string, map[string]any, string, string) error) {
	m.addPlatformToProject = fn
}

func (m *ManagementServer) SetRemovePlatformFromProject(fn func(string, string) error) {
	m.removePlatformFromProject = fn
}

func (m *ManagementServer) SetCreateProject(fn func(string, string, string, string) (string, bool, error)) {
	m.createProject = fn
}

func (m *ManagementServer) SetRemoveProject(fn func(string) error) {
	m.removeProject = fn
}

func (m *ManagementServer) SetConfigFilePath(path string) {
	m.configFilePath = path
}

func (m *ManagementServer) SetSaveProjectSettings(fn func(string, ProjectSettingsUpdate) error) {
	m.saveProjectSettings = fn
}

func (m *ManagementServer) SetGetProjectConfig(fn func(string) map[string]any) {
	m.getProjectConfig = fn
}

func (m *ManagementServer) SetSaveProviderRefs(fn func(string, []string) error) {
	m.saveProviderRefs = fn
}

func (m *ManagementServer) SetGetGlobalSettings(fn func() map[string]any) {
	m.getGlobalSettings = fn
}

func (m *ManagementServer) SetSaveGlobalSettings(fn func(map[string]any) error) {
	m.saveGlobalSettings = fn
}

// GlobalProviderInfo is the wire type for global provider CRUD in the management API.
type GlobalProviderInfo struct {
	Name       string            `json:"name"`
	APIKey     string            `json:"api_key,omitempty"`
	BaseURL    string            `json:"base_url,omitempty"`
	Model      string            `json:"model,omitempty"`
	Thinking   string            `json:"thinking,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	AgentTypes []string          `json:"agent_types,omitempty"`
	Models     []struct {
		Model string `json:"model"`
		Alias string `json:"alias,omitempty"`
	} `json:"models,omitempty"`
}

func (m *ManagementServer) SetListGlobalProviders(fn func() ([]GlobalProviderInfo, error)) {
	m.listGlobalProviders = fn
}
func (m *ManagementServer) SetAddGlobalProvider(fn func(GlobalProviderInfo) error) {
	m.addGlobalProvider = fn
}
func (m *ManagementServer) SetUpdateGlobalProvider(fn func(string, GlobalProviderInfo) error) {
	m.updateGlobalProvider = fn
}
func (m *ManagementServer) SetRemoveGlobalProvider(fn func(string) error) {
	m.removeGlobalProvider = fn
}
func (m *ManagementServer) SetFetchPresets(fn func() (*ProviderPresetsResponse, error)) {
	m.fetchPresets = fn
}

func (m *ManagementServer) Start() {
	mux := http.NewServeMux()
	handler := m.buildHandler(mux)

	addr := tcpListenAddr(m.host, m.port)
	m.server = &http.Server{
		Addr:    addr,
		Handler: handler,
	}
	go func() {
		if err := m.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("management api server error", "error", err)
		}
	}()
	slog.Info("management api started", "addr", addr)
}

func (m *ManagementServer) buildHandler(mux *http.ServeMux) http.Handler {
	prefix := "/api/v1"

	// System
	mux.HandleFunc(prefix+"/status", m.wrap(m.handleStatus))
	mux.HandleFunc(prefix+"/restart", m.wrap(m.handleRestart))
	mux.HandleFunc(prefix+"/reload", m.wrap(m.handleReload))
	mux.HandleFunc(prefix+"/config", m.wrap(m.handleConfig))
	mux.HandleFunc(prefix+"/settings", m.wrap(m.handleGlobalSettings))
	mux.HandleFunc(prefix+"/filesystem/directories", m.wrap(m.handleFilesystemDirectories))

	// Projects
	mux.HandleFunc(prefix+"/projects", m.wrap(m.handleProjects))
	mux.HandleFunc(prefix+"/projects/", m.wrap(m.handleProjectRoutes))

	// Cron (global)
	mux.HandleFunc(prefix+"/cron", m.wrap(m.handleCron))
	mux.HandleFunc(prefix+"/cron/", m.wrap(m.handleCronByID))

	// Setup (QR onboarding for feishu/weixin)
	mux.HandleFunc(prefix+"/setup/feishu/begin", m.wrap(m.handleSetupFeishuBegin))
	mux.HandleFunc(prefix+"/setup/feishu/poll", m.wrap(m.handleSetupFeishuPoll))
	mux.HandleFunc(prefix+"/setup/feishu/save", m.wrap(m.handleSetupFeishuSave))
	mux.HandleFunc(prefix+"/setup/weixin/begin", m.wrap(m.handleSetupWeixinBegin))
	mux.HandleFunc(prefix+"/setup/weixin/poll", m.wrap(m.handleSetupWeixinPoll))
	mux.HandleFunc(prefix+"/setup/weixin/save", m.wrap(m.handleSetupWeixinSave))

	// Global Providers
	mux.HandleFunc(prefix+"/providers", m.wrap(m.handleGlobalProviders))
	mux.HandleFunc(prefix+"/providers/", m.wrap(m.handleGlobalProviderRoutes))

	// Bridge
	mux.HandleFunc(prefix+"/bridge/adapters", m.wrap(m.handleBridgeAdapters))

	// Frontend app registry
	mux.HandleFunc(prefix+"/apps", m.wrap(m.handleFrontendApps))
	mux.HandleFunc(prefix+"/apps/", m.wrap(m.handleFrontendAppRoutes))

	// Static file serving for cc-connect-web (SPA)
	return m.withStaticFallback(mux)
}

func tcpListenAddr(host string, port int) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Sprintf(":%d", port)
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func (m *ManagementServer) Stop() {
	if m.server != nil {
		m.server.Close()
	}
}

// withStaticFallback wraps the API mux with a file server for the web UI.
// API requests (/api/) go to the mux; everything else tries embedded static
// files, falling back to index.html for SPA routing.
func (m *ManagementServer) withStaticFallback(apiMux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			apiMux.ServeHTTP(w, r)
			return
		}
		if m.bridgeServer != nil && r.URL.Path == m.bridgeServer.path {
			m.bridgeServer.handleWS(w, r)
			return
		}
		assets := GetWebAssets()
		if assets == nil {
			apiMux.ServeHTTP(w, r)
			return
		}
		m.setCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Try to serve the exact file from the embedded FS.
		urlPath := strings.TrimPrefix(r.URL.Path, "/")
		if urlPath == "" {
			urlPath = "index.html"
		}
		if f, err := assets.Open(urlPath); err == nil {
			f.Close()
			http.FileServer(http.FS(assets)).ServeHTTP(w, r)
			return
		}
		// SPA fallback: serve index.html for any non-file route.
		indexData, err := fs.ReadFile(assets, "index.html")
		if err != nil {
			apiMux.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexData)
	})
}

// ── Auth & Middleware ──────────────────────────────────────────

func (m *ManagementServer) wrap(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.setCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !m.authenticate(r) {
			mgmtError(w, http.StatusUnauthorized, "unauthorized: missing or invalid token")
			return
		}
		handler(w, r)
	}
}

func (m *ManagementServer) authenticate(r *http.Request) bool {
	if m.token == "" {
		return true
	}
	// Bearer token
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, "Bearer ")), []byte(m.token)) == 1
	}
	// Query param
	if t := r.URL.Query().Get("token"); t != "" {
		return subtle.ConstantTimeCompare([]byte(t), []byte(m.token)) == 1
	}
	return false
}

func (m *ManagementServer) setCORS(w http.ResponseWriter, r *http.Request) {
	if len(m.corsOrigins) == 0 {
		return
	}
	origin := r.Header.Get("Origin")
	for _, o := range m.corsOrigins {
		if o == "*" || o == origin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			break
		}
	}
}

// ── Response helpers ──────────────────────────────────────────

func mgmtJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data}); err != nil {
		slog.Error("management api: write JSON failed", "error", err)
	}
}

func splitSessionKey(key string) []string {
	return strings.SplitN(key, ":", 3)
}

func mgmtError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg}); err != nil {
		slog.Error("management api: write error JSON failed", "error", err)
	}
}

func mgmtOK(w http.ResponseWriter, msg string) {
	mgmtJSON(w, http.StatusOK, map[string]string{"message": msg})
}

// ── System endpoints ──────────────────────────────────────────

func (m *ManagementServer) handleFilesystemDirectories(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		path, err := resolveFilesystemDirectoryPath(r.URL.Query().Get("path"))
		if err != nil {
			mgmtError(w, http.StatusBadRequest, err.Error())
			return
		}
		listing, err := listFilesystemDirectories(path)
		if err != nil {
			status := http.StatusBadRequest
			if os.IsPermission(err) {
				status = http.StatusForbidden
			}
			mgmtError(w, status, err.Error())
			return
		}
		mgmtJSON(w, http.StatusOK, listing)

	case http.MethodPost:
		var body struct {
			Parent string `json:"parent"`
			Path   string `json:"path"`
			Name   string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		parent := strings.TrimSpace(body.Parent)
		if parent == "" {
			parent = strings.TrimSpace(body.Path)
		}
		parentPath, err := resolveFilesystemDirectoryPath(parent)
		if err != nil {
			mgmtError(w, http.StatusBadRequest, err.Error())
			return
		}
		name := strings.TrimSpace(body.Name)
		if err := validateNewDirectoryName(name); err != nil {
			mgmtError(w, http.StatusBadRequest, err.Error())
			return
		}
		target := filepath.Join(parentPath, name)
		if err := os.Mkdir(target, 0o755); err != nil {
			mgmtError(w, http.StatusBadRequest, "create directory: "+err.Error())
			return
		}
		listing, err := listFilesystemDirectories(parentPath)
		if err != nil {
			mgmtError(w, http.StatusInternalServerError, err.Error())
			return
		}
		mgmtJSON(w, http.StatusCreated, map[string]any{
			"created": map[string]any{
				"name":   name,
				"path":   target,
				"hidden": strings.HasPrefix(name, "."),
			},
			"listing": listing,
		})

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

func resolveFilesystemDirectoryPath(raw string) (string, error) {
	home, _ := os.UserHomeDir()
	path := strings.TrimSpace(raw)
	if path == "" {
		path = home
	}
	if strings.HasPrefix(path, "~") {
		if home == "" {
			return "", fmt.Errorf("home directory is not available")
		}
		if path == "~" {
			path = home
		} else if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
			path = filepath.Join(home, strings.TrimLeft(path[1:], `/\`))
		}
	}
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		path = wd
	}
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve path: %w", err)
		}
		path = abs
	}
	return filepath.Clean(path), nil
}

func listFilesystemDirectories(path string) (map[string]any, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path is not a directory")
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read directory: %w", err)
	}
	type directoryEntry struct {
		Name   string `json:"name"`
		Path   string `json:"path"`
		Hidden bool   `json:"hidden"`
	}
	dirs := make([]directoryEntry, 0)
	for _, entry := range entries {
		entryPath := filepath.Join(path, entry.Name())
		isDir := entry.IsDir()
		if !isDir && entry.Type()&os.ModeSymlink != 0 {
			if targetInfo, err := os.Stat(entryPath); err == nil && targetInfo.IsDir() {
				isDir = true
			}
		}
		if !isDir {
			continue
		}
		dirs = append(dirs, directoryEntry{
			Name:   entry.Name(),
			Path:   entryPath,
			Hidden: strings.HasPrefix(entry.Name(), "."),
		})
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})

	parent := filepath.Dir(path)
	if parent == path {
		parent = ""
	}
	home, _ := os.UserHomeDir()
	return map[string]any{
		"path":      path,
		"parent":    parent,
		"home":      home,
		"separator": string(os.PathSeparator),
		"entries":   dirs,
	}, nil
}

func validateNewDirectoryName(name string) error {
	if name == "" {
		return fmt.Errorf("directory name is required")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid directory name")
	}
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("directory name must not contain path separators")
	}
	if filepath.Clean(name) != name {
		return fmt.Errorf("invalid directory name")
	}
	return nil
}

func (m *ManagementServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		mgmtError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	platformSet := make(map[string]bool)
	for _, e := range m.engines {
		for _, p := range e.platforms {
			platformSet[p.Name()] = true
		}
	}
	platforms := make([]string, 0, len(platformSet))
	for p := range platformSet {
		platforms = append(platforms, p)
	}

	var adapters []map[string]any
	if m.bridgeServer != nil {
		adapters = m.listBridgeAdapters()
	}

	resp := map[string]any{
		"version":             CurrentVersion,
		"uptime_seconds":      int(time.Since(m.startedAt).Seconds()),
		"connected_platforms": platforms,
		"projects_count":      len(m.engines),
		"bridge_adapters":     adapters,
	}
	if m.frontendRegistry != nil {
		frontendServices, err := m.frontendRegistry.ListServiceSummaries()
		if err != nil {
			slog.Warn("management api: list frontend services failed", "error", err)
		} else {
			resp["frontend_services"] = frontendServices
		}
	}
	if m.bridgeServer != nil {
		resp["bridge"] = map[string]any{
			"enabled":   true,
			"port":      m.bridgeServer.port,
			"path":      m.bridgeServer.path,
			"token":     m.bridgeServer.token,
			"token_set": m.bridgeServer.token != "",
		}
	}
	mgmtJSON(w, http.StatusOK, resp)
}

func (m *ManagementServer) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		mgmtError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		SessionKey string `json:"session_key"`
		Platform   string `json:"platform"`
	}
	// Body is optional; ignore decode errors from empty body
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	select {
	case RestartCh <- RestartRequest{SessionKey: body.SessionKey, Platform: body.Platform}:
		mgmtOK(w, "restart initiated")
	default:
		mgmtError(w, http.StatusConflict, "restart already in progress")
	}
}

func (m *ManagementServer) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		mgmtError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	var updated []string
	for name, e := range m.engines {
		if e.configReloadFunc != nil {
			if _, err := e.configReloadFunc(); err != nil {
				mgmtError(w, http.StatusInternalServerError, fmt.Sprintf("reload %s: %v", name, err))
				return
			}
			updated = append(updated, name)
		}
	}

	mgmtJSON(w, http.StatusOK, map[string]any{
		"message":          "config reloaded",
		"projects_updated": updated,
	})
}

func (m *ManagementServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		mgmtError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}

	if m.configFilePath == "" {
		mgmtError(w, http.StatusNotFound, "config file path not set")
		return
	}
	data, err := os.ReadFile(m.configFilePath)
	if err != nil {
		mgmtError(w, http.StatusInternalServerError, "read config: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (m *ManagementServer) handleGlobalSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if m.getGlobalSettings == nil {
			mgmtError(w, http.StatusServiceUnavailable, "global settings not available")
			return
		}
		mgmtJSON(w, http.StatusOK, m.getGlobalSettings())

	case http.MethodPatch:
		if m.saveGlobalSettings == nil {
			mgmtError(w, http.StatusServiceUnavailable, "global settings save not available")
			return
		}
		var updates map[string]any
		if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := m.saveGlobalSettings(updates); err != nil {
			mgmtError(w, http.StatusInternalServerError, "save: "+err.Error())
			return
		}
		if m.getGlobalSettings != nil {
			mgmtJSON(w, http.StatusOK, m.getGlobalSettings())
		} else {
			mgmtOK(w, "settings saved")
		}

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET or PATCH only")
	}
}

// ── Project endpoints ─────────────────────────────────────────

func (m *ManagementServer) handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		m.mu.RLock()
		defer m.mu.RUnlock()

		projects := make([]map[string]any, 0, len(m.engines))
		for name, e := range m.engines {
			platNames := make([]string, len(e.platforms))
			for i, p := range e.platforms {
				platNames[i] = p.Name()
			}

			sessCount := 0
			e.interactiveMu.Lock()
			sessCount = len(e.interactiveStates)
			e.interactiveMu.Unlock()

			hbEnabled := false
			if m.heartbeatScheduler != nil {
				if st := m.heartbeatScheduler.Status(name); st != nil {
					hbEnabled = st.Enabled
				}
			}

			projects = append(projects, map[string]any{
				"name":              name,
				"display_name":      e.DisplayName(),
				"agent_type":        e.agent.Name(),
				"platforms":         platNames,
				"sessions_count":    sessCount,
				"heartbeat_enabled": hbEnabled,
			})
		}
		mgmtJSON(w, http.StatusOK, map[string]any{"projects": projects})
	case http.MethodPost:
		if m.createProject == nil {
			mgmtError(w, http.StatusServiceUnavailable, "project creation not available")
			return
		}
		var body struct {
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
			WorkDir     string `json:"work_dir"`
			AgentType   string `json:"agent_type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		createdProjectName, restartRequired, err := m.createProject(body.Name, body.DisplayName, body.WorkDir, body.AgentType)
		if err != nil {
			mgmtError(w, http.StatusInternalServerError, "save config: "+err.Error())
			return
		}
		createdName := strings.TrimSpace(body.DisplayName)
		if createdName == "" {
			createdName = strings.TrimSpace(createdProjectName)
			if createdName == "" {
				createdName = "project"
			}
		}
		mgmtJSON(w, http.StatusCreated, map[string]any{
			"message":          fmt.Sprintf("project %q created", createdName),
			"name":             strings.TrimSpace(createdProjectName),
			"display_name":     strings.TrimSpace(body.DisplayName),
			"restart_required": restartRequired,
		})
	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

// handleProjectRoutes dispatches /api/v1/projects/{name}/...
func (m *ManagementServer) handleProjectRoutes(w http.ResponseWriter, r *http.Request) {
	// Parse: /api/v1/projects/{name}[/sub[/subsub]]
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) == 0 || parts[0] == "" {
		mgmtError(w, http.StatusBadRequest, "project name required")
		return
	}

	projName := parts[0]
	m.mu.RLock()
	engine, ok := m.engines[projName]
	m.mu.RUnlock()
	if !ok {
		mgmtError(w, http.StatusNotFound, fmt.Sprintf("project not found: %s", projName))
		return
	}

	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	rest := ""
	if len(parts) > 2 {
		rest = parts[2]
	}

	switch sub {
	case "":
		m.handleProjectDetail(w, r, projName, engine)
	case "sessions":
		m.handleProjectSessions(w, r, projName, engine, rest)
	case "send":
		m.handleProjectSend(w, r, engine)
	case "providers":
		m.handleProjectProviders(w, r, engine, rest)
	case "provider-refs":
		m.handleProjectProviderRefs(w, r, projName, engine)
	case "models":
		m.handleProjectModels(w, r, engine)
	case "model":
		m.handleProjectModel(w, r, engine)
	case "heartbeat":
		m.handleProjectHeartbeat(w, r, projName, rest)
	case "users":
		m.handleProjectUsers(w, r, engine)
	case "add-platform":
		m.handleProjectAddPlatform(w, r, projName)
	case "platforms":
		m.handleProjectRemovePlatform(w, r, projName, rest)
	default:
		mgmtError(w, http.StatusNotFound, "not found")
	}
}

func (m *ManagementServer) handleProjectDetail(w http.ResponseWriter, r *http.Request, name string, e *Engine) {
	if r.Method == http.MethodGet {
		platInfos := make([]map[string]any, len(e.platforms))
		for i, p := range e.platforms {
			platInfos[i] = map[string]any{
				"type":      p.Name(),
				"connected": true,
			}
		}

		e.interactiveMu.Lock()
		sessCount := len(e.interactiveStates)
		keys := make([]string, 0, sessCount)
		for k := range e.interactiveStates {
			keys = append(keys, k)
		}
		e.interactiveMu.Unlock()

		data := map[string]any{
			"name":                name,
			"display_name":        e.DisplayName(),
			"agent_type":          e.agent.Name(),
			"platforms":           platInfos,
			"sessions_count":      sessCount,
			"active_session_keys": keys,
		}

		if m.heartbeatScheduler != nil {
			if st := m.heartbeatScheduler.Status(name); st != nil {
				data["heartbeat"] = map[string]any{
					"enabled":       st.Enabled,
					"paused":        st.Paused,
					"interval_mins": st.IntervalMins,
					"session_key":   st.SessionKey,
				}
			}
		}

		e.userRolesMu.RLock()
		adminFrom := e.adminFrom
		e.userRolesMu.RUnlock()

		data["settings"] = map[string]any{
			"language":          string(e.i18n.CurrentLang()),
			"admin_from":        adminFrom,
			"disabled_commands": e.GetDisabledCommands(),
		}

		var workDir string
		if wd, ok := e.agent.(interface{ GetWorkDir() string }); ok {
			workDir = wd.GetWorkDir()
		}
		var agentMode string
		if am, ok := e.agent.(interface{ GetMode() string }); ok {
			agentMode = am.GetMode()
		}
		data["work_dir"] = workDir
		data["agent_mode"] = agentMode
		if switcher, ok := e.agent.(ModeSwitcher); ok {
			data["permission_modes"] = switcher.PermissionModes()
		}

		if m.getProjectConfig != nil {
			if extra := m.getProjectConfig(name); extra != nil {
				for k, v := range extra {
					data[k] = v
				}
			}
		}

		mgmtJSON(w, http.StatusOK, data)
		return
	}

	if r.Method == http.MethodPatch {
		var body struct {
			DisplayName          *string           `json:"display_name"`
			Language             *string           `json:"language"`
			AdminFrom            *string           `json:"admin_from"`
			DisabledCommands     []string          `json:"disabled_commands"`
			WorkDir              *string           `json:"work_dir"`
			Mode                 *string           `json:"mode"`
			ShowContextIndicator *bool             `json:"show_context_indicator"`
			ReplyFooter          *bool             `json:"reply_footer"`
			PlatformAllowFrom    map[string]string `json:"platform_allow_from"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		if body.DisplayName != nil {
			e.SetDisplayName(strings.TrimSpace(*body.DisplayName))
			syncProjectAliasesForEngine(e)
		}
		if body.Language != nil {
			switch *body.Language {
			case "en":
				e.i18n.SetLang(LangEnglish)
			case "zh":
				e.i18n.SetLang(LangChinese)
			case "zh-TW":
				e.i18n.SetLang(LangTraditionalChinese)
			case "ja":
				e.i18n.SetLang(LangJapanese)
			case "es":
				e.i18n.SetLang(LangSpanish)
			}
		}
		if body.AdminFrom != nil {
			e.SetAdminFrom(*body.AdminFrom)
		}
		if body.DisabledCommands != nil {
			e.SetDisabledCommands(body.DisabledCommands)
		}
		if body.WorkDir != nil {
			if switcher, ok := e.agent.(WorkDirSwitcher); ok {
				switcher.SetWorkDir(*body.WorkDir)
			}
		}
		if body.Mode != nil {
			if switcher, ok := e.agent.(ModeSwitcher); ok {
				switcher.SetMode(*body.Mode)
			}
		}
		if body.ShowContextIndicator != nil {
			e.SetShowContextIndicator(*body.ShowContextIndicator)
		}
		if body.ReplyFooter != nil {
			e.SetReplyFooterEnabled(*body.ReplyFooter)
		}

		if m.saveProjectSettings != nil {
			patch := ProjectSettingsUpdate{
				DisplayName:          body.DisplayName,
				Language:             body.Language,
				AdminFrom:            body.AdminFrom,
				DisabledCommands:     body.DisabledCommands,
				WorkDir:              body.WorkDir,
				Mode:                 body.Mode,
				ShowContextIndicator: body.ShowContextIndicator,
				ReplyFooter:          body.ReplyFooter,
				PlatformAllowFrom:    body.PlatformAllowFrom,
			}
			if err := m.saveProjectSettings(name, patch); err != nil {
				slog.Warn("management: failed to persist project settings", "project", name, "error", err)
			}
		}

		mgmtOK(w, "settings updated")
		return
	}

	if r.Method == http.MethodDelete {
		if m.removeProject == nil {
			mgmtError(w, http.StatusNotImplemented, "project removal not configured")
			return
		}
		if err := m.removeProject(name); err != nil {
			mgmtError(w, http.StatusInternalServerError, err.Error())
			return
		}
		mgmtJSON(w, http.StatusOK, map[string]any{
			"message":          fmt.Sprintf("project %q removed from config", name),
			"restart_required": true,
		})
		return
	}

	mgmtError(w, http.StatusMethodNotAllowed, "GET, PATCH or DELETE only")
}

// ── Users endpoints ──────────────────────────────────────────

func (m *ManagementServer) handleProjectUsers(w http.ResponseWriter, r *http.Request, e *Engine) {
	switch r.Method {
	case http.MethodGet:
		e.userRolesMu.RLock()
		urm := e.userRoles
		e.userRolesMu.RUnlock()
		mgmtJSON(w, http.StatusOK, urm.Snapshot())

	case http.MethodPatch:
		var body struct {
			DefaultRole string                     `json:"default_role"`
			Roles       map[string]json.RawMessage `json:"roles"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		var roles []RoleInput
		for name, raw := range body.Roles {
			var rc struct {
				UserIDs          []string `json:"user_ids"`
				DisabledCommands []string `json:"disabled_commands"`
				RateLimit        *struct {
					MaxMessages int `json:"max_messages"`
					WindowSecs  int `json:"window_secs"`
				} `json:"rate_limit"`
			}
			if err := json.Unmarshal(raw, &rc); err != nil {
				mgmtError(w, http.StatusBadRequest, fmt.Sprintf("invalid role %q: %s", name, err))
				return
			}
			ri := RoleInput{
				Name:             name,
				UserIDs:          rc.UserIDs,
				DisabledCommands: rc.DisabledCommands,
			}
			if rc.RateLimit != nil {
				ri.RateLimit = &RateLimitCfg{
					MaxMessages: rc.RateLimit.MaxMessages,
					Window:      time.Duration(rc.RateLimit.WindowSecs) * time.Second,
				}
			}
			roles = append(roles, ri)
		}

		defaultRole := body.DefaultRole
		if defaultRole == "" {
			defaultRole = "member"
		}

		if err := ValidateRoleInputs(defaultRole, roles); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid users config: "+err.Error())
			return
		}

		urm := NewUserRoleManager()
		urm.Configure(defaultRole, roles)
		e.SetUserRoles(urm)

		mgmtOK(w, "users config updated")

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET or PATCH only")
	}
}

// ── Session endpoints ─────────────────────────────────────────

type managementSessionLive struct {
	activeKeys  map[string]string
	bySessionID map[string]string
	byKey       map[string]string
	activeIDs   map[string]bool
}

func managementSessionLiveSnapshot(e *Engine, idToKey map[string]string, activeIDs map[string]bool) managementSessionLive {
	live := managementSessionLive{
		activeKeys:  make(map[string]string),
		bySessionID: make(map[string]string),
		byKey:       make(map[string]string),
		activeIDs:   activeIDs,
	}
	if e == nil {
		return live
	}

	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	for runtimeKey, state := range e.interactiveStates {
		pName := ""
		if state != nil && state.platform != nil {
			pName = state.platform.Name()
		}
		live.activeKeys[runtimeKey] = pName

		matchedSpecific := false
		for sessionID, sessionKey := range idToKey {
			if managementRuntimeKeyMatchesSession(runtimeKey, sessionKey, sessionID) {
				live.bySessionID[sessionID] = pName
				matchedSpecific = true
			}
		}
		if matchedSpecific {
			continue
		}
		for _, sessionKey := range idToKey {
			if managementRuntimeKeyMatchesSessionKey(runtimeKey, sessionKey) {
				live.byKey[sessionKey] = pName
			}
		}
	}
	return live
}

func (l managementSessionLive) platformForSession(sessionID, sessionKey string) (string, bool) {
	if p, ok := l.bySessionID[sessionID]; ok {
		return p, true
	}
	if !l.activeIDs[sessionID] {
		return "", false
	}
	if p, ok := l.byKey[sessionKey]; ok {
		return p, true
	}
	return "", false
}

func managementRuntimeKeyMatchesSession(runtimeKey, sessionKey, sessionID string) bool {
	if runtimeKey == "" || sessionKey == "" || sessionID == "" {
		return false
	}
	want := sessionKey + "::" + sessionID
	return runtimeKey == want || strings.HasSuffix(runtimeKey, ":"+want)
}

func managementRuntimeKeyMatchesSessionKey(runtimeKey, sessionKey string) bool {
	if runtimeKey == "" || sessionKey == "" {
		return false
	}
	return runtimeKey == sessionKey || strings.HasSuffix(runtimeKey, ":"+sessionKey)
}

func (m *ManagementServer) handleProjectSessions(w http.ResponseWriter, r *http.Request, projName string, e *Engine, rest string) {
	// sub-routes like /sessions/switch
	if rest == "switch" {
		m.handleProjectSessionSwitch(w, r, e)
		return
	}
	if rest != "" {
		m.handleProjectSessionDetail(w, r, e, rest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		idToKey, activeIDs := e.sessions.SessionKeyMap()
		live := managementSessionLiveSnapshot(e, idToKey, activeIDs)
		stored := e.sessions.AllSessions()
		sessions := make([]map[string]any, 0, len(stored))
		for _, s := range stored {
			s.mu.Lock()
			histCount := len(s.History)
			var lastMsg map[string]any
			if histCount > 0 {
				last := s.History[histCount-1]
				preview := last.Content
				if len(preview) > 200 {
					preview = preview[:200]
				}
				lastMsg = map[string]any{
					"role":      last.Role,
					"content":   preview,
					"timestamp": last.Timestamp,
				}
			}
			info := map[string]any{
				"id":            s.ID,
				"name":          s.Name,
				"alias_mode":    s.AliasMode,
				"alias_suffix":  s.AliasSuffix,
				"session_key":   idToKey[s.ID],
				"agent_type":    s.AgentType,
				"active":        activeIDs[s.ID],
				"history_count": histCount,
				"created_at":    s.CreatedAt,
				"updated_at":    s.UpdatedAt,
				"last_message":  lastMsg,
			}
			s.mu.Unlock()

			sessionKey := idToKey[s.ID]
			livePlatform, sessionLive := live.platformForSession(s.ID, sessionKey)
			info["live"] = sessionLive
			if p := livePlatform; p != "" {
				info["platform"] = p
			} else if len(sessionKey) > 0 {
				parts := splitSessionKey(sessionKey)
				if len(parts) > 0 {
					info["platform"] = parts[0]
				}
			}

			if meta := e.sessions.GetUserMeta(sessionKey); meta != nil {
				info["user_name"] = meta.UserName
				info["chat_name"] = meta.ChatName
			}

			sessions = append(sessions, info)
		}

		mgmtJSON(w, http.StatusOK, map[string]any{
			"sessions":    sessions,
			"active_keys": live.activeKeys,
		})

	case http.MethodPost:
		var body struct {
			SessionKey string `json:"session_key"`
			Name       string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.SessionKey == "" {
			mgmtError(w, http.StatusBadRequest, "session_key is required")
			return
		}

		name := strings.TrimSpace(body.Name)
		if name == "" {
			name = "default"
		}
		s := e.sessions.NewSession(body.SessionKey, name)

		mgmtJSON(w, http.StatusOK, map[string]any{
			"id":          s.ID,
			"session_key": body.SessionKey,
			"name":        s.GetName(),
			"created_at":  s.CreatedAt,
			"updated_at":  s.UpdatedAt,
		})

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

func (m *ManagementServer) handleProjectSessionDetail(w http.ResponseWriter, r *http.Request, e *Engine, sessionID string) {
	switch r.Method {
	case http.MethodGet:
		s := e.sessions.FindByID(sessionID)
		if s == nil {
			mgmtError(w, http.StatusNotFound, "session not found")
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
			histJSON[i] = historyEntryMap(h, true)
		}

		idToKey, activeIDs := e.sessions.SessionKeyMap()
		sessionKey := idToKey[s.ID]

		live := managementSessionLiveSnapshot(e, idToKey, activeIDs)
		livePlatform, sessionLive := live.platformForSession(s.ID, sessionKey)

		s.mu.Lock()
		data := map[string]any{
			"id":               s.ID,
			"name":             s.Name,
			"alias_mode":       s.AliasMode,
			"alias_suffix":     s.AliasSuffix,
			"session_key":      sessionKey,
			"agent_session_id": s.AgentSessionID,
			"agent_type":       s.AgentType,
			"active":           activeIDs[s.ID],
			"live":             sessionLive,
			"history_count":    len(s.History),
			"created_at":       s.CreatedAt,
			"updated_at":       s.UpdatedAt,
			"history":          histJSON,
		}
		s.mu.Unlock()

		if livePlatform != "" {
			data["platform"] = livePlatform
		} else if len(sessionKey) > 0 {
			parts := splitSessionKey(sessionKey)
			if len(parts) > 0 {
				data["platform"] = parts[0]
			}
		}

		mgmtJSON(w, http.StatusOK, data)

	case http.MethodPatch:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			mgmtError(w, http.StatusBadRequest, "name is required")
			return
		}
		if ok := e.sessions.SetSessionManualName(sessionID, name); !ok {
			mgmtError(w, http.StatusNotFound, "session not found")
			return
		}
		mgmtJSON(w, http.StatusOK, map[string]any{"id": sessionID, "name": name})

	case http.MethodDelete:
		if e.sessions.DeleteByID(sessionID) {
			mgmtOK(w, "session deleted")
		} else {
			mgmtError(w, http.StatusNotFound, "session not found")
		}

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET, PATCH or DELETE only")
	}
}

func (m *ManagementServer) handleProjectSessionSwitch(w http.ResponseWriter, r *http.Request, e *Engine) {
	if r.Method != http.MethodPost {
		mgmtError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		SessionKey string `json:"session_key"`
		SessionID  string `json:"session_id"`
		Target     string `json:"target,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	sessionID := strings.TrimSpace(body.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(body.Target)
	}
	if body.SessionKey == "" || sessionID == "" {
		mgmtError(w, http.StatusBadRequest, "session_key and session_id are required")
		return
	}
	s, err := e.sessions.SwitchSession(body.SessionKey, sessionID)
	if err != nil {
		mgmtError(w, http.StatusNotFound, err.Error())
		return
	}
	mgmtJSON(w, http.StatusOK, map[string]any{
		"message":           "active session switched",
		"session_key":       body.SessionKey,
		"session_id":        s.ID,
		"active_session_id": s.ID,
	})
}

func (m *ManagementServer) handleProjectSend(w http.ResponseWriter, r *http.Request, e *Engine) {
	if r.Method != http.MethodPost {
		mgmtError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		SessionKey string            `json:"session_key"`
		SessionID  string            `json:"session_id,omitempty"`
		Message    string            `json:"message"`
		Images     []bridgeImageData `json:"images,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	images, err := bridgeImagesToAttachments(body.Images)
	if err != nil {
		mgmtError(w, http.StatusBadRequest, "invalid images: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Message) == "" && len(images) == 0 {
		mgmtError(w, http.StatusBadRequest, "message or attachment is required")
		return
	}

	sessionKey := strings.TrimSpace(body.SessionKey)
	sessionID := strings.TrimSpace(body.SessionID)
	if sessionID != "" {
		idToKey, _ := e.sessions.SessionKeyMap()
		keyForID := idToKey[sessionID]
		if keyForID == "" {
			mgmtError(w, http.StatusNotFound, "session not found")
			return
		}
		if sessionKey == "" {
			sessionKey = keyForID
		} else if sessionKey != keyForID {
			mgmtError(w, http.StatusBadRequest, "session_id does not belong to session_key")
			return
		}
	}

	if sessionID != "" {
		err = e.SendToSessionWithSessionIDAndAttachments(sessionKey, sessionID, body.Message, images, nil)
	} else {
		err = e.SendToSessionWithAttachments(sessionKey, body.Message, images, nil)
	}
	if err != nil {
		mgmtError(w, http.StatusInternalServerError, err.Error())
		return
	}
	data := map[string]any{"message": "message sent"}
	if sessionKey != "" {
		data["session_key"] = sessionKey
	}
	if sessionID != "" {
		data["session_id"] = sessionID
	}
	mgmtJSON(w, http.StatusOK, data)
}

// SendToSessionID sends a message to a specific live cc-connect session under
// a session_key. This preserves multi-conversation routing for management API
// callers that provide session_id.
func (e *Engine) SendToSessionID(sessionKey, sessionID, message string) error {
	sessionKey = strings.TrimSpace(sessionKey)
	sessionID = strings.TrimSpace(sessionID)
	if sessionKey == "" {
		return fmt.Errorf("session_key is required")
	}
	if sessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	if message == "" {
		return fmt.Errorf("message is required")
	}
	return e.SendToSessionWithSessionIDAndAttachments(sessionKey, sessionID, message, nil, nil)
}

// ── Provider endpoints ────────────────────────────────────────

func (m *ManagementServer) handleProjectProviders(w http.ResponseWriter, r *http.Request, e *Engine, rest string) {
	ps, ok := e.agent.(ProviderSwitcher)
	if !ok {
		mgmtError(w, http.StatusBadRequest, "agent does not support provider switching")
		return
	}

	// /providers/{name}/activate
	if rest != "" {
		parts := strings.SplitN(rest, "/", 2)
		provName := parts[0]
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}
		if action == "activate" && r.Method == http.MethodPost {
			if !ps.SetActiveProvider(provName) {
				mgmtError(w, http.StatusNotFound, fmt.Sprintf("provider not found: %s", provName))
				return
			}
			if e.providerSaveFunc != nil {
				_ = e.providerSaveFunc(provName)
			}
			mgmtJSON(w, http.StatusOK, map[string]any{
				"active_provider": provName,
				"message":         "provider activated",
			})
			return
		}
		if r.Method == http.MethodDelete {
			current := ps.GetActiveProvider()
			if current != nil && current.Name == provName {
				mgmtError(w, http.StatusBadRequest, "cannot remove active provider; switch to another first")
				return
			}
			providers := ps.ListProviders()
			var remaining []ProviderConfig
			found := false
			for _, p := range providers {
				if p.Name == provName {
					found = true
					continue
				}
				remaining = append(remaining, p)
			}
			if !found {
				mgmtError(w, http.StatusNotFound, fmt.Sprintf("provider not found: %s", provName))
				return
			}
			ps.SetProviders(remaining)
			if e.providerRemoveSaveFunc != nil {
				_ = e.providerRemoveSaveFunc(provName)
			}
			mgmtOK(w, "provider removed")
			return
		}
		mgmtError(w, http.StatusNotFound, "not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		providers := ps.ListProviders()
		current := ps.GetActiveProvider()
		provList := make([]map[string]any, len(providers))
		activeName := ""
		if current != nil {
			activeName = current.Name
		}
		for i, p := range providers {
			provList[i] = map[string]any{
				"name":     p.Name,
				"active":   p.Name == activeName,
				"model":    p.Model,
				"base_url": p.BaseURL,
			}
		}
		mgmtJSON(w, http.StatusOK, map[string]any{
			"providers":       provList,
			"active_provider": activeName,
		})

	case http.MethodPost:
		var body struct {
			Name     string            `json:"name"`
			APIKey   string            `json:"api_key"`
			BaseURL  string            `json:"base_url"`
			Model    string            `json:"model"`
			Thinking string            `json:"thinking"`
			Env      map[string]string `json:"env"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.Name == "" {
			mgmtError(w, http.StatusBadRequest, "name is required")
			return
		}
		prov := ProviderConfig{
			Name:     body.Name,
			APIKey:   body.APIKey,
			BaseURL:  body.BaseURL,
			Model:    body.Model,
			Thinking: body.Thinking,
			Env:      body.Env,
		}
		providers := ps.ListProviders()
		providers = append(providers, prov)
		ps.SetProviders(providers)
		if e.providerAddSaveFunc != nil {
			_ = e.providerAddSaveFunc(prov)
		}
		mgmtJSON(w, http.StatusOK, map[string]any{
			"name":    body.Name,
			"message": "provider added",
		})

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

func (m *ManagementServer) handleProjectProviderRefs(w http.ResponseWriter, r *http.Request, projName string, e *Engine) {
	switch r.Method {
	case http.MethodGet:
		if m.getProjectConfig == nil {
			mgmtJSON(w, http.StatusOK, map[string]any{"provider_refs": []string{}})
			return
		}
		cfg := m.getProjectConfig(projName)
		refs, _ := cfg["provider_refs"].([]string)
		if refs == nil {
			refs = []string{}
		}
		mgmtJSON(w, http.StatusOK, map[string]any{"provider_refs": refs})

	case http.MethodPut:
		if m.saveProviderRefs == nil {
			mgmtError(w, http.StatusNotImplemented, "provider refs saving not available")
			return
		}
		var body struct {
			ProviderRefs []string `json:"provider_refs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := m.saveProviderRefs(projName, body.ProviderRefs); err != nil {
			mgmtError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Reload providers into the running engine
		ps, ok := e.agent.(ProviderSwitcher)
		if ok && m.listGlobalProviders != nil {
			globals, _ := m.listGlobalProviders()
			globalMap := make(map[string]GlobalProviderInfo, len(globals))
			for _, g := range globals {
				globalMap[g.Name] = g
			}
			existing := ps.ListProviders()
			existingNames := make(map[string]bool, len(existing))
			for _, p := range existing {
				existingNames[p.Name] = true
			}
			for _, ref := range body.ProviderRefs {
				if existingNames[ref] {
					continue
				}
				if g, ok := globalMap[ref]; ok {
					ps.SetProviders(append(ps.ListProviders(), ProviderConfig{
						Name:    g.Name,
						APIKey:  g.APIKey,
						BaseURL: g.BaseURL,
						Model:   g.Model,
					}))
				}
			}
		}
		mgmtOK(w, "provider refs updated")

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET or PUT only")
	}
}

func (m *ManagementServer) handleProjectModels(w http.ResponseWriter, r *http.Request, e *Engine) {
	if r.Method != http.MethodGet {
		mgmtError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	ms, ok := e.agent.(ModelSwitcher)
	if !ok {
		mgmtError(w, http.StatusBadRequest, "agent does not support model switching")
		return
	}
	fetchCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	models := ms.AvailableModels(fetchCtx)
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.Name
	}
	mgmtJSON(w, http.StatusOK, map[string]any{
		"models":  names,
		"current": ms.GetModel(),
	})
}

func (m *ManagementServer) handleProjectModel(w http.ResponseWriter, r *http.Request, e *Engine) {
	if r.Method != http.MethodPost {
		mgmtError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if _, ok := e.agent.(ModelSwitcher); !ok {
		mgmtError(w, http.StatusBadRequest, "agent does not support model switching")
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Model == "" {
		mgmtError(w, http.StatusBadRequest, "model is required")
		return
	}
	model, err := e.switchModel(body.Model)
	if err != nil {
		mgmtError(w, http.StatusInternalServerError, err.Error())
		return
	}
	mgmtJSON(w, http.StatusOK, map[string]any{
		"model":   model,
		"message": "model updated",
	})
}

// ── Heartbeat endpoints ───────────────────────────────────────

func (m *ManagementServer) handleProjectHeartbeat(w http.ResponseWriter, r *http.Request, projName, rest string) {
	if m.heartbeatScheduler == nil {
		mgmtError(w, http.StatusServiceUnavailable, "heartbeat scheduler not available")
		return
	}

	switch rest {
	case "", "status":
		if r.Method != http.MethodGet {
			mgmtError(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		st := m.heartbeatScheduler.Status(projName)
		if st == nil {
			mgmtJSON(w, http.StatusOK, map[string]any{"enabled": false})
			return
		}
		data := map[string]any{
			"enabled":        st.Enabled,
			"paused":         st.Paused,
			"interval_mins":  st.IntervalMins,
			"only_when_idle": st.OnlyWhenIdle,
			"session_key":    st.SessionKey,
			"silent":         st.Silent,
			"run_count":      st.RunCount,
			"error_count":    st.ErrorCount,
			"skipped_busy":   st.SkippedBusy,
			"last_error":     st.LastError,
		}
		if !st.LastRun.IsZero() {
			data["last_run"] = st.LastRun.Format(time.RFC3339)
		}
		mgmtJSON(w, http.StatusOK, data)

	case "pause":
		if r.Method != http.MethodPost {
			mgmtError(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		if m.heartbeatScheduler.Pause(projName) {
			mgmtOK(w, "heartbeat paused")
		} else {
			mgmtError(w, http.StatusNotFound, "heartbeat not found for project")
		}

	case "resume":
		if r.Method != http.MethodPost {
			mgmtError(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		if m.heartbeatScheduler.Resume(projName) {
			mgmtOK(w, "heartbeat resumed")
		} else {
			mgmtError(w, http.StatusNotFound, "heartbeat not found for project")
		}

	case "run":
		if r.Method != http.MethodPost {
			mgmtError(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		if m.heartbeatScheduler.TriggerNow(projName) {
			mgmtOK(w, "heartbeat triggered")
		} else {
			mgmtError(w, http.StatusNotFound, "heartbeat not found for project")
		}

	case "interval":
		if r.Method != http.MethodPost {
			mgmtError(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		var body struct {
			Minutes int `json:"minutes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.Minutes < 1 {
			mgmtError(w, http.StatusBadRequest, "minutes must be >= 1")
			return
		}
		if m.heartbeatScheduler.SetInterval(projName, body.Minutes) {
			mgmtJSON(w, http.StatusOK, map[string]any{
				"interval_mins": body.Minutes,
				"message":       "interval updated",
			})
		} else {
			mgmtError(w, http.StatusNotFound, "heartbeat not found for project")
		}

	default:
		mgmtError(w, http.StatusNotFound, "not found")
	}
}

// ── Cron endpoints ────────────────────────────────────────────

func (m *ManagementServer) handleCron(w http.ResponseWriter, r *http.Request) {
	if m.cronScheduler == nil {
		mgmtError(w, http.StatusServiceUnavailable, "cron scheduler not available")
		return
	}

	switch r.Method {
	case http.MethodGet:
		project := r.URL.Query().Get("project")
		var jobs []*CronJob
		if project != "" {
			jobs = m.cronScheduler.Store().ListByProject(project)
		} else {
			jobs = m.cronScheduler.Store().List()
		}
		mgmtJSON(w, http.StatusOK, map[string]any{"jobs": jobs})

	case http.MethodPost:
		var req CronAddRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.CronExpr == "" {
			mgmtError(w, http.StatusBadRequest, "cron_expr is required")
			return
		}
		if req.Prompt == "" && req.Exec == "" {
			mgmtError(w, http.StatusBadRequest, "either prompt or exec is required")
			return
		}
		if req.Prompt != "" && req.Exec != "" {
			mgmtError(w, http.StatusBadRequest, "prompt and exec are mutually exclusive")
			return
		}

		project := req.Project
		if project == "" {
			m.mu.RLock()
			if len(m.engines) == 1 {
				for name := range m.engines {
					project = name
				}
			}
			m.mu.RUnlock()
		}
		if project == "" {
			mgmtError(w, http.StatusBadRequest, "project is required (multiple projects configured)")
			return
		}

		job := &CronJob{
			ID:          GenerateCronID(),
			Project:     project,
			SessionKey:  req.SessionKey,
			CronExpr:    req.CronExpr,
			Prompt:      req.Prompt,
			Exec:        req.Exec,
			WorkDir:     req.WorkDir,
			Description: req.Description,
			Enabled:     true,
			Silent:      req.Silent,
			SessionMode: NormalizeCronSessionMode(req.SessionMode),
			Mode:        req.Mode,
			TimeoutMins: req.TimeoutMins,
			CreatedAt:   time.Now(),
		}
		if err := m.cronScheduler.AddJob(job); err != nil {
			mgmtError(w, http.StatusBadRequest, err.Error())
			return
		}
		mgmtJSON(w, http.StatusOK, job)

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

func (m *ManagementServer) handleCronByID(w http.ResponseWriter, r *http.Request) {
	if m.cronScheduler == nil {
		mgmtError(w, http.StatusServiceUnavailable, "cron scheduler not available")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/cron/")
	if id == "" {
		mgmtError(w, http.StatusBadRequest, "cron job id required")
		return
	}

	switch r.Method {
	case http.MethodDelete:
		if m.cronScheduler.RemoveJob(id) {
			mgmtOK(w, "cron job deleted")
		} else {
			mgmtError(w, http.StatusNotFound, fmt.Sprintf("cron job not found: %s", id))
		}

	case http.MethodPatch:
		var updates map[string]any
		if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		for field, value := range updates {
			if err := m.cronScheduler.UpdateJob(id, field, value); err != nil {
				mgmtError(w, http.StatusBadRequest, fmt.Sprintf("update %s: %s", field, err.Error()))
				return
			}
		}
		job := m.cronScheduler.Store().Get(id)
		if job == nil {
			mgmtError(w, http.StatusNotFound, "cron job not found after update")
			return
		}
		mgmtJSON(w, http.StatusOK, job)

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "DELETE or PATCH only")
	}
}

// ── Bridge endpoints ──────────────────────────────────────────

func (m *ManagementServer) handleBridgeAdapters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		mgmtError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	adapters := m.listBridgeAdapters()
	mgmtJSON(w, http.StatusOK, map[string]any{"adapters": adapters})
}

func (m *ManagementServer) listBridgeAdapters() []map[string]any {
	if m.bridgeServer == nil {
		return nil
	}
	m.bridgeServer.mu.RLock()
	defer m.bridgeServer.mu.RUnlock()

	adapters := make([]map[string]any, 0, len(m.bridgeServer.adapters))
	for name, a := range m.bridgeServer.adapters {
		caps := make([]string, 0, len(a.capabilities))
		for c := range a.capabilities {
			caps = append(caps, c)
		}

		project := ""
		m.bridgeServer.enginesMu.RLock()
		for pName, ref := range m.bridgeServer.engines {
			if ref.platform != nil && ref.platform.Name() == name {
				project = pName
				break
			}
		}
		m.bridgeServer.enginesMu.RUnlock()

		adapters = append(adapters, map[string]any{
			"platform":     name,
			"project":      project,
			"capabilities": caps,
		})
	}
	return adapters
}

// ── Frontend app registry endpoints ───────────────────────────

type frontendAppRequest struct {
	ID          string            `json:"id"`
	AppID       string            `json:"app_id"`
	Name        string            `json:"name"`
	Project     string            `json:"project"`
	Description string            `json:"description"`
	Metadata    map[string]string `json:"metadata"`
}

type frontendSlotRequest struct {
	Slot            string            `json:"slot"`
	Label           string            `json:"label"`
	URL             string            `json:"url"`
	APIBase         string            `json:"api_base"`
	AdapterPlatform string            `json:"adapter_platform"`
	Enabled         *bool             `json:"enabled"`
	Metadata        map[string]string `json:"metadata"`
}

type frontendAppPatchRequest struct {
	Name        *string           `json:"name"`
	Project     *string           `json:"project"`
	Description *string           `json:"description"`
	Metadata    map[string]string `json:"metadata"`
}

type frontendSlotPatchRequest struct {
	Label           *string           `json:"label"`
	URL             *string           `json:"url"`
	APIBase         *string           `json:"api_base"`
	AdapterPlatform *string           `json:"adapter_platform"`
	Enabled         *bool             `json:"enabled"`
	Metadata        map[string]string `json:"metadata"`
}

type frontendServiceRequest struct {
	ServiceID           string            `json:"service_id"`
	URL                 string            `json:"url"`
	APIBase             string            `json:"api_base"`
	Version             string            `json:"version"`
	Build               string            `json:"build"`
	HeartbeatTTLSeconds int               `json:"heartbeat_ttl_seconds"`
	Metadata            map[string]string `json:"metadata"`
}

func (m *ManagementServer) handleFrontendApps(w http.ResponseWriter, r *http.Request) {
	if m.frontendRegistry == nil {
		mgmtError(w, http.StatusServiceUnavailable, "frontend app registry not configured")
		return
	}

	switch r.Method {
	case http.MethodGet:
		apps, err := m.frontendRegistry.ListAppViews()
		if err != nil {
			mgmtError(w, http.StatusInternalServerError, err.Error())
			return
		}
		mgmtJSON(w, http.StatusOK, map[string]any{"apps": apps})

	case http.MethodPost:
		var body frontendAppRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		id := strings.TrimSpace(body.ID)
		if id == "" {
			id = strings.TrimSpace(body.AppID)
		}
		app, err := m.frontendRegistry.CreateApp(FrontendApp{
			ID:          id,
			Name:        body.Name,
			Project:     body.Project,
			Description: body.Description,
			Metadata:    body.Metadata,
		})
		if err != nil {
			mgmtError(w, http.StatusBadRequest, err.Error())
			return
		}
		appView, ok, err := m.frontendRegistry.GetAppView(app.ID)
		if err != nil || !ok {
			mgmtJSON(w, http.StatusCreated, map[string]any{"app": app})
			return
		}
		mgmtJSON(w, http.StatusCreated, map[string]any{"app": appView})

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

func (m *ManagementServer) handleFrontendAppRoutes(w http.ResponseWriter, r *http.Request) {
	if m.frontendRegistry == nil {
		mgmtError(w, http.StatusServiceUnavailable, "frontend app registry not configured")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/v1/apps/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		mgmtError(w, http.StatusBadRequest, "app id required")
		return
	}
	appID := parts[0]

	if len(parts) == 1 {
		m.handleFrontendAppDetail(w, r, appID)
		return
	}
	if parts[1] != "slots" {
		mgmtError(w, http.StatusNotFound, "not found")
		return
	}
	if len(parts) == 2 {
		m.handleFrontendSlots(w, r, appID)
		return
	}
	if strings.TrimSpace(parts[2]) == "" {
		mgmtError(w, http.StatusBadRequest, "slot required")
		return
	}
	if len(parts) == 3 {
		m.handleFrontendSlotDetail(w, r, appID, parts[2])
		return
	}
	if len(parts) == 4 && parts[3] == "promote" {
		m.handleFrontendSlotPromote(w, r, appID, parts[2])
		return
	}
	if len(parts) == 4 && parts[3] == "service" {
		m.handleFrontendSlotService(w, r, appID, parts[2])
		return
	}
	if len(parts) == 5 && parts[3] == "service" {
		switch parts[4] {
		case "register":
			m.handleFrontendSlotServiceRegister(w, r, appID, parts[2])
		case "heartbeat":
			m.handleFrontendSlotServiceHeartbeat(w, r, appID, parts[2])
		default:
			mgmtError(w, http.StatusNotFound, "not found")
		}
		return
	}
	mgmtError(w, http.StatusNotFound, "not found")
}

func (m *ManagementServer) handleFrontendAppDetail(w http.ResponseWriter, r *http.Request, appID string) {
	switch r.Method {
	case http.MethodGet:
		app, ok, err := m.frontendRegistry.GetAppView(appID)
		if err != nil {
			mgmtError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			mgmtError(w, http.StatusNotFound, "frontend app not found")
			return
		}
		mgmtJSON(w, http.StatusOK, map[string]any{"app": app})

	case http.MethodPatch:
		var body frontendAppPatchRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		app, err := m.frontendRegistry.UpdateApp(appID, FrontendAppUpdate{
			Name:        body.Name,
			Project:     body.Project,
			Description: body.Description,
			Metadata:    body.Metadata,
		})
		if err != nil {
			mgmtError(w, http.StatusBadRequest, err.Error())
			return
		}
		appView, ok, err := m.frontendRegistry.GetAppView(app.ID)
		if err != nil || !ok {
			mgmtJSON(w, http.StatusOK, map[string]any{"app": app})
			return
		}
		mgmtJSON(w, http.StatusOK, map[string]any{"app": appView})

	case http.MethodDelete:
		if err := m.frontendRegistry.DeleteApp(appID); err != nil {
			mgmtError(w, http.StatusNotFound, err.Error())
			return
		}
		mgmtOK(w, "frontend app deleted")

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET, PATCH, or DELETE only")
	}
}

func (m *ManagementServer) handleFrontendSlots(w http.ResponseWriter, r *http.Request, appID string) {
	switch r.Method {
	case http.MethodGet:
		slots, err := m.frontendRegistry.ListSlotViews(appID)
		if err != nil {
			mgmtError(w, http.StatusNotFound, err.Error())
			return
		}
		mgmtJSON(w, http.StatusOK, map[string]any{"slots": slots})

	case http.MethodPost:
		var body frontendSlotRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		slot, err := m.frontendRegistry.UpsertSlot(appID, FrontendSlot{
			Slot:            body.Slot,
			Label:           body.Label,
			URL:             body.URL,
			APIBase:         body.APIBase,
			AdapterPlatform: body.AdapterPlatform,
			Enabled:         enabled,
			Metadata:        body.Metadata,
		})
		if err != nil {
			mgmtError(w, http.StatusBadRequest, err.Error())
			return
		}
		slotView, ok, err := m.frontendRegistry.GetSlotView(appID, slot.Slot)
		if err != nil || !ok {
			mgmtJSON(w, http.StatusCreated, map[string]any{"slot": slot})
			return
		}
		mgmtJSON(w, http.StatusCreated, map[string]any{"slot": slotView})

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

func (m *ManagementServer) handleFrontendSlotDetail(w http.ResponseWriter, r *http.Request, appID, slotName string) {
	switch r.Method {
	case http.MethodGet:
		slot, ok, err := m.frontendRegistry.GetSlotView(appID, slotName)
		if err != nil {
			mgmtError(w, http.StatusNotFound, err.Error())
			return
		}
		if !ok {
			mgmtError(w, http.StatusNotFound, "frontend slot not found")
			return
		}
		mgmtJSON(w, http.StatusOK, map[string]any{"slot": slot})

	case http.MethodPatch:
		var body frontendSlotPatchRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		slot, err := m.frontendRegistry.UpdateSlot(appID, slotName, FrontendSlotUpdate{
			Label:           body.Label,
			URL:             body.URL,
			APIBase:         body.APIBase,
			AdapterPlatform: body.AdapterPlatform,
			Enabled:         body.Enabled,
			Metadata:        body.Metadata,
		})
		if err != nil {
			mgmtError(w, http.StatusBadRequest, err.Error())
			return
		}
		slotView, ok, err := m.frontendRegistry.GetSlotView(appID, slot.Slot)
		if err != nil || !ok {
			mgmtJSON(w, http.StatusOK, map[string]any{"slot": slot})
			return
		}
		mgmtJSON(w, http.StatusOK, map[string]any{"slot": slotView})

	case http.MethodDelete:
		if err := m.frontendRegistry.DeleteSlot(appID, slotName); err != nil {
			mgmtError(w, http.StatusNotFound, err.Error())
			return
		}
		mgmtOK(w, "frontend slot deleted")

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET, PATCH, or DELETE only")
	}
}

func (m *ManagementServer) handleFrontendSlotPromote(w http.ResponseWriter, r *http.Request, appID, sourceSlot string) {
	if r.Method != http.MethodPost {
		mgmtError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		TargetSlot string `json:"target_slot"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	targetSlot := strings.TrimSpace(body.TargetSlot)
	if targetSlot == "" {
		targetSlot = "stable"
	}
	slot, err := m.frontendRegistry.PromoteSlot(appID, sourceSlot, targetSlot)
	if err != nil {
		mgmtError(w, http.StatusBadRequest, err.Error())
		return
	}
	slotView, ok, viewErr := m.frontendRegistry.GetSlotView(appID, slot.Slot)
	if viewErr == nil && ok {
		mgmtJSON(w, http.StatusOK, map[string]any{
			"message": "frontend slot promoted",
			"slot":    slotView,
		})
		return
	}
	mgmtJSON(w, http.StatusOK, map[string]any{
		"message": "frontend slot promoted",
		"slot":    slot,
	})
}

func (m *ManagementServer) handleFrontendSlotService(w http.ResponseWriter, r *http.Request, appID, slotName string) {
	switch r.Method {
	case http.MethodGet:
		slot, ok, err := m.frontendRegistry.GetSlotView(appID, slotName)
		if err != nil {
			mgmtError(w, http.StatusNotFound, err.Error())
			return
		}
		if !ok {
			mgmtError(w, http.StatusNotFound, "frontend slot not found")
			return
		}
		mgmtJSON(w, http.StatusOK, map[string]any{"service": slot.Service})

	case http.MethodDelete:
		if err := m.frontendRegistry.ClearSlotService(appID, slotName); err != nil {
			mgmtError(w, http.StatusNotFound, err.Error())
			return
		}
		slot, ok, err := m.frontendRegistry.GetSlotView(appID, slotName)
		if err != nil {
			mgmtError(w, http.StatusNotFound, err.Error())
			return
		}
		if !ok {
			mgmtError(w, http.StatusNotFound, "frontend slot not found")
			return
		}
		mgmtJSON(w, http.StatusOK, map[string]any{
			"message": "frontend service cleared",
			"service": slot.Service,
		})

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET or DELETE only")
	}
}

func (m *ManagementServer) handleFrontendSlotServiceRegister(w http.ResponseWriter, r *http.Request, appID, slotName string) {
	if r.Method != http.MethodPost {
		mgmtError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body frontendServiceRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	service, err := m.frontendRegistry.RegisterSlotService(appID, slotName, FrontendServiceRegistration{
		ServiceID:           body.ServiceID,
		URL:                 body.URL,
		APIBase:             body.APIBase,
		Version:             body.Version,
		Build:               body.Build,
		HeartbeatTTLSeconds: body.HeartbeatTTLSeconds,
		Metadata:            body.Metadata,
	})
	if err != nil {
		mgmtError(w, http.StatusBadRequest, err.Error())
		return
	}
	mgmtJSON(w, http.StatusOK, map[string]any{"service": service})
}

func (m *ManagementServer) handleFrontendSlotServiceHeartbeat(w http.ResponseWriter, r *http.Request, appID, slotName string) {
	if r.Method != http.MethodPost {
		mgmtError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body frontendServiceRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	service, err := m.frontendRegistry.HeartbeatSlotService(appID, slotName, FrontendServiceHeartbeat{
		ServiceID:           body.ServiceID,
		URL:                 body.URL,
		APIBase:             body.APIBase,
		Version:             body.Version,
		Build:               body.Build,
		HeartbeatTTLSeconds: body.HeartbeatTTLSeconds,
		Metadata:            body.Metadata,
	})
	if err != nil {
		mgmtError(w, http.StatusBadRequest, err.Error())
		return
	}
	mgmtJSON(w, http.StatusOK, map[string]any{"service": service})
}

// ── Global provider endpoints ─────────────────────────────────

func (m *ManagementServer) handleGlobalProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if m.listGlobalProviders == nil {
			mgmtJSON(w, http.StatusOK, map[string]any{"providers": []any{}})
			return
		}
		providers, err := m.listGlobalProviders()
		if err != nil {
			mgmtError(w, http.StatusInternalServerError, err.Error())
			return
		}
		mgmtJSON(w, http.StatusOK, map[string]any{"providers": providers})

	case http.MethodPost:
		if m.addGlobalProvider == nil {
			mgmtError(w, http.StatusNotImplemented, "not configured")
			return
		}
		var body GlobalProviderInfo
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if body.Name == "" {
			mgmtError(w, http.StatusBadRequest, "name is required")
			return
		}
		if err := m.addGlobalProvider(body); err != nil {
			if strings.Contains(err.Error(), "already exists") {
				mgmtError(w, http.StatusConflict, err.Error())
			} else {
				mgmtError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		mgmtJSON(w, http.StatusOK, map[string]any{"name": body.Name, "message": "provider added"})

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

func (m *ManagementServer) handleGlobalProviderRoutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/providers/")
	if rest == "" {
		m.handleGlobalProviders(w, r)
		return
	}

	// /providers/presets
	if rest == "presets" {
		m.handleProviderPresets(w, r)
		return
	}

	// /providers/{name} or /providers/{name}/...
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]

	switch r.Method {
	case http.MethodPut, http.MethodPatch:
		if m.updateGlobalProvider == nil {
			mgmtError(w, http.StatusNotImplemented, "not configured")
			return
		}
		var body GlobalProviderInfo
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			mgmtError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := m.updateGlobalProvider(name, body); err != nil {
			if strings.Contains(err.Error(), "not found") {
				mgmtError(w, http.StatusNotFound, err.Error())
			} else {
				mgmtError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		mgmtOK(w, "provider updated")

	case http.MethodDelete:
		if m.removeGlobalProvider == nil {
			mgmtError(w, http.StatusNotImplemented, "not configured")
			return
		}
		if err := m.removeGlobalProvider(name); err != nil {
			if strings.Contains(err.Error(), "not found") {
				mgmtError(w, http.StatusNotFound, err.Error())
			} else {
				mgmtError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		mgmtOK(w, "provider removed")

	default:
		mgmtError(w, http.StatusMethodNotAllowed, "PUT, PATCH or DELETE only")
	}
}

func (m *ManagementServer) handleProviderPresets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		mgmtError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if m.fetchPresets == nil {
		mgmtJSON(w, http.StatusOK, &ProviderPresetsResponse{Version: 1})
		return
	}
	data, err := m.fetchPresets()
	if err != nil {
		mgmtError(w, http.StatusBadGateway, "fetch presets: "+err.Error())
		return
	}
	mgmtJSON(w, http.StatusOK, data)
}
