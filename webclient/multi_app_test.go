package webclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/webclient/internal/store"
)

func TestMultiApp_LegacyAndNamespacedRoutesAreIsolated(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "tenant1-crm"},
			{ID: "support", Platform: "web-support", DataNamespace: "tenant1-support"},
		},
		DefaultApp: "crm",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if s.defaultApp == nil || s.defaultApp.appID != "crm" {
		t.Fatalf("defaultApp=%v", s.defaultAppID)
	}
	if got := filepath.Clean(s.apps["crm"].root); !strings.HasSuffix(got, filepath.Join("webclient", "apps", "tenant1-crm")) {
		t.Fatalf("crm root=%q", got)
	}
	if got := filepath.Clean(s.apps["support"].root); !strings.HasSuffix(got, filepath.Join("webclient", "apps", "tenant1-support")) {
		t.Fatalf("support root=%q", got)
	}

	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	// Create one session in default app via legacy route.
	{
		body := `{"session_key":"crm:web:1","name":"crm"}`
		res, err := ts.Client().Post(ts.URL+"/api/v1/projects/proj/sessions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST legacy create session: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("legacy create status=%d body=%s", res.StatusCode, string(b))
		}
	}

	// Create one session in support app via namespaced route.
	{
		body := `{"session_key":"support:web:1","name":"support"}`
		res, err := ts.Client().Post(ts.URL+"/apps/support/api/v1/projects/proj/sessions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST support create session: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("support create status=%d body=%s", res.StatusCode, string(b))
		}
	}

	// Legacy list should only see default app sessions.
	{
		res, err := ts.Client().Get(ts.URL + "/api/v1/projects/proj/sessions")
		if err != nil {
			t.Fatalf("GET legacy list: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("legacy list status=%d body=%s", res.StatusCode, string(b))
		}
		var env struct {
			OK   bool `json:"ok"`
			Data struct {
				Sessions []struct {
					SessionKey string `json:"session_key"`
				} `json:"sessions"`
			} `json:"data"`
		}
		if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
			t.Fatalf("decode legacy list: %v", err)
		}
		if !env.OK || len(env.Data.Sessions) != 1 || env.Data.Sessions[0].SessionKey != "crm:web:1" {
			t.Fatalf("legacy list unexpected: %+v", env)
		}
	}

	// Namespaced list should only see support sessions.
	{
		res, err := ts.Client().Get(ts.URL + "/apps/support/api/v1/projects/proj/sessions")
		if err != nil {
			t.Fatalf("GET support list: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("support list status=%d body=%s", res.StatusCode, string(b))
		}
		var env struct {
			OK   bool `json:"ok"`
			Data struct {
				Sessions []struct {
					SessionKey string `json:"session_key"`
				} `json:"sessions"`
			} `json:"data"`
		}
		if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
			t.Fatalf("decode support list: %v", err)
		}
		if !env.OK || len(env.Data.Sessions) != 1 || env.Data.Sessions[0].SessionKey != "support:web:1" {
			t.Fatalf("support list unexpected: %+v", env)
		}
	}
}

