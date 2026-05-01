package codex

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCollectCodexGeneratedImages(t *testing.T) {
	codexHome := t.TempDir()
	sessionID := "session-123"
	imageDir := filepath.Join(codexHome, "generated_images", sessionID)
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	imagePath := filepath.Join(imageDir, "ig_test.png")
	if err := os.WriteFile(imagePath, []byte("\x89PNG\r\n\x1a\nimage"), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}

	delivered := make(map[string]struct{})
	images := collectCodexGeneratedImages(codexHome, sessionID, time.Now().Add(-time.Minute), delivered)
	if len(images) != 1 {
		t.Fatalf("images = %#v, want one", images)
	}
	if images[0].MimeType != "image/png" || images[0].FileName != "ig_test.png" {
		t.Fatalf("image = %#v", images[0])
	}

	again := collectCodexGeneratedImages(codexHome, sessionID, time.Now().Add(-time.Minute), delivered)
	if len(again) != 0 {
		t.Fatalf("second collect = %#v, want none", again)
	}
}

func TestCollectCodexGeneratedImages_IgnoresOldImages(t *testing.T) {
	codexHome := t.TempDir()
	sessionID := "session-123"
	imageDir := filepath.Join(codexHome, "generated_images", sessionID)
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	imagePath := filepath.Join(imageDir, "old.png")
	if err := os.WriteFile(imagePath, []byte("\x89PNG\r\n\x1a\nimage"), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(imagePath, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	images := collectCodexGeneratedImages(codexHome, sessionID, time.Now(), nil)
	if len(images) != 0 {
		t.Fatalf("images = %#v, want none", images)
	}
}
