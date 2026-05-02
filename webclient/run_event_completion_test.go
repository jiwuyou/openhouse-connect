package webclient

import (
	"testing"

	"github.com/chenhg5/cc-connect/webclient/internal/store"
)

func TestAdapter_PersistDurableAssistant_AppendsRunCompletedEvent(t *testing.T) {
	t.Parallel()

	s, err := NewServer(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	c := newAdapterClient(s)

	project := "proj"
	sessionKey := "webnew:web-admin:proj"
	sessionID := "sess1"
	runID := "run1"

	c.SetActiveRun(project, sessionKey, sessionID, runID, runID)

	raw := []byte(`{"type":"reply","session_key":"` + sessionKey + `","session_id":"` + sessionID + `","content":"ok"}`)
	if err := c.persistDurableAssistant("reply", raw); err != nil {
		t.Fatalf("persistDurableAssistant: %v", err)
	}

	msgs, err := s.store.ReadMessages(project, sessionID)
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != store.RoleAssistant || msgs[0].Content != "ok" {
		t.Fatalf("msgs=%#v", msgs)
	}

	evts, err := s.store.ReadRunEvents(project, sessionID, 50)
	if err != nil {
		t.Fatalf("ReadRunEvents: %v", err)
	}
	if len(evts) == 0 {
		t.Fatalf("expected run_events to include run_completed")
	}
	last := evts[len(evts)-1]
	if last.Type != "run_completed" || last.Status != "completed" || last.RunID != runID {
		t.Fatalf("last run_event=%#v", last)
	}
}

func TestAdapter_PersistDurableError_AppendsRunErrorEvent(t *testing.T) {
	t.Parallel()

	s, err := NewServer(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	c := newAdapterClient(s)

	project := "proj"
	sessionKey := "webnew:web-admin:proj"
	sessionID := "sess_err_1"
	runID := "run_err_1"
	c.SetActiveRun(project, sessionKey, sessionID, runID, runID)

	raw := []byte(`{"type":"error","session_key":"` + sessionKey + `","session_id":"` + sessionID + `","code":"E_TEST","message":"boom"}`)
	if err := c.persistDurableError(raw); err != nil {
		t.Fatalf("persistDurableError: %v", err)
	}

	evts, err := s.store.ReadRunEvents(project, sessionID, 50)
	if err != nil {
		t.Fatalf("ReadRunEvents: %v", err)
	}
	if len(evts) == 0 {
		t.Fatalf("expected run_events to include run_error")
	}
	last := evts[len(evts)-1]
	if last.Type != "run_error" || last.Status != "error" || last.RunID != runID {
		t.Fatalf("last run_event=%#v", last)
	}
}
