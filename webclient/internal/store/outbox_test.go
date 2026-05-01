package store

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOutbox_CreateListMarkPersist(t *testing.T) {
	root := t.TempDir()
	st, err := New(root, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	now := time.Now().UTC()

	item1, err := st.CreateOutboxItem(CreateOutboxItemInput{
		Project:     "p1",
		SessionID:   "s1",
		SessionKey:  "webnew:web-admin:p1",
		Payload:     json.RawMessage(`{"type":"send","content":"hi"}`),
		NextRetryAt: now.Add(-1 * time.Second),
	})
	if err != nil {
		t.Fatalf("CreateOutboxItem item1: %v", err)
	}
	item2, err := st.CreateOutboxItem(CreateOutboxItemInput{
		Project:     "p2",
		SessionID:   "s2",
		SessionKey:  "webnew:web-admin:p2",
		Payload:     json.RawMessage(`{"type":"send","content":"later"}`),
		NextRetryAt: now.Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateOutboxItem item2: %v", err)
	}

	due, err := st.ListOutboxDue(now, 0)
	if err != nil {
		t.Fatalf("ListOutboxDue: %v", err)
	}
	if len(due) != 1 || due[0].ID != item1.ID {
		t.Fatalf("due=%v, want only item1", due)
	}

	failedAt := now.Add(2 * time.Minute)
	failed, err := st.MarkOutboxFailed(item1.Project, item1.ID, "upstream down", failedAt)
	if err != nil {
		t.Fatalf("MarkOutboxFailed: %v", err)
	}
	if failed.Status != OutboxFailed {
		t.Fatalf("Status=%q, want failed", failed.Status)
	}
	if failed.Attempts != 1 {
		t.Fatalf("Attempts=%d, want 1", failed.Attempts)
	}
	if failed.NextRetryAt.UTC() != failedAt.UTC() {
		t.Fatalf("NextRetryAt=%s, want %s", failed.NextRetryAt.UTC(), failedAt.UTC())
	}

	due2, err := st.ListOutboxDue(now, 0)
	if err != nil {
		t.Fatalf("ListOutboxDue after fail: %v", err)
	}
	if len(due2) != 0 {
		t.Fatalf("due2 len=%d, want 0", len(due2))
	}

	due3, err := st.ListOutboxDue(now.Add(3*time.Minute), 0)
	if err != nil {
		t.Fatalf("ListOutboxDue after time advance: %v", err)
	}
	if len(due3) != 1 || due3[0].ID != item1.ID {
		t.Fatalf("due3=%v, want only item1", due3)
	}

	sent, err := st.MarkOutboxSent(item1.Project, item1.ID)
	if err != nil {
		t.Fatalf("MarkOutboxSent: %v", err)
	}
	if sent.Status != OutboxSent {
		t.Fatalf("Status=%q, want sent", sent.Status)
	}
	if sent.Attempts != 2 {
		t.Fatalf("Attempts=%d, want 2", sent.Attempts)
	}

	// Ensure not due once sent.
	due4, err := st.ListOutboxDue(now.Add(10*time.Minute), 0)
	if err != nil {
		t.Fatalf("ListOutboxDue after sent: %v", err)
	}
	if len(due4) != 0 {
		t.Fatalf("due4 len=%d, want 0", len(due4))
	}

	// Restart recovery.
	st2, err := New(root, "")
	if err != nil {
		t.Fatalf("New restart: %v", err)
	}
	gotSent, err := st2.GetOutboxItem(item1.Project, item1.ID)
	if err != nil {
		t.Fatalf("GetOutboxItem item1: %v", err)
	}
	if gotSent.Status != OutboxSent || gotSent.Attempts != 2 {
		t.Fatalf("gotSent=%+v, want status=sent attempts=2", gotSent)
	}
	gotPending, err := st2.GetOutboxItem(item2.Project, item2.ID)
	if err != nil {
		t.Fatalf("GetOutboxItem item2: %v", err)
	}
	if gotPending.Status != OutboxPending {
		t.Fatalf("item2 status=%q, want pending", gotPending.Status)
	}
}
