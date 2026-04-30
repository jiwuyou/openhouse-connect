package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	DefaultOutboxPollInterval = time.Second
	DefaultOutboxStableFor    = 1500 * time.Millisecond

	defaultOutboxMaxBytes = 50 << 20
)

// OutboxItem is a deliverable file discovered in a session outbox.
// Exactly one of Image or File is populated.
type OutboxItem struct {
	Path     string
	FileName string
	MimeType string
	Image    *ImageAttachment
	File     *FileAttachment
}

type OutboxWatcherConfig struct {
	Dir          string
	PollInterval time.Duration
	StableFor    time.Duration
	MaxBytes     int64
	Deliver      func(context.Context, OutboxItem) error
}

type OutboxWatcher struct {
	cfg OutboxWatcherConfig

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	files  map[string]*outboxFileState
}

type outboxFileState struct {
	size        int64
	modTime     time.Time
	stableSince time.Time
	failures    int
	nextAttempt time.Time
}

// NewOutboxWatcher returns a watcher configured with defaults for omitted
// timing and size values. Call Start to begin polling in the background.
func NewOutboxWatcher(cfg OutboxWatcherConfig) *OutboxWatcher {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultOutboxPollInterval
	}
	if cfg.StableFor <= 0 {
		cfg.StableFor = DefaultOutboxStableFor
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultOutboxMaxBytes
	}
	cfg.Dir = filepath.Clean(strings.TrimSpace(cfg.Dir))
	return &OutboxWatcher{
		cfg:   cfg,
		files: make(map[string]*outboxFileState),
	}
}

// Start begins polling the configured outbox directory. It is safe to call
// Start more than once; subsequent calls while running are ignored.
func (w *OutboxWatcher) Start(ctx context.Context) {
	if w == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)

	w.mu.Lock()
	if w.cancel != nil {
		w.mu.Unlock()
		cancel()
		return
	}
	done := make(chan struct{})
	w.cancel = cancel
	w.done = done
	w.mu.Unlock()

	go func() {
		defer close(done)
		defer func() {
			w.mu.Lock()
			if w.done == done {
				w.cancel = nil
				w.done = nil
			}
			w.mu.Unlock()
		}()

		w.scanOnce(ctx)

		ticker := time.NewTicker(w.cfg.PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.scanOnce(ctx)
			}
		}
	}()
}

// Stop cancels the watcher and waits for its goroutine to exit.
func (w *OutboxWatcher) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	cancel := w.cancel
	done := w.done
	w.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
}

// StopWithTimeout cancels the watcher and waits up to timeout for its
// goroutine to exit. It returns false when the watcher is still shutting down.
func (w *OutboxWatcher) StopWithTimeout(timeout time.Duration) bool {
	if w == nil {
		return true
	}
	w.mu.Lock()
	cancel := w.cancel
	done := w.done
	w.mu.Unlock()
	if cancel == nil {
		return true
	}
	cancel()
	if done == nil {
		return true
	}
	if timeout <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// SessionOutboxDir returns the session-specific outbox path. Project and
// session components are sanitized so they cannot escape dataDir.
func SessionOutboxDir(dataDir, projectName, sessionID string) string {
	return filepath.Join(
		filepath.Clean(strings.TrimSpace(dataDir)),
		"outbox",
		sanitizeOutboxComponent(projectName, "project"),
		sanitizeOutboxComponent(sessionID, "default"),
	)
}

func sanitizeOutboxComponent(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}

	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		allowed := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' || r == ':'
		if allowed {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}

	out := strings.Trim(b.String(), "._")
	if out == "" || out == "." || out == ".." {
		return fallback
	}
	if len(out) > 120 {
		out = strings.TrimRight(out[:120], "._")
		if out == "" {
			return fallback
		}
	}
	return out
}

