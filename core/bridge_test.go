package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// helpers ------------------------------------------------------------------

func startTestBridge(t *testing.T, token string) (*BridgeServer, string) {
	t.Helper()
	bs := NewBridgeServer(0, token, "/bridge/ws", nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/bridge/ws", bs.handleWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/bridge/ws"
	return bs, wsURL
}

func dialWS(t *testing.T, url string, headers http.Header) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, headers)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func register(t *testing.T, conn *websocket.Conn, platform string, caps []string) {
	t.Helper()
	msg := map[string]any{
		"type":         "register",
		"platform":     platform,
		"capabilities": caps,
	}
	mustWriteJSON(t, conn, msg)
	var ack map[string]any
	mustReadJSON(t, conn, &ack)
	if ack["ok"] != true {
		t.Fatalf("register failed: %v", ack["error"])
	}
}

func registerWithMetadata(t *testing.T, conn *websocket.Conn, platform string, caps []string, metadata map[string]any) {
	t.Helper()
	msg := map[string]any{
		"type":         "register",
		"platform":     platform,
		"capabilities": caps,
		"metadata":     metadata,
	}
	mustWriteJSON(t, conn, msg)
	var ack map[string]any
	mustReadJSON(t, conn, &ack)
	if ack["ok"] != true {
		t.Fatalf("register failed: %v", ack["error"])
	}
}

func frontendConnect(t *testing.T, conn *websocket.Conn, platform, sessionKey, project, clientID string) {
	t.Helper()
	msg := map[string]any{
		"type":         "frontend_connect",
		"platform":     platform,
		"slot":         platform,
		"route":        platform,
		"client_id":    clientID,
		"session_key":  sessionKey,
		"project":      project,
		"capabilities": []string{"text", "card", "buttons", "typing", "update_message", "preview", "reconstruct_reply"},
	}
	mustWriteJSON(t, conn, msg)
	var ack map[string]any
	mustReadJSON(t, conn, &ack)
	if ack["ok"] != true {
		t.Fatalf("frontend connect failed: %v", ack["error"])
	}
	if ack["frontend"] != true {
		t.Fatalf("frontend ack missing frontend marker: %v", ack)
	}
}

func readMsg(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var m map[string]any
	if err := conn.ReadJSON(&m); err != nil {
		t.Fatalf("read message: %v", err)
	}
	return m
}

func readMsgWithin(t *testing.T, conn *websocket.Conn, timeout time.Duration) (map[string]any, error) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var m map[string]any
	if err := conn.ReadJSON(&m); err != nil {
		return nil, err
	}
	return m, nil
}

func mustWriteJSON(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	if err := conn.WriteJSON(v); err != nil {
		t.Fatalf("write JSON: %v", err)
	}
}

func mustReadJSON(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	if err := conn.ReadJSON(v); err != nil {
		t.Fatalf("read JSON: %v", err)
	}
}

func mustDecodeJSON(t *testing.T, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
}

func mustEncodeJSON(t *testing.T, w io.Writer, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode JSON: %v", err)
	}
}

func mustUnmarshalJSON(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal JSON: %v", err)
	}
}

// tests --------------------------------------------------------------------

func TestBridge_RegisterAndConnect(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")

	conn := dialWS(t, wsURL, nil)
	register(t, conn, "test-chat", []string{"text", "buttons"})

	adapters := bs.ConnectedAdapters()
	if len(adapters) != 1 || adapters[0] != "test-chat" {
		t.Fatalf("expected [test-chat], got %v", adapters)
	}
}

func TestBridge_RegisterSendsCapabilitiesSnapshotWhenAdapterSupportsIt(t *testing.T) {
	prevVersion, prevCommit, prevBuildTime := CurrentVersion, CurrentCommit, CurrentBuildTime
	CurrentVersion = "v2.0.0"
	CurrentCommit = "deadbeef"
	CurrentBuildTime = "2026-04-11T00:00:00Z"
	defer func() {
		CurrentVersion = prevVersion
		CurrentCommit = prevCommit
		CurrentBuildTime = prevBuildTime
	}()

	bs, wsURL := startTestBridge(t, "")
	bp := bs.NewPlatform("test-proj")
	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	e.AddCommand("deploy", "Deploy app", "ship it", "", "", "config")
	bs.RegisterEngine("test-proj", e, bp)

	conn := dialWS(t, wsURL, nil)
	registerWithMetadata(t, conn, "bridge", []string{"text"}, map[string]any{
		"control_plane": []string{bridgeCapabilitiesSnapshotProto},
	})

	msg := readMsg(t, conn)
	if msg["type"] != bridgeCapabilitiesSnapshotType {
		t.Fatalf("type = %v, want %q", msg["type"], bridgeCapabilitiesSnapshotType)
	}
	if got := int(msg["v"].(float64)); got != 1 {
		t.Fatalf("v = %d, want 1", got)
	}
	host, ok := msg["host"].(map[string]any)
	if !ok {
		t.Fatalf("host = %T, want object", msg["host"])
	}
	if host["cc_connect_version"] != "v2.0.0" {
		t.Fatalf("cc_connect_version = %v, want %q", host["cc_connect_version"], "v2.0.0")
	}
	projects, ok := msg["projects"].([]any)
	if !ok || len(projects) != 1 {
		t.Fatalf("projects = %T/%d, want 1 project", msg["projects"], len(projects))
	}
	project, ok := projects[0].(map[string]any)
	if !ok {
		t.Fatalf("project = %T, want object", projects[0])
	}
	if project["project"] != "test-proj" {
		t.Fatalf("project name = %v, want %q", project["project"], "test-proj")
	}
	commands, ok := project["commands"].([]any)
	if !ok || len(commands) == 0 {
		t.Fatalf("commands = %T/%d, want non-empty list", project["commands"], len(commands))
	}
	foundDeploy := false
	for _, raw := range commands {
		cmd, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("command = %T, want object", raw)
		}
		if cmd["name"] == "deploy" {
			foundDeploy = true
		}
	}
	if !foundDeploy {
		t.Fatal("expected deploy command in capabilities snapshot")
	}
}

func TestBridge_AuthRequired(t *testing.T) {
	_, wsURL := startTestBridge(t, "secret123")

	// No auth → should fail
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected connection to be rejected")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	// With auth → should succeed
	headers := http.Header{}
	headers.Set("Authorization", "Bearer secret123")
	conn := dialWS(t, wsURL, headers)
	register(t, conn, "authed-chat", []string{"text"})
}

