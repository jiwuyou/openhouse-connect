package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type deadlineAwareModelAgent struct {
	stubModelModeAgent
	mu          sync.Mutex
	hasDeadline bool
}

func (a *deadlineAwareModelAgent) AvailableModels(ctx context.Context) []ModelOption {
	a.mu.Lock()
	_, ok := ctx.Deadline()
	a.hasDeadline = ok
	a.mu.Unlock()
	return []ModelOption{{Name: "gpt-4.1"}}
}

func (a *deadlineAwareModelAgent) sawDeadline() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hasDeadline
}

// testManagementServer creates a ManagementServer with a test engine and returns an httptest.Server.
func testManagementServer(t *testing.T, token string) (*ManagementServer, *httptest.Server, *Engine) {
	t.Helper()

	agent := &stubAgent{}
	sm := NewSessionManager("")
	e := NewEngine("test-project", agent, nil, "", LangEnglish)
	e.sessions = sm

	mgmt := NewManagementServer(0, token, nil)
	mgmt.RegisterEngine("test-project", e)

	mux := http.NewServeMux()
	prefix := "/api/v1"
	mux.HandleFunc(prefix+"/status", mgmt.wrap(mgmt.handleStatus))
	mux.HandleFunc(prefix+"/restart", mgmt.wrap(mgmt.handleRestart))
	mux.HandleFunc(prefix+"/reload", mgmt.wrap(mgmt.handleReload))
	mux.HandleFunc(prefix+"/config", mgmt.wrap(mgmt.handleConfig))
	mux.HandleFunc(prefix+"/filesystem/directories", mgmt.wrap(mgmt.handleFilesystemDirectories))
	mux.HandleFunc(prefix+"/projects", mgmt.wrap(mgmt.handleProjects))
	mux.HandleFunc(prefix+"/projects/", mgmt.wrap(mgmt.handleProjectRoutes))
	mux.HandleFunc(prefix+"/cron", mgmt.wrap(mgmt.handleCron))
	mux.HandleFunc(prefix+"/cron/", mgmt.wrap(mgmt.handleCronByID))
	mux.HandleFunc(prefix+"/bridge/adapters", mgmt.wrap(mgmt.handleBridgeAdapters))
	mux.HandleFunc(prefix+"/apps", mgmt.wrap(mgmt.handleFrontendApps))
	mux.HandleFunc(prefix+"/apps/", mgmt.wrap(mgmt.handleFrontendAppRoutes))

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return mgmt, ts, e
}

func TestTCPListenAddr(t *testing.T) {
	tests := []struct {
		name string
		host string
		port int
		want string
	}{
		{name: "all interfaces", host: "", port: 9820, want: ":9820"},
		{name: "ipv4 loopback", host: "127.0.0.1", port: 9820, want: "127.0.0.1:9820"},
		{name: "trim host", host: "  localhost  ", port: 9810, want: "localhost:9810"},
		{name: "ipv6 loopback", host: "::1", port: 9820, want: "[::1]:9820"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tcpListenAddr(tt.host, tt.port); got != tt.want {
				t.Fatalf("tcpListenAddr(%q, %d) = %q, want %q", tt.host, tt.port, got, tt.want)
			}
		})
	}
}

type mgmtResponse struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

func mgmtGet(t *testing.T, url, token string) mgmtResponse {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var r mgmtResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	return r
}

func mgmtPost(t *testing.T, url, token string, body any) mgmtResponse {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode POST body: %v", err)
		}
	}
	req, _ := http.NewRequest("POST", url, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var r mgmtResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode POST response: %v", err)
	}
	return r
}

func mgmtPatch(t *testing.T, url, token string, body any) mgmtResponse {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode PATCH body: %v", err)
		}
	}
	req, _ := http.NewRequest("PATCH", url, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	defer resp.Body.Close()
	var r mgmtResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode PATCH response: %v", err)
	}
	return r
}

func mgmtDelete(t *testing.T, url, token string) mgmtResponse {
	t.Helper()
	req, _ := http.NewRequest("DELETE", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	defer resp.Body.Close()
	var r mgmtResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode DELETE response: %v", err)
	}
	return r
}

func TestMgmt_AuthRequired(t *testing.T) {
	_, ts, _ := testManagementServer(t, "secret-token")

	r := mgmtGet(t, ts.URL+"/api/v1/status", "")
	if r.OK {
		t.Fatal("expected auth failure without token")
	}
	if !strings.Contains(r.Error, "unauthorized") {
		t.Fatalf("expected unauthorized error, got: %s", r.Error)
	}

	r = mgmtGet(t, ts.URL+"/api/v1/status", "wrong-token")
	if r.OK {
		t.Fatal("expected auth failure with wrong token")
	}

	r = mgmtGet(t, ts.URL+"/api/v1/status", "secret-token")
	if !r.OK {
		t.Fatalf("expected success with correct token, got error: %s", r.Error)
	}
}

func TestMgmt_AuthQueryParam(t *testing.T) {
	_, ts, _ := testManagementServer(t, "qp-token")

	r := mgmtGet(t, ts.URL+"/api/v1/status?token=qp-token", "")
	if !r.OK {
		t.Fatalf("expected success with query param token, got: %s", r.Error)
	}
}

func TestMgmt_NoAuthRequired(t *testing.T) {
	_, ts, _ := testManagementServer(t, "")

	r := mgmtGet(t, ts.URL+"/api/v1/status", "")
	if !r.OK {
		t.Fatalf("expected success without token when no token configured, got: %s", r.Error)
	}
}

func TestMgmt_Status(t *testing.T) {
	_, ts, _ := testManagementServer(t, "tok")

	r := mgmtGet(t, ts.URL+"/api/v1/status", "tok")
	if !r.OK {
		t.Fatalf("status failed: %s", r.Error)
	}

	var data map[string]any
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal status data: %v", err)
	}
	if data["projects_count"] != float64(1) {
		t.Fatalf("expected 1 project, got %v", data["projects_count"])
	}
}

