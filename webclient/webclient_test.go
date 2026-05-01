package webclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestHTTPPostPersistsAndCallsHandler(t *testing.T) {
	tmp := t.TempDir()
	s, err := NewServer(Options{
		Host:    "127.0.0.1",
		Port:    9831,
		DataDir: tmp,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	got := make(chan *core.Message, 1)
	p := s.Platform("proj")
	if err := p.Start(func(_ core.Platform, msg *core.Message) {
		got <- msg
	}); err != nil {
		t.Fatalf("platform.Start: %v", err)
	}

	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	body := `{"content":"hello"}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/projects/proj/sessions/s1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("POST status=%d body=%s", res.StatusCode, string(b))
	}

	select {
	case msg := <-got:
		if msg.Platform != "webclient" {
			t.Fatalf("msg.Platform=%q", msg.Platform)
		}
		if msg.Content != "hello" {
			t.Fatalf("msg.Content=%q", msg.Content)
		}
		if msg.SessionKey != "webclient:proj:s1" {
			t.Fatalf("msg.SessionKey=%q", msg.SessionKey)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for handler")
	}

	// History includes the user message.
	res2, err := ts.Client().Get(ts.URL + "/api/projects/proj/sessions/s1/messages")
	if err != nil {
		t.Fatalf("GET messages: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res2.Body)
		t.Fatalf("GET status=%d body=%s", res2.StatusCode, string(b))
	}
	var msgs []map[string]any
	if err := json.NewDecoder(res2.Body).Decode(&msgs); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages len=%d", len(msgs))
	}
	if role, _ := msgs[0]["role"].(string); role != "user" {
		t.Fatalf("role=%v", msgs[0]["role"])
	}
	if content, _ := msgs[0]["content"].(string); content != "hello" {
		t.Fatalf("content=%v", msgs[0]["content"])
	}
}

func TestPlatformSendImageAndAttachmentFetch(t *testing.T) {
	tmp := t.TempDir()
	s, err := NewServer(Options{
		Host:    "127.0.0.1",
		Port:    9831,
		DataDir: tmp,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	p := s.Platform("proj")
	if err := p.Start(func(_ core.Platform, _ *core.Message) {}); err != nil {
		t.Fatalf("platform.Start: %v", err)
	}

	rc := replyContext{Project: "proj", Session: "s1"}
	imgData := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a} // PNG header
	if err := p.(core.ImageSender).SendImage(context.Background(), rc, core.ImageAttachment{
		MimeType: "image/png",
		Data:     imgData,
		FileName: "t.png",
	}); err != nil {
		t.Fatalf("SendImage: %v", err)
	}

	// Read back messages from store via API.
	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)
	res, err := ts.Client().Get(ts.URL + "/api/projects/proj/sessions/s1/messages")
	if err != nil {
		t.Fatalf("GET messages: %v", err)
	}
	defer res.Body.Close()
	var msgs []struct {
		Role        string `json:"role"`
		Attachments []struct {
			ID      string `json:"id"`
			URL     string `json:"url"`
			Mime    string `json:"mime_type"`
			File    string `json:"file_name"`
			Kind    string `json:"kind"`
			Size    int    `json:"size"`
			Ignored any    `json:"-"`
		} `json:"attachments"`
	}
	if err := json.NewDecoder(res.Body).Decode(&msgs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len=%d", len(msgs))
	}
	if msgs[0].Role != "assistant" {
		t.Fatalf("role=%q", msgs[0].Role)
	}
	if len(msgs[0].Attachments) != 1 {
		t.Fatalf("attachments len=%d", len(msgs[0].Attachments))
	}
	att := msgs[0].Attachments[0]
	if att.Kind != "image" {
		t.Fatalf("kind=%q", att.Kind)
	}
	if !strings.HasPrefix(att.URL, "/attachments/") {
		t.Fatalf("url=%q", att.URL)
	}
	if att.ID == "" {
		t.Fatalf("id empty")
	}

	// Fetch the attachment bytes.
	res2, err := ts.Client().Get(ts.URL + att.URL)
	if err != nil {
		t.Fatalf("GET attachment: %v", err)
	}
	defer res2.Body.Close()
	b, _ := io.ReadAll(res2.Body)
	if !bytes.Equal(b, imgData) {
		t.Fatalf("attachment bytes mismatch: got %x want %x", b, imgData)
	}
	if gotCT := res2.Header.Get("Content-Type"); gotCT != "image/png" {
		t.Fatalf("Content-Type=%q", gotCT)
	}

	// Ensure the data is on disk in expected subtree.
	wantPrefix := filepath.Join(tmp, "webclient", "attachments")
	_, path, err := s.store.OpenAttachment(att.ID)
	if err != nil {
		t.Fatalf("store.OpenAttachment: %v", err)
	}
	if !strings.HasPrefix(path, wantPrefix) {
		t.Fatalf("attachment path=%q want prefix %q", path, wantPrefix)
	}
}