func (w *OutboxWatcher) scanOnce(ctx context.Context) {
	if w == nil {
		return
	}
	if err := w.ensureDirs(); err != nil {
		slog.Warn("outbox: ensure directories failed", "dir", w.cfg.Dir, "error", err)
		return
	}

	dir, err := openOutboxDirNoFollow(w.cfg.Dir)
	if err != nil {
		slog.Warn("outbox: read directory failed", "dir", w.cfg.Dir, "error", err)
		return
	}
	defer dir.Close()

	entries, err := dir.ReadDir(-1)
	if err != nil {
		slog.Warn("outbox: read directory failed", "dir", w.cfg.Dir, "error", err)
		return
	}

	now := time.Now()
	present := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if shouldIgnoreOutboxEntry(entry) {
			continue
		}

		path := filepath.Join(w.cfg.Dir, entry.Name())
		present[path] = struct{}{}

		info, err := outboxFileInfoAtNoFollow(dir, entry.Name())
		if err != nil {
			slog.Warn("outbox: stat file failed", "path", path, "error", err)
			continue
		}

		if info.Size() > w.cfg.MaxBytes {
			if err := w.moveToFailed(dir, entry.Name()); err != nil {
				slog.Warn("outbox: oversized file could not be archived", "path", path, "size", info.Size(), "max_bytes", w.cfg.MaxBytes, "error", err)
				w.backoff(path, info, now)
				continue
			}
			slog.Warn("outbox: oversized file moved to failed", "path", path, "size", info.Size(), "max_bytes", w.cfg.MaxBytes)
			delete(w.files, path)
			continue
		}

		state := w.files[path]
		if state == nil {
			w.files[path] = &outboxFileState{
				size:        info.Size(),
				modTime:     info.ModTime(),
				stableSince: now,
			}
			continue
		}
		if state.size != info.Size() || !state.modTime.Equal(info.ModTime()) {
			state.size = info.Size()
			state.modTime = info.ModTime()
			state.stableSince = now
			state.failures = 0
			state.nextAttempt = time.Time{}
			continue
		}
		if now.Sub(state.stableSince) < w.cfg.StableFor {
			continue
		}
		if !state.nextAttempt.IsZero() && now.Before(state.nextAttempt) {
			continue
		}

		if err := w.deliver(ctx, dir, path, entry.Name(), info); err != nil {
			slog.Warn("outbox: deliver failed", "path", path, "error", err)
			w.backoff(path, info, now)
			continue
		}
		delete(w.files, path)
	}

	for path := range w.files {
		if _, ok := present[path]; !ok {
			delete(w.files, path)
		}
	}
}

func (w *OutboxWatcher) ensureDirs() error {
	if w.cfg.Dir == "" || w.cfg.Dir == "." {
		return errors.New("outbox directory is required")
	}
	root, err := ensureOutboxDirNoFollow(w.cfg.Dir)
	if err != nil {
		return err
	}
	defer root.Close()

	for _, dir := range []string{".sent", ".failed"} {
		opened, err := ensureOutboxChildDirNoFollow(root, dir)
		if err != nil {
			return fmt.Errorf("unsafe outbox directory %s: %w", filepath.Join(w.cfg.Dir, dir), err)
		}
		if err := opened.Close(); err != nil {
			return fmt.Errorf("close outbox directory %s: %w", filepath.Join(w.cfg.Dir, dir), err)
		}
	}
	return nil
}

