package webclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/webclient/internal/store"
)

func TestV1SessionsAndSend_PersistAndDispatch(t *testing.T) {
	t.Parallel()

	const (
		projectName = "proj"
		webToken    = "" // keep open for this test
		mgmtToken   = "mgmt-secret"
		sessionKey  = "webnew:web-admin:proj"
		sessionName = "work"
		upstreamID  = "sess_up_1"
	)

	bridge := startFakeBridge(t, "bridge-secret")
	t.Cleanup(bridge.Server.Close)

	var gotAuth []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectName+"/sessions":
			// Create session (upstream).
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			now := time.Now().UTC()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"data": map[string]any{
					"id":          upstreamID,
					"session_key": body["session_key"],
					"name":        body["name"],
					"created_at":  now,
					"updated_at":  now,
				},
			})
			return

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectName:
			// Project detail used to fill agent_type.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"data": map[string]any{
					"name":       projectName,
					"agent_type": "claudecode",
				},
			})
			return

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/status":
			// Used by adapter bootstrap (bridge config) and proxy passthrough sanity check.
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
			return

		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"ok":false,"error":"not found"}`))
			return
		}
	}))
	t.Cleanup(upstream.Close)

	s, err := NewServer(Options{
		Host:              "",
		Port:              0,
		Token:             webToken,
		DataDir:           t.TempDir(),
		ManagementBaseURL: upstream.URL,
		ManagementToken:   mgmtToken,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if s.adapter == nil {
		t.Fatalf("expected adapter to be configured")
	}
	s.adapter.Start()
	t.Cleanup(s.adapter.Stop)
	{
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.adapter.WaitConnected(ctx); err != nil {
			t.Fatalf("adapter not connected: %v", err)
		}
	}

	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	// 1) Create session via v1 facade.
	createBody := `{"session_key":"` + sessionKey + `","name":"` + sessionName + `"}`
	res, err := ts.Client().Post(ts.URL+"/api/v1/projects/"+projectName+"/sessions", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST create session: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("create status=%d body=%s", res.StatusCode, string(b))
	}
	var created struct {
		OK   bool `json:"ok"`
		Data struct {
			ID         string `json:"id"`
			SessionKey string `json:"session_key"`
			Name       string `json:"name"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if !created.OK {
		t.Fatalf("create ok=false error=%q", created.Error)
	}
	if created.Data.ID != upstreamID {
		t.Fatalf("id=%q want %q", created.Data.ID, upstreamID)
	}

	// 2) List sessions should include created session from local metadata store.
	res2, err := ts.Client().Get(ts.URL + "/api/v1/projects/" + projectName + "/sessions")
	if err != nil {
		t.Fatalf("GET sessions: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res2.Body)
		t.Fatalf("list status=%d body=%s", res2.StatusCode, string(b))
	}
	var listing struct {
		OK   bool `json:"ok"`
		Data struct {
			Sessions []struct {
				ID           string `json:"id"`
				SessionKey   string `json:"session_key"`
				Name         string `json:"name"`
				Platform     string `json:"platform"`
				AgentType    string `json:"agent_type"`
				HistoryCount int    `json:"history_count"`
			} `json:"sessions"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(res2.Body).Decode(&listing); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if !listing.OK {
		t.Fatalf("list ok=false error=%q", listing.Error)
	}
	if len(listing.Data.Sessions) != 1 {
		t.Fatalf("sessions len=%d want 1", len(listing.Data.Sessions))
	}
	if got := listing.Data.Sessions[0].ID; got != upstreamID {
		t.Fatalf("list id=%q want %q", got, upstreamID)
	}
	if got := listing.Data.Sessions[0].Platform; got != "webnew" {
		t.Fatalf("platform=%q want webnew", got)
	}
	if got := listing.Data.Sessions[0].AgentType; got != "claudecode" {
		t.Fatalf("agent_type=%q want claudecode", got)
	}
	if got := listing.Data.Sessions[0].HistoryCount; got != 0 {
		t.Fatalf("history_count=%d want 0", got)
	}

	// 3) Send message: must persist locally then dispatch to handler.
	sendPayload := `{"session_key":"` + sessionKey + `","session_id":"` + upstreamID + `","message":"hello"}`
	res3, err := ts.Client().Post(ts.URL+"/api/v1/projects/"+projectName+"/send", "application/json", strings.NewReader(sendPayload))
	if err != nil {
		t.Fatalf("POST send: %v", err)
	}
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res3.Body)
		t.Fatalf("send status=%d body=%s", res3.StatusCode, string(b))
	}
	select {
	case m := <-bridge.Got:
		if m["type"] != "message" {
			t.Fatalf("bridge msg type=%v", m["type"])
		}
		if m["session_key"] != sessionKey {
			t.Fatalf("bridge session_key=%v", m["session_key"])
		}
		if m["session_id"] != upstreamID {
			t.Fatalf("bridge session_id=%v", m["session_id"])
		}
		if m["content"] != "hello" {
			t.Fatalf("bridge content=%v", m["content"])
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for bridge dispatch")
	}

	// 4) Session detail should include persisted history.
	res4, err := ts.Client().Get(ts.URL + "/api/v1/projects/" + projectName + "/sessions/" + upstreamID + "?history_limit=200")
	if err != nil {
		t.Fatalf("GET session detail: %v", err)
	}
	defer res4.Body.Close()
	if res4.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res4.Body)
		t.Fatalf("detail status=%d body=%s", res4.StatusCode, string(b))
	}
	var detail struct {
		OK   bool `json:"ok"`
		Data struct {
			ID      string `json:"id"`
			History []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"history"`
			HistoryCount int `json:"history_count"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(res4.Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if !detail.OK {
		t.Fatalf("detail ok=false error=%q", detail.Error)
	}
	if detail.Data.ID != upstreamID {
		t.Fatalf("detail id=%q want %q", detail.Data.ID, upstreamID)
	}
	if detail.Data.HistoryCount != 1 {
		t.Fatalf("history_count=%d want 1", detail.Data.HistoryCount)
	}
	if len(detail.Data.History) != 1 || detail.Data.History[0].Role != "user" || detail.Data.History[0].Content != "hello" {
		b, _ := json.Marshal(detail.Data.History)
		t.Fatalf("unexpected history: %s", string(b))
	}

	// 5) Uncovered v1 endpoints should still proxy upstream.
	res5, err := ts.Client().Get(ts.URL + "/api/v1/status")
	if err != nil {
		t.Fatalf("GET status proxy: %v", err)
	}
	defer res5.Body.Close()
	if res5.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res5.Body)
		t.Fatalf("status proxy status=%d body=%s", res5.StatusCode, string(b))
	}

	// Upstream auth headers should all be management token.
	if len(gotAuth) == 0 {
		t.Fatalf("expected upstream calls")
	}
	for _, a := range gotAuth {
		if a != "Bearer "+mgmtToken {
			t.Fatalf("upstream Authorization=%q want Bearer %s", a, mgmtToken)
		}
	}
}

func TestV1ListSessions_ReturnsActiveKeys(t *testing.T) {
	t.Parallel()

	s, err := NewServer(Options{
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	create := func(name string) string {
		body := `{"session_key":"webnew:web-admin:proj","name":"` + name + `"}`
		res, err := ts.Client().Post(ts.URL+"/api/v1/projects/proj/sessions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST create session: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("create status=%d body=%s", res.StatusCode, string(b))
		}
		var env struct {
			OK   bool `json:"ok"`
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
			t.Fatalf("decode create: %v", err)
		}
		if !env.OK || env.Data.ID == "" {
			t.Fatalf("create ok=%v id=%q", env.OK, env.Data.ID)
		}
		return env.Data.ID
	}

	id1 := create("one")
	id2 := create("two")

	switchBody := `{"session_key":"webnew:web-admin:proj","session_id":"` + id2 + `"}`
	resSwitch, err := ts.Client().Post(ts.URL+"/api/v1/projects/proj/sessions/switch", "application/json", strings.NewReader(switchBody))
	if err != nil {
		t.Fatalf("POST switch: %v", err)
	}
	defer resSwitch.Body.Close()
	if resSwitch.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resSwitch.Body)
		t.Fatalf("switch status=%d body=%s", resSwitch.StatusCode, string(b))
	}

	res, err := ts.Client().Get(ts.URL + "/api/v1/projects/proj/sessions")
	if err != nil {
		t.Fatalf("GET sessions: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("list status=%d body=%s", res.StatusCode, string(b))
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Sessions   []any             `json:"sessions"`
			ActiveKeys map[string]string `json:"active_keys"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if !env.OK {
		t.Fatalf("ok=false")
	}
	if env.Data.ActiveKeys["webnew:web-admin:proj"] != id2 {
		t.Fatalf("active_keys[session_key]=%q want %q (id1=%q)", env.Data.ActiveKeys["webnew:web-admin:proj"], id2, id1)
	}
}

func TestV1SendWithImages_PersistsAttachments(t *testing.T) {
	t.Parallel()

	bridge := startFakeBridge(t, "bridge-secret")
	t.Cleanup(bridge.Server.Close)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"data": map[string]any{
					"bridge": map[string]any{
						"enabled": true,
						"port":    bridge.Port,
						"path":    "/bridge/ws",
						"token":   bridge.Token,
					},
				},
			})
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"ok":false,"error":"not found"}`))
			return
		}
	}))
	t.Cleanup(upstream.Close)

	s, err := NewServer(Options{
		DataDir: t.TempDir(),
		// enable external adapter path
		ManagementBaseURL: upstream.URL,
		ManagementToken:   "mgmt-secret",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if s.adapter == nil {
		t.Fatalf("expected adapter to be configured")
	}
	s.adapter.Start()
	t.Cleanup(s.adapter.Stop)
	{
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.adapter.WaitConnected(ctx); err != nil {
			t.Fatalf("adapter not connected: %v", err)
		}
	}

	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	// 1x1 transparent png (base64, tiny)
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQImWNgYGD4DwABBAEAoJ6p3wAAAABJRU5ErkJggg=="
	body := map[string]any{
		"session_key": "webnew:web-admin:proj",
		"session_id":  "sess_img_1",
		"message":     "",
		"images": []map[string]any{
			{"mime_type": "image/png", "data": pngB64, "file_name": "t.png"},
		},
	}
	b, _ := json.Marshal(body)
	res, err := ts.Client().Post(ts.URL+"/api/v1/projects/proj/send", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST send: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(res.Body)
		t.Fatalf("send status=%d body=%s", res.StatusCode, string(rb))
	}
	var sendEnv struct {
		OK   bool `json:"ok"`
		Data struct {
			OutboxID string `json:"outbox_id"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&sendEnv); err != nil {
		t.Fatalf("decode send: %v", err)
	}
	if !sendEnv.OK {
		t.Fatalf("send ok=false error=%q", sendEnv.Error)
	}
	if sendEnv.Data.OutboxID == "" {
		t.Fatalf("missing outbox_id")
	}

	// Detail history should include images with url pointing at /attachments/.
	res2, err := ts.Client().Get(ts.URL + "/api/v1/projects/proj/sessions/sess_img_1?history_limit=50")
	if err != nil {
		t.Fatalf("GET detail: %v", err)
	}
	defer res2.Body.Close()
	var detail struct {
		OK   bool `json:"ok"`
		Data struct {
			History []map[string]any `json:"history"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res2.Body).Decode(&detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !detail.OK {
		t.Fatalf("detail ok=false")
	}
	if len(detail.Data.History) != 1 {
		t.Fatalf("history len=%d want 1", len(detail.Data.History))
	}
	images, _ := detail.Data.History[0]["images"].([]any)
	if len(images) != 1 {
		t.Fatalf("images len=%d want 1", len(images))
	}
	img0, _ := images[0].(map[string]any)
	urlVal, _ := img0["url"].(string)
	if !strings.HasPrefix(urlVal, "/attachments/") {
		t.Fatalf("image url=%q", urlVal)
	}

	// Outbox should be recorded and marked sent in the internal store.
	item, err := s.store.GetOutboxItem("proj", sendEnv.Data.OutboxID)
	if err != nil {
		t.Fatalf("store.GetOutboxItem: %v", err)
	}
	if item.Status != store.OutboxSent {
		t.Fatalf("outbox status=%q want %q", item.Status, store.OutboxSent)
	}
}

func TestV1Sessions_FileAttachmentsInHistoryAndLastMessage(t *testing.T) {
	t.Parallel()

	s, err := NewServer(Options{
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	const (
		project    = "proj"
		sessionID  = "sess_file_1"
		sessionKey = "webnew:web-admin:proj"
	)
	if _, err := s.store.CreateClientSession(project, store.CreateClientSessionInput{
		ID:         sessionID,
		SessionKey: sessionKey,
		Name:       "files",
	}); err != nil {
		t.Fatalf("store.CreateClientSession: %v", err)
	}

	// 1) Append a card/buttons-like message as plain text; REST must preserve content.
	cardContent := `{"type":"card","title":"hello","buttons":[{"text":"A","value":"a"}]}`
	if _, err := s.store.AppendMessage(project, sessionID, store.Message{
		Role:      store.RoleAssistant,
		Content:   cardContent,
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("store.AppendMessage(card): %v", err)
	}

	// 2) Append a user message with a file attachment (this becomes last_message).
	_, att, err := s.storeSaveFile(core.FileAttachment{
		MimeType: "text/plain",
		FileName: "a.txt",
		Data:     []byte("abc"),
	})
	if err != nil {
		t.Fatalf("storeSaveFile: %v", err)
	}
	if _, err := s.store.AppendMessage(project, sessionID, store.Message{
		Role:        store.RoleUser,
		Content:     "see file",
		Timestamp:   time.Now().UTC(),
		Attachments: []store.Attachment{att},
	}); err != nil {
		t.Fatalf("store.AppendMessage(file): %v", err)
	}

	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	// Session list: last_message must include files.
	resList, err := ts.Client().Get(ts.URL + "/api/v1/projects/" + project + "/sessions")
	if err != nil {
		t.Fatalf("GET list: %v", err)
	}
	defer resList.Body.Close()
	if resList.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resList.Body)
		t.Fatalf("list status=%d body=%s", resList.StatusCode, string(b))
	}
	var listEnv struct {
		OK   bool `json:"ok"`
		Data struct {
			Sessions []map[string]any `json:"sessions"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resList.Body).Decode(&listEnv); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if !listEnv.OK {
		t.Fatalf("list ok=false error=%q", listEnv.Error)
	}
	if len(listEnv.Data.Sessions) != 1 {
		t.Fatalf("sessions len=%d want 1", len(listEnv.Data.Sessions))
	}
	last, _ := listEnv.Data.Sessions[0]["last_message"].(map[string]any)
	if last == nil {
		t.Fatalf("missing last_message")
	}
	filesAny, _ := last["files"].([]any)
	if len(filesAny) != 1 {
		t.Fatalf("last_message.files len=%d want 1", len(filesAny))
	}
	f0, _ := filesAny[0].(map[string]any)
	urlVal, _ := f0["url"].(string)
	if !strings.HasPrefix(urlVal, "/attachments/") {
		t.Fatalf("file url=%q", urlVal)
	}

	// Session detail: history must include card content and file attachment urls.
	resDetail, err := ts.Client().Get(ts.URL + "/api/v1/projects/" + project + "/sessions/" + sessionID + "?history_limit=50")
	if err != nil {
		t.Fatalf("GET detail: %v", err)
	}
	defer resDetail.Body.Close()
	if resDetail.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resDetail.Body)
		t.Fatalf("detail status=%d body=%s", resDetail.StatusCode, string(b))
	}
	var detailEnv struct {
		OK   bool `json:"ok"`
		Data struct {
			History []map[string]any `json:"history"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resDetail.Body).Decode(&detailEnv); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if !detailEnv.OK {
		t.Fatalf("detail ok=false error=%q", detailEnv.Error)
	}
	if len(detailEnv.Data.History) != 2 {
		t.Fatalf("history len=%d want 2", len(detailEnv.Data.History))
	}
	if got, _ := detailEnv.Data.History[0]["content"].(string); got != cardContent {
		t.Fatalf("history[0].content=%q want %q", got, cardContent)
	}
	files2, _ := detailEnv.Data.History[1]["files"].([]any)
	if len(files2) != 1 {
		t.Fatalf("history[1].files len=%d want 1", len(files2))
	}
	f2, _ := files2[0].(map[string]any)
	urlVal2, _ := f2["url"].(string)
	if !strings.HasPrefix(urlVal2, "/attachments/") {
		t.Fatalf("history file url=%q", urlVal2)
	}
	if kind, _ := f2["kind"].(string); kind != "file" {
		t.Fatalf("history file kind=%q want file", kind)
	}
}