func TestMgmt_StatusIncludesBridgeToken(t *testing.T) {
	mgmt, ts, _ := testManagementServer(t, "tok")
	mgmt.SetBridgeServer(NewBridgeServer(9810, "bridge-secret", "/bridge/ws", nil))

	r := mgmtGet(t, ts.URL+"/api/v1/status", "tok")
	if !r.OK {
		t.Fatalf("status failed: %s", r.Error)
	}

	var data struct {
		Bridge struct {
			Enabled bool   `json:"enabled"`
			Port    int    `json:"port"`
			Path    string `json:"path"`
			Token   string `json:"token"`
		} `json:"bridge"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal status data: %v", err)
	}
	if !data.Bridge.Enabled {
		t.Fatal("expected bridge to be enabled")
	}
	if data.Bridge.Token != "bridge-secret" {
		t.Fatalf("expected bridge token, got %q", data.Bridge.Token)
	}
}

func TestMgmt_Projects(t *testing.T) {
	_, ts, _ := testManagementServer(t, "tok")

	r := mgmtGet(t, ts.URL+"/api/v1/projects", "tok")
	if !r.OK {
		t.Fatalf("projects failed: %s", r.Error)
	}

	var data struct {
		Projects []map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal projects data: %v", err)
	}
	if len(data.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(data.Projects))
	}
	if data.Projects[0]["name"] != "test-project" {
		t.Fatalf("expected test-project, got %v", data.Projects[0]["name"])
	}
}

func TestMgmt_ProjectDetail(t *testing.T) {
	_, ts, _ := testManagementServer(t, "tok")

	r := mgmtGet(t, ts.URL+"/api/v1/projects/test-project", "tok")
	if !r.OK {
		t.Fatalf("project detail failed: %s", r.Error)
	}

	var data map[string]any
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal project detail: %v", err)
	}
	if data["name"] != "test-project" {
		t.Fatalf("expected test-project, got %v", data["name"])
	}

	r = mgmtGet(t, ts.URL+"/api/v1/projects/nonexistent", "tok")
	if r.OK {
		t.Fatal("expected 404 for nonexistent project")
	}
}

func TestMgmt_ProjectDetailIncludesPermissionModes(t *testing.T) {
	_, ts, e := testManagementServer(t, "tok")
	e.agent = &stubModelModeAgent{mode: "yolo"}

	r := mgmtGet(t, ts.URL+"/api/v1/projects/test-project", "tok")
	if !r.OK {
		t.Fatalf("project detail failed: %s", r.Error)
	}

	var data struct {
		AgentMode       string `json:"agent_mode"`
		PermissionModes []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"permission_modes"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal project detail: %v", err)
	}
	if data.AgentMode != "yolo" {
		t.Fatalf("agent_mode = %q, want yolo", data.AgentMode)
	}
	if len(data.PermissionModes) != 2 {
		t.Fatalf("permission_modes len = %d, want 2", len(data.PermissionModes))
	}
	if data.PermissionModes[1].Key != "yolo" {
		t.Fatalf("permission_modes[1].key = %q, want yolo", data.PermissionModes[1].Key)
	}
}

func TestMgmt_ProjectCreateAllowsEmptyWorkDir(t *testing.T) {
	mgmt, ts, _ := testManagementServer(t, "tok")

	var gotName, gotDisplayName, gotWorkDir, gotAgentType string
	mgmt.createProject = func(name, displayName, workDir, agentType string) (string, bool, error) {
		gotName = name
		gotDisplayName = displayName
		gotWorkDir = workDir
		gotAgentType = agentType
		return "p-ab12cd34", false, nil
	}

	r := mgmtPost(t, ts.URL+"/api/v1/projects", "tok", map[string]string{
		"display_name": "mobile",
		"agent_type":   "codex",
	})
	if !r.OK {
		t.Fatalf("project create failed: %s", r.Error)
	}
	if gotName != "" {
		t.Fatalf("name = %q, want empty", gotName)
	}
	if gotDisplayName != "mobile" {
		t.Fatalf("displayName = %q, want mobile", gotDisplayName)
	}
	if gotWorkDir != "" {
		t.Fatalf("workDir = %q, want empty", gotWorkDir)
	}
	if gotAgentType != "codex" {
		t.Fatalf("agentType = %q, want codex", gotAgentType)
	}

	var data map[string]any
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal project create response: %v", err)
	}
	if data["name"] != "p-ab12cd34" {
		t.Fatalf("response name = %v, want p-ab12cd34", data["name"])
	}
	if data["display_name"] != "mobile" {
		t.Fatalf("response display_name = %v, want mobile", data["display_name"])
	}
}

func TestMgmt_ProjectCreatePropagatesRestartRequired(t *testing.T) {
	mgmt, ts, _ := testManagementServer(t, "tok")
	mgmt.createProject = func(name, displayName, workDir, agentType string) (string, bool, error) {
		return "p-hotload", false, nil
	}

	r := mgmtPost(t, ts.URL+"/api/v1/projects", "tok", map[string]string{
		"display_name": "hotload",
		"agent_type":   "claudecode",
	})
	if !r.OK {
		t.Fatalf("project create failed: %s", r.Error)
	}

	var data map[string]any
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal project create response: %v", err)
	}
	if data["restart_required"] != false {
		t.Fatalf("restart_required = %v, want false", data["restart_required"])
	}
}

func TestMgmt_FilesystemDirectoriesListAndCreate(t *testing.T) {
	_, ts, _ := testManagementServer(t, "tok")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Beta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}

	reqURL := ts.URL + "/api/v1/filesystem/directories?path=" + url.QueryEscape(root)
	r := mgmtGet(t, reqURL, "tok")
	if !r.OK {
		t.Fatalf("directories failed: %s", r.Error)
	}
	var data struct {
		Path    string `json:"path"`
		Parent  string `json:"parent"`
		Entries []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal directories: %v", err)
	}
	if data.Path != root {
		t.Fatalf("path = %q, want %q", data.Path, root)
	}
	if len(data.Entries) != 2 {
		t.Fatalf("entries = %+v, want two directories", data.Entries)
	}
	if data.Entries[0].Name != "alpha" || data.Entries[1].Name != "Beta" {
		t.Fatalf("entries sorted/filtered incorrectly: %+v", data.Entries)
	}

	r = mgmtPost(t, ts.URL+"/api/v1/filesystem/directories", "tok", map[string]string{
		"parent": root,
		"name":   "created",
	})
	if !r.OK {
		t.Fatalf("create directory failed: %s", r.Error)
	}
	if info, err := os.Stat(filepath.Join(root, "created")); err != nil || !info.IsDir() {
		t.Fatalf("created directory missing or not directory: info=%v err=%v", info, err)
	}

	r = mgmtPost(t, ts.URL+"/api/v1/filesystem/directories", "tok", map[string]string{
		"parent": root,
		"name":   "../bad",
	})
	if r.OK {
		t.Fatal("expected nested/path traversal directory name to be rejected")
	}
}

func TestMgmt_ProjectDetailIncludesDisplayName(t *testing.T) {
	mgmt, ts, e := testManagementServer(t, "tok")
	e.SetDisplayName("Alias")
	mgmt.RegisterEngine("test-project", e)

	r := mgmtGet(t, ts.URL+"/api/v1/projects/test-project", "tok")
	if !r.OK {
		t.Fatalf("detail failed: %s", r.Error)
	}
	var data map[string]any
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal detail data: %v", err)
	}
	if data["display_name"] != "Alias" {
		t.Fatalf("display_name = %v, want Alias", data["display_name"])
	}
}

func TestMgmt_ProjectPatch(t *testing.T) {
	_, ts, _ := testManagementServer(t, "tok")

	r := mgmtPatch(t, ts.URL+"/api/v1/projects/test-project", "tok", map[string]any{
		"language": "zh",
	})
	if !r.OK {
		t.Fatalf("patch failed: %s", r.Error)
	}
}

func TestMgmt_ProjectPlatformDeleteReturnsRestartRequired(t *testing.T) {
	mgmt, ts, _ := testManagementServer(t, "tok")

	var gotProject, gotSelector string
	mgmt.SetRemovePlatformFromProject(func(projectName, selector string) error {
		gotProject = projectName
		gotSelector = selector
		return nil
	})

	r := mgmtDelete(t, ts.URL+"/api/v1/projects/test-project/platforms/0", "tok")
	if !r.OK {
		t.Fatalf("delete platform failed: %s", r.Error)
	}
	if gotProject != "test-project" || gotSelector != "0" {
		t.Fatalf("callback got project=%q selector=%q", gotProject, gotSelector)
	}

	var data struct {
		Message         string `json:"message"`
		Project         string `json:"project"`
		Selector        string `json:"selector"`
		RestartRequired bool   `json:"restart_required"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal delete platform response: %v", err)
	}
	if data.Project != "test-project" || data.Selector != "0" {
		t.Fatalf("response project=%q selector=%q", data.Project, data.Selector)
	}
	if !data.RestartRequired {
		t.Fatal("restart_required = false, want true")
	}
	if !strings.Contains(data.Message, `platform "0" removed`) {
		t.Fatalf("message = %q, want platform removed", data.Message)
	}
}