func shouldIgnoreOutboxEntry(entry fs.DirEntry) bool {
	name := entry.Name()
	if name == "" || strings.HasPrefix(name, ".") {
		return true
	}
	if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
		return true
	}
	lower := strings.ToLower(name)
	for _, suffix := range []string{".tmp", ".part", ".crdownload", ".download"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func (w *OutboxWatcher) deliver(ctx context.Context, dir *os.File, path, name string, info fs.FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	item, err := outboxItemFromFileAt(dir, path, name, info, w.cfg.MaxBytes)
	if err != nil {
		return err
	}
	if w.cfg.Deliver == nil {
		return errors.New("outbox deliver function is required")
	}
	if err := w.cfg.Deliver(ctx, item); err != nil {
		return err
	}
	if err := w.moveToSent(dir, name); err != nil {
		return fmt.Errorf("archive delivered file: %w", err)
	}
	return nil
}

func outboxItemFromFileAt(dir *os.File, path, name string, info fs.FileInfo, maxBytes int64) (OutboxItem, error) {
	if info == nil {
		return OutboxItem{}, errors.New("missing file info")
	}
	if info.Size() < 0 {
		return OutboxItem{}, fmt.Errorf("invalid file size %d", info.Size())
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return OutboxItem{}, fmt.Errorf("file exceeds size limit: %d > %d bytes", info.Size(), maxBytes)
	}
	data, err := readOutboxFileAtNoFollow(dir, name, info, maxBytes)
	if err != nil {
		return OutboxItem{}, fmt.Errorf("read file: %w", err)
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return OutboxItem{}, fmt.Errorf("file exceeds size limit: %d > %d bytes", len(data), maxBytes)
	}
	return outboxItemFromData(path, name, data), nil
}

func outboxItemFromFile(path, name string, size, maxBytes int64) (OutboxItem, error) {
	if size < 0 {
		return OutboxItem{}, fmt.Errorf("invalid file size %d", size)
	}
	if maxBytes > 0 && size > maxBytes {
		return OutboxItem{}, fmt.Errorf("file exceeds size limit: %d > %d bytes", size, maxBytes)
	}
	data, err := readOutboxFileNoFollow(path, size, maxBytes)
	if err != nil {
		return OutboxItem{}, fmt.Errorf("read file: %w", err)
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return OutboxItem{}, fmt.Errorf("file exceeds size limit: %d > %d bytes", len(data), maxBytes)
	}
	return outboxItemFromData(path, name, data), nil
}

func outboxItemFromData(path, name string, data []byte) OutboxItem {
	mimeType := detectOutboxMimeType(name, data)
	item := OutboxItem{
		Path:     path,
		FileName: name,
		MimeType: mimeType,
	}
	if isOutboxImageMime(mimeType) {
		item.Image = &ImageAttachment{MimeType: mimeType, Data: data, FileName: name}
		return item
	}
	item.File = &FileAttachment{MimeType: mimeType, Data: data, FileName: name}
	return item
}

func readOutboxFileAtNoFollow(dir *os.File, name string, expected fs.FileInfo, maxBytes int64) ([]byte, error) {
	if expected == nil {
		return nil, errors.New("missing expected file info")
	}
	if err := validateOutboxRegularFile(expected); err != nil {
		return nil, err
	}
	if maxBytes > 0 && expected.Size() > maxBytes {
		return nil, fmt.Errorf("file exceeds size limit: %d > %d bytes", expected.Size(), maxBytes)
	}

	file, opened, err := openOutboxFileAtNoFollow(dir, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	if !outboxSameFileInfo(expected, opened) {
		return nil, errors.New("file changed before open")
	}
	if expected.Size() != opened.Size() {
		return nil, fmt.Errorf("file changed before read: size %d != %d", opened.Size(), expected.Size())
	}
	if !expected.ModTime().Equal(opened.ModTime()) {
		return nil, errors.New("file changed before read: modtime changed")
	}

	data, readInfo, err := readAllFromStableOutboxFile(file, opened, maxBytes)
	if err != nil {
		return nil, err
	}
	if !outboxSameFileInfo(opened, readInfo) {
		return nil, errors.New("file changed while reading")
	}
	return data, nil
}

func readOutboxFileNoFollow(path string, expectedSize, maxBytes int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("lstat: %w", err)
	}
	if err := validateOutboxRegularFile(before); err != nil {
		return nil, err
	}
	if expectedSize >= 0 && before.Size() != expectedSize {
		return nil, fmt.Errorf("file changed before read: size %d != %d", before.Size(), expectedSize)
	}
	if maxBytes > 0 && before.Size() > maxBytes {
		return nil, fmt.Errorf("file exceeds size limit: %d > %d bytes", before.Size(), maxBytes)
	}

	file, err := openOutboxFileNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	after, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("fstat: %w", err)
	}
	if err := validateOutboxRegularFile(after); err != nil {
		return nil, err
	}
	if !outboxSameFileInfo(before, after) {
		return nil, errors.New("file changed before open")
	}
	if expectedSize >= 0 && after.Size() != expectedSize {
		return nil, fmt.Errorf("file changed before read: size %d != %d", after.Size(), expectedSize)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("file changed before read: modtime changed")
	}

	data, _, err := readAllFromStableOutboxFile(file, after, maxBytes)
	return data, nil
}

