package store

import (
	"bufio"
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

// ClientSession is the persisted, client-side "fact" session model used by the
// copied admin frontend.
//
// Active is computed from the persisted active-keys map. Live is ephemeral and
// set by the server at runtime.
type ClientSession struct {
	ID           string             `json:"id"`
	SessionKey   string             `json:"session_key"`
	Name         string             `json:"name"`
	Platform     string             `json:"platform"`
	AgentType    string             `json:"agent_type"`
	Active       bool               `json:"active"`
	Live         bool               `json:"live"`
	CreatedAt    time.Time          `json:"created_at"`
	UpdatedAt    time.Time          `json:"updated_at"`
	HistoryCount int                `json:"history_count"`
	LastMessage  *ClientLastMessage `json:"last_message"`
}

type ClientLastMessage struct {
	Role      string    `json:"role"`
	Content   string    `json:"content,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	// Keep attachments in the store so the server can derive image/file payloads.
	Attachments []Attachment `json:"attachments,omitempty"`
}

type ClientSessionDetail struct {
	ClientSession
	History []Message `json:"history"`
}

type CreateClientSessionInput struct {
	ID         string
	SessionKey string
	Name       string
	Platform   string
	AgentType  string
	CreatedAt  time.Time
}

func (s *Store) CreateClientSession(project string, in CreateClientSessionInput) (ClientSession, error) {
	if err := ValidateSegment("project", project); err != nil {
		return ClientSession{}, err
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = uuid.NewString()
	}
	if err := ValidateSegment("session", id); err != nil {
		return ClientSession{}, err
	}
	key := strings.TrimSpace(in.SessionKey)
	if key == "" {
		return ClientSession{}, fmt.Errorf("session_key is required")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = id
	}
	platform := strings.TrimSpace(in.Platform)
	if platform == "" {
		platform = platformFromSessionKey(key)
	}
	agentType := strings.TrimSpace(in.AgentType)
	now := time.Now().UTC()
	created := in.CreatedAt
	if created.IsZero() {
		created = now
	}

	meta := ClientSession{
		ID:           id,
		SessionKey:   key,
		Name:         name,
		Platform:     platform,
		AgentType:    agentType,
		CreatedAt:    created,
		UpdatedAt:    now,
		HistoryCount: 0,
		LastMessage:  nil,
	}

	// Session-scoped lock protects both meta and JSONL message file.
	lock := s.sessionLock(project, id)
	lock.Lock()
	defer lock.Unlock()

	metaPath := s.sessionMetaPath(project, id)
	if _, err := os.Stat(metaPath); err == nil {
		return ClientSession{}, fmt.Errorf("session already exists")
	}
	if err := atomicWriteJSON(metaPath, meta); err != nil {
		return ClientSession{}, err
	}

	// Ensure the messages file exists.
	msgPath, err := s.messagesPath(project, id)
	if err != nil {
		return ClientSession{}, err
	}
	if err := os.MkdirAll(filepath.Dir(msgPath), 0o755); err != nil {
		return ClientSession{}, fmt.Errorf("mkdir session dir: %w", err)
	}
	f, err := os.OpenFile(msgPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return ClientSession{}, fmt.Errorf("open messages: %w", err)
	}
	_ = f.Close()

	// Persist active session binding if this session_key has no active session yet.
	_, activeKeys, err := s.readActiveKeys(project)
	if err != nil {
		return ClientSession{}, err
	}
	if _, ok := activeKeys[key]; !ok {
		activeKeys[key] = id
		if err := s.writeActiveKeys(project, activeKeys); err != nil {
			return ClientSession{}, err
		}
		meta.Active = true
	}
	return meta, nil
}

// ListClientSessions returns all sessions under a project, plus the persisted
// active session id for each session_key.
func (s *Store) ListClientSessions(project string) ([]ClientSession, map[string]string, error) {
	if err := ValidateSegment("project", project); err != nil {
		return nil, nil, err
	}

	// Migrate legacy message-only sessions.
	if err := s.ensureSessionMetasFromMessages(project); err != nil {
		return nil, nil, err
	}

	_, activeKeys, err := s.readActiveKeys(project)
	if err != nil {
		return nil, nil, err
	}

	dir := filepath.Join(s.sessionsDir, project)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, activeKeys, nil
		}
		return nil, nil, fmt.Errorf("read sessions dir: %w", err)
	}

	out := make([]ClientSession, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if name == "active_keys.json" {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		meta, err := s.readSessionMeta(project, id)
		if err != nil {
			continue
		}
		meta.Active = activeKeys[meta.SessionKey] == meta.ID && strings.TrimSpace(meta.SessionKey) != ""
		meta.Live = s.sessionLive(project, meta.ID)
		out = append(out, meta)
	}

	sort.Slice(out, func(i, j int) bool {
		// Prefer updated_at desc; fall back to created_at.
		ti := out[i].UpdatedAt
		tj := out[j].UpdatedAt
		if ti.Equal(tj) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return ti.After(tj)
	})
	return out, activeKeys, nil
}

func (s *Store) GetClientSession(project, id string, historyLimit int) (ClientSessionDetail, error) {
	if err := ValidateSegment("project", project); err != nil {
		return ClientSessionDetail{}, err
	}
	if err := ValidateSegment("session", id); err != nil {
		return ClientSessionDetail{}, err
	}

	// Ensure meta exists (supports migration and legacy message-only data).
	if err := s.ensureSessionMetaFromMessages(project, id); err != nil {
		return ClientSessionDetail{}, err
	}

	meta, err := s.readSessionMeta(project, id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ClientSessionDetail{}, ErrNotFound
		}
		return ClientSessionDetail{}, err
	}

	msgs, err := s.ReadMessages(project, id)
	if err != nil {
		return ClientSessionDetail{}, err
	}
	if historyLimit > 0 && len(msgs) > historyLimit {
		msgs = msgs[len(msgs)-historyLimit:]
	}

	_, activeKeys, err := s.readActiveKeys(project)
	if err != nil {
		return ClientSessionDetail{}, err
	}
	meta.Active = activeKeys[meta.SessionKey] == meta.ID && strings.TrimSpace(meta.SessionKey) != ""
	meta.Live = s.sessionLive(project, meta.ID)

	return ClientSessionDetail{ClientSession: meta, History: msgs}, nil
}

// FindProjectForClientSession attempts to locate which project a given
// (session_key, session_id) belongs to by scanning persisted session metas.
//
// This is a best-effort fallback used by the external bridge adapter when an
// inbound bridge event arrives without an explicit project tag.
func (s *Store) FindProjectForClientSession(sessionKey, sessionID string) (project string, ok bool, err error) {
	sessionKey = strings.TrimSpace(sessionKey)
	sessionID = strings.TrimSpace(sessionID)
	if sessionKey == "" || sessionID == "" {
		return "", false, nil
	}
	if err := ValidateSegment("session", sessionID); err != nil {
		return "", false, err
	}
	ents, err := os.ReadDir(s.sessionsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		p := e.Name()
		meta, err := s.readSessionMeta(p, sessionID)
		if err != nil {
			continue
		}
		if strings.TrimSpace(meta.SessionKey) == sessionKey {
			return p, true, nil
		}
	}
	return "", false, nil
}

func (s *Store) RenameClientSession(project, id, name string) (ClientSession, error) {
	if err := ValidateSegment("project", project); err != nil {
		return ClientSession{}, err
	}
	if err := ValidateSegment("session", id); err != nil {
		return ClientSession{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ClientSession{}, fmt.Errorf("name is required")
	}

	lock := s.sessionLock(project, id)
	lock.Lock()
	defer lock.Unlock()

	if err := s.ensureSessionMetaFromMessagesLocked(project, id); err != nil {
		return ClientSession{}, err
	}
	meta, err := s.readSessionMeta(project, id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ClientSession{}, ErrNotFound
		}
		return ClientSession{}, err
	}
	meta.Name = name
	meta.UpdatedAt = time.Now().UTC()
	if err := atomicWriteJSON(s.sessionMetaPath(project, id), meta); err != nil {
		return ClientSession{}, err
	}

	_, activeKeys, err := s.readActiveKeys(project)
	if err != nil {
		return ClientSession{}, err
	}
	meta.Active = activeKeys[meta.SessionKey] == meta.ID && strings.TrimSpace(meta.SessionKey) != ""
	meta.Live = s.sessionLive(project, meta.ID)
	return meta, nil
}

func (s *Store) DeleteClientSession(project, id string) error {
	if err := ValidateSegment("project", project); err != nil {
		return err
	}
	if err := ValidateSegment("session", id); err != nil {
		return err
	}

	lock := s.sessionLock(project, id)
	lock.Lock()
	defer lock.Unlock()

	// Load meta (best-effort) so we can clean active bindings.
	var key string
	meta, err := s.readSessionMeta(project, id)
	if err == nil {
		key = meta.SessionKey
	}

	_ = os.Remove(s.sessionMetaPath(project, id))

	msgPath, err := s.messagesPath(project, id)
	if err == nil {
		_ = os.Remove(msgPath)
	}

	s.setSessionLive(project, id, false)

	if strings.TrimSpace(key) != "" {
		_, activeKeys, err := s.readActiveKeys(project)
		if err == nil {
			changed := false
			for k, sid := range activeKeys {
				if k == key && sid == id {
					delete(activeKeys, k)
					changed = true
				}
			}
			if changed {
				_ = s.writeActiveKeys(project, activeKeys)
			}
		}
	}
	return nil
}

func (s *Store) SetActiveSession(project, sessionKey, sessionID string) error {
	if err := ValidateSegment("project", project); err != nil {
		return err
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return fmt.Errorf("session_key is required")
	}
	if err := ValidateSegment("session", sessionID); err != nil {
		return err
	}
	if err := s.ensureSessionMetaFromMessages(project, sessionID); err != nil {
		return err
	}

	_, activeKeys, err := s.readActiveKeys(project)
	if err != nil {
		return err
	}
	activeKeys[sessionKey] = sessionID
	return s.writeActiveKeys(project, activeKeys)
}

func (s *Store) ActiveSessionID(project, sessionKey string) (string, error) {
	if err := ValidateSegment("project", project); err != nil {
		return "", err
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return "", ErrNotFound
	}
	_, activeKeys, err := s.readActiveKeys(project)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(activeKeys[sessionKey])
	if id == "" {
		return "", ErrNotFound
	}
	return id, nil
}

func (s *Store) AppendMessageBySessionKey(project, sessionKey string, msg Message) (Message, string, error) {
	id, err := s.ActiveSessionID(project, sessionKey)
	if err != nil {
		return Message{}, "", err
	}
	stored, err := s.AppendMessage(project, id, msg)
	if err != nil {
		return Message{}, "", err
	}
	return stored, id, nil
}

func (s *Store) ReadMessagesBySessionKey(project, sessionKey string) ([]Message, string, error) {
	id, err := s.ActiveSessionID(project, sessionKey)
	if err != nil {
		return nil, "", err
	}
	msgs, err := s.ReadMessages(project, id)
	if err != nil {
		return nil, "", err
	}
	return msgs, id, nil
}

func (s *Store) SetSessionLive(project, id string, live bool) error {
	if err := ValidateSegment("project", project); err != nil {
		return err
	}
	if err := ValidateSegment("session", id); err != nil {
		return err
	}
	s.setSessionLive(project, id, live)
	return nil
}

func (s *Store) setSessionLive(project, id string, live bool) {
	k := project + "\n" + id
	s.liveMu.Lock()
	if live {
		s.live[k] = true
	} else {
		delete(s.live, k)
	}
	s.liveMu.Unlock()
}

func (s *Store) sessionLive(project, id string) bool {
	k := project + "\n" + id
	s.liveMu.RLock()
	v := s.live[k]
	s.liveMu.RUnlock()
	return v
}

func (s *Store) noteSessionMessage(project, id string, msg Message) error {
	if err := ValidateSegment("project", project); err != nil {
		return err
	}
	if err := ValidateSegment("session", id); err != nil {
		return err
	}

	lock := s.sessionLock(project, id)
	lock.Lock()
	defer lock.Unlock()

	return s.noteSessionMessageLocked(project, id, msg)
}

// noteSessionMessageLocked updates session meta based on an appended message.
// Caller must hold the per-session lock (see sessionLock).
func (s *Store) noteSessionMessageLocked(project, id string, msg Message) error {
	// Ensure meta exists for this session id.
	if err := s.ensureSessionMetaFromMessagesLocked(project, id); err != nil {
		return err
	}
	meta, err := s.readSessionMeta(project, id)
	if err != nil {
		// If we can't load meta, don't fail message persistence.
		return nil
	}

	meta.HistoryCount++
	meta.UpdatedAt = msg.Timestamp
	meta.LastMessage = &ClientLastMessage{
		Role:        msg.Role,
		Content:     msg.Content,
		Timestamp:   msg.Timestamp,
		Attachments: msg.Attachments,
	}
	if err := atomicWriteJSON(s.sessionMetaPath(project, id), meta); err != nil {
		return err
	}

	// If no active binding exists yet, set this session as active for its key.
	if strings.TrimSpace(meta.SessionKey) != "" {
		_, activeKeys, err := s.readActiveKeys(project)
		if err == nil {
			if strings.TrimSpace(activeKeys[meta.SessionKey]) == "" {
				activeKeys[meta.SessionKey] = id
				_ = s.writeActiveKeys(project, activeKeys)
			}
		}
	}
	return nil
}

func (s *Store) sessionMetaPath(project, id string) string {
	return filepath.Join(s.sessionsDir, project, id+".json")
}

func (s *Store) activeKeysPath(project string) string {
	return filepath.Join(s.sessionsDir, project, "active_keys.json")
}

func (s *Store) readSessionMeta(project, id string) (ClientSession, error) {
	path := s.sessionMetaPath(project, id)
	b, err := os.ReadFile(path)
	if err != nil {
		return ClientSession{}, err
	}
	var meta ClientSession
	if err := json.Unmarshal(b, &meta); err != nil {
		return ClientSession{}, fmt.Errorf("unmarshal session meta: %w", err)
	}
	if meta.ID == "" {
		meta.ID = id
	}
	return meta, nil
}

func (s *Store) readActiveKeys(project string) (string, map[string]string, error) {
	path := s.activeKeysPath(project)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return path, make(map[string]string), nil
		}
		return "", nil, fmt.Errorf("read active keys: %w", err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return "", nil, fmt.Errorf("unmarshal active keys: %w", err)
	}
	if m == nil {
		m = make(map[string]string)
	}
	return path, m, nil
}

func (s *Store) writeActiveKeys(project string, m map[string]string) error {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	return atomicWriteJSON(s.activeKeysPath(project), m)
}

func (s *Store) ensureSessionMetasFromMessages(project string) error {
	// Ensure project dir exists.
	if err := os.MkdirAll(filepath.Join(s.sessionsDir, project), 0o755); err != nil {
		return fmt.Errorf("mkdir sessions project dir: %w", err)
	}

	msgDir := filepath.Join(s.messagesDir, project)
	ents, err := os.ReadDir(msgDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read messages dir: %w", err)
	}

	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		_ = s.ensureSessionMetaFromMessages(project, id)
	}
	return nil
}

func (s *Store) ensureSessionMetaFromMessages(project, id string) error {
	lock := s.sessionLock(project, id)
	lock.Lock()
	defer lock.Unlock()
	return s.ensureSessionMetaFromMessagesLocked(project, id)
}

func (s *Store) ensureSessionMetaFromMessagesLocked(project, id string) error {
	metaPath := s.sessionMetaPath(project, id)
	if _, err := os.Stat(metaPath); err == nil {
		return nil
	}

	// Derive minimal meta from messages file.
	msgPath, err := s.messagesPath(project, id)
	if err != nil {
		return err
	}
	st, err := os.Stat(msgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("stat messages: %w", err)
	}

	firstTS, lastTS, count, lastMsg := scanMessagesFile(msgPath)
	created := firstTS
	if created.IsZero() {
		created = st.ModTime().UTC()
	}
	updated := lastTS
	if updated.IsZero() {
		updated = st.ModTime().UTC()
	}

	key := defaultSessionKey(project, id)
	meta := ClientSession{
		ID:           id,
		SessionKey:   key,
		Name:         id,
		Platform:     platformFromSessionKey(key),
		AgentType:    "",
		CreatedAt:    created,
		UpdatedAt:    updated,
		HistoryCount: count,
		LastMessage:  lastMsg,
	}
	if err := atomicWriteJSON(metaPath, meta); err != nil {
		return err
	}

	// Best-effort set active mapping if none exists.
	_, activeKeys, err := s.readActiveKeys(project)
	if err == nil {
		if strings.TrimSpace(activeKeys[key]) == "" {
			activeKeys[key] = id
			_ = s.writeActiveKeys(project, activeKeys)
		}
	}
	return nil
}

func scanMessagesFile(path string) (firstTS, lastTS time.Time, count int, lastMsg *ClientLastMessage) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, time.Time{}, 0, nil
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 4*1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m Message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m.Timestamp.IsZero() {
			continue
		}
		if firstTS.IsZero() {
			firstTS = m.Timestamp
		}
		lastTS = m.Timestamp
		count++
		lastMsg = &ClientLastMessage{
			Role:        m.Role,
			Content:     m.Content,
			Timestamp:   m.Timestamp,
			Attachments: m.Attachments,
		}
	}
	return firstTS, lastTS, count, lastMsg
}

func defaultSessionKey(project, id string) string {
	return "webclient:" + project + ":" + id
}

func platformFromSessionKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if idx := strings.IndexByte(key, ':'); idx > 0 {
		return key[:idx]
	}
	return ""
}