func TestMgmt_Sessions(t *testing.T) {
	_, ts, e := testManagementServer(t, "tok")

	first := e.sessions.GetOrCreateActive("user1")

	r := mgmtGet(t, ts.URL+"/api/v1/projects/test-project/sessions", "tok")
	if !r.OK {
		t.Fatalf("sessions list failed: %s", r.Error)
	}

	// Create a session via API
	r = mgmtPost(t, ts.URL+"/api/v1/projects/test-project/sessions", "tok", map[string]string{
		"session_key": "user1",
		"name":        "work",
	})
	if !r.OK {
		t.Fatalf("create session failed: %s", r.Error)
	}
	var created struct {
		ID         string `json:"id"`
		SessionKey string `json:"session_key"`
		Name       string `json:"name"`
		CreatedAt  string `json:"created_at"`
		UpdatedAt  string `json:"updated_at"`
	}
	if err := json.Unmarshal(r.Data, &created); err != nil {
		t.Fatalf("unmarshal created session: %v", err)
	}
	if created.ID == "" || created.ID == first.ID {
		t.Fatalf("created id = %q, want non-empty and different from %q", created.ID, first.ID)
	}
	if created.SessionKey != "user1" {
		t.Fatalf("session_key = %q, want user1", created.SessionKey)
	}
	if created.Name != "work" {
		t.Fatalf("name = %q, want work", created.Name)
	}
	if created.CreatedAt == "" || created.UpdatedAt == "" {
		t.Fatalf("timestamps missing: created_at=%q updated_at=%q", created.CreatedAt, created.UpdatedAt)
	}
	if got := e.sessions.ListSessions("user1"); len(got) != 2 {
		t.Fatalf("ListSessions(user1) len = %d, want 2", len(got))
	}
	if active := e.sessions.ActiveSessionID("user1"); active != created.ID {
		t.Fatalf("active session id = %q, want %q", active, created.ID)
	}
}

