package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type OutboxStatus string

const (
	OutboxPending OutboxStatus = "pending"
	OutboxSent    OutboxStatus = "sent"
	OutboxFailed  OutboxStatus = "failed"
)

// OutboxItem is a durable record of an outgoing deliver attempt from the
// webclient backend to an upstream control-plane backend.
//
// The store treats the Payload as opaque JSON, so higher layers can evolve
// without changing the persistence schema.
type OutboxItem struct {
	ID         string       `json:"id"`
	Project    string       `json:"project"`
	SessionID  string       `json:"session_id"`
	SessionKey string       `json:"session_key"`
	Status     OutboxStatus `json:"status"`

	Attempts    int       `json:"attempts"`
	NextRetryAt time.Time `json:"next_retry_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`

	Payload json.RawMessage `json:"payload,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateOutboxItemInput struct {
	ID          string
	Project     string
	SessionID   string
	SessionKey  string
	Payload     json.RawMessage
	NextRetryAt time.Time
	CreatedAt   time.Time
}

func (s *Store) CreateOutboxItem(in CreateOutboxItemInput) (OutboxItem, error) {
	project := strings.TrimSpace(in.Project)
	if err := ValidateSegment("project", project); err != nil {
		return OutboxItem{}, err
	}
	if err := ValidateSegment("session", in.SessionID); err != nil {
		return OutboxItem{}, err
	}
	sessionKey := strings.TrimSpace(in.SessionKey)
	if sessionKey == "" {
		return OutboxItem{}, fmt.Errorf("session_key is required")
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = uuid.NewString()
	}
	if err := ValidateSegment("outbox_id", id); err != nil {
		return OutboxItem{}, err
	}

	now := time.Now().UTC()
	created := in.CreatedAt
	if created.IsZero() {
		created = now
	}
	next := in.NextRetryAt
	if next.IsZero() {
		next = now
	}

	item := OutboxItem{
		ID:          id,
		Project:     project,
		SessionID:   in.SessionID,
		SessionKey:  sessionKey,
		Status:      OutboxPending,
		Attempts:    0,
		NextRetryAt: next,
		LastError:   "",
		Payload:     in.Payload,
		CreatedAt:   created,
		UpdatedAt:   now,
	}

	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()

	path, err := s.outboxItemPath(project, id)
	if err != nil {
		return OutboxItem{}, err
	}
	if _, err := os.Stat(path); err == nil {
		return OutboxItem{}, fmt.Errorf("outbox item already exists")
	}
	if err := atomicWriteJSON(path, item); err != nil {
		return OutboxItem{}, err
	}
	return item, nil
}

func (s *Store) GetOutboxItem(project, id string) (OutboxItem, error) {
	project = strings.TrimSpace(project)
	if err := ValidateSegment("project", project); err != nil {
		return OutboxItem{}, err
	}
	id = strings.TrimSpace(id)
	if err := ValidateSegment("outbox_id", id); err != nil {
		return OutboxItem{}, err
	}

	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	return s.readOutboxItemLocked(project, id)
}

// ListOutboxDue returns due items across all projects:
// - status == pending, next_retry_at <= now
// - status == failed, next_retry_at <= now
func (s *Store) ListOutboxDue(now time.Time, limit int) ([]OutboxItem, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}

	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()

	projects, err := os.ReadDir(s.outboxDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read outbox dir: %w", err)
	}

	var out []OutboxItem
	for _, pe := range projects {
		if !pe.IsDir() {
			continue
		}
		project := pe.Name()
		ents, err := os.ReadDir(filepath.Join(s.outboxDir, project))
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			id := strings.TrimSuffix(name, ".json")
			item, err := s.readOutboxItemLocked(project, id)
			if err != nil {
				continue
			}
			if item.Status != OutboxPending && item.Status != OutboxFailed {
				continue
			}
			if item.NextRetryAt.After(now) {
				continue
			}
			out = append(out, item)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		// Prefer next_retry_at asc; fall back to created_at.
		ti := out[i].NextRetryAt
		tj := out[j].NextRetryAt
		if ti.Equal(tj) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return ti.Before(tj)
	})

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) MarkOutboxSent(project, id string) (OutboxItem, error) {
	project = strings.TrimSpace(project)
	if err := ValidateSegment("project", project); err != nil {
		return OutboxItem{}, err
	}
	id = strings.TrimSpace(id)
	if err := ValidateSegment("outbox_id", id); err != nil {
		return OutboxItem{}, err
	}

	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()

	item, err := s.readOutboxItemLocked(project, id)
	if err != nil {
		return OutboxItem{}, err
	}
	item.Attempts++
	item.Status = OutboxSent
	item.LastError = ""
	item.UpdatedAt = time.Now().UTC()

	path, err := s.outboxItemPath(project, id)
	if err != nil {
		return OutboxItem{}, err
	}
	if err := atomicWriteJSON(path, item); err != nil {
		return OutboxItem{}, err
	}
	return item, nil
}

func (s *Store) MarkOutboxFailed(project, id, lastError string, nextRetryAt time.Time) (OutboxItem, error) {
	project = strings.TrimSpace(project)
	if err := ValidateSegment("project", project); err != nil {
		return OutboxItem{}, err
	}
	id = strings.TrimSpace(id)
	if err := ValidateSegment("outbox_id", id); err != nil {
		return OutboxItem{}, err
	}
	lastError = strings.TrimSpace(lastError)
	if lastError == "" {
		lastError = "unknown error"
	}
	if nextRetryAt.IsZero() {
		nextRetryAt = time.Now().UTC()
	}

	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()

	item, err := s.readOutboxItemLocked(project, id)
	if err != nil {
		return OutboxItem{}, err
	}
	item.Attempts++
	item.Status = OutboxFailed
	item.LastError = lastError
	item.NextRetryAt = nextRetryAt
	item.UpdatedAt = time.Now().UTC()

	path, err := s.outboxItemPath(project, id)
	if err != nil {
		return OutboxItem{}, err
	}
	if err := atomicWriteJSON(path, item); err != nil {
		return OutboxItem{}, err
	}
	return item, nil
}

func (s *Store) outboxItemPath(project, id string) (string, error) {
	if err := ValidateSegment("project", project); err != nil {
		return "", err
	}
	if err := ValidateSegment("outbox_id", id); err != nil {
		return "", err
	}
	return filepath.Join(s.outboxDir, project, id+".json"), nil
}

func (s *Store) readOutboxItemLocked(project, id string) (OutboxItem, error) {
	path, err := s.outboxItemPath(project, id)
	if err != nil {
		return OutboxItem{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return OutboxItem{}, ErrNotFound
		}
		return OutboxItem{}, fmt.Errorf("read outbox item: %w", err)
	}
	var item OutboxItem
	if err := json.Unmarshal(b, &item); err != nil {
		return OutboxItem{}, fmt.Errorf("unmarshal outbox item: %w", err)
	}
	if item.ID == "" {
		item.ID = id
	}
	if item.Project == "" {
		item.Project = project
	}
	return item, nil
}
