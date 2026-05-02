package webclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/webclient/internal/store"
)

func TestAdapter_OutboxRecoveryLoop_DeliversDueItems(t *testing.T) {
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
		DataDir:           t.TempDir(),
		ManagementBaseURL: upstream.URL,
		ManagementToken:   "mgmt-secret",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if s.adapter == nil {
		t.Fatalf("expected adapter to be configured")
	}

	// Seed a due outbox item before the adapter becomes available.
	project := "proj"
	sessionKey := "webnew:web-admin:proj"
	sessionID := "sess_outbox_1"
	payload, err := json.Marshal(outboxPayloadV1Send{
		Kind:       outboxPayloadKindV1Send,
		SessionKey: sessionKey,
		SessionID:  sessionID,
		Message:    "hello",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	_, err = s.store.CreateOutboxItem(store.CreateOutboxItemInput{
		ID:          "ob_auto_1",
		Project:     project,
		SessionID:   sessionID,
		SessionKey:  sessionKey,
		Payload:     payload,
		NextRetryAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateOutboxItem: %v", err)
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

	select {
	case msg := <-bridge.Got:
		if msg["type"] != "message" {
			t.Fatalf("bridge got type=%v", msg["type"])
		}
		if msg["msg_id"] != "ob_auto_1" {
			t.Fatalf("bridge msg_id=%v", msg["msg_id"])
		}
		if msg["content"] != "hello" {
			t.Fatalf("bridge content=%v", msg["content"])
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for auto-recovered outbox delivery")
	}

	item, err := s.store.GetOutboxItem(project, "ob_auto_1")
	if err != nil {
		t.Fatalf("GetOutboxItem: %v", err)
	}
	if item.Status != store.OutboxSent {
		t.Fatalf("outbox status=%q want %q", item.Status, store.OutboxSent)
	}
}