func TestMultiApp_AttachmentsAreNamespacedAndIsolated(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm"},
			{ID: "support", Platform: "web-support", DataNamespace: "support"},
		},
		DefaultApp: "crm",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	crm := s.apps["crm"]
	support := s.apps["support"]
	if crm == nil || support == nil {
		t.Fatalf("expected both apps to exist")
	}

	// Save one attachment into each app store and append a message referencing it.
	crmBytes := []byte{0x89, 0x50, 0x4e, 0x47}
	crmMeta, err := crm.store.SaveAttachment(store.AttachmentMeta{FileName: "a.png", MimeType: "image/png"}, bytes.NewReader(crmBytes))
	if err != nil {
		t.Fatalf("crm SaveAttachment: %v", err)
	}
	_, err = crm.store.AppendMessage("proj", "s1", store.Message{
		Role:    store.RoleAssistant,
		Content: "crm img",
		Attachments: []store.Attachment{
			{ID: crmMeta.ID, Kind: "image", FileName: crmMeta.FileName, MimeType: crmMeta.MimeType, Size: crmMeta.Size},
		},
	})
	if err != nil {
		t.Fatalf("crm AppendMessage: %v", err)
	}

	supBytes := []byte{0x01, 0x02, 0x03}
	supMeta, err := support.store.SaveAttachment(store.AttachmentMeta{FileName: "b.bin", MimeType: "application/octet-stream"}, bytes.NewReader(supBytes))
	if err != nil {
		t.Fatalf("support SaveAttachment: %v", err)
	}
	_, err = support.store.AppendMessage("proj", "s2", store.Message{
		Role:    store.RoleAssistant,
		Content: "support file",
		Attachments: []store.Attachment{
			{ID: supMeta.ID, Kind: "file", FileName: supMeta.FileName, MimeType: supMeta.MimeType, Size: supMeta.Size},
		},
	})
	if err != nil {
		t.Fatalf("support AppendMessage: %v", err)
	}

	// Verify on-disk isolation.
	{
		_, crmPath, err := crm.store.OpenAttachment(crmMeta.ID)
		if err != nil {
			t.Fatalf("crm OpenAttachment: %v", err)
		}
		if !strings.HasPrefix(crmPath, filepath.Join(crm.root, "attachments")) {
			t.Fatalf("crm attachment path=%q want prefix %q", crmPath, filepath.Join(crm.root, "attachments"))
		}
		_, supPath, err := support.store.OpenAttachment(supMeta.ID)
		if err != nil {
			t.Fatalf("support OpenAttachment: %v", err)
		}
		if !strings.HasPrefix(supPath, filepath.Join(support.root, "attachments")) {
			t.Fatalf("support attachment path=%q want prefix %q", supPath, filepath.Join(support.root, "attachments"))
		}
	}

	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	// Messages API should rewrite attachment URLs to the app namespace.
	var crmURL string
	{
		res, err := ts.Client().Get(ts.URL + "/apps/crm/api/projects/proj/sessions/s1/messages")
		if err != nil {
			t.Fatalf("GET crm messages: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("crm messages status=%d body=%s", res.StatusCode, string(b))
		}
		var msgs []struct {
			Attachments []struct {
				URL string `json:"url"`
			} `json:"attachments"`
		}
		if err := json.NewDecoder(res.Body).Decode(&msgs); err != nil {
			t.Fatalf("decode crm messages: %v", err)
		}
		if len(msgs) != 1 || len(msgs[0].Attachments) != 1 {
			t.Fatalf("unexpected crm message shape: %+v", msgs)
		}
		crmURL = msgs[0].Attachments[0].URL
		if !strings.HasPrefix(crmURL, "/apps/crm/attachments/") {
			t.Fatalf("crm attachment url=%q", crmURL)
		}
	}

	// Attachment fetch should succeed via rewritten URL.
	{
		res, err := ts.Client().Get(ts.URL + crmURL)
		if err != nil {
			t.Fatalf("GET crm attachment: %v", err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("crm attachment status=%d body=%s", res.StatusCode, string(b))
		}
		if !bytes.Equal(b, crmBytes) {
			t.Fatalf("crm attachment bytes mismatch")
		}
	}

	// Cross-app fetch should not work.
	{
		res, err := ts.Client().Get(ts.URL + "/apps/support/attachments/" + crmMeta.ID)
		if err != nil {
			t.Fatalf("GET cross-app attachment: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("cross-app status=%d body=%s", res.StatusCode, string(b))
		}
	}
}

func TestMultiApp_DisabledAppsDoNotBlockStartup(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	disabled := false
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm"},
			{Enabled: &disabled}, // placeholder with missing fields
		},
		DefaultApp: "crm",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if s.defaultApp == nil || s.defaultApp.appID != "crm" {
		t.Fatalf("defaultApp=%v", s.defaultAppID)
	}
	if len(s.apps) != 1 {
		t.Fatalf("apps=%d want 1", len(s.apps))
	}
}

func TestMultiApp_DisabledDefaultAppErrors(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	disabled := false
	_, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm"},
			{ID: "support", Platform: "web-support", DataNamespace: "support", Enabled: &disabled},
		},
		DefaultApp: "support",
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "default_app") {
		t.Fatalf("error=%q", err.Error())
	}
}

