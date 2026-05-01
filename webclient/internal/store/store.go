package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

var ErrNotFound = errors.New("not found")

type Attachment struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"` // "image" or "file"
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Size     int    `json:"size,omitempty"`
	URL      string `json:"url,omitempty"`
}

type Message struct {
	ID          string       `json:"id"`
	Role        string       `json:"role"`
	Content     string       `json:"content,omitempty"`
	Timestamp   time.Time    `json:"timestamp"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

type SessionSummary struct {
	Session      string `json:"session"`
	UpdatedAt    string `json:"updated_at,omitempty"`
	MessageCount int    `json:"message_count,omitempty"`
}

type AttachmentMeta struct {
	ID        string    `json:"id"`
	FileName  string    `json:"file_name,omitempty"`
	MimeType  string    `json:"mime_type,omitempty"`
	Size      int       `json:"size,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Store struct {
	root        string
	publicURL   string
	messagesDir string
	attachDir   string

	mu        sync.Mutex
	sessLocks map[string]*sync.Mutex

	attachMu sync.RWMutex
	attach   map[string]AttachmentMeta
}

func New(root string, publicURL string) (*Store, error) {
	root = filepath.Clean(root)
	publicURL = strings.TrimRight(strings.TrimSpace(publicURL), "/")
	s := &Store{
		root:        root,
		publicURL:   publicURL,
		messagesDir: filepath.Join(root, "messages"),
		attachDir:   filepath.Join(root, "attachments"),
		sessLocks:   make(map[string]*sync.Mutex),
		attach:      make(map[string]AttachmentMeta),
	}
	if err := os.MkdirAll(s.messagesDir, 0o755); err != nil {
		return nil, fmt.Errorf("webclient store: mkdir messages: %w", err)
	}
	if err := os.MkdirAll(s.attachDir, 0o755); err != nil {
		return nil, fmt.Errorf("webclient store: mkdir attachments: %w", err)
	}
	if err := s.loadAttachmentIndex(); err != nil {
		return nil, err
	}
	return s, nil
}

func ValidateSegment(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if strings.ContainsAny(value, "/\\") || strings.Contains(value, "..") || strings.ContainsRune(value, 0) {
		return fmt.Errorf("invalid %s", name)
	}
	return nil
}

func (s *Store) AppendMessage(project, session string, msg Message) (Message, error) {
	if err := ValidateSegment("project", project); err != nil {
		return Message{}, err
	}
	if err := ValidateSegment("session", session); err != nil {
		return Message{}, err
	}
	if msg.Role != RoleUser && msg.Role != RoleAssistant {
		return Message{}, fmt.Errorf("invalid role %q", msg.Role)
	}
	if msg.ID == "" {
		msg.ID = uuid.NewString()
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now().UTC()
	}

	lock := s.sessionLock(project, session)
	lock.Lock()
	defer lock.Unlock()

	path, err := s.messagesPath(project, session)
	if err != nil {
		return Message{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Message{}, fmt.Errorf("mkdir session dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return Message{}, fmt.Errorf("open messages: %w", err)
	}
	defer f.Close()

	b, err := json.Marshal(msg)
	if err != nil {
		return Message{}, fmt.Errorf("marshal message: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return Message{}, fmt.Errorf("append message: %w", err)
	}
	return msg, nil
}

func (s *Store) ReadMessages(project, session string) ([]Message, error) {
	if err := ValidateSegment("project", project); err != nil {
		return nil, err
	}
	if err := ValidateSegment("session", session); err != nil {
		return nil, err
	}
	path, err := s.messagesPath(project, session)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open messages: %w", err)
	}
	defer f.Close()

	var out []Message
	sc := bufio.NewScanner(f)
	// allow larger lines (attachments URLs etc)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m Message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			// Tolerate partial/truncated last line.
			continue
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("scan messages: %w", err)
	}
	return out, nil
}

func (s *Store) ListSessions(project string) ([]SessionSummary, error) {
	if err := ValidateSegment("project", project); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.messagesDir, project)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sessions dir: %w", err)
	}

	out := make([]SessionSummary, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		session := strings.TrimSuffix(name, ".jsonl")
		full := filepath.Join(dir, name)
		st, err := os.Stat(full)
		if err != nil {
			continue
		}
		count, _ := countLines(full)
		out = append(out, SessionSummary{
			Session:      session,
			UpdatedAt:    st.ModTime().UTC().Format(time.RFC3339Nano),
			MessageCount: count,
		})
	}
	return out, nil
}

