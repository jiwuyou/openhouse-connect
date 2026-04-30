package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionOutboxDirSanitizesPathComponents(t *testing.T) {
	base := t.TempDir()
	dir := SessionOutboxDir(base, "../proj/../../evil", "/../../session:s1")

	rel, err := filepath.Rel(filepath.Join(base, "outbox"), dir)
	if err != nil {
		t.Fatalf("Rel() error: %v", err)
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		t.Fatalf("outbox dir escaped base: %q", dir)
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) != 2 {
		t.Fatalf("relative path parts = %v, want project/session", parts)
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `/\`) {
			t.Fatalf("unsafe path component %q in %q", part, dir)
		}
	}
}

func TestOutboxWatcherIgnoresUnsupportedEntries(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, ".hidden.png"), []byte("hidden"))
	writeTestFile(t, filepath.Join(dir, "skip.tmp"), []byte("tmp"))
	writeTestFile(t, filepath.Join(dir, "skip.part"), []byte("part"))
	writeTestFile(t, filepath.Join(dir, "skip.crdownload"), []byte("download"))
	writeTestFile(t, filepath.Join(dir, "skip.download"), []byte("download"))
	writeTestFile(t, filepath.Join(dir, "ready.txt"), []byte("ready"))
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatalf("Mkdir() error: %v", err)
	}
	target := filepath.Join(dir, "target.txt")
	writeTestFile(t, target, []byte("target"))
	if err := os.Symlink(target, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("Symlink() error: %v", err)
	}

	var delivered []OutboxItem
	w := NewOutboxWatcher(OutboxWatcherConfig{
		Dir:          dir,
		PollInterval: time.Millisecond,
		StableFor:    time.Millisecond,
		Deliver: func(_ context.Context, item OutboxItem) error {
			delivered = append(delivered, item)
			return nil
		},
	})

	w.scanOnce(context.Background())
	time.Sleep(2 * time.Millisecond)
	w.scanOnce(context.Background())

	if len(delivered) != 2 {
		t.Fatalf("delivered %d items, want ready.txt and target.txt", len(delivered))
	}
	names := map[string]bool{}
	for _, item := range delivered {
		names[item.FileName] = true
	}
	for _, want := range []string{"ready.txt", "target.txt"} {
		if !names[want] {
			t.Fatalf("delivered names = %v, want %s", names, want)
		}
	}
	for _, ignored := range []string{".hidden.png", "skip.tmp", "skip.part", "skip.crdownload", "skip.download", "link.txt"} {
		if _, err := os.Lstat(filepath.Join(dir, ignored)); err != nil {
			t.Fatalf("ignored file %s should remain in place: %v", ignored, err)
		}
	}
}

func TestOutboxItemFromFileRejectsSymlinkReplacedBeforeDelivery(t *testing.T) {
	dir := t.TempDir()
	externalDir := t.TempDir()
	path := filepath.Join(dir, "result.txt")
	external := filepath.Join(externalDir, "secret.txt")

	writeTestFile(t, path, []byte("seen"))
	writeTestFile(t, external, []byte("external secret"))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}
	if err := os.Symlink(external, path); err != nil {
		t.Fatalf("Symlink() error: %v", err)
	}

	item, err := outboxItemFromFile(path, "result.txt", info.Size(), defaultOutboxMaxBytes)
	if err == nil {
		t.Fatalf("outboxItemFromFile() delivered %#v, want symlink rejection", item)
	}
	if item.File != nil && strings.Contains(string(item.File.Data), "external secret") {
		t.Fatalf("delivered external symlink content")
	}
}

func TestOutboxWatcherRejectsSymlinkedOutboxDirectory(t *testing.T) {
	base := t.TempDir()
	external := t.TempDir()
	writeTestFile(t, filepath.Join(external, "leak.txt"), []byte("secret"))
	link := filepath.Join(base, "outbox-link")
	if err := os.Symlink(external, link); err != nil {
		t.Fatalf("Symlink() error: %v", err)
	}

	var delivered []OutboxItem
	w := NewOutboxWatcher(OutboxWatcherConfig{
		Dir:          link,
		PollInterval: time.Millisecond,
		StableFor:    time.Millisecond,
		Deliver: func(_ context.Context, item OutboxItem) error {
			delivered = append(delivered, item)
			return nil
		},
	})

	w.scanOnce(context.Background())
	time.Sleep(2 * time.Millisecond)
	w.scanOnce(context.Background())

	if len(delivered) != 0 {
		t.Fatalf("delivered from symlinked outbox dir: %#v", delivered)
	}
}

func TestOutboxWatcherRejectsSymlinkedParentDirectory(t *testing.T) {
	base := t.TempDir()
	external := t.TempDir()
	linkParent := filepath.Join(base, "project-link")
	if err := os.Symlink(external, linkParent); err != nil {
		t.Fatalf("Symlink() error: %v", err)
	}
	outboxDir := filepath.Join(linkParent, "session")
	if err := os.MkdirAll(filepath.Join(external, "session"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error: %v", err)
	}
	writeTestFile(t, filepath.Join(external, "session", "leak.txt"), []byte("secret"))

	var delivered []OutboxItem
	w := NewOutboxWatcher(OutboxWatcherConfig{
		Dir:          outboxDir,
		PollInterval: time.Millisecond,
		StableFor:    time.Millisecond,
		Deliver: func(_ context.Context, item OutboxItem) error {
			delivered = append(delivered, item)
			return nil
		},
	})

	w.scanOnce(context.Background())
	time.Sleep(2 * time.Millisecond)
	w.scanOnce(context.Background())

	if len(delivered) != 0 {
		t.Fatalf("delivered through symlinked parent dir: %#v", delivered)
	}
}

func TestOutboxWatcherRejectsHardlinkedFile(t *testing.T) {
	dir := t.TempDir()
	external := filepath.Join(t.TempDir(), "source.txt")
	path := filepath.Join(dir, "linked.txt")
	writeTestFile(t, external, []byte("secret"))
	if err := os.Link(external, path); err != nil {
		t.Skipf("hardlinks unsupported: %v", err)
	}

	deliveries := 0
	w := NewOutboxWatcher(OutboxWatcherConfig{
		Dir:          dir,
		PollInterval: time.Millisecond,
		StableFor:    time.Millisecond,
		Deliver: func(context.Context, OutboxItem) error {
			deliveries++
			return nil
		},
	})

	w.scanOnce(context.Background())
	time.Sleep(2 * time.Millisecond)
	w.scanOnce(context.Background())

	if deliveries != 0 {
		t.Fatalf("deliveries = %d, want hardlinked file rejected", deliveries)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("hardlinked file should remain in place: %v", err)
	}
}

func TestOutboxWatcherWaitsForStableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.txt")
	writeTestFile(t, path, []byte("a"))

	deliveries := 0
	w := NewOutboxWatcher(OutboxWatcherConfig{
		Dir:          dir,
		PollInterval: time.Millisecond,
		StableFor:    25 * time.Millisecond,
		Deliver: func(_ context.Context, _ OutboxItem) error {
			deliveries++
			return nil
		},
	})

	w.scanOnce(context.Background())
	time.Sleep(5 * time.Millisecond)
	if deliveries != 0 {
		t.Fatalf("delivered before stable window")
	}

	writeTestFile(t, path, []byte("ab"))
	w.scanOnce(context.Background())
	time.Sleep(10 * time.Millisecond)
	w.scanOnce(context.Background())
	if deliveries != 0 {
		t.Fatalf("delivered before rewritten file became stable")
	}

	time.Sleep(30 * time.Millisecond)
	w.scanOnce(context.Background())
	if deliveries != 1 {
		t.Fatalf("deliveries = %d, want 1", deliveries)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source Stat error = %v, want not exist after success", err)
	}
}

func TestOutboxWatcherClassifiesImagesAndFiles(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "chart.png"), []byte("\x89PNG\r\n\x1a\nimage"))
	writeTestFile(t, filepath.Join(dir, "report.txt"), []byte("plain text"))

	var delivered []OutboxItem
	w := NewOutboxWatcher(OutboxWatcherConfig{
		Dir:          dir,
		PollInterval: time.Millisecond,
		StableFor:    time.Millisecond,
		Deliver: func(_ context.Context, item OutboxItem) error {
			delivered = append(delivered, item)
			return nil
		},
	})

	w.scanOnce(context.Background())
	time.Sleep(2 * time.Millisecond)
	w.scanOnce(context.Background())

	if len(delivered) != 2 {
		t.Fatalf("delivered %d items, want 2", len(delivered))
	}
	byName := make(map[string]OutboxItem)
	for _, item := range delivered {
		byName[item.FileName] = item
	}

	img := byName["chart.png"]
	if img.Image == nil || img.File != nil {
		t.Fatalf("chart.png item = %#v, want image", img)
	}
	if img.MimeType != "image/png" || img.Image.MimeType != "image/png" || string(img.Image.Data) != "\x89PNG\r\n\x1a\nimage" {
		t.Fatalf("chart.png classification/data = %#v", img)
	}

	file := byName["report.txt"]
	if file.File == nil || file.Image != nil {
		t.Fatalf("report.txt item = %#v, want file", file)
	}
	if file.MimeType == "" || file.File.FileName != "report.txt" || string(file.File.Data) != "plain text" {
		t.Fatalf("report.txt classification/data = %#v", file)
	}
}

func TestOutboxWatcherStopWithTimeoutReturnsFalseWhenDeliveryBlocks(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "block.txt"), []byte("block"))

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	w := NewOutboxWatcher(OutboxWatcherConfig{
		Dir:          dir,
		PollInterval: time.Millisecond,
		StableFor:    time.Nanosecond,
		Deliver: func(context.Context, OutboxItem) error {
			enteredOnce.Do(func() { close(entered) })
			<-release
			return nil
		},
	})
	defer func() {
		releaseOnce.Do(func() { close(release) })
		w.StopWithTimeout(time.Second)
	}()

	w.Start(context.Background())
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("watcher did not enter blocking delivery")
	}

	start := time.Now()
	if w.StopWithTimeout(20 * time.Millisecond) {
		t.Fatal("StopWithTimeout() = true, want false while delivery blocks")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("StopWithTimeout() took %s, want bounded return", elapsed)
	}

	releaseOnce.Do(func() { close(release) })
	if !w.StopWithTimeout(time.Second) {
		t.Fatal("StopWithTimeout() = false after delivery was released, want true")
	}
}

func TestOutboxWatcherMovesSuccessfulDeliveryWithCollisionSafeName(t *testing.T) {
	dir := t.TempDir()
	sentDir := filepath.Join(dir, ".sent")
	if err := os.Mkdir(sentDir, 0o755); err != nil {
		t.Fatalf("Mkdir() error: %v", err)
	}
	writeTestFile(t, filepath.Join(sentDir, "report.txt"), []byte("old"))
	writeTestFile(t, filepath.Join(dir, "report.txt"), []byte("new"))

	w := NewOutboxWatcher(OutboxWatcherConfig{
		Dir:          dir,
		PollInterval: time.Millisecond,
		StableFor:    time.Millisecond,
		Deliver:      func(context.Context, OutboxItem) error { return nil },
	})

	w.scanOnce(context.Background())
	time.Sleep(2 * time.Millisecond)
	w.scanOnce(context.Background())

	old, err := os.ReadFile(filepath.Join(sentDir, "report.txt"))
	if err != nil {
		t.Fatalf("ReadFile(old) error: %v", err)
	}
	if string(old) != "old" {
		t.Fatalf("existing archive overwritten: %q", string(old))
	}
	newData, err := os.ReadFile(filepath.Join(sentDir, "report-2.txt"))
	if err != nil {
		t.Fatalf("ReadFile(collision copy) error: %v", err)
	}
	if string(newData) != "new" {
		t.Fatalf("collision archive content = %q, want new", string(newData))
	}
}

func TestOutboxWatcherRetryBackoffAndOversize(t *testing.T) {
	t.Run("delivery error backs off and leaves file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "retry.txt")
		writeTestFile(t, path, []byte("retry"))

		var mu sync.Mutex
		attempts := 0
		w := NewOutboxWatcher(OutboxWatcherConfig{
			Dir:          dir,
			PollInterval: 2 * time.Millisecond,
			StableFor:    time.Millisecond,
			Deliver: func(context.Context, OutboxItem) error {
				mu.Lock()
				defer mu.Unlock()
				attempts++
				return errors.New("platform unavailable")
			},
		})

		w.scanOnce(context.Background())
		time.Sleep(2 * time.Millisecond)
		w.scanOnce(context.Background())
		if got := readAttempts(&mu, &attempts); got != 1 {
			t.Fatalf("attempts = %d, want 1", got)
		}
		w.scanOnce(context.Background())
		if got := readAttempts(&mu, &attempts); got != 1 {
			t.Fatalf("attempts after immediate rescan = %d, want 1", got)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("source file should remain after delivery error: %v", err)
		}

		time.Sleep(15 * time.Millisecond)
		w.scanOnce(context.Background())
		if got := readAttempts(&mu, &attempts); got != 2 {
			t.Fatalf("attempts after backoff = %d, want 2", got)
		}
	})

	t.Run("oversize moves to failed without delivery", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "big.bin")
		writeTestFile(t, path, []byte("12345"))
		attempts := 0
		w := NewOutboxWatcher(OutboxWatcherConfig{
			Dir:          dir,
			PollInterval: time.Millisecond,
			StableFor:    time.Millisecond,
			MaxBytes:     4,
			Deliver: func(context.Context, OutboxItem) error {
				attempts++
				return nil
			},
		})

		w.scanOnce(context.Background())
		w.scanOnce(context.Background())
		if attempts != 0 {
			t.Fatalf("attempts = %d, want 0", attempts)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source Stat error = %v, want not exist", err)
		}
		failed, err := os.ReadFile(filepath.Join(dir, ".failed", "big.bin"))
		if err != nil {
			t.Fatalf("ReadFile(failed) error: %v", err)
		}
		if string(failed) != "12345" {
			t.Fatalf("failed file content = %q", string(failed))
		}
	})
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error: %v", path, err)
	}
}

func readAttempts(mu *sync.Mutex, attempts *int) int {
	mu.Lock()
	defer mu.Unlock()
	return *attempts
}
