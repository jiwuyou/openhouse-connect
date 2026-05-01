package webclient

import (
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

func TestV1Send_UsesInternalStoreOutbox_Recovery(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()

	// First boot: no upstream configured and no in-process handler, so /send fails
	// but must persist a durable outbox item.
	s1, err := NewServer(Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServer(1): %v", err)
	}
	ts1 := httptest.NewServer(s1.handler)
	t.Cleanup(ts1.Close)

	createBody := `{"session_key":"webnew:web-admin:proj","name":"work"}`
	resCreate, err := ts1.Client().Post(ts1.URL+"/api/v1/projects/proj/sessions", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST create: %v", err)
	}
	defer resCreate.Body.Close()
	if resCreate.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resCreate.Body)
		t.Fatalf("create status=%d body=%s", resCreate.StatusCode, string(b))
	}
	var created struct {
		OK   bool `json:"ok"`
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resCreate.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if !created.OK || created.Data.ID == "" {
		t.Fatalf("create ok=%v id=%q err=%q", created.OK, created.Data.ID, created.Error)
	}

	sendBody := `{"session_key":"webnew:web-admin:proj","session_id":"` + created.Data.ID + `","message":"hello"}`
	resSend, err := ts1.Client().Post(ts1.URL+"/api/v1/projects/proj/send", "application/json", strings.NewReader(sendBody))
	if err != nil {
		t.Fatalf("POST send: %v", err)
	}
	defer resSend.Body.Close()
	if resSend.StatusCode != http.StatusServiceUnavailable {
		b, _ := io.ReadAll(resSend.Body)
		t.Fatalf("send status=%d want %d body=%s", resSend.StatusCode, http.StatusServiceUnavailable, string(b))
	}

	// Confirm internal store outbox has a due (failed) item after the failed send.
	now := time.Now().UTC().Add(2 * time.Minute)
	due, err := s1.store.ListOutboxDue(now, 10)
	if err != nil {
		t.Fatalf("ListOutboxDue: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("due len=%d want 1", len(due))
	}
	if due[0].Project != "proj" {
		t.Fatalf("due project=%q", due[0].Project)
	}
	if due[0].Status != store.OutboxFailed && due[0].Status != store.OutboxPending {
		t.Fatalf("due status=%q", due[0].Status)
	}
	outboxID := due[0].ID

	// Second boot: register in-process handler and run one-shot recovery to deliver
	// the due item, then ensure it is marked sent.
	s2, err := NewServer(Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServer(2): %v", err)
	}

	got := make(chan *core.Message, 1)
	p := s2.Platform("proj")
	if err := p.Start(func(_ core.Platform, msg *core.Message) {
		got <- msg
	}); err != nil {
		t.Fatalf("platform.Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if n, err := s2.recoverOutboxOnce(ctx, 10); err != nil {
		t.Fatalf("recoverOutboxOnce: %v", err)
	} else if n != 1 {
		t.Fatalf("recover attempted=%d want 1", n)
	}

	select {
	case <-got:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for recovered send dispatch")
	}

	item, err := s2.store.GetOutboxItem("proj", outboxID)
	if err != nil {
		t.Fatalf("GetOutboxItem: %v", err)
	}
	if item.Status != store.OutboxSent {
		t.Fatalf("outbox status=%q want %q", item.Status, store.OutboxSent)
	}
}
