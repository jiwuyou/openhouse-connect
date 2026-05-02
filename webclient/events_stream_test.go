package webclient

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/webclient/internal/store"
)

func readSSEEvent(t *testing.T, r *bufio.Reader, timeout time.Duration) (string, []byte) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	eventName := ""
	var dataLines []string

	for {
		if time.Now().After(deadline) {
			t.Fatalf("timeout reading SSE event (event=%q data_lines=%d)", eventName, len(dataLines))
		}
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// End of event.
			if eventName == "" {
				eventName = "message"
			}
			return eventName, []byte(strings.Join(dataLines, "\n"))
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
	}
}

func TestEventsStream_PublishesMessagesAndRunEvents(t *testing.T) {
	t.Parallel()

	s, err := NewServer(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.handler)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/projects/proj/sessions/s1/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("events status=%d body=%s", resp.StatusCode, string(b))
	}

	reader := bufio.NewReader(resp.Body)

	// Initial comment ping (": ok\n\n").
	_, _ = reader.ReadString('\n')
	_, _ = reader.ReadString('\n')

	// Publish a message.
	msg, err := s.store.AppendMessage("proj", "s1", store.Message{Role: store.RoleAssistant, Content: "hi"})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	s.events.Publish("proj", "s1", msg)

	// Read message event.
	typ1, data1 := readSSEEvent(t, reader, 2*time.Second)
	if typ1 != "message" {
		t.Fatalf("event1=%q want message", typ1)
	}
	var gotMsg store.Message
	if err := json.Unmarshal(data1, &gotMsg); err != nil {
		t.Fatalf("unmarshal message: %v data=%s", err, string(data1))
	}
	if gotMsg.Content != "hi" {
		t.Fatalf("message content=%q want hi", gotMsg.Content)
	}

	// Publish a run_event.
	ev, err := s.store.AppendRunEvent("proj", "s1", store.RunEvent{
		RunID:         "r1",
		UserMessageID: "r1",
		SessionID:     "s1",
		Type:          "typing_stop",
		Status:        "completed",
		CreatedAt:     time.Now().UTC(),
		Timestamp:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("AppendRunEvent: %v", err)
	}
	s.runEvts.Publish("proj", "s1", ev)

	// Read run_event event.
	typ2, data2 := readSSEEvent(t, reader, 2*time.Second)
	if typ2 != "run_event" {
		t.Fatalf("event2=%q want run_event", typ2)
	}
	var gotEv store.RunEvent
	if err := json.Unmarshal(data2, &gotEv); err != nil {
		t.Fatalf("unmarshal run_event: %v data=%s", err, string(data2))
	}
	if gotEv.Type != "typing_stop" || gotEv.Status != "completed" {
		t.Fatalf("run_event=%#v", gotEv)
	}
	if gotEv.Seq <= 0 {
		t.Fatalf("run_event seq=%d want positive", gotEv.Seq)
	}
}