func TestMultiApp_NamespacedNonV1PostDispatchesToHandlerAndIsolated(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm"},
			{ID: "support", Platform: "web-support", DataNamespace: "support"},
		},
		DefaultApp: "crm",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	support := s.apps["support"]
	crm := s.apps["crm"]
	if support == nil || crm == nil {
		t.Fatalf("expected both apps to exist")
	}

	done := make(chan struct{})
	err = s.Platform("proj").Start(func(p core.Platform, msg *core.Message) {
		_ = p.Send(context.Background(), msg.ReplyCtx, "pong")
		close(done)
	})
	if err != nil {
		t.Fatalf("platform start: %v", err)
	}

	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	res, err := ts.Client().Post(ts.URL+"/apps/support/api/projects/proj/sessions/s1/messages", "application/json", strings.NewReader(`{"content":"ping"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d want %d", res.StatusCode, http.StatusAccepted)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for handler")
	}

	msgs, err := support.store.ReadMessages("proj", "s1")
	if err != nil {
		t.Fatalf("support ReadMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("support msgs=%d want 2", len(msgs))
	}
	if msgs[0].Role != store.RoleUser || msgs[0].Content != "ping" {
		t.Fatalf("support user msg=%+v", msgs[0])
	}
	if msgs[1].Role != store.RoleAssistant || msgs[1].Content != "pong" {
		t.Fatalf("support assistant msg=%+v", msgs[1])
	}

	crmMsgs, err := crm.store.ReadMessages("proj", "s1")
	if err != nil {
		t.Fatalf("crm ReadMessages: %v", err)
	}
	if len(crmMsgs) != 0 {
		t.Fatalf("crm msgs=%d want 0", len(crmMsgs))
	}
}

func TestMultiApp_NamespacedNonV1PostRequiresDeliveryPath(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir: tmp,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm"},
		},
		DefaultApp: "crm",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	res, err := ts.Client().Post(ts.URL+"/apps/crm/api/projects/proj/sessions/s1/messages", "application/json", strings.NewReader(`{"content":"ping"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status=%d want %d body=%s", res.StatusCode, http.StatusServiceUnavailable, string(body))
	}

	msgs, err := s.apps["crm"].store.ReadMessages("proj", "s1")
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("msgs=%d want 0 when no delivery path exists", len(msgs))
	}
}

func TestMultiApp_NamespacedNonV1PostCreatesOutboxForAdapter(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	s, err := NewServer(Options{
		DataDir:           tmp,
		ManagementBaseURL: "http://127.0.0.1:1",
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm"},
		},
		DefaultApp: "crm",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	res, err := ts.Client().Post(ts.URL+"/apps/crm/api/projects/proj/sessions/s1/messages", "application/json", strings.NewReader(`{"content":"ping"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status=%d want %d body=%s", res.StatusCode, http.StatusAccepted, string(body))
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if strings.TrimSpace(body.ID) == "" {
		t.Fatalf("response id is empty")
	}

	rt := s.apps["crm"]
	ob, err := rt.store.GetOutboxItem("proj", body.ID)
	if err != nil {
		t.Fatalf("GetOutboxItem: %v", err)
	}
	if ob.SessionID != "s1" {
		t.Fatalf("SessionID=%q want s1", ob.SessionID)
	}
	if strings.TrimSpace(ob.SessionKey) == "" {
		t.Fatalf("SessionKey is empty")
	}
	if !strings.Contains(string(ob.Payload), `"message":"ping"`) {
		t.Fatalf("payload=%s", string(ob.Payload))
	}
}

func TestMultiApp_AdaptersRegisterDistinctPlatforms(t *testing.T) {
	t.Parallel()

	bridge := startFakeBridge(t, "bridge-secret")
	t.Cleanup(bridge.Server.Close)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"data": map[string]any{
					"name": "management",
					"bridge": map[string]any{
						"enabled": true,
						"port":    bridge.Port,
						"path":    "/bridge/ws",
						"token":   bridge.Token,
					},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"ok":false,"error":"not found"}`))
		}
	}))
	t.Cleanup(upstream.Close)

	s, err := NewServer(Options{
		DataDir:           t.TempDir(),
		ManagementBaseURL: upstream.URL,
		Apps: []AppOptions{
			{ID: "crm", Platform: "web-crm", DataNamespace: "crm"},
			{ID: "support", Platform: "web-support", DataNamespace: "support"},
		},
		DefaultApp: "crm",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Start adapters (no need to start the HTTP server for this test).
	for _, rt := range s.apps {
		if rt.adapter != nil {
			rt.adapter.Start()
			t.Cleanup(rt.adapter.Stop)
		}
	}

	// Expect two registrations with distinct platforms.
	gotPlatforms := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case reg := <-bridge.GotRegister:
			if plat, _ := reg["platform"].(string); strings.TrimSpace(plat) != "" {
				gotPlatforms[strings.TrimSpace(plat)] = true
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for adapter register")
		}
	}
	if !gotPlatforms["web-crm"] || !gotPlatforms["web-support"] {
		t.Fatalf("gotPlatforms=%v", gotPlatforms)
	}
}