func validateOutboxChildName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("invalid outbox file name %q", name)
	}
	return nil
}

func readAllFromStableOutboxFile(file *os.File, opened fs.FileInfo, maxBytes int64) ([]byte, fs.FileInfo, error) {
	reader := io.Reader(file)
	if maxBytes > 0 {
		reader = io.LimitReader(file, maxBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, err
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, nil, fmt.Errorf("file exceeds size limit: %d > %d bytes", len(data), maxBytes)
	}

	readInfo, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("fstat after read: %w", err)
	}
	if err := validateOutboxRegularFile(readInfo); err != nil {
		return nil, nil, err
	}
	if !outboxSameFileInfo(opened, readInfo) {
		return nil, nil, errors.New("file changed while reading")
	}
	if opened.Size() != readInfo.Size() {
		return nil, nil, fmt.Errorf("file changed while reading: size %d != %d", readInfo.Size(), opened.Size())
	}
	if !opened.ModTime().Equal(readInfo.ModTime()) {
		return nil, nil, errors.New("file changed while reading: modtime changed")
	}
	if int64(len(data)) != readInfo.Size() {
		return nil, nil, fmt.Errorf("file changed while reading: read %d bytes, expected %d", len(data), readInfo.Size())
	}
	return data, readInfo, nil
}

func validateOutboxRegularFile(info fs.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to read symlink")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to read non-regular file: mode %s", info.Mode())
	}
	if linkCount, ok := outboxHardlinkCount(info); ok && linkCount > 1 {
		return fmt.Errorf("refusing to read hardlinked file: links=%d", linkCount)
	}
	return nil
}

func detectOutboxMimeType(fileName string, data []byte) string {
	byExt := strings.TrimSpace(mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName))))
	if base := mimeBaseType(byExt); isOutboxImageMime(base) {
		return base
	}
	if sniff := sniffOutboxMimeType(data); isOutboxImageMime(sniff) {
		return sniff
	}
	if byExt != "" {
		return byExt
	}
	if sniff := sniffOutboxMimeType(data); sniff != "" {
		return sniff
	}
	return "application/octet-stream"
}

func sniffOutboxMimeType(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	sniff := data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	return mimeBaseType(http.DetectContentType(sniff))
}

func mimeBaseType(mimeType string) string {
	if mediaType, _, err := mime.ParseMediaType(mimeType); err == nil {
		return mediaType
	}
	if mediaType, _, ok := strings.Cut(mimeType, ";"); ok {
		return strings.TrimSpace(mediaType)
	}
	return strings.TrimSpace(mimeType)
}

func isOutboxImageMime(mimeType string) bool {
	switch strings.ToLower(mimeBaseType(mimeType)) {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func (w *OutboxWatcher) backoff(path string, info fs.FileInfo, now time.Time) {
	state := w.files[path]
	if state == nil {
		state = &outboxFileState{}
		w.files[path] = state
	}
	state.size = info.Size()
	state.modTime = info.ModTime()
	if state.stableSince.IsZero() {
		state.stableSince = now
	}
	state.failures++
	state.nextAttempt = now.Add(w.retryDelay(state.failures))
}

func (w *OutboxWatcher) retryDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := w.cfg.PollInterval * time.Duration(1<<outboxMinInt(failures-1, 6))
	if delay <= 0 {
		delay = DefaultOutboxPollInterval
	}
	delay *= 5
	if delay > time.Minute {
		return time.Minute
	}
	return delay
}

func (w *OutboxWatcher) moveToSent(dir *os.File, name string) error {
	return moveOutboxFileAt(dir, ".sent", name)
}

func (w *OutboxWatcher) moveToFailed(dir *os.File, name string) error {
	return moveOutboxFileAt(dir, ".failed", name)
}

func uniqueOutboxArchivePath(dir, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		name = "file"
	}
	name = filepath.Base(name)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" {
		stem = "file"
	}
	for i := 0; i < 10000; i++ {
		candidateName := name
		if i > 0 {
			candidateName = fmt.Sprintf("%s-%d%s", stem, i+1, ext)
		}
		candidate := filepath.Join(dir, candidateName)
		_, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate archive name for %q", name)
}

func outboxMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