func TestMgmt_SessionsLiveUsesSessionIDWhenRuntimeKeyIncludesIt(t *testing.T) {
	_, ts, e := testManagementServer(t, "tok")

	first := e.sessions.NewSession("webnew:web-admin:proj", "first")
	second := e.sessions.NewSession("webnew:web-admin:proj", "second")
	p := &stubPlatformEngine{n: "webnew"}
	e.interactiveMu.Lock()
	e.interactiveStates["webnew:web-admin:proj::"+second.ID] = &interactiveState{
		platform: p,
		replyCtx: "ctx",
	}
	e.interactiveMu.Unlock()

	r := mgmtGet(t, ts.URL+"/api/v1/projects/test-project/sessions", "tok")
	if !r.OK {
		t.Fatalf("sessions list failed: %s", r.Error)
	}
	var data struct {
		Sessions []struct {
			ID       string `json:"id"`
			Live     bool   `json:"live"`
			Platform string `json:"platform"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal sessions: %v", err)
	}
	liveByID := make(map[string]bool)
	platformByID := make(map[string]string)
	for _, s := range data.Sessions {
		liveByID[s.ID] = s.Live
		platformByID[s.ID] = s.Platform
	}
	if liveByID[first.ID] {
		t.Fatalf("first session live = true, want false")
	}
	if !liveByID[second.ID] {
		t.Fatalf("second session live = false, want true")
	}
	if platformByID[second.ID] != "webnew" {
		t.Fatalf("second platform = %q, want webnew", platformByID[second.ID])
	}
}

func TestMgmt_SessionDetail(t *testing.T) {
	_, ts, e := testManagementServer(t, "tok")

	s := e.sessions.GetOrCreateActive("user1")
	s.AddHistoryWithImages("user", "hello", []ImageAttachment{{
		MimeType: "image/jpeg",
		Data:     []byte("photo"),
		FileName: "photo.jpg",
	}})
	s.AddHistory("assistant", "hi there")

	r := mgmtGet(t, ts.URL+"/api/v1/projects/test-project/sessions/"+s.ID, "tok")
	if !r.OK {
		t.Fatalf("session detail failed: %s", r.Error)
	}

	var data struct {
		History []map[string]any `json:"history"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal session detail: %v", err)
	}
	if len(data.History) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(data.History))
	}
	images, ok := data.History[0]["images"].([]any)
	if !ok || len(images) != 1 {
		t.Fatalf("history[0].images = %#v, want one image", data.History[0]["images"])
	}
	image, ok := images[0].(map[string]any)
	if !ok {
		t.Fatalf("history[0].images[0] = %#v, want object", images[0])
	}
	if image["mime_type"] != "image/jpeg" {
		t.Fatalf("mime_type = %#v, want image/jpeg", image["mime_type"])
	}
	if image["data"] != "cGhvdG8=" {
		t.Fatalf("data = %#v, want base64 image", image["data"])
	}
	if image["file_name"] != "photo.jpg" {
		t.Fatalf("file_name = %#v, want photo.jpg", image["file_name"])
	}
	if image["size"] != float64(5) {
		t.Fatalf("size = %#v, want 5", image["size"])
	}
}

func TestMgmt_SessionDelete(t *testing.T) {
	_, ts, e := testManagementServer(t, "tok")

	s := e.sessions.GetOrCreateActive("user1")
	sid := s.ID

	r := mgmtDelete(t, ts.URL+"/api/v1/projects/test-project/sessions/"+sid, "tok")
	if !r.OK {
		t.Fatalf("delete session failed: %s", r.Error)
	}

	r = mgmtGet(t, ts.URL+"/api/v1/projects/test-project/sessions/"+sid, "tok")
	if r.OK {
		t.Fatal("expected 404 after deletion")
	}
}

func TestMgmt_SessionPatchRename(t *testing.T) {
	_, ts, e := testManagementServer(t, "tok")
	s := e.sessions.GetOrCreateActive("user1")

	r := mgmtPatch(t, ts.URL+"/api/v1/projects/test-project/sessions/"+s.ID, "tok", map[string]string{
		"name": "手动会话",
	})
	if !r.OK {
		t.Fatalf("patch failed: %s", r.Error)
	}
	if got := s.GetName(); got != "手动会话" {
		t.Fatalf("GetName = %q", got)
	}
	mode, _ := s.AliasInfo()
	if mode != SessionAliasModeManual {
		t.Fatalf("AliasMode = %q", mode)
	}
}

