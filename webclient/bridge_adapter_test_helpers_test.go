package webclient

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type fakeBridge struct {
	Server *httptest.Server
	Port   int
	Token  string
	Got    chan map[string]any
}

func startFakeBridge(t *testing.T, token string) *fakeBridge {
	t.Helper()
	if strings.TrimSpace(token) == "" {
		token = "bridge-secret"
	}
	got := make(chan map[string]any, 32)

	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bridge/ws" {
			http.NotFound(w, r)
			return
		}
		if q := strings.TrimSpace(r.URL.Query().Get("token")); q != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			// First frame: register.
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var reg map[string]any
			_ = json.Unmarshal(raw, &reg)
			_ = conn.WriteJSON(map[string]any{"type": "register_ack", "ok": true})

			for {
				_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
				_, raw, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var msg map[string]any
				if err := json.Unmarshal(raw, &msg); err != nil {
					continue
				}
				if typ, _ := msg["type"].(string); typ == "ping" {
					_ = conn.WriteJSON(map[string]any{"type": "pong", "ts": time.Now().UnixMilli()})
					continue
				}
				select {
				case got <- msg:
				default:
				}
			}
		}()
	}))

	port := 0
	if u := srv.URL; u != "" {
		if host := strings.TrimPrefix(u, "http://"); host != "" {
			_, p, _ := net.SplitHostPort(host)
			if pp, err := strconvAtoiPositive(p); err == nil {
				port = pp
			}
		}
	}
	if port == 0 {
		// Fallback: derive from listener addr.
		if addr, ok := srv.Listener.Addr().(*net.TCPAddr); ok {
			port = addr.Port
		}
	}
	return &fakeBridge{Server: srv, Port: port, Token: token, Got: got}
}
