package webclient

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestBridgeWSProxy_PersistsAndForwards(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	gotMsgCh := make(chan map[string]any, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bridge/ws" {
			http.NotFound(w, r)
			return
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()

		// frontend_connect
		_, _, err = c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{"type": "register_ack", "ok": true})

		// message
		_, raw, err := c.ReadMessage()
		if err != nil {
			return
		}
		var msg map[string]any
		_ = json.Unmarshal(raw, &msg)
		gotMsgCh <- msg
	}))
	t.Cleanup(upstream.Close)

	s, err := NewServer(Options{
		DataDir:           t.TempDir(),
		ManagementBaseURL: upstream.URL,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/bridge/ws?token=bridge-secret"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	project := "proj"
	sessionKey := "webnew:web-admin:proj"
	sessionID := "sess_ws_1"

	_ = c.WriteJSON(map[string]any{
		"type":                  "frontend_connect",
		"platform":              "stable",
		"slot":                  "stable",
		"app":                   "cc-connect-web",
		"session_key":           sessionKey,
		"transport_session_key": sessionKey,
		"route":                 "stable",
		"project":               project,
		"capabilities":          []string{"text", "image"},
	})

	// register_ack forwarded from upstream
	var ack map[string]any
	if err := c.ReadJSON(&ack); err != nil {
		t.Fatalf("ReadJSON ack: %v", err)
	}
	if ack["type"] != "register_ack" {
		t.Fatalf("ack type=%v", ack["type"])
	}

	_ = c.WriteJSON(map[string]any{
		"type":        "message",
		"msg_id":      "m1",
		"session_key": sessionKey,
		"session_id":  sessionID,
		"user_id":     "web-admin",
		"user_name":   "Web Admin",
		"content":     "hello",
		"reply_ctx":   sessionKey,
		"project":     project,
	})

	select {
	case got := <-gotMsgCh:
		if got["type"] != "message" {
			t.Fatalf("upstream type=%v", got["type"])
		}
		if got["content"] != "hello" {
			t.Fatalf("upstream content=%v", got["content"])
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting upstream message")
	}

	// Local persistence: user message should be stored under sessionID.
	deadline := time.Now().Add(2 * time.Second)
	for {
		msgs, _ := s.store.ReadMessages(project, sessionID)
		if len(msgs) == 1 && msgs[0].Role == "user" && msgs[0].Content == "hello" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected persisted message not found")
		}
		time.Sleep(30 * time.Millisecond)
	}

	// Outbox should be marked sent.
	ob, err := s.store.GetOutboxItem(project, "m1")
	if err != nil {
		t.Fatalf("GetOutboxItem: %v", err)
	}
	if ob.Status != "sent" {
		t.Fatalf("expected outbox sent, got %s", ob.Status)
	}
}

func TestBridgeWSProxy_OutboundUpstreamWriteFailure_CreatesFailedOutbox(t *testing.T) {
	// Avoid sharing an upstream session between tests.
	defaultBridgeHub = newBridgeHub()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	upstreamConnCh := make(chan *websocket.Conn, 1)
	gotMsgCh := make(chan map[string]any, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bridge/ws" {
			http.NotFound(w, r)
			return
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		upstreamConnCh <- c
		defer c.Close()

		// frontend_connect
		_, _, err = c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{"type": "register_ack", "ok": true})

		// Try to read one message (may or may not arrive depending on race).
		_, raw, err := c.ReadMessage()
		if err != nil {
			return
		}
		var msg map[string]any
		_ = json.Unmarshal(raw, &msg)
		gotMsgCh <- msg
	}))
	t.Cleanup(upstream.Close)

	s, err := NewServer(Options{
		DataDir:           t.TempDir(),
		ManagementBaseURL: upstream.URL,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/bridge/ws?token=bridge-secret"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	project := "proj"
	sessionKey := "webnew:web-admin:proj"
	sessionID := "sess_ws_fail"

	_ = c.WriteJSON(map[string]any{
		"type":                  "frontend_connect",
		"platform":              "stable",
		"slot":                  "stable",
		"app":                   "cc-connect-web",
		"session_key":           sessionKey,
		"transport_session_key": sessionKey,
		"route":                 "stable",
		"project":               project,
		"capabilities":          []string{"text", "image"},
	})

	// register_ack forwarded from upstream
	var ack map[string]any
	if err := c.ReadJSON(&ack); err != nil {
		t.Fatalf("ReadJSON ack: %v", err)
	}
	msgID := "ob_fail_1"

	oldHook := bridgeWriteUpstreamOverride
	bridgeWriteUpstreamOverride = func(_ *bridgeUpstreamSession, _ int, raw []byte) (bool, error) {
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			return false, nil
		}
		if msg["type"] == "message" && msg["msg_id"] == msgID {
			return true, errors.New("forced upstream write failure")
		}
		return false, nil
	}
	t.Cleanup(func() { bridgeWriteUpstreamOverride = oldHook })

	_ = c.WriteJSON(map[string]any{
		"type":        "message",
		"msg_id":      msgID,
		"session_key": sessionKey,
		"session_id":  sessionID,
		"user_id":     "web-admin",
		"user_name":   "Web Admin",
		"content":     "hello",
		"reply_ctx":   sessionKey,
		"project":     project,
	})

	// Wait for the local outbox item to appear and be due (failed).
	deadline := time.Now().Add(2 * time.Second)
	for {
		items, err := s.store.ListOutboxDue(time.Now().UTC(), 50)
		if err != nil {
			t.Fatalf("ListOutboxDue: %v", err)
		}
		// Debug hint if we never see it.
		// t.Logf("due items: %#v", items)
		for _, it := range items {
			if it.Project == project && it.ID == msgID && it.Status == "failed" {
				return
			}
		}
		if time.Now().After(deadline) {
			it, gerr := s.store.GetOutboxItem(project, msgID)
			if gerr == nil {
				t.Fatalf("outbox item exists but not due/failed yet: status=%s next=%s last_error=%s", it.Status, it.NextRetryAt.Format(time.RFC3339Nano), it.LastError)
			}
			// If we somehow forwarded, at least assert it didn't get marked sent.
			select {
			case <-gotMsgCh:
			default:
			}
			t.Fatalf("expected failed due outbox item %q not found", msgID)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestBridgeWSProxy_OutboundPersistFailure_NoForward(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	gotCh := make(chan []byte, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bridge/ws" {
			http.NotFound(w, r)
			return
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()

		// frontend_connect
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{"type": "register_ack", "ok": true})

		// message should never arrive if persistence fails.
		_ = c.SetReadDeadline(time.Now().Add(600 * time.Millisecond))
		_, raw, err := c.ReadMessage()
		if err == nil {
			gotCh <- raw
			return
		}
		gotCh <- nil
	}))
	t.Cleanup(upstream.Close)

	s, err := NewServer(Options{
		DataDir:           t.TempDir(),
		ManagementBaseURL: upstream.URL,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/bridge/ws?token=bridge-secret"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	project := "proj"
	sessionKey := "webnew:web-admin:proj"
	sessionID := "sess_ws_2"

	_ = c.WriteJSON(map[string]any{
		"type":                  "frontend_connect",
		"platform":              "stable",
		"slot":                  "stable",
		"app":                   "cc-connect-web",
		"session_key":           sessionKey,
		"transport_session_key": sessionKey,
		"route":                 "stable",
		"project":               project,
		"capabilities":          []string{"text", "image"},
	})

	var ack map[string]any
	if err := c.ReadJSON(&ack); err != nil {
		t.Fatalf("ReadJSON ack: %v", err)
	}
	if ack["type"] != "register_ack" {
		t.Fatalf("ack type=%v", ack["type"])
	}

	// Invalid base64 image should cause persistence error and thus no forward.
	_ = c.WriteJSON(map[string]any{
		"type":        "message",
		"msg_id":      "m_bad",
		"session_key": sessionKey,
		"session_id":  sessionID,
		"user_id":     "web-admin",
		"user_name":   "Web Admin",
		"content":     "hello",
		"reply_ctx":   sessionKey,
		"project":     project,
		"images": []map[string]any{
			{"mime_type": "image/png", "data": "###", "file_name": "bad.png"},
		},
	})

	select {
	case raw := <-gotCh:
		if raw != nil {
			t.Fatalf("unexpected upstream forward: %s", string(raw))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting upstream read")
	}

	// No local persistence should have happened for this failed message.
	msgs, _ := s.store.ReadMessages(project, sessionID)
	if len(msgs) != 0 {
		t.Fatalf("expected 0 persisted messages, got %d", len(msgs))
	}
}

func TestBridgeWSProxy_InboundTypes_PersistedAndForwarded(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	upstreamDone := make(chan struct{})

	const png1x1 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMB/axn8QAAAABJRU5ErkJggg=="
	fileTxt := base64.StdEncoding.EncodeToString([]byte("hello-file"))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bridge/ws" {
			http.NotFound(w, r)
			return
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()

		// frontend_connect
		_, _, err = c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{"type": "register_ack", "ok": true})

		// Wait for outbound user message.
		_, _, err = c.ReadMessage()
		if err != nil {
			return
		}

		// Send a representative set of inbound bridge events.
		_ = c.WriteJSON(map[string]any{
			"type":        "reply",
			"session_key": "sk",
			"session_id":  "sid",
			"reply_ctx":   "sk",
			"content":     "assistant-reply",
		})
		_ = c.WriteJSON(map[string]any{
			"type":        "image",
			"session_key": "sk",
			"session_id":  "sid",
			"reply_ctx":   "sk",
			"content":     "",
			"image": map[string]any{
				"mime_type": "image/png",
				"data":      png1x1,
				"file_name": "one.png",
			},
		})
		_ = c.WriteJSON(map[string]any{
			"type":        "file",
			"session_key": "sk",
			"session_id":  "sid",
			"reply_ctx":   "sk",
			"file": map[string]any{
				"mime_type": "text/plain",
				"data":      fileTxt,
				"file_name": "a.txt",
			},
		})
		_ = c.WriteJSON(map[string]any{
			"type":        "card",
			"session_key": "sk",
			"session_id":  "sid",
			"reply_ctx":   "sk",
			"card":        map[string]any{"header": map[string]any{"title": "T"}},
		})
		_ = c.WriteJSON(map[string]any{
			"type":        "buttons",
			"session_key": "sk",
			"session_id":  "sid",
			"reply_ctx":   "sk",
			"content":     "pick",
			"buttons": [][]map[string]any{
				{
					{"text": "OK", "data": "x"},
				},
			},
		})
		_ = c.WriteJSON(map[string]any{
			"type":        "preview_start",
			"ref_id":      "r1",
			"session_key": "sk",
			"session_id":  "sid",
			"reply_ctx":   "sk",
			"content":     "thinking",
		})
		_ = c.WriteJSON(map[string]any{
			"type":        "reply_stream",
			"session_key": "sk",
			"session_id":  "sid",
			"reply_ctx":   "sk",
			"delta":       "a",
			"full_text":   "a",
			"done":        false,
		})
		_ = c.WriteJSON(map[string]any{
			"type":        "reply_stream",
			"session_key": "sk",
			"session_id":  "sid",
			"reply_ctx":   "sk",
			"delta":       "b",
			"full_text":   "ab",
			"done":        true,
		})
		_ = c.WriteJSON(map[string]any{
			"type":           "update_message",
			"session_key":    "sk",
			"session_id":     "sid",
			"preview_handle": "h1",
			"content":        "upd",
		})
		_ = c.WriteJSON(map[string]any{
			"type":           "delete_message",
			"session_key":    "sk",
			"session_id":     "sid",
			"preview_handle": "h1",
		})

		// Avoid racing TempDir cleanup: keep upstream alive until the test has
		// observed forwarded frames and persistence.
		select {
		case <-upstreamDone:
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(upstream.Close)

	s, err := NewServer(Options{
		DataDir:           t.TempDir(),
		ManagementBaseURL: upstream.URL,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/bridge/ws?token=bridge-secret"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	project := "proj"
	sessionKey := "sk"
	sessionID := "sid"

	_ = c.WriteJSON(map[string]any{
		"type":                  "frontend_connect",
		"platform":              "stable",
		"slot":                  "stable",
		"app":                   "cc-connect-web",
		"session_key":           sessionKey,
		"transport_session_key": sessionKey,
		"route":                 "stable",
		"project":               project,
		"capabilities":          []string{"text", "image", "file", "card", "buttons", "typing", "update_message", "preview"},
	})

	var ack map[string]any
	if err := c.ReadJSON(&ack); err != nil {
		t.Fatalf("ReadJSON ack: %v", err)
	}

	_ = c.WriteJSON(map[string]any{
		"type":        "message",
		"msg_id":      "m1",
		"session_key": sessionKey,
		"session_id":  sessionID,
		"user_id":     "web-admin",
		"user_name":   "Web Admin",
		"content":     "hello",
		"reply_ctx":   sessionKey,
		"project":     project,
	})

	// Read a handful of forwarded inbound messages to ensure the WS proxy still
	// forwards after persisting.
	expected := map[string]bool{
		"reply":          true,
		"image":          true,
		"file":           true,
		"card":           true,
		"buttons":        true,
		"reply_stream":   true,
		"preview_start":  true,
		"update_message": true,
		"delete_message": true,
	}
	gotTypes := make(map[string]bool)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		var m map[string]any
		if err := c.ReadJSON(&m); err != nil {
			break
		}
		if typ, _ := m["type"].(string); typ != "" {
			gotTypes[typ] = true
		}
		all := true
		for k := range expected {
			if !gotTypes[k] {
				all = false
				break
			}
		}
		if all {
			break
		}
	}
	if !gotTypes["reply"] || !gotTypes["image"] || !gotTypes["file"] || !gotTypes["card"] || !gotTypes["buttons"] || !gotTypes["reply_stream"] || !gotTypes["update_message"] || !gotTypes["delete_message"] {
		t.Fatalf("missing forwarded types: %v", gotTypes)
	}

	// Persistence: verify content markers and attachments exist.
	deadline := time.Now().Add(2 * time.Second)
	for {
		msgs, _ := s.store.ReadMessages(project, sessionID)
		if len(msgs) >= 4 {
			var hasReply, hasImage, hasFile, hasCard bool
			for _, m := range msgs {
				if m.Role != "assistant" {
					continue
				}
				if m.Content == "assistant-reply" {
					hasReply = true
				}
				if m.Content == "[image]" && len(m.Attachments) > 0 && m.Attachments[0].URL != "" {
					hasImage = true
				}
				if strings.HasPrefix(m.Content, "[file]") && len(m.Attachments) > 0 && m.Attachments[0].URL != "" {
					hasFile = true
				}
				if strings.HasPrefix(m.Content, "[card]") {
					hasCard = true
				}
			}
			if hasReply && hasImage && hasFile && hasCard {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected persisted assistant messages not found")
		}
		time.Sleep(30 * time.Millisecond)
	}

	close(upstreamDone)
}

func TestBridgeWSProxy_ReplayBuffer_AllowsReconnectToReceiveLateReply(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	upstreamReplySent := make(chan struct{}, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bridge/ws" {
			http.NotFound(w, r)
			return
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()

		// frontend_connect
		_, _, err = c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{"type": "register_ack", "ok": true})

		// Wait for outbound user message.
		_, _, err = c.ReadMessage()
		if err != nil {
			return
		}

		// Send reply after a short delay to simulate "late" delivery after the
		// browser tab is closed.
		time.Sleep(150 * time.Millisecond)
		_ = c.WriteJSON(map[string]any{
			"type":        "reply",
			"session_key": "sk",
			"session_id":  "sid",
			"reply_ctx":   "sk",
			"content":     "late-reply",
		})
		upstreamReplySent <- struct{}{}

		// Keep the connection open for a bit.
		time.Sleep(300 * time.Millisecond)
	}))
	t.Cleanup(upstream.Close)

	s, err := NewServer(Options{
		DataDir:           t.TempDir(),
		ManagementBaseURL: upstream.URL,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/bridge/ws?token=bridge-secret"

	project := "proj"
	sessionKey := "sk"
	sessionID := "sid"

	// Browser-1: connect, send message, then close quickly.
	c1, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial c1: %v", err)
	}
	_ = c1.WriteJSON(map[string]any{
		"type":                  "frontend_connect",
		"platform":              "stable",
		"slot":                  "stable",
		"app":                   "cc-connect-web",
		"session_key":           sessionKey,
		"transport_session_key": sessionKey,
		"route":                 "stable",
		"project":               project,
	})
	var ack1 map[string]any
	_ = c1.ReadJSON(&ack1)
	_ = c1.WriteJSON(map[string]any{
		"type":        "message",
		"msg_id":      "m1",
		"session_key": sessionKey,
		"session_id":  sessionID,
		"user_id":     "web-admin",
		"user_name":   "Web Admin",
		"content":     "hello",
		"reply_ctx":   sessionKey,
		"project":     project,
	})
	_ = c1.Close()

	// Ensure upstream sent the reply while no downstream was connected.
	select {
	case <-upstreamReplySent:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting upstream reply send")
	}

	// Browser-2: reconnect; should receive cached ack + buffered reply.
	c2, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial c2: %v", err)
	}
	defer c2.Close()
	_ = c2.WriteJSON(map[string]any{
		"type":                  "frontend_connect",
		"platform":              "stable",
		"slot":                  "stable",
		"app":                   "cc-connect-web",
		"session_key":           sessionKey,
		"transport_session_key": sessionKey,
		"route":                 "stable",
		"project":               project,
	})

	var ack2 map[string]any
	if err := c2.ReadJSON(&ack2); err != nil {
		t.Fatalf("ReadJSON ack2: %v", err)
	}
	if ack2["type"] != "register_ack" {
		t.Fatalf("ack2 type=%v", ack2["type"])
	}

	var reply map[string]any
	_ = c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := c2.ReadJSON(&reply); err != nil {
		t.Fatalf("ReadJSON reply: %v", err)
	}
	if reply["type"] != "reply" || reply["content"] != "late-reply" {
		t.Fatalf("unexpected reply=%v", reply)
	}

	// Persistence should also have happened.
	msgs, _ := s.store.ReadMessages(project, sessionID)
	found := false
	for _, m := range msgs {
		if m.Role == "assistant" && m.Content == "late-reply" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected persisted late reply")
	}
}