func TestBridge_AuthQueryParam(t *testing.T) {
	_, wsURL := startTestBridge(t, "qtoken")

	conn := dialWS(t, wsURL+"?token=qtoken", nil)
	register(t, conn, "qp-chat", []string{"text"})
}

func TestBridge_RegisterMissingPlatform(t *testing.T) {
	_, wsURL := startTestBridge(t, "")
	conn := dialWS(t, wsURL, nil)

	mustWriteJSON(t, conn, map[string]any{
		"type":         "register",
		"platform":     "",
		"capabilities": []string{"text"},
	})

	var ack map[string]any
	mustReadJSON(t, conn, &ack)
	if ack["ok"] == true {
		t.Fatal("expected registration to fail for empty platform")
	}
}

func TestBridge_MessageRouting(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")

	var received *Message
	var receivedMu sync.Mutex

	bp := bs.NewPlatform("test-proj")

	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	bs.RegisterEngine("test-proj", e, bp)
	bp.handler = func(p Platform, msg *Message) {
		receivedMu.Lock()
		received = msg
		receivedMu.Unlock()
	}

	conn := dialWS(t, wsURL, nil)
	register(t, conn, "mychat", []string{"text"})

	imgData := base64.StdEncoding.EncodeToString([]byte("fakepng"))
	mustWriteJSON(t, conn, map[string]any{
		"type":        "message",
		"msg_id":      "m1",
		"session_key": "mychat:user1:user1",
		"session_id":  "s42",
		"user_id":     "user1",
		"user_name":   "Alice",
		"content":     "hello bridge",
		"reply_ctx":   "conv-1",
		"images":      []map[string]any{{"mime_type": "image/png", "data": imgData, "file_name": "test.png"}},
	})

	time.Sleep(100 * time.Millisecond)

	receivedMu.Lock()
	defer receivedMu.Unlock()
	if received == nil {
		t.Fatal("expected message to be received")
	}
	if received.Content != "hello bridge" {
		t.Fatalf("content = %q, want %q", received.Content, "hello bridge")
	}
	if received.Platform != "mychat" {
		t.Fatalf("platform = %q, want %q", received.Platform, "mychat")
	}
	if received.SessionID != "s42" {
		t.Fatalf("session_id = %q, want s42", received.SessionID)
	}
	if received.UserName != "Alice" {
		t.Fatalf("user_name = %q, want %q", received.UserName, "Alice")
	}
	if len(received.Images) != 1 {
		t.Fatalf("images count = %d, want 1", len(received.Images))
	}
	if received.Images[0].FileName != "test.png" {
		t.Fatalf("image filename = %q, want %q", received.Images[0].FileName, "test.png")
	}
}

