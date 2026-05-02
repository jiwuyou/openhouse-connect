package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RunEvent is a transient, webclient-local event that describes intermediate
// progress (preview/update/delete/typing/stream/status, etc).
//
// These are NOT part of the durable chat history; they are attached to a user
// message via RunID/UserMessageID so the UI can optionally render them between
// the user's message and the assistant's final response.
type RunEvent struct {
	ID            string         `json:"id"`
	Seq           int            `json:"seq,omitempty"`
	RunID         string         `json:"run_id"`
	UserMessageID string         `json:"user_message_id"`
	SessionID     string         `json:"session_id"`
	Type          string         `json:"type"`
	Content       string         `json:"content,omitempty"`
	Status        string         `json:"status"` // "active" | "completed" | "error"
	CreatedAt     time.Time      `json:"created_at"`
	Timestamp     time.Time      `json:"timestamp,omitempty"` // compatibility with older clients
	Metadata      map[string]any `json:"metadata,omitempty"`
}

func (s *Store) runEventsPath(project, session string) (string, error) {
	if err := ValidateSegment("project", project); err != nil {
		return "", err
	}
	if err := ValidateSegment("session", session); err != nil {
		return "", err
	}
	return filepath.Join(s.runEventsDir, project, session+".jsonl"), nil
}

func (s *Store) AppendRunEvent(project, session string, ev RunEvent) (RunEvent, error) {
	if err := ValidateSegment("project", project); err != nil {
		return RunEvent{}, err
	}
	if err := ValidateSegment("session", session); err != nil {
		return RunEvent{}, err
	}
	ev.Type = strings.TrimSpace(ev.Type)
	if ev.Type == "" {
		return RunEvent{}, fmt.Errorf("run_event type is required")
	}
	if ev.ID == "" {
		ev.ID = uuid.NewString()
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = ev.CreatedAt
	}
	if ev.Status == "" {
		ev.Status = "active"
	}

	lock := s.sessionLock(project, session)
	lock.Lock()
	defer lock.Unlock()

	path, err := s.runEventsPath(project, session)
	if err != nil {
		return RunEvent{}, err
	}
	if ev.Seq <= 0 {
		n, err := countRunEventLines(path)
		if err != nil {
			return RunEvent{}, err
		}
		ev.Seq = n + 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return RunEvent{}, fmt.Errorf("mkdir run_events dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return RunEvent{}, fmt.Errorf("open run_events: %w", err)
	}
	defer f.Close()
	b, err := json.Marshal(ev)
	if err != nil {
		return RunEvent{}, fmt.Errorf("marshal run_event: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return RunEvent{}, fmt.Errorf("append run_event: %w", err)
	}
	return ev, nil
}

func countRunEventLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("open run_events: %w", err)
	}
	defer f.Close()

	count := 0
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 4*1024*1024)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			count++
		}
	}
	if err := sc.Err(); err != nil {
		return count, fmt.Errorf("scan run_events: %w", err)
	}
	return count, nil
}

func (s *Store) ReadRunEvents(project, session string, limit int) ([]RunEvent, error) {
	if err := ValidateSegment("project", project); err != nil {
		return nil, err
	}
	if err := ValidateSegment("session", session); err != nil {
		return nil, err
	}
	path, err := s.runEventsPath(project, session)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open run_events: %w", err)
	}
	defer f.Close()

	var out []RunEvent
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev RunEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("scan run_events: %w", err)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}