func TestMgmt_SendAcceptsSessionID(t *testing.T) {
	_, ts, e := testManagementServer(t, "tok")

	first := e.sessions.NewSession("webnew:web-admin:proj", "first")
	second := e.sessions.NewSession("webnew:web-admin:proj", "second")
	firstPlatform := &stubPlatformEngine{n: "webnew"}
	secondPlatform := &stubPlatformEngine{n: "webnew"}
	e.platforms = []Platform{firstPlatform, secondPlatform}
	e.interactiveMu.Lock()
	e.interactiveStates["webnew:web-admin:proj::"+first.ID] = &interactiveState{
		platform: firstPlatform,
		replyCtx: "ctx-first",
	}
	e.interactiveStates["webnew:web-admin:proj::"+second.ID] = &interactiveState{
		platform: secondPlatform,
		replyCtx: "ctx-second",
	}
	e.interactiveMu.Unlock()

	r := mgmtPost(t, ts.URL+"/api/v1/projects/test-project/send", "tok", map[string]string{
		"session_id": second.ID,
		"message":    "hello from api",
	})
	if !r.OK {
		t.Fatalf("send failed: %s", r.Error)
	}
	var data struct {
		SessionKey string `json:"session_key"`
		SessionID  string `json:"session_id"`
		Message    string `json:"message"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal send response: %v", err)
	}
	if data.SessionKey != "webnew:web-admin:proj" {
		t.Fatalf("session_key = %q, want webnew:web-admin:proj", data.SessionKey)
	}
	if data.SessionID != second.ID {
		t.Fatalf("session_id = %q, want %q", data.SessionID, second.ID)
	}
	if sent := firstPlatform.getSent(); len(sent) != 0 {
		t.Fatalf("first session sent = %#v, want none", sent)
	}
	if sent := secondPlatform.getSent(); len(sent) != 1 || sent[0] != "hello from api" {
		t.Fatalf("second session sent = %#v, want one API message", sent)
	}
}

func TestMgmt_SendAcceptsImages(t *testing.T) {
	_, ts, e := testManagementServer(t, "tok")

	first := e.sessions.NewSession("webnew:web-admin:proj", "first")
	second := e.sessions.NewSession("webnew:web-admin:proj", "second")
	firstPlatform := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "webnew"}}
	secondPlatform := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "webnew"}}
	e.platforms = []Platform{firstPlatform, secondPlatform}
	e.interactiveMu.Lock()
	e.interactiveStates["webnew:web-admin:proj::"+first.ID] = &interactiveState{
		platform: firstPlatform,
		replyCtx: "ctx-first",
	}
	e.interactiveStates["webnew:web-admin:proj::"+second.ID] = &interactiveState{
		platform: secondPlatform,
		replyCtx: "ctx-second",
	}
	e.interactiveMu.Unlock()

	r := mgmtPost(t, ts.URL+"/api/v1/projects/test-project/send", "tok", map[string]any{
		"session_id": second.ID,
		"images": []map[string]any{{
			"mime_type": "image/png",
			"data":      "aW1n",
			"file_name": "chart.png",
		}},
	})
	if !r.OK {
		t.Fatalf("send failed: %s", r.Error)
	}
	var data struct {
		SessionKey string `json:"session_key"`
		SessionID  string `json:"session_id"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal send response: %v", err)
	}
	if data.SessionKey != "webnew:web-admin:proj" {
		t.Fatalf("session_key = %q, want webnew:web-admin:proj", data.SessionKey)
	}
	if data.SessionID != second.ID {
		t.Fatalf("session_id = %q, want %q", data.SessionID, second.ID)
	}
	if len(firstPlatform.images) != 0 {
		t.Fatalf("first images = %#v, want none", firstPlatform.images)
	}
	if len(secondPlatform.images) != 1 {
		t.Fatalf("second images len = %d, want 1", len(secondPlatform.images))
	}
	if img := secondPlatform.images[0]; img.MimeType != "image/png" || string(img.Data) != "img" || img.FileName != "chart.png" {
		t.Fatalf("second image = %#v, want decoded chart.png", img)
	}
	if sent := secondPlatform.getSent(); len(sent) != 0 {
		t.Fatalf("second text sent = %#v, want none for pure image", sent)
	}
}

func TestMgmt_ProjectPatchSyncsSessionAliases(t *testing.T) {
	mgmt, ts, e := testManagementServer(t, "tok")
	e.SetDisplayName("旧项目")
	mgmt.RegisterEngine("test-project", e)
	key := "user1"
	first := e.sessions.GetOrCreateActive(key)
	e.ensureSessionAlias(first, e.sessions, key, "主会话")
	second := e.sessions.NewSession(key, "")
	e.ensureSessionAlias(second, e.sessions, key, "第二个会话")
	third := e.sessions.NewSession(key, "")
	e.ensureSessionAlias(third, e.sessions, key, "第三个会话")
	third.SetManualName("固定名称")
	e.sessions.Save()

	r := mgmtPatch(t, ts.URL+"/api/v1/projects/test-project", "tok", map[string]string{
		"display_name": "新项目",
	})
	if !r.OK {
		t.Fatalf("project patch failed: %s", r.Error)
	}
	if got := first.GetName(); got != "新项目 · 主会话" {
		t.Fatalf("first GetName = %q", got)
	}
	if got := second.GetName(); got != "新项目 · 第二个会话" {
		t.Fatalf("second GetName = %q", got)
	}
	if got := third.GetName(); got != "固定名称" {
		t.Fatalf("third GetName = %q", got)
	}
}