func TestBridge_MessageRejectsInvalidImages(t *testing.T) {
	tests := []struct {
		name   string
		images []map[string]any
	}{
		{
			name: "too many",
			images: []map[string]any{
				{"mime_type": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("1"))},
				{"mime_type": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("2"))},
				{"mime_type": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("3"))},
				{"mime_type": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("4"))},
				{"mime_type": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("5"))},
			},
		},
		{
			name: "unsupported mime",
			images: []map[string]any{
				{"mime_type": "image/svg+xml", "data": base64.StdEncoding.EncodeToString([]byte("svg"))},
			},
		},
		{
			name: "too large",
			images: []map[string]any{
				{"mime_type": "image/png", "data": strings.Repeat("a", base64.StdEncoding.EncodedLen(maxImageAttachmentBytes)+4)},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bs, wsURL := startTestBridge(t, "")
			received := make(chan *Message, 1)

			bp := bs.NewPlatform("test-proj")
			e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
			bs.RegisterEngine("test-proj", e, bp)
			bp.handler = func(p Platform, msg *Message) {
				received <- msg
			}

			conn := dialWS(t, wsURL, nil)
			register(t, conn, "mychat", []string{"text"})

			mustWriteJSON(t, conn, map[string]any{
				"type":        "message",
				"msg_id":      "m1",
				"session_key": "mychat:user1:user1",
				"user_id":     "user1",
				"content":     "hello bridge",
				"reply_ctx":   "conv-1",
				"images":      tt.images,
			})

			errMsg := readMsg(t, conn)
			if errMsg["type"] != "error" {
				t.Fatalf("type = %#v, want error", errMsg["type"])
			}
			if errMsg["code"] != "invalid_images" {
				t.Fatalf("code = %#v, want invalid_images", errMsg["code"])
			}
			select {
			case got := <-received:
				t.Fatalf("handler received invalid message: %#v", got)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestBridge_MessageReplyCtxCarriesProgressHints(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")

	gotCh := make(chan *bridgeReplyCtx, 1)

	bp := bs.NewPlatform("test-proj")
	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	bs.RegisterEngine("test-proj", e, bp)
	bp.handler = func(p Platform, msg *Message) {
		rc, ok := msg.ReplyCtx.(*bridgeReplyCtx)
		if !ok {
			t.Fatalf("reply ctx type = %T, want *bridgeReplyCtx", msg.ReplyCtx)
		}
		gotCh <- rc
	}

	conn := dialWS(t, wsURL, nil)
	registerWithMetadata(t, conn, "bridge", []string{"text", "card", "preview", "update_message"}, map[string]any{
		"adapter": "bot-gateway",
	})

	mustWriteJSON(t, conn, map[string]any{
		"type":        "message",
		"msg_id":      "m1",
		"session_key": "bridge:room-1:user-1",
		"user_id":     "user-1",
		"content":     "hello",
		"reply_ctx":   "ctx-1",
	})

	var got *bridgeReplyCtx
	select {
	case got = <-gotCh:
	case <-time.After(5 * time.Second):
		t.Fatal("expected reply ctx to be captured")
	}
	if got.progressStyleHint() != progressStyleCard {
		t.Fatalf("progressStyleHint() = %q, want %q", got.progressStyleHint(), progressStyleCard)
	}
	if !got.supportsProgressCardPayloadHint() {
		t.Fatal("supportsProgressCardPayloadHint() = false, want true")
	}
}

func TestBridge_ReplyRouting(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")

	bp := bs.NewPlatform("test-proj")

	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	bs.RegisterEngine("test-proj", e, bp)
	bp.handler = func(p Platform, msg *Message) {
		if err := p.Reply(context.TODO(), msg.ReplyCtx, "pong"); err != nil {
			t.Fatalf("Reply: %v", err)
		}
	}

	conn := dialWS(t, wsURL, nil)
	register(t, conn, "rc", []string{"text"})

	mustWriteJSON(t, conn, map[string]any{
		"type":        "message",
		"msg_id":      "m1",
		"session_key": "rc:u1:u1",
		"session_id":  "s99",
		"user_id":     "u1",
		"content":     "ping",
		"reply_ctx":   "ctx-1",
	})

	reply := readMsg(t, conn)
	if reply["type"] != "reply" {
		t.Fatalf("type = %q, want reply", reply["type"])
	}
	if reply["content"] != "pong" {
		t.Fatalf("content = %q, want pong", reply["content"])
	}
	if reply["reply_ctx"] != "ctx-1" {
		t.Fatalf("reply_ctx = %q, want ctx-1", reply["reply_ctx"])
	}
	if reply["session_id"] != "s99" {
		t.Fatalf("session_id = %q, want s99", reply["session_id"])
	}
}

func TestBridge_CardActionCommandCarriesSessionID(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")

	gotCh := make(chan *Message, 1)

	bp := bs.NewPlatform("test-proj")
	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	bs.RegisterEngine("test-proj", e, bp)
	bp.handler = func(p Platform, msg *Message) {
		gotCh <- msg
	}

	conn := dialWS(t, wsURL, nil)
	register(t, conn, "webnew", []string{"text", "card"})

	mustWriteJSON(t, conn, map[string]any{
		"type":        "card_action",
		"session_key": "webnew:web-admin:proj",
		"session_id":  "s-cmd",
		"action":      "cmd:/model",
		"reply_ctx":   "ctx-cmd",
	})

	var got *Message
	select {
	case got = <-gotCh:
	case <-time.After(5 * time.Second):
		t.Fatal("expected card action to dispatch as message")
	}
	if got.Content != "/model" {
		t.Fatalf("content = %q, want /model", got.Content)
	}
	if got.SessionID != "s-cmd" {
		t.Fatalf("session_id = %q, want s-cmd", got.SessionID)
	}
	rc, ok := got.ReplyCtx.(*bridgeReplyCtx)
	if !ok {
		t.Fatalf("reply ctx type = %T, want *bridgeReplyCtx", got.ReplyCtx)
	}
	if rc.SessionID != "s-cmd" {
		t.Fatalf("reply ctx session_id = %q, want s-cmd", rc.SessionID)
	}
}

func TestBridge_CardActionNavReplyCarriesSessionID(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")

	gotCh := make(chan string, 1)

	bp := bs.NewPlatform("test-proj")
	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	bs.RegisterEngine("test-proj", e, bp)
	bp.SetCardNavigationHandlerWithSessionID(func(action string, sessionKey string, sessionID string) *Card {
		if action != "nav:/model" {
			t.Fatalf("action = %q, want nav:/model", action)
		}
		if sessionKey != "webnew:web-admin:proj" {
			t.Fatalf("sessionKey = %q, want webnew:web-admin:proj", sessionKey)
		}
		gotCh <- sessionID
		return NewCard().Markdown("updated").Build()
	})

	conn := dialWS(t, wsURL, nil)
	register(t, conn, "webnew", []string{"text", "card"})

	mustWriteJSON(t, conn, map[string]any{
		"type":        "card_action",
		"session_key": "webnew:web-admin:proj",
		"session_id":  "s-nav",
		"action":      "nav:/model",
		"reply_ctx":   "ctx-nav",
	})

	card := readMsg(t, conn)
	if card["type"] != "card" {
		t.Fatalf("type = %q, want card", card["type"])
	}
	if card["session_key"] != "webnew:web-admin:proj" {
		t.Fatalf("session_key = %q, want webnew:web-admin:proj", card["session_key"])
	}
	if card["session_id"] != "s-nav" {
		t.Fatalf("session_id = %q, want s-nav", card["session_id"])
	}
	if card["reply_ctx"] != "ctx-nav" {
		t.Fatalf("reply_ctx = %q, want ctx-nav", card["reply_ctx"])
	}

	select {
	case got := <-gotCh:
		if got != "s-nav" {
			t.Fatalf("handler session_id = %q, want s-nav", got)
		}
	default:
		t.Fatal("expected session-aware nav handler to receive session_id")
	}
}

func TestBridge_CardActionNavFallsBackToLegacyHandler(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")

	gotCh := make(chan struct{}, 1)

	bp := bs.NewPlatform("test-proj")
	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	bs.RegisterEngine("test-proj", e, bp)
	bp.SetCardNavigationHandler(func(action string, sessionKey string) *Card {
		if action != "act:/model opus" {
			t.Fatalf("action = %q, want act:/model opus", action)
		}
		if sessionKey != "webnew:web-admin:proj" {
			t.Fatalf("sessionKey = %q, want webnew:web-admin:proj", sessionKey)
		}
		gotCh <- struct{}{}
		return NewCard().Markdown("legacy").Build()
	})

	conn := dialWS(t, wsURL, nil)
	register(t, conn, "webnew", []string{"text", "card"})

	mustWriteJSON(t, conn, map[string]any{
		"type":        "card_action",
		"session_key": "webnew:web-admin:proj",
		"session_id":  "s-legacy",
		"action":      "act:/model opus",
		"reply_ctx":   "ctx-legacy",
	})

	card := readMsg(t, conn)
	if card["type"] != "card" {
		t.Fatalf("type = %q, want card", card["type"])
	}
	if card["session_id"] != "s-legacy" {
		t.Fatalf("session_id = %q, want s-legacy", card["session_id"])
	}

	select {
	case <-gotCh:
	default:
		t.Fatal("expected legacy nav handler to be called")
	}
}

func TestBridge_ReconstructReplyCtx_RequiresCapability(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")
	bp := bs.NewPlatform("advisor-gemini")

	conn := dialWS(t, wsURL, nil)
	register(t, conn, "bridge", []string{"text"})

	_, err := bp.ReconstructReplyCtx("bridge:1491487450722341088:relay")
	if err == nil || !strings.Contains(err.Error(), "does not support reconstruct_reply") {
		t.Fatalf("ReconstructReplyCtx() error = %v, want reconstruct_reply capability error", err)
	}
}

func TestBridge_ReconstructReplyCtx_UsesStructuredPayload(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")
	bp := bs.NewPlatform("advisor-gemini")

	conn := dialWS(t, wsURL, nil)
	register(t, conn, "bridge", []string{"text", "reconstruct_reply"})

	replyCtx, err := bp.ReconstructReplyCtx("bridge:1491487450722341088:relay")
	if err != nil {
		t.Fatalf("ReconstructReplyCtx() error = %v", err)
	}

	rc, ok := replyCtx.(*bridgeReplyCtx)
	if !ok {
		t.Fatalf("reply ctx type = %T, want *bridgeReplyCtx", replyCtx)
	}
	if rc.Platform != "bridge" {
		t.Fatalf("Platform = %q, want bridge", rc.Platform)
	}
	if rc.SessionKey != "bridge:1491487450722341088:relay" {
		t.Fatalf("SessionKey = %q, want relay session key", rc.SessionKey)
	}

	var payload bridgeReconstructReplyCtxPayload
	if err := json.Unmarshal([]byte(rc.ReplyCtx), &payload); err != nil {
		t.Fatalf("unmarshal reply_ctx: %v", err)
	}
	if payload.Kind != bridgeReconstructReplyCtxKind {
		t.Fatalf("kind = %q, want %q", payload.Kind, bridgeReconstructReplyCtxKind)
	}
	if payload.Version != 1 {
		t.Fatalf("version = %d, want 1", payload.Version)
	}
	if payload.SenderProject != "advisor-gemini" {
		t.Fatalf("sender_project = %q, want advisor-gemini", payload.SenderProject)
	}
	if payload.TransportChatID != "1491487450722341088" {
		t.Fatalf("transport_chat_id = %q, want 1491487450722341088", payload.TransportChatID)
	}
	if payload.TransportSessionKey != "bridge:1491487450722341088:relay" {
		t.Fatalf("transport_session_key = %q, want relay session key", payload.TransportSessionKey)
	}
}

func TestBridge_ReconstructReplyCtx_UsesAdapterProgressHints(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")
	bp := bs.NewPlatform("test-proj")

	conn := dialWS(t, wsURL, nil)
	registerWithMetadata(t, conn, "bridge", []string{"text", "card", "preview", "update_message", "reconstruct_reply"}, map[string]any{
		"adapter": "bot-gateway",
	})

	replyCtx, err := bp.ReconstructReplyCtx("bridge:room-1:user-1")
	if err != nil {
		t.Fatalf("ReconstructReplyCtx() error = %v", err)
	}

	rc, ok := replyCtx.(*bridgeReplyCtx)
	if !ok {
		t.Fatalf("reply ctx type = %T, want *bridgeReplyCtx", replyCtx)
	}
	if rc.progressStyleHint() != progressStyleCard {
		t.Fatalf("progressStyleHint() = %q, want %q", rc.progressStyleHint(), progressStyleCard)
	}
	if !rc.supportsProgressCardPayloadHint() {
		t.Fatal("supportsProgressCardPayloadHint() = false, want true")
	}
}

func TestBridge_CardFallback(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")

	bp := bs.NewPlatform("test-proj")

	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	bs.RegisterEngine("test-proj", e, bp)
	bp.handler = func(p Platform, msg *Message) {
		cs, ok := p.(CardSender)
		if !ok {
			t.Fatal("BridgePlatform should implement CardSender")
		}
		card := NewCard().Title("Test", "blue").Markdown("hello").Build()
		if err := cs.SendCard(context.TODO(), msg.ReplyCtx, card); err != nil {
			t.Fatalf("SendCard: %v", err)
		}
	}

	// Adapter declares NO card capability → should get text fallback
	conn := dialWS(t, wsURL, nil)
	register(t, conn, "nocards", []string{"text"})

	mustWriteJSON(t, conn, map[string]any{
		"type":        "message",
		"msg_id":      "m1",
		"session_key": "nocards:u1:u1",
		"user_id":     "u1",
		"content":     "hi",
		"reply_ctx":   "c1",
	})

	reply := readMsg(t, conn)
	if reply["type"] != "reply" {
		t.Fatalf("expected text fallback, got type=%q", reply["type"])
	}
	content, _ := reply["content"].(string)
	if !strings.Contains(content, "hello") {
		t.Fatalf("fallback should contain 'hello', got %q", content)
	}
}

func TestBridge_SendImage(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")

	bp := bs.NewPlatform("test-proj")
	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	bs.RegisterEngine("test-proj", e, bp)
	errCh := make(chan error, 1)
	bp.handler = func(p Platform, msg *Message) {
		sender, ok := p.(ImageSender)
		if !ok {
			errCh <- fmt.Errorf("BridgePlatform should implement ImageSender")
			return
		}
		errCh <- sender.SendImage(context.TODO(), msg.ReplyCtx, ImageAttachment{
			MimeType: "image/png",
			Data:     []byte("img"),
			FileName: "chart.png",
		})
	}

	conn := dialWS(t, wsURL, nil)
	register(t, conn, "withimages", []string{"text", "image"})

	mustWriteJSON(t, conn, map[string]any{
		"type":        "message",
		"msg_id":      "m1",
		"session_key": "withimages:u1:u1",
		"session_id":  "s1",
		"user_id":     "u1",
		"content":     "send image",
		"reply_ctx":   "c1",
	})

	reply := readMsg(t, conn)
	if err := <-errCh; err != nil {
		t.Fatalf("SendImage: %v", err)
	}
	if reply["type"] != "image" {
		t.Fatalf("type = %#v, want image", reply["type"])
	}
	if reply["session_key"] != "withimages:u1:u1" {
		t.Fatalf("session_key = %#v", reply["session_key"])
	}
	if reply["session_id"] != "s1" {
		t.Fatalf("session_id = %#v", reply["session_id"])
	}
	if reply["reply_ctx"] != "c1" {
		t.Fatalf("reply_ctx = %#v", reply["reply_ctx"])
	}
	image, ok := reply["image"].(map[string]any)
	if !ok {
		t.Fatalf("image = %#v, want object", reply["image"])
	}
	if image["mime_type"] != "image/png" {
		t.Fatalf("mime_type = %#v, want image/png", image["mime_type"])
	}
	if image["data"] != "aW1n" {
		t.Fatalf("data = %#v, want base64 image", image["data"])
	}
	if image["file_name"] != "chart.png" {
		t.Fatalf("file_name = %#v, want chart.png", image["file_name"])
	}
	if image["size"] != float64(3) {
		t.Fatalf("size = %#v, want 3", image["size"])
	}
}

func TestBridge_CardNative(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")

	bp := bs.NewPlatform("test-proj")

	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	bs.RegisterEngine("test-proj", e, bp)
	bp.handler = func(p Platform, msg *Message) {
		cs := p.(CardSender)
		card := NewCard().Title("Test", "blue").Markdown("hello").Build()
		if err := cs.SendCard(context.TODO(), msg.ReplyCtx, card); err != nil {
			t.Fatalf("SendCard: %v", err)
		}
	}

	// Adapter declares card capability → should get card
	conn := dialWS(t, wsURL, nil)
	register(t, conn, "withcards", []string{"text", "card"})

	mustWriteJSON(t, conn, map[string]any{
		"type":        "message",
		"msg_id":      "m1",
		"session_key": "withcards:u1:u1",
		"user_id":     "u1",
		"content":     "hi",
		"reply_ctx":   "c1",
	})

	reply := readMsg(t, conn)
	if reply["type"] != "card" {
		t.Fatalf("expected card, got type=%q", reply["type"])
	}
	cardData, ok := reply["card"].(map[string]any)
	if !ok {
		t.Fatal("card field should be a map")
	}
	header, _ := cardData["header"].(map[string]any)
	if header["title"] != "Test" {
		t.Fatalf("card title = %q, want Test", header["title"])
	}
}

func TestBridge_Ping(t *testing.T) {
	_, wsURL := startTestBridge(t, "")
	conn := dialWS(t, wsURL, nil)
	register(t, conn, "pingtest", []string{"text"})

	mustWriteJSON(t, conn, map[string]any{"type": "ping", "ts": time.Now().UnixMilli()})
	pong := readMsg(t, conn)
	if pong["type"] != "pong" {
		t.Fatalf("expected pong, got %q", pong["type"])
	}
}

func TestBridge_AdapterReplace(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")

	conn1 := dialWS(t, wsURL, nil)
	register(t, conn1, "replaceme", []string{"text"})

	if len(bs.ConnectedAdapters()) != 1 {
		t.Fatal("expected 1 adapter")
	}

	conn2 := dialWS(t, wsURL, nil)
	register(t, conn2, "replaceme", []string{"text", "card"})

	if len(bs.ConnectedAdapters()) != 1 {
		t.Fatal("expected still 1 adapter after replace")
	}

	a := bs.getAdapter("replaceme")
	if !a.capabilities["card"] {
		t.Fatal("replaced adapter should have card capability")
	}
}

func TestBridge_FrontendClientsShareServiceWithoutRegisteringAdapters(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")
	sessionKey := "stable:web-admin:test-proj"

	bp := bs.NewPlatform("test-proj")
	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	bs.RegisterEngine("test-proj", e, bp)
	bp.handler = func(p Platform, msg *Message) {
		rc, ok := msg.ReplyCtx.(*bridgeReplyCtx)
		if !ok {
			t.Errorf("reply ctx type = %T, want *bridgeReplyCtx", msg.ReplyCtx)
			return
		}
		if rc.Platform != "stable" {
			t.Errorf("reply ctx platform = %q, want stable", rc.Platform)
		}
		if rc.ClientID != "tab-1" {
			t.Errorf("reply ctx client_id = %q, want tab-1", rc.ClientID)
		}
		if err := p.Reply(context.TODO(), msg.ReplyCtx, "pong"); err != nil {
			t.Errorf("Reply: %v", err)
		}
	}

	conn1 := dialWS(t, wsURL, nil)
	frontendConnect(t, conn1, "stable", sessionKey, "test-proj", "tab-1")
	conn2 := dialWS(t, wsURL, nil)
	frontendConnect(t, conn2, "stable", sessionKey, "test-proj", "tab-2")

	if adapters := bs.ConnectedAdapters(); len(adapters) != 0 {
		t.Fatalf("frontend clients should not register bridge adapters, got %v", adapters)
	}
	services := bs.ConnectedFrontendServices()
	if len(services) != 1 {
		t.Fatalf("frontend services = %d, want 1", len(services))
	}
	if services[0].Platform != "stable" || services[0].ConnectedClients != 2 {
		t.Fatalf("frontend service snapshot = %+v, want stable with 2 clients", services[0])
	}

	mustWriteJSON(t, conn1, map[string]any{
		"type":        "message",
		"msg_id":      "m1",
		"session_key": sessionKey,
		"session_id":  "s1",
		"user_id":     "web-admin",
		"user_name":   "Web Admin",
		"content":     "ping",
		"reply_ctx":   sessionKey,
		"project":     "test-proj",
	})

	reply := readMsg(t, conn1)
	if reply["type"] != "reply" || reply["content"] != "pong" {
		t.Fatalf("reply = %v, want pong reply", reply)
	}
	if reply["session_key"] != sessionKey {
		t.Fatalf("reply session_key = %q, want %q", reply["session_key"], sessionKey)
	}
	if _, err := readMsgWithin(t, conn2, 150*time.Millisecond); err == nil {
		t.Fatal("second frontend client received targeted first-client reply")
	}
}

func TestBridge_LegacyTabRegisterBecomesFrontendClient(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")
	sessionKey := "beta:web-admin:test-proj"

	bp := bs.NewPlatform("test-proj")
	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	bs.RegisterEngine("test-proj", e, bp)
	bp.handler = func(p Platform, msg *Message) {
		if msg.Platform != "beta" {
			t.Errorf("message platform = %q, want beta", msg.Platform)
		}
		if err := p.Reply(context.TODO(), msg.ReplyCtx, "ok"); err != nil {
			t.Errorf("Reply: %v", err)
		}
	}

	conn := dialWS(t, wsURL, nil)
	registerWithMetadata(t, conn, "beta-tab-abc-page", []string{"text", "card", "buttons"}, map[string]any{
		"route":                 "beta",
		"transport_session_key": sessionKey,
		"project":               "test-proj",
	})

	if adapters := bs.ConnectedAdapters(); len(adapters) != 0 {
		t.Fatalf("legacy tab registration should not create bridge adapters, got %v", adapters)
	}
	services := bs.ConnectedFrontendServices()
	if len(services) != 1 || services[0].Platform != "beta" || services[0].ConnectedClients != 1 {
		t.Fatalf("frontend services = %+v, want one beta service with one client", services)
	}

	mustWriteJSON(t, conn, map[string]any{
		"type":                  "message",
		"msg_id":                "m1",
		"session_key":           sessionKey,
		"transport_session_key": sessionKey,
		"route":                 "beta",
		"user_id":               "web-admin",
		"content":               "hello",
		"reply_ctx":             sessionKey,
		"project":               "test-proj",
	})

	reply := readMsg(t, conn)
	if reply["type"] != "reply" || reply["content"] != "ok" {
		t.Fatalf("reply = %v, want ok reply", reply)
	}
}

func TestBridge_FrontendPreviewAckRoutesThroughService(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")
	sessionKey := "smallphone:web-admin:test-proj"
	errCh := make(chan error, 1)

	bp := bs.NewPlatform("test-proj")
	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	bs.RegisterEngine("test-proj", e, bp)
	bp.handler = func(p Platform, msg *Message) {
		go func() {
			starter := p.(PreviewStarter)
			updater := p.(MessageUpdater)
			handle, err := starter.SendPreviewStart(context.Background(), msg.ReplyCtx, "thinking")
			if err != nil {
				errCh <- err
				return
			}
			if err := updater.UpdateMessage(context.Background(), handle, "updated"); err != nil {
				errCh <- err
				return
			}
			errCh <- nil
		}()
	}

	conn := dialWS(t, wsURL, nil)
	frontendConnect(t, conn, "smallphone", sessionKey, "test-proj", "phone-tab")

	mustWriteJSON(t, conn, map[string]any{
		"type":        "message",
		"msg_id":      "m1",
		"session_key": sessionKey,
		"user_id":     "web-admin",
		"content":     "stream",
		"reply_ctx":   sessionKey,
		"project":     "test-proj",
	})

	start := readMsg(t, conn)
	if start["type"] != "preview_start" {
		t.Fatalf("type = %q, want preview_start", start["type"])
	}
	refID, _ := start["ref_id"].(string)
	if refID == "" {
		t.Fatalf("preview_start missing ref_id: %v", start)
	}
	mustWriteJSON(t, conn, map[string]any{
		"type":           "preview_ack",
		"ref_id":         refID,
		"preview_handle": "preview-1",
	})

	update := readMsg(t, conn)
	if update["type"] != "update_message" {
		t.Fatalf("type = %q, want update_message", update["type"])
	}
	if update["preview_handle"] != "preview-1" || update["content"] != "updated" {
		t.Fatalf("update payload = %v, want preview-1 updated", update)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("preview flow error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for preview flow")
	}
}

func TestBridge_FrontendServicePreservesCardButtonsAndTyping(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")
	sessionKey := "stable:web-admin:test-proj"

	bp := bs.NewPlatform("test-proj")
	e := NewEngine("test-proj", &stubAgent{}, []Platform{bp}, "", LangEnglish)
	bs.RegisterEngine("test-proj", e, bp)
	bp.handler = func(p Platform, msg *Message) {
		stop := p.(TypingIndicator).StartTyping(context.Background(), msg.ReplyCtx)
		stop()
		if err := p.(InlineButtonSender).SendWithButtons(context.Background(), msg.ReplyCtx, "choose", [][]ButtonOption{{
			{Text: "Yes", Data: "cmd:/yes"},
		}}); err != nil {
			t.Errorf("SendWithButtons: %v", err)
			return
		}
		card := NewCard().Title("Card Title", "blue").Markdown("body").Build()
		if err := p.(CardSender).SendCard(context.Background(), msg.ReplyCtx, card); err != nil {
			t.Errorf("SendCard: %v", err)
		}
	}

	conn := dialWS(t, wsURL, nil)
	frontendConnect(t, conn, "stable", sessionKey, "test-proj", "tab-card")

	mustWriteJSON(t, conn, map[string]any{
		"type":        "message",
		"msg_id":      "m1",
		"session_key": sessionKey,
		"user_id":     "web-admin",
		"content":     "interactive",
		"reply_ctx":   sessionKey,
		"project":     "test-proj",
	})

	typingStart := readMsg(t, conn)
	if typingStart["type"] != "typing_start" {
		t.Fatalf("type = %q, want typing_start", typingStart["type"])
	}
	typingStop := readMsg(t, conn)
	if typingStop["type"] != "typing_stop" {
		t.Fatalf("type = %q, want typing_stop", typingStop["type"])
	}
	buttons := readMsg(t, conn)
	if buttons["type"] != "buttons" || buttons["content"] != "choose" {
		t.Fatalf("buttons payload = %v, want buttons choose", buttons)
	}
	cardMsg := readMsg(t, conn)
	if cardMsg["type"] != "card" {
		t.Fatalf("type = %q, want card", cardMsg["type"])
	}
	cardData, ok := cardMsg["card"].(map[string]any)
	if !ok {
		t.Fatalf("card = %T, want object", cardMsg["card"])
	}
	header, _ := cardData["header"].(map[string]any)
	if header["title"] != "Card Title" {
		t.Fatalf("card title = %v, want Card Title", header["title"])
	}
}

func TestBridge_ReconstructReplyCtxFindsFrontendServiceForLogicalSessionKey(t *testing.T) {
	bs, wsURL := startTestBridge(t, "")
	logicalSessionKey := "webnew:web-admin:test-proj"

	bp := bs.NewPlatform("test-proj")
	conn := dialWS(t, wsURL, nil)
	frontendConnect(t, conn, "stable", logicalSessionKey, "test-proj", "tab-logical")

	replyCtx, err := bp.ReconstructReplyCtx(logicalSessionKey)
	if err != nil {
		t.Fatalf("ReconstructReplyCtx: %v", err)
	}
	rc, ok := replyCtx.(*bridgeReplyCtx)
	if !ok {
		t.Fatalf("reply ctx type = %T, want *bridgeReplyCtx", replyCtx)
	}
	if rc.Platform != "stable" {
		t.Fatalf("reply ctx platform = %q, want stable", rc.Platform)
	}

	if err := bp.Reply(context.Background(), replyCtx, "proactive"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	reply := readMsg(t, conn)
	if reply["type"] != "reply" || reply["content"] != "proactive" {
		t.Fatalf("reply = %v, want proactive reply", reply)
	}
	if reply["session_key"] != logicalSessionKey {
		t.Fatalf("reply session_key = %q, want %q", reply["session_key"], logicalSessionKey)
	}
}

func TestSerializeCard(t *testing.T) {
	card := NewCard().
		Title("Model", "blue").
		Markdown("Choose:").
		Buttons(PrimaryBtn("GPT-4", "cmd:/model switch gpt-4"), DefaultBtn("Claude", "cmd:/model switch claude")).
		Divider().
		Note("tip").
		Build()

	result := serializeCard(card)

	header, _ := result["header"].(map[string]string)
	if header["title"] != "Model" || header["color"] != "blue" {
		t.Fatalf("header = %v", header)
	}

	elements, _ := result["elements"].([]map[string]any)
	if len(elements) != 4 {
		t.Fatalf("elements count = %d, want 4", len(elements))
	}
	if elements[0]["type"] != "markdown" {
		t.Fatalf("first element type = %q", elements[0]["type"])
	}
	if elements[1]["type"] != "actions" {
		t.Fatalf("second element type = %q", elements[1]["type"])
	}
	if elements[2]["type"] != "divider" {
		t.Fatalf("third element type = %q", elements[2]["type"])
	}
	if elements[3]["type"] != "note" {
		t.Fatalf("fourth element type = %q", elements[3]["type"])
	}

	btns, _ := elements[1]["buttons"].([]map[string]any)
	if len(btns) != 2 {
		t.Fatalf("buttons count = %d", len(btns))
	}
	if btns[0]["text"] != "GPT-4" || btns[0]["value"] != "cmd:/model switch gpt-4" {
		t.Fatalf("button[0] = %v", btns[0])
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("serialized card is empty")
	}
}

// ---------------------------------------------------------------------------
// Session Management REST API tests
// ---------------------------------------------------------------------------

// startTestBridgeWithREST creates a bridge server with both WS and REST endpoints.
func startTestBridgeWithREST(t *testing.T, token string) (*BridgeServer, string) {
	t.Helper()
	bs := NewBridgeServer(0, token, "/bridge/ws", nil)

	agent := &stubAgent{}
	sm := NewSessionManager("")
	engine := NewEngine("test-proj", agent, nil, "", LangEnglish)
	engine.sessions = sm

	bp := bs.NewPlatform("test-proj")
	bs.RegisterEngine("test-proj", engine, bp)

	mux := http.NewServeMux()
	mux.HandleFunc("/bridge/ws", bs.handleWS)
	mux.HandleFunc("/bridge/sessions", bs.authHTTP(bs.handleSessions))
	mux.HandleFunc("/bridge/sessions/", bs.authHTTP(bs.handleSessionRoutes))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return bs, srv.URL
}

type bridgeAPIResponse struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

func bridgeGet(t *testing.T, url, token string) bridgeAPIResponse {
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
	var r bridgeAPIResponse
	mustDecodeJSON(t, resp.Body, &r)
	return r
}

func bridgePost(t *testing.T, url, token string, body any) bridgeAPIResponse {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		mustEncodeJSON(t, &buf, body)
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
	var r bridgeAPIResponse
	mustDecodeJSON(t, resp.Body, &r)
	return r
}

func bridgeDel(t *testing.T, url, token string) bridgeAPIResponse {
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
	var r bridgeAPIResponse
	mustDecodeJSON(t, resp.Body, &r)
	return r
}

func TestBridge_SessionList(t *testing.T) {
	_, baseURL := startTestBridgeWithREST(t, "tok")

	// List sessions for a new key — should create a default session
	r := bridgeGet(t, baseURL+"/bridge/sessions?session_key=test:u1:u1&token=tok", "")
	if !r.OK {
		t.Logf("no sessions yet: %s", r.Error)
	}

	// Create a session first
	r = bridgePost(t, baseURL+"/bridge/sessions", "tok", map[string]string{
		"session_key": "test:u1:u1",
		"name":        "work",
	})
	if !r.OK {
		t.Fatalf("create session failed: %s", r.Error)
	}
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	mustUnmarshalJSON(t, r.Data, &created)
	if created.ID == "" {
		t.Fatal("expected session ID")
	}
	if created.Name != "work" {
		t.Fatalf("expected name 'work', got %q", created.Name)
	}

	// Now list — should have 1 session
	r = bridgeGet(t, baseURL+"/bridge/sessions?session_key=test:u1:u1", "tok")
	if !r.OK {
		t.Fatalf("list sessions failed: %s", r.Error)
	}
	var listData struct {
		Sessions []map[string]any `json:"sessions"`
	}
	mustUnmarshalJSON(t, r.Data, &listData)
	if len(listData.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(listData.Sessions))
	}
}

func TestBridge_SessionCreateAndDetail(t *testing.T) {
	_, baseURL := startTestBridgeWithREST(t, "tok")

	// Create
	r := bridgePost(t, baseURL+"/bridge/sessions", "tok", map[string]string{
		"session_key": "test:u1:u1",
		"name":        "dev",
	})
	if !r.OK {
		t.Fatalf("create failed: %s", r.Error)
	}
	var created struct {
		ID         string `json:"id"`
		SessionKey string `json:"session_key"`
		Name       string `json:"name"`
		CreatedAt  string `json:"created_at"`
		UpdatedAt  string `json:"updated_at"`
	}
	mustUnmarshalJSON(t, r.Data, &created)
	if created.SessionKey != "test:u1:u1" {
		t.Fatalf("session_key = %q, want test:u1:u1", created.SessionKey)
	}
	if created.Name != "dev" {
		t.Fatalf("name = %q, want dev", created.Name)
	}
	if created.CreatedAt == "" || created.UpdatedAt == "" {
		t.Fatalf("timestamps missing: created_at=%q updated_at=%q", created.CreatedAt, created.UpdatedAt)
	}

	// Get detail
	r = bridgeGet(t, baseURL+"/bridge/sessions/"+created.ID+"?session_key=test:u1:u1", "tok")
	if !r.OK {
		t.Fatalf("get detail failed: %s", r.Error)
	}
	var detail struct {
		ID         string           `json:"id"`
		SessionKey string           `json:"session_key"`
		Name       string           `json:"name"`
		CreatedAt  string           `json:"created_at"`
		UpdatedAt  string           `json:"updated_at"`
		History    []map[string]any `json:"history"`
	}
	mustUnmarshalJSON(t, r.Data, &detail)
	if detail.ID != created.ID {
		t.Fatalf("expected id %q, got %q", created.ID, detail.ID)
	}
	if detail.SessionKey != "test:u1:u1" {
		t.Fatalf("session_key = %q, want test:u1:u1", detail.SessionKey)
	}
	if detail.Name != "dev" {
		t.Fatalf("expected name 'dev', got %q", detail.Name)
	}
	if detail.CreatedAt == "" || detail.UpdatedAt == "" {
		t.Fatalf("detail timestamps missing: created_at=%q updated_at=%q", detail.CreatedAt, detail.UpdatedAt)
	}
}

func TestBridge_SessionDetailIncludesImages(t *testing.T) {
	bs, baseURL := startTestBridgeWithREST(t, "tok")

	bs.enginesMu.RLock()
	ref := bs.engines["test-proj"]
	bs.enginesMu.RUnlock()
	if ref == nil {
		t.Fatal("test engine not registered")
	}

	s := ref.engine.sessions.NewSession("test:u1:u1", "images")
	s.AddHistoryWithImages("user", "attached", []ImageAttachment{{
		MimeType: "image/webp",
		Data:     []byte("webp"),
		FileName: "mock.webp",
	}})

	r := bridgeGet(t, baseURL+"/bridge/sessions/"+s.ID+"?session_key=test:u1:u1", "tok")
	if !r.OK {
		t.Fatalf("get detail failed: %s", r.Error)
	}
	var detail struct {
		History []map[string]any `json:"history"`
	}
	mustUnmarshalJSON(t, r.Data, &detail)
	if len(detail.History) != 1 {
		t.Fatalf("history len = %d, want 1", len(detail.History))
	}
	images, ok := detail.History[0]["images"].([]any)
	if !ok || len(images) != 1 {
		t.Fatalf("history[0].images = %#v, want one image", detail.History[0]["images"])
	}
	image, ok := images[0].(map[string]any)
	if !ok {
		t.Fatalf("history[0].images[0] = %#v, want object", images[0])
	}
	if image["mime_type"] != "image/webp" {
		t.Fatalf("mime_type = %#v, want image/webp", image["mime_type"])
	}
	if image["data"] != "d2VicA==" {
		t.Fatalf("data = %#v, want base64 image", image["data"])
	}
	if image["file_name"] != "mock.webp" {
		t.Fatalf("file_name = %#v, want mock.webp", image["file_name"])
	}
	if image["size"] != float64(4) {
		t.Fatalf("size = %#v, want 4", image["size"])
	}
}

func TestBridge_SessionCreateSameKeyCreatesDistinctSessions(t *testing.T) {
	_, baseURL := startTestBridgeWithREST(t, "tok")

	first := bridgePost(t, baseURL+"/bridge/sessions", "tok", map[string]string{
		"session_key": "test:u1:u1",
		"name":        "first",
	})
	if !first.OK {
		t.Fatalf("create first failed: %s", first.Error)
	}
	second := bridgePost(t, baseURL+"/bridge/sessions", "tok", map[string]string{
		"session_key": "test:u1:u1",
		"name":        "second",
	})
	if !second.OK {
		t.Fatalf("create second failed: %s", second.Error)
	}
	var a, b struct {
		ID string `json:"id"`
	}
	mustUnmarshalJSON(t, first.Data, &a)
	mustUnmarshalJSON(t, second.Data, &b)
	if a.ID == "" || b.ID == "" || a.ID == b.ID {
		t.Fatalf("expected distinct session ids, got first=%q second=%q", a.ID, b.ID)
	}

	list := bridgeGet(t, baseURL+"/bridge/sessions?session_key=test:u1:u1", "tok")
	if !list.OK {
		t.Fatalf("list sessions failed: %s", list.Error)
	}
	var listData struct {
		Sessions []map[string]any `json:"sessions"`
	}
	mustUnmarshalJSON(t, list.Data, &listData)
	if len(listData.Sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(listData.Sessions))
	}
}

func TestBridge_SessionDelete(t *testing.T) {
	_, baseURL := startTestBridgeWithREST(t, "tok")

	r := bridgePost(t, baseURL+"/bridge/sessions", "tok", map[string]string{
		"session_key": "test:u1:u1",
		"name":        "temp",
	})
	if !r.OK {
		t.Fatalf("create failed: %s", r.Error)
	}
	var created struct {
		ID string `json:"id"`
	}
	mustUnmarshalJSON(t, r.Data, &created)

	// Delete
	r = bridgeDel(t, baseURL+"/bridge/sessions/"+created.ID+"?session_key=test:u1:u1", "tok")
	if !r.OK {
		t.Fatalf("delete failed: %s", r.Error)
	}

	// Verify deleted
	r = bridgeGet(t, baseURL+"/bridge/sessions/"+created.ID+"?session_key=test:u1:u1", "tok")
	if r.OK {
		t.Fatal("expected 404 after deletion")
	}
}

func TestBridge_SessionSwitch(t *testing.T) {
	_, baseURL := startTestBridgeWithREST(t, "tok")

	// Create two sessions
	r := bridgePost(t, baseURL+"/bridge/sessions", "tok", map[string]string{
		"session_key": "test:u1:u1",
		"name":        "first",
	})
	if !r.OK {
		t.Fatalf("create first failed: %s", r.Error)
	}

	r = bridgePost(t, baseURL+"/bridge/sessions", "tok", map[string]string{
		"session_key": "test:u1:u1",
		"name":        "second",
	})
	if !r.OK {
		t.Fatalf("create second failed: %s", r.Error)
	}
	var second struct {
		ID string `json:"id"`
	}
	mustUnmarshalJSON(t, r.Data, &second)

	// Switch to second
	r = bridgePost(t, baseURL+"/bridge/sessions/switch", "tok", map[string]string{
		"session_key": "test:u1:u1",
		"session_id":  second.ID,
	})
	if !r.OK {
		t.Fatalf("switch failed: %s", r.Error)
	}
	var switched struct {
		ActiveSessionID string `json:"active_session_id"`
	}
	mustUnmarshalJSON(t, r.Data, &switched)
	if switched.ActiveSessionID != second.ID {
		t.Fatalf("expected active=%s, got %s", second.ID, switched.ActiveSessionID)
	}
}

func TestBridge_SessionAuthRequired(t *testing.T) {
	_, baseURL := startTestBridgeWithREST(t, "secret")

	r := bridgeGet(t, baseURL+"/bridge/sessions?session_key=test:u1:u1", "")
	if r.OK {
		t.Fatal("expected auth failure without token")
	}

	r = bridgeGet(t, baseURL+"/bridge/sessions?session_key=test:u1:u1", "secret")
	if !r.OK {
		t.Fatalf("expected success with token, got: %s", r.Error)
	}
}

func TestBridge_SessionMissingParams(t *testing.T) {
	_, baseURL := startTestBridgeWithREST(t, "tok")

	// Missing session_key
	r := bridgeGet(t, baseURL+"/bridge/sessions", "tok")
	if r.OK {
		t.Fatal("expected error without session_key")
	}

	// Missing session_key in POST
	r = bridgePost(t, baseURL+"/bridge/sessions", "tok", map[string]string{
		"name": "test",
	})
	if r.OK {
		t.Fatal("expected error without session_key in POST")
	}

	// Missing params in switch
	r = bridgePost(t, baseURL+"/bridge/sessions/switch", "tok", map[string]string{
		"session_key": "test:u1:u1",
	})
	if r.OK {
		t.Fatal("expected error without target in switch")
	}
}
