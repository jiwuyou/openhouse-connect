package core

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleSend_AllowsAttachmentOnly(t *testing.T) {
	engine := NewEngine("test", &stubAgent{}, []Platform{&stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}}, "", LangEnglish)
	engine.interactiveStates["session-1"] = &interactiveState{
		platform: &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}},
		replyCtx: "reply-ctx",
	}

	api := &APIServer{engines: map[string]*Engine{"test": engine}}
	reqBody := SendRequest{
		Project:    "test",
		SessionKey: "session-1",
		Images: []ImageAttachment{{
			MimeType: "image/png",
			Data:     []byte("img"),
			FileName: "chart.png",
		}},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/send", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleSend(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleSend_RoutesWithSessionID(t *testing.T) {
	baseKey := "webnew:web-admin:test"
	p1 := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "webnew"}}
	p2 := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "webnew"}}
	engine := NewEngine("test", &stubAgent{}, []Platform{p1, p2}, "", LangEnglish)
	engine.interactiveStates[interactiveKeyWithSessionID(baseKey, "s1")] = &interactiveState{
		platform: p1,
		replyCtx: "ctx-s1",
	}
	engine.interactiveStates[interactiveKeyWithSessionID(baseKey, "s2")] = &interactiveState{
		platform: p2,
		replyCtx: "ctx-s2",
	}

	api := &APIServer{engines: map[string]*Engine{"test": engine}}
	reqBody := SendRequest{
		Project:    "test",
		SessionKey: baseKey,
		SessionID:  "s2",
		Message:    "delivery ready",
		Images: []ImageAttachment{{
			MimeType: "image/png",
			Data:     []byte("img"),
			FileName: "chart.png",
		}},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/send", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleSend(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := p1.getSent(); len(got) != 0 {
		t.Fatalf("s1 sent text = %#v, want none", got)
	}
	if len(p1.images) != 0 {
		t.Fatalf("s1 images = %#v, want none", p1.images)
	}
	if got := p2.getSent(); len(got) != 1 || got[0] != "delivery ready" {
		t.Fatalf("s2 sent text = %#v, want delivery ready", got)
	}
	if len(p2.images) != 1 || p2.images[0].FileName != "chart.png" {
		t.Fatalf("s2 images = %#v", p2.images)
	}
}