func TestMgmt_Config(t *testing.T) {
	srv, ts, _ := testManagementServer(t, "tok")

	// Write a temp TOML file and point the server at it
	tmp := t.TempDir()
	cfgPath := tmp + "/config.toml"
	if err := os.WriteFile(cfgPath, []byte("[display]\ntitle = \"test\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	srv.SetConfigFilePath(cfgPath)

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/config", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "title") {
		t.Fatalf("expected TOML content, got: %s", body)
	}
}

func TestMgmt_Reload(t *testing.T) {
	_, ts, e := testManagementServer(t, "tok")

	reloaded := false
	e.configReloadFunc = func() (*ConfigReloadResult, error) {
		reloaded = true
		return &ConfigReloadResult{}, nil
	}

	r := mgmtPost(t, ts.URL+"/api/v1/reload", "tok", nil)
	if !r.OK {
		t.Fatalf("reload failed: %s", r.Error)
	}
	if !reloaded {
		t.Fatal("expected config reload to be triggered")
	}
}

func TestMgmt_BridgeAdapters(t *testing.T) {
	_, ts, _ := testManagementServer(t, "tok")

	r := mgmtGet(t, ts.URL+"/api/v1/bridge/adapters", "tok")
	if !r.OK {
		t.Fatalf("bridge adapters failed: %s", r.Error)
	}
}

func TestMgmt_FrontendAppsCRUDAndPromote(t *testing.T) {
	mgmt, ts, _ := testManagementServer(t, "tok")
	storePath := filepath.Join(t.TempDir(), "frontend_apps.json")
	mgmt.SetFrontendAppRegistry(NewFrontendAppRegistry(storePath))

	r := mgmtPost(t, ts.URL+"/api/v1/apps", "tok", map[string]any{
		"id":      "smallphone",
		"name":    "SmallPhone",
		"project": "smallphone-3e9fc251",
	})
	if !r.OK {
		t.Fatalf("create frontend app failed: %s", r.Error)
	}

	r = mgmtPost(t, ts.URL+"/api/v1/apps/smallphone/slots", "tok", map[string]any{
		"slot":             "stable",
		"url":              "http://100.120.221.72:18080/",
		"api_base":         "http://100.120.221.72:3100/api",
		"adapter_platform": "smallphone",
	})
	if !r.OK {
		t.Fatalf("create stable slot failed: %s", r.Error)
	}

	r = mgmtPost(t, ts.URL+"/api/v1/apps/smallphone/slots", "tok", map[string]any{
		"slot":             "beta",
		"url":              "http://100.120.221.72:18082/",
		"api_base":         "http://100.120.221.72:3100/api",
		"adapter_platform": "smallphone",
	})
	if !r.OK {
		t.Fatalf("create beta slot failed: %s", r.Error)
	}

	r = mgmtPost(t, ts.URL+"/api/v1/apps/smallphone/slots/beta/promote", "tok", map[string]any{
		"target_slot": "stable",
	})
	if !r.OK {
		t.Fatalf("promote beta slot failed: %s", r.Error)
	}

	r = mgmtGet(t, ts.URL+"/api/v1/apps/smallphone", "tok")
	if !r.OK {
		t.Fatalf("get frontend app failed: %s", r.Error)
	}
	var data struct {
		App FrontendApp `json:"app"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal frontend app: %v", err)
	}
	if data.App.ID != "smallphone" || data.App.Project != "smallphone-3e9fc251" {
		t.Fatalf("unexpected app: %+v", data.App)
	}
	stable := data.App.Slots["stable"]
	if stable.URL != "http://100.120.221.72:18082/" {
		t.Fatalf("stable url = %q, want promoted beta url", stable.URL)
	}
	if stable.Metadata["promoted_from"] != "beta" {
		t.Fatalf("stable promoted_from = %q, want beta", stable.Metadata["promoted_from"])
	}
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("registry file was not persisted: %v", err)
	}
}

func TestMgmt_FrontendSlotServiceRegisterHeartbeatAndStatus(t *testing.T) {
	mgmt, ts, _ := testManagementServer(t, "tok")
	storePath := filepath.Join(t.TempDir(), "frontend_apps.json")
	mgmt.SetFrontendAppRegistry(NewFrontendAppRegistry(storePath))

	r := mgmtPost(t, ts.URL+"/api/v1/apps", "tok", map[string]any{
		"id":      "smallphone",
		"name":    "SmallPhone",
		"project": "smallphone-3e9fc251",
	})
	if !r.OK {
		t.Fatalf("create frontend app failed: %s", r.Error)
	}
	r = mgmtPost(t, ts.URL+"/api/v1/apps/smallphone/slots", "tok", map[string]any{
		"slot":     "stable",
		"url":      "http://frontend.example/stable",
		"api_base": "http://frontend.example/api",
	})
	if !r.OK {
		t.Fatalf("create stable slot failed: %s", r.Error)
	}

	r = mgmtGet(t, ts.URL+"/api/v1/apps/smallphone/slots/stable/service", "tok")
	if !r.OK {
		t.Fatalf("get offline service failed: %s", r.Error)
	}
	var serviceData struct {
		Service FrontendServiceState `json:"service"`
	}
	if err := json.Unmarshal(r.Data, &serviceData); err != nil {
		t.Fatalf("unmarshal offline service: %v", err)
	}
	if serviceData.Service.Status != FrontendServiceStatusOffline {
		t.Fatalf("initial service status = %q, want offline", serviceData.Service.Status)
	}

	r = mgmtPost(t, ts.URL+"/api/v1/apps/smallphone/slots/stable/service/register", "tok", map[string]any{
		"service_id":            "frontend-smallphone-stable",
		"url":                   "http://127.0.0.1:18080",
		"api_base":              "http://127.0.0.1:3100/api",
		"version":               "1.0.0",
		"heartbeat_ttl_seconds": 120,
		"metadata": map[string]string{
			"channel": "stable",
		},
	})
	if !r.OK {
		t.Fatalf("register frontend service failed: %s", r.Error)
	}
	if err := json.Unmarshal(r.Data, &serviceData); err != nil {
		t.Fatalf("unmarshal registered service: %v", err)
	}
	if serviceData.Service.Status != FrontendServiceStatusOnline {
		t.Fatalf("registered service status = %q, want online", serviceData.Service.Status)
	}
	if serviceData.Service.ServiceID != "frontend-smallphone-stable" {
		t.Fatalf("service id = %q", serviceData.Service.ServiceID)
	}

	r = mgmtPost(t, ts.URL+"/api/v1/apps/smallphone/slots/stable/service/heartbeat", "tok", map[string]any{
		"service_id": "frontend-smallphone-stable",
		"version":    "1.0.1",
	})
	if !r.OK {
		t.Fatalf("heartbeat frontend service failed: %s", r.Error)
	}
	if err := json.Unmarshal(r.Data, &serviceData); err != nil {
		t.Fatalf("unmarshal heartbeat service: %v", err)
	}
	if serviceData.Service.Version != "1.0.1" {
		t.Fatalf("heartbeat version = %q, want 1.0.1", serviceData.Service.Version)
	}

	r = mgmtGet(t, ts.URL+"/api/v1/apps/smallphone/slots", "tok")
	if !r.OK {
		t.Fatalf("list slots failed: %s", r.Error)
	}
	var slotsData struct {
		Slots []FrontendSlotView `json:"slots"`
	}
	if err := json.Unmarshal(r.Data, &slotsData); err != nil {
		t.Fatalf("unmarshal slots: %v", err)
	}
	if len(slotsData.Slots) != 1 {
		t.Fatalf("slots len = %d, want 1", len(slotsData.Slots))
	}
	if slotsData.Slots[0].Service.Status != FrontendServiceStatusOnline {
		t.Fatalf("slot service status = %q, want online", slotsData.Slots[0].Service.Status)
	}

	r = mgmtGet(t, ts.URL+"/api/v1/status", "tok")
	if !r.OK {
		t.Fatalf("status failed: %s", r.Error)
	}
	var statusData struct {
		FrontendServices []FrontendServiceSummary `json:"frontend_services"`
	}
	if err := json.Unmarshal(r.Data, &statusData); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if len(statusData.FrontendServices) != 1 {
		t.Fatalf("frontend services len = %d, want 1", len(statusData.FrontendServices))
	}
	if statusData.FrontendServices[0].Service.ServiceID != "frontend-smallphone-stable" {
		t.Fatalf("status service id = %q", statusData.FrontendServices[0].Service.ServiceID)
	}

	raw, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read registry file: %v", err)
	}
	if strings.Contains(string(raw), "frontend-smallphone-stable") {
		t.Fatalf("runtime service state was persisted: %s", string(raw))
	}
}

func TestMgmt_FrontendAppsRequireRegistry(t *testing.T) {
	_, ts, _ := testManagementServer(t, "tok")

	r := mgmtGet(t, ts.URL+"/api/v1/apps", "tok")
	if r.OK {
		t.Fatal("expected frontend app registry error")
	}
	if !strings.Contains(r.Error, "not configured") {
		t.Fatalf("error = %q, want not configured", r.Error)
	}
}

func TestMgmt_HeartbeatNotConfigured(t *testing.T) {
	_, ts, _ := testManagementServer(t, "tok")

	r := mgmtGet(t, ts.URL+"/api/v1/projects/test-project/heartbeat", "tok")
	if r.OK {
		var data map[string]any
		if err := json.Unmarshal(r.Data, &data); err != nil {
			t.Fatalf("unmarshal heartbeat data: %v", err)
		}
		// heartbeat scheduler is nil, so we expect service unavailable
	}
}

func TestMgmt_HeartbeatWithScheduler(t *testing.T) {
	mgmt, ts, _ := testManagementServer(t, "tok")
	hs := NewHeartbeatScheduler("")
	mgmt.SetHeartbeatScheduler(hs)

	r := mgmtGet(t, ts.URL+"/api/v1/projects/test-project/heartbeat", "tok")
	if !r.OK {
		t.Fatalf("heartbeat status failed: %s", r.Error)
	}

	var data map[string]any
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("unmarshal heartbeat status: %v", err)
	}
	if data["enabled"] != false {
		t.Fatalf("expected heartbeat disabled, got %v", data["enabled"])
	}
}

func TestMgmt_CronNilScheduler(t *testing.T) {
	_, ts, _ := testManagementServer(t, "tok")

	r := mgmtGet(t, ts.URL+"/api/v1/cron", "tok")
	if r.OK {
		t.Fatal("expected error when cron scheduler is nil")
	}
}

func TestMgmt_CronWithScheduler(t *testing.T) {
	mgmt, ts, e := testManagementServer(t, "tok")
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cs := NewCronScheduler(store)
	cs.RegisterEngine("test-project", e)
	mgmt.SetCronScheduler(cs)

	// List (empty)
	r := mgmtGet(t, ts.URL+"/api/v1/cron", "tok")
	if !r.OK {
		t.Fatalf("cron list failed: %s", r.Error)
	}

	// Add
	r = mgmtPost(t, ts.URL+"/api/v1/cron", "tok", map[string]any{
		"project":     "test-project",
		"session_key": "user1",
		"cron_expr":   "0 9 * * *",
		"prompt":      "hello",
		"description": "test cron",
	})
	if !r.OK {
		t.Fatalf("cron add failed: %s", r.Error)
	}

	var job CronJob
	if err := json.Unmarshal(r.Data, &job); err != nil {
		t.Fatalf("unmarshal cron job: %v", err)
	}
	if job.ID == "" {
		t.Fatal("expected cron job ID")
	}

	// List (should have 1)
	r = mgmtGet(t, ts.URL+"/api/v1/cron", "tok")
	if !r.OK {
		t.Fatalf("cron list failed: %s", r.Error)
	}

	// Delete
	r = mgmtDelete(t, ts.URL+"/api/v1/cron/"+job.ID, "tok")
	if !r.OK {
		t.Fatalf("cron delete failed: %s", r.Error)
	}

	// Delete nonexistent
	r = mgmtDelete(t, ts.URL+"/api/v1/cron/nonexistent", "tok")
	if r.OK {
		t.Fatal("expected 404 for nonexistent cron job")
	}
}

func TestMgmt_CORS(t *testing.T) {
	mgmt := NewManagementServer(0, "", []string{"http://localhost:3000"})
	mgmt.RegisterEngine("p", NewEngine("p", &stubAgent{}, nil, "", LangEnglish))

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", mgmt.wrap(mgmt.handleStatus))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("OPTIONS", ts.URL+"/api/v1/status", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for OPTIONS, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Fatalf("expected CORS origin header, got %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestMgmt_BridgeWebSocketPathProxiesToBridgeServer(t *testing.T) {
	mgmt := NewManagementServer(0, "", []string{"*"})
	mgmt.RegisterEngine("p", NewEngine("p", &stubAgent{}, nil, "", LangEnglish))
	mgmt.SetBridgeServer(NewBridgeServer(9810, "bridge-secret", "/bridge/ws", []string{"*"}))

	mux := http.NewServeMux()
	ts := httptest.NewServer(mgmt.buildHandler(mux))
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/bridge/ws?token=bridge-secret", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected websocket upgrade, got %d", resp.StatusCode)
	}
}

func TestMgmt_BridgeWebSocketPathWorksWhenBridgeServerSetAfterHandlerBuild(t *testing.T) {
	mgmt := NewManagementServer(0, "", []string{"*"})
	mgmt.RegisterEngine("p", NewEngine("p", &stubAgent{}, nil, "", LangEnglish))

	mux := http.NewServeMux()
	ts := httptest.NewServer(mgmt.buildHandler(mux))
	defer ts.Close()

	mgmt.SetBridgeServer(NewBridgeServer(9810, "bridge-secret", "/bridge/ws", []string{"*"}))

	req, _ := http.NewRequest("GET", ts.URL+"/bridge/ws?token=bridge-secret", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected websocket upgrade after late bridge setup, got %d", resp.StatusCode)
	}
}

func TestMgmt_MethodNotAllowed(t *testing.T) {
	_, ts, _ := testManagementServer(t, "tok")

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	var r mgmtResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode method not allowed response: %v", err)
	}
	resp.Body.Close()
}

func TestMgmt_ProjectModel_UsesSwitchModelWithActiveProvider(t *testing.T) {
	agent := &stubModelModeAgent{
		model: "gpt-4.1-mini",
		providers: []ProviderConfig{
			{
				Name:   "openai",
				Model:  "gpt-4.1-mini",
				Models: []ModelOption{{Name: "gpt-4.1", Alias: "gpt"}, {Name: "gpt-4.1-mini", Alias: "mini"}},
			},
		},
		active: "openai",
	}
	e := NewEngine("test-project", agent, nil, "", LangEnglish)
	var savedProvider, savedModel string
	e.SetProviderModelSaveFunc(func(providerName, model string) error {
		savedProvider = providerName
		savedModel = model
		return nil
	})

	mgmt := NewManagementServer(0, "tok", nil)
	mgmt.RegisterEngine("test-project", e)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/", mgmt.wrap(mgmt.handleProjectRoutes))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	r := mgmtPost(t, ts.URL+"/api/v1/projects/test-project/model", "tok", map[string]string{"model": "gpt-4.1"})
	if !r.OK {
		t.Fatalf("update model failed: %s", r.Error)
	}

	if got := agent.GetModel(); got != "gpt-4.1" {
		t.Fatalf("GetModel() = %q, want gpt-4.1", got)
	}
	if got := agent.GetActiveProvider(); got == nil || got.Model != "gpt-4.1" {
		t.Fatalf("active provider model = %#v, want gpt-4.1", got)
	}
	if savedProvider != "openai" || savedModel != "gpt-4.1" {
		t.Fatalf("saved provider/model = %q/%q, want openai/gpt-4.1", savedProvider, savedModel)
	}
}

func TestMgmt_ProjectModel_SavesModelWithoutActiveProvider(t *testing.T) {
	agent := &stubModelModeAgent{
		model: "gpt-4.1-mini",
		providers: []ProviderConfig{
			{
				Name:   "openai",
				Model:  "gpt-4.1-mini",
				Models: []ModelOption{{Name: "gpt-4.1", Alias: "gpt"}, {Name: "gpt-4.1-mini", Alias: "mini"}},
			},
		},
	}
	e := NewEngine("test-project", agent, nil, "", LangEnglish)
	var savedModel string
	var providerSaveCalled bool
	e.SetModelSaveFunc(func(model string) error {
		savedModel = model
		return nil
	})
	e.SetProviderModelSaveFunc(func(providerName, model string) error {
		providerSaveCalled = true
		return nil
	})

	mgmt := NewManagementServer(0, "tok", nil)
	mgmt.RegisterEngine("test-project", e)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/", mgmt.wrap(mgmt.handleProjectRoutes))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	r := mgmtPost(t, ts.URL+"/api/v1/projects/test-project/model", "tok", map[string]string{"model": "gpt-4.1"})
	if !r.OK {
		t.Fatalf("update model failed: %s", r.Error)
	}

	if got := agent.GetModel(); got != "gpt-4.1" {
		t.Fatalf("GetModel() = %q, want gpt-4.1", got)
	}
	if savedModel != "gpt-4.1" {
		t.Fatalf("saved model = %q, want gpt-4.1", savedModel)
	}
	if providerSaveCalled {
		t.Fatal("provider save callback should not be called without active provider")
	}
}

func TestMgmt_ProjectModel_ReturnsErrorWhenModelSaveFails(t *testing.T) {
	agent := &stubModelModeAgent{
		model: "gpt-4.1-mini",
		providers: []ProviderConfig{
			{
				Name:   "openai",
				Model:  "gpt-4.1-mini",
				Models: []ModelOption{{Name: "gpt-4.1", Alias: "gpt"}, {Name: "gpt-4.1-mini", Alias: "mini"}},
			},
		},
	}
	e := NewEngine("test-project", agent, nil, "", LangEnglish)
	e.SetModelSaveFunc(func(model string) error {
		return errors.New("disk full")
	})

	mgmt := NewManagementServer(0, "tok", nil)
	mgmt.RegisterEngine("test-project", e)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/", mgmt.wrap(mgmt.handleProjectRoutes))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	r := mgmtPost(t, ts.URL+"/api/v1/projects/test-project/model", "tok", map[string]string{"model": "gpt-4.1"})
	if r.OK {
		t.Fatal("update model unexpectedly succeeded")
	}
	if !strings.Contains(r.Error, "disk full") {
		t.Fatalf("error = %q, want save failure", r.Error)
	}
	if got := agent.GetModel(); got != "gpt-4.1-mini" {
		t.Fatalf("GetModel() = %q, want unchanged gpt-4.1-mini", got)
	}
}

func TestMgmt_ProjectModels_UsesTimeoutContext(t *testing.T) {
	agent := &deadlineAwareModelAgent{}
	e := NewEngine("test-project", agent, nil, "", LangEnglish)

	mgmt := NewManagementServer(0, "tok", nil)
	mgmt.RegisterEngine("test-project", e)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/", mgmt.wrap(mgmt.handleProjectRoutes))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	r := mgmtGet(t, ts.URL+"/api/v1/projects/test-project/models", "tok")
	if !r.OK {
		t.Fatalf("project models failed: %s", r.Error)
	}
	if !agent.sawDeadline() {
		t.Fatal("AvailableModels context has no deadline; want timeout-bounded context")
	}
}