func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var n int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n, sc.Err()
}

func (s *Store) messagesPath(project, session string) (string, error) {
	if err := ValidateSegment("project", project); err != nil {
		return "", err
	}
	if err := ValidateSegment("session", session); err != nil {
		return "", err
	}
	return filepath.Join(s.messagesDir, project, session+".jsonl"), nil
}

func (s *Store) sessionLock(project, session string) *sync.Mutex {
	k := project + "\n" + session
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.sessLocks[k]; l != nil {
		return l
	}
	l := &sync.Mutex{}
	s.sessLocks[k] = l
	return l
}

func (s *Store) AttachmentURL(id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	if s.publicURL != "" {
		return s.publicURL + "/attachments/" + id
	}
	return "/attachments/" + id
}

func (s *Store) SaveAttachment(meta AttachmentMeta, r io.Reader) (AttachmentMeta, error) {
	if strings.TrimSpace(meta.ID) == "" {
		meta.ID = uuid.NewString()
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}

	dataPath := filepath.Join(s.attachDir, meta.ID)
	tmpPath := dataPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return AttachmentMeta{}, fmt.Errorf("open attachment tmp: %w", err)
	}
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return AttachmentMeta{}, fmt.Errorf("write attachment: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return AttachmentMeta{}, fmt.Errorf("close attachment: %w", closeErr)
	}
	meta.Size = int(n)
	if err := os.Rename(tmpPath, dataPath); err != nil {
		_ = os.Remove(tmpPath)
		return AttachmentMeta{}, fmt.Errorf("rename attachment: %w", err)
	}

	// Write meta as JSON next to the data.
	metaPath := dataPath + ".json"
	if err := atomicWriteJSON(metaPath, meta); err != nil {
		return AttachmentMeta{}, err
	}

	s.attachMu.Lock()
	s.attach[meta.ID] = meta
	s.attachMu.Unlock()
	return meta, nil
}

func (s *Store) OpenAttachment(id string) (AttachmentMeta, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return AttachmentMeta{}, "", ErrNotFound
	}
	s.attachMu.RLock()
	meta, ok := s.attach[id]
	s.attachMu.RUnlock()
	if !ok {
		// Best-effort refresh in case index is stale.
		if err := s.loadOneAttachmentMeta(id); err != nil {
			return AttachmentMeta{}, "", ErrNotFound
		}
		s.attachMu.RLock()
		meta, ok = s.attach[id]
		s.attachMu.RUnlock()
		if !ok {
			return AttachmentMeta{}, "", ErrNotFound
		}
	}
	dataPath := filepath.Join(s.attachDir, id)
	if _, err := os.Stat(dataPath); err != nil {
		return AttachmentMeta{}, "", ErrNotFound
	}
	return meta, dataPath, nil
}

func (s *Store) loadAttachmentIndex() error {
	ents, err := os.ReadDir(s.attachDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read attachments dir: %w", err)
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
		_ = s.loadOneAttachmentMeta(id)
	}
	return nil
}

func (s *Store) loadOneAttachmentMeta(id string) error {
	path := filepath.Join(s.attachDir, id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var meta AttachmentMeta
	if err := json.Unmarshal(b, &meta); err != nil {
		return err
	}
	if meta.ID == "" {
		meta.ID = id
	}
	s.attachMu.Lock()
	s.attach[meta.ID] = meta
	s.attachMu.Unlock()
	return nil
}

func atomicWriteJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp := path + ".tmp"
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename tmp: %w", err)
	}
	return nil
}

