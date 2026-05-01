package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClientSession_CreateAppendPersist(t *testing.T) {
	root := t.TempDir()
	st, err := New(root, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	project := "p1"
	created, err := st.CreateClientSession(project, CreateClientSessionInput{
		SessionKey: "webnew:web-admin:p1",
		Name:       "Chat 1",
		Platform:   "webnew",
		AgentType:  "codex",
	})
	if err != nil {
		t.Fatalf("CreateClientSession: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("expected non-empty session id")
	}

	if _, err := st.AppendMessage(project, created.ID, Message{Role: RoleUser, Content: "hi"}); err != nil {
		t.Fatalf("AppendMessage user: %v", err)
	}
	if _, err := st.AppendMessage(project, created.ID, Message{Role: RoleAssistant, Content: "hello"}); err != nil {
		t.Fatalf("AppendMessage assistant: %v", err)
	}

	list, activeKeys, err := st.ListClientSessions(project)
	if err != nil {
		t.Fatalf("ListClientSessions: %v", err)
	}
	if got := activeKeys[created.SessionKey]; got != created.ID {
		t.Fatalf("activeKeys[%q]=%q, want %q", created.SessionKey, got, created.ID)
	}
	if len(list) != 1 {
		t.Fatalf("sessions len=%d, want 1", len(list))
	}
	sess := list[0]
	if sess.HistoryCount != 2 {
		t.Fatalf("HistoryCount=%d, want 2", sess.HistoryCount)
	}
	if sess.LastMessage == nil || sess.LastMessage.Content != "hello" {
		t.Fatalf("LastMessage=%v, want content=hello", sess.LastMessage)
	}

	// Simulate restart.
	st2, err := New(root, "")
	if err != nil {
		t.Fatalf("New restart: %v", err)
	}
	list2, activeKeys2, err := st2.ListClientSessions(project)
	if err != nil {
		t.Fatalf("ListClientSessions restart: %v", err)
	}
	if got := activeKeys2[created.SessionKey]; got != created.ID {
		t.Fatalf("restart activeKeys[%q]=%q, want %q", created.SessionKey, got, created.ID)
	}
	if len(list2) != 1 {
		t.Fatalf("restart sessions len=%d, want 1", len(list2))
	}
	if list2[0].HistoryCount != 2 {
		t.Fatalf("restart HistoryCount=%d, want 2", list2[0].HistoryCount)
	}

	detail, err := st2.GetClientSession(project, created.ID, 1)
	if err != nil {
		t.Fatalf("GetClientSession: %v", err)
	}
	if len(detail.History) != 1 {
		t.Fatalf("history len=%d, want 1", len(detail.History))
	}
	if detail.History[0].Role != RoleAssistant || detail.History[0].Content != "hello" {
		t.Fatalf("history[0]=%+v, want assistant hello", detail.History[0])
	}

	renamed, err := st2.RenameClientSession(project, created.ID, "Renamed")
	if err != nil {
		t.Fatalf("RenameClientSession: %v", err)
	}
	if renamed.Name != "Renamed" {
		t.Fatalf("Name=%q, want Renamed", renamed.Name)
	}

	if err := st2.DeleteClientSession(project, created.ID); err != nil {
		t.Fatalf("DeleteClientSession: %v", err)
	}
	list3, activeKeys3, err := st2.ListClientSessions(project)
	if err != nil {
		t.Fatalf("ListClientSessions after delete: %v", err)
	}
	if len(list3) != 0 {
		t.Fatalf("sessions len=%d, want 0", len(list3))
	}
	if _, ok := activeKeys3[created.SessionKey]; ok {
		t.Fatalf("expected active key removed for %q", created.SessionKey)
	}
}

func TestClientSession_MigrateFromMessagesOnly(t *testing.T) {
	root := t.TempDir()

	project := "p1"
	id := "legacy"
	msgDir := filepath.Join(root, "messages", project)
	if err := os.MkdirAll(msgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	msg := Message{
		ID:        "m1",
		Role:      RoleUser,
		Content:   "from-legacy",
		Timestamp: time.Now().UTC(),
	}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(msgDir, id+".jsonl"), append(b, '\n'), 0o644); err != nil {
		t.Fatalf("WriteFile legacy jsonl: %v", err)
	}

	st, err := New(root, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sessions, _, err := st.ListClientSessions(project)
	if err != nil {
		t.Fatalf("ListClientSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len=%d, want 1", len(sessions))
	}
	got := sessions[0]
	if got.ID != id {
		t.Fatalf("id=%q, want %q", got.ID, id)
	}
	if got.SessionKey != "webclient:p1:legacy" {
		t.Fatalf("session_key=%q, want webclient:p1:legacy", got.SessionKey)
	}
	if got.HistoryCount != 1 {
		t.Fatalf("HistoryCount=%d, want 1", got.HistoryCount)
	}
	if got.LastMessage == nil || got.LastMessage.Content != "from-legacy" {
		t.Fatalf("LastMessage=%v, want content=from-legacy", got.LastMessage)
	}

	if _, err := os.Stat(filepath.Join(root, "sessions", project, id+".json")); err != nil {
		t.Fatalf("expected migrated meta file: %v", err)
	}
}
