package webclient

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/webclient/internal/store"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var webclientBridgeUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// bridgeIdleTTL controls how long the backend keeps an upstream bridge
// connection alive after the last browser client disconnects. This reduces the
// "tab-bound" behavior and allows late upstream replies to be persisted and
// replayed on reconnect.
var bridgeIdleTTL = 45 * time.Second

// bridgeReplayBufferMax is the maximum number of inbound frames retained for
// replay when a browser reconnects.
var bridgeReplayBufferMax = 256

type bridgeFrame struct {
	mt  int
	raw []byte
}

type bridgeDownstream struct {
	conn *websocket.Conn
	send chan bridgeFrame

	closeOnce sync.Once
}

func newBridgeDownstream(conn *websocket.Conn) *bridgeDownstream {
	return &bridgeDownstream{
		conn: conn,
		send: make(chan bridgeFrame, 32),
	}
}

func (d *bridgeDownstream) close() {
	d.closeOnce.Do(func() {
		close(d.send)
		_ = d.conn.Close()
	})
}

func (d *bridgeDownstream) writeLoop(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case f, ok := <-d.send:
			if !ok {
				return
			}
			_ = d.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := d.conn.WriteMessage(f.mt, f.raw); err != nil {
				return
			}
		}
	}
}

type bridgeHub struct {
	mu       sync.Mutex
	sessions map[string]*bridgeUpstreamSession
}

func newBridgeHub() *bridgeHub {
	return &bridgeHub{sessions: make(map[string]*bridgeUpstreamSession)}
}

var defaultBridgeHub = newBridgeHub()

type bridgeUpstreamSession struct {
	key        string
	upstreamWS string
	srv        *Server

	state *bridgeProxyState

	upConn    *websocket.Conn
	upWriteMu sync.Mutex

	done chan struct{}

	mu       sync.Mutex
	subs     map[*bridgeDownstream]struct{}
	idleStop *time.Timer

	// cached register_ack for late joiners
	ackMu sync.RWMutex
	ack   []byte

	// replay buffer for late joiners
	bufMu sync.Mutex
	buf   []bridgeFrame
}

func bridgeSessionKey(s *Server, upstreamURL string, fc bridgeFrontendConnect) string {
	// Include DataDir so multiple servers in one process (tests) do not share
	// sessions.
	return strings.TrimSpace(s.opts.DataDir) + "\n" +
		strings.TrimSpace(upstreamURL) + "\n" +
		strings.TrimSpace(fc.Platform) + "\n" +
		strings.TrimSpace(fc.Slot) + "\n" +
		strings.TrimSpace(fc.SessionKey) + "\n" +
		strings.TrimSpace(fc.Project)
}

func (h *bridgeHub) getOrCreate(key string, create func() (*bridgeUpstreamSession, error)) (*bridgeUpstreamSession, error) {
	h.mu.Lock()
	if sess, ok := h.sessions[key]; ok {
		h.mu.Unlock()
		return sess, nil
	}
	h.mu.Unlock()

	sess, err := create()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	// Double-check for races.
	if existing, ok := h.sessions[key]; ok {
		h.mu.Unlock()
		_ = sess.close()
		return existing, nil
	}
	h.sessions[key] = sess
	h.mu.Unlock()
	return sess, nil
}

func (h *bridgeHub) delete(key string) {
	h.mu.Lock()
	delete(h.sessions, key)
	h.mu.Unlock()
}

func (sess *bridgeUpstreamSession) attach(d *bridgeDownstream) {
	sess.mu.Lock()
	if sess.subs == nil {
		sess.subs = make(map[*bridgeDownstream]struct{})
	}
	sess.subs[d] = struct{}{}
	if sess.idleStop != nil {
		sess.idleStop.Stop()
		sess.idleStop = nil
	}
	sess.mu.Unlock()

	// Send cached register_ack if available.
	sess.ackMu.RLock()
	ack := append([]byte(nil), sess.ack...)
	sess.ackMu.RUnlock()
	if len(ack) > 0 {
		select {
		case d.send <- bridgeFrame{mt: websocket.TextMessage, raw: ack}:
		default:
		}
	}

	// Replay buffered inbound frames.
	sess.bufMu.Lock()
	buf := make([]bridgeFrame, len(sess.buf))
	copy(buf, sess.buf)
	sess.bufMu.Unlock()
	for _, f := range buf {
		select {
		case d.send <- f:
		default:
			break
		}
	}
}

func (sess *bridgeUpstreamSession) detach(d *bridgeDownstream) {
	sess.mu.Lock()
	if sess.subs != nil {
		delete(sess.subs, d)
	}
	empty := sess.subs == nil || len(sess.subs) == 0
	if empty && sess.idleStop == nil {
		sess.idleStop = time.AfterFunc(bridgeIdleTTL, func() {
			_ = sess.close()
			defaultBridgeHub.delete(sess.key)
		})
	}
	sess.mu.Unlock()

	// Stop this downstream client promptly; future broadcasts should not attempt
	// to deliver into a dead browser connection.
	d.close()
}

func (sess *bridgeUpstreamSession) close() error {
	select {
	case <-sess.done:
		// already closed
		return nil
	default:
		close(sess.done)
	}
	if sess.upConn != nil {
		_ = sess.upConn.Close()
	}
	sess.mu.Lock()
	for d := range sess.subs {
		d.close()
	}
	sess.subs = nil
	sess.mu.Unlock()
	return nil
}

func (sess *bridgeUpstreamSession) broadcast(mt int, raw []byte) {
	f := bridgeFrame{mt: mt, raw: append([]byte(nil), raw...)}

	// Store into replay buffer.
	sess.bufMu.Lock()
	sess.buf = append(sess.buf, f)
	if len(sess.buf) > bridgeReplayBufferMax {
		sess.buf = sess.buf[len(sess.buf)-bridgeReplayBufferMax:]
	}
	sess.bufMu.Unlock()

	sess.mu.Lock()
	for d := range sess.subs {
		select {
		case d.send <- f:
		default:
			// Backpressure: best-effort drop for this client.
		}
	}
	sess.mu.Unlock()
}

func (sess *bridgeUpstreamSession) broadcastOnly(mt int, raw []byte) {
	f := bridgeFrame{mt: mt, raw: append([]byte(nil), raw...)}
	sess.mu.Lock()
	for d := range sess.subs {
		select {
		case d.send <- f:
		default:
		}
	}
	sess.mu.Unlock()
}

var bridgeWriteUpstreamOverride func(*bridgeUpstreamSession, int, []byte) (bool, error)

func (sess *bridgeUpstreamSession) writeUpstream(mt int, raw []byte) error {
	sess.upWriteMu.Lock()
	defer sess.upWriteMu.Unlock()
	if bridgeWriteUpstreamOverride != nil {
		if handled, err := bridgeWriteUpstreamOverride(sess, mt, raw); handled {
			return err
		}
	}
	_ = sess.upConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return sess.upConn.WriteMessage(mt, raw)
}

type bridgeProxyState struct {
	mu sync.RWMutex

	project    string
	sessionKey string
	route      string
	slot       string
}

func (st *bridgeProxyState) setFromFrontendConnect(fc bridgeFrontendConnect) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if strings.TrimSpace(fc.Project) != "" {
		st.project = strings.TrimSpace(fc.Project)
	}
	if strings.TrimSpace(fc.SessionKey) != "" {
		st.sessionKey = strings.TrimSpace(fc.SessionKey)
	}
	if strings.TrimSpace(fc.Route) != "" {
		st.route = strings.TrimSpace(fc.Route)
	}
	if strings.TrimSpace(fc.Slot) != "" {
		st.slot = strings.TrimSpace(fc.Slot)
	}
}

func (st *bridgeProxyState) snapshot() (project, sessionKey, route, slot string) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.project, st.sessionKey, st.route, st.slot
}

// handleBridgeWS intercepts WebSocket upgrades to /bridge/ws so this webclient
// backend can persist chat history locally while still using the upstream bridge
// runtime.
//
// Non-WebSocket requests fall back to the upstream reverse proxy (used by tests
// and for compatibility).
func (s *Server) handleBridgeWS(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		s.handleBridgeProxy(w, r)
		return
	}
	base := strings.TrimSpace(s.opts.ManagementBaseURL)
	if base == "" {
		http.Error(w, "management proxy is not configured", http.StatusServiceUnavailable)
		return
	}

	downConn, err := webclientBridgeUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("webclient: bridge ws upgrade failed", "error", err)
		return
	}

	upstreamURL, err := s.upstreamBridgeWSURL(r)
	if err != nil {
		slog.Warn("webclient: bridge upstream ws url failed", "error", err)
		_ = downConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "bad upstream url"), time.Now().Add(2*time.Second))
		_ = downConn.Close()
		return
	}

	// The first frame must be frontend_connect for browser clients.
	_ = downConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	mt0, raw0, err := downConn.ReadMessage()
	if err != nil {
		_ = downConn.Close()
		return
	}
	_ = downConn.SetReadDeadline(time.Time{})

	var base0 bridgeBase
	if err := json.Unmarshal(raw0, &base0); err != nil || base0.Type != "frontend_connect" {
		_ = downConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "frontend_connect required"), time.Now().Add(2*time.Second))
		_ = downConn.Close()
		return
	}
	var fc bridgeFrontendConnect
	if err := json.Unmarshal(raw0, &fc); err != nil {
		_ = downConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseUnsupportedData, "bad frontend_connect"), time.Now().Add(2*time.Second))
		_ = downConn.Close()
		return
	}

	key := bridgeSessionKey(s, upstreamURL, fc)
	sess, err := defaultBridgeHub.getOrCreate(key, func() (*bridgeUpstreamSession, error) {
		upConn, _, err := websocket.DefaultDialer.Dial(upstreamURL, nil)
		if err != nil {
			return nil, err
		}
		st := &bridgeProxyState{}
		st.setFromFrontendConnect(fc)
		ns := &bridgeUpstreamSession{
			key:        key,
			upstreamWS: upstreamURL,
			srv:        s,
			state:      st,
			upConn:     upConn,
			done:       make(chan struct{}),
			subs:       make(map[*bridgeDownstream]struct{}),
		}
		// Forward frontend_connect to upstream to register this backend session.
		if err := ns.writeUpstream(mt0, raw0); err != nil {
			_ = upConn.Close()
			return nil, err
		}
		go ns.upstreamReadLoop()
		return ns, nil
	})
	if err != nil {
		slog.Warn("webclient: bridge upstream session create failed", "url", upstreamURL, "error", err)
		_ = downConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "upstream unavailable"), time.Now().Add(2*time.Second))
		_ = downConn.Close()
		return
	}

	d := newBridgeDownstream(downConn)
	sess.attach(d)
	defer sess.detach(d)

	go d.writeLoop(sess.done)

	// Read loop for this downstream client.
	for {
		mt, raw, err := downConn.ReadMessage()
		if err != nil {
			_ = downConn.Close()
			return
		}
		if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
			continue
		}

		// Ignore additional frontend_connect frames from this browser; treat it
		// as a no-op subscription refresh.
		var b bridgeBase
		if err := json.Unmarshal(raw, &b); err == nil && b.Type == "frontend_connect" {
			var fc2 bridgeFrontendConnect
			if err := json.Unmarshal(raw, &fc2); err == nil {
				sess.state.setFromFrontendConnect(fc2)
			}
			continue
		}

		// Persist first; never forward user messages upstream if persistence
		// fails.
		obProject, obID, err := s.bridgePersistOutbound(sess.state, raw)
		if err != nil {
			slog.Warn("webclient: bridge outbound persist failed", "error", err)
			_ = downConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "persist failed"), time.Now().Add(2*time.Second))
			_ = downConn.Close()
			return
		}
		if err := sess.writeUpstream(mt, raw); err != nil {
			if obID != "" {
				_, _ = s.store.MarkOutboxFailed(obProject, obID, err.Error(), time.Now().UTC())
			}
			slog.Warn("webclient: bridge upstream write failed", "error", err)
			_ = downConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "upstream write failed"), time.Now().Add(2*time.Second))
			_ = downConn.Close()
			return
		}
		if obID != "" {
			if _, err := s.store.MarkOutboxSent(obProject, obID); err != nil {
				slog.Warn("webclient: bridge outbox mark sent failed", "error", err)
			}
		}
	}
}

func (s *Server) upstreamBridgeWSURL(r *http.Request) (string, error) {
	baseURL, err := url.Parse(strings.TrimSpace(s.opts.ManagementBaseURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return "", fmt.Errorf("invalid management_base_url")
	}

	wsScheme := ""
	switch strings.ToLower(baseURL.Scheme) {
	case "http":
		wsScheme = "ws"
	case "https":
		wsScheme = "wss"
	default:
		return "", fmt.Errorf("unsupported upstream scheme %q", baseURL.Scheme)
	}

	u := &url.URL{
		Scheme:   wsScheme,
		Host:     baseURL.Host,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery, // must preserve ?token=... exactly
	}
	return u.String(), nil
}

func (sess *bridgeUpstreamSession) upstreamReadLoop() {
	defer func() {
		_ = sess.close()
		defaultBridgeHub.delete(sess.key)
	}()
	for {
		select {
		case <-sess.done:
			return
		default:
		}
		mt, raw, err := sess.upConn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
			continue
		}

		// Cache register_ack for late joiners.
		var base bridgeBase
		if err := json.Unmarshal(raw, &base); err == nil && base.Type == "register_ack" {
			sess.ackMu.Lock()
			sess.ack = append([]byte(nil), raw...)
			sess.ackMu.Unlock()
			// Forward immediately, but do not put into replay buffer to avoid
			// duplicate acks when a browser reconnects.
			sess.broadcastOnly(mt, raw)
			continue
		}

		// Persist first; never forward inbound frames to browsers if persistence
		// fails.
		if err := sess.srv.bridgePersistInbound(sess.state, raw); err != nil {
			slog.Warn("webclient: bridge inbound persist failed", "error", err)
			return
		}

		sess.broadcast(mt, raw)
	}
}

type bridgeBase struct {
	Type string `json:"type"`
}

type bridgeFrontendConnect struct {
	Type       string `json:"type"`
	Project    string `json:"project,omitempty"`
	SessionKey string `json:"session_key,omitempty"`
	Route      string `json:"route,omitempty"`
	Slot       string `json:"slot,omitempty"`
	Platform   string `json:"platform,omitempty"`
}

type bridgeImageData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
	FileName string `json:"file_name,omitempty"`
	Size     int    `json:"size,omitempty"`
}

type bridgeFileData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
	FileName string `json:"file_name"`
}

type bridgeMessage struct {
	Type       string            `json:"type"`
	MsgID      string            `json:"msg_id,omitempty"`
	SessionKey string            `json:"session_key"`
	SessionID  string            `json:"session_id,omitempty"`
	Content    string            `json:"content,omitempty"`
	Project    string            `json:"project,omitempty"`
	Images     []bridgeImageData `json:"images,omitempty"`
	Files      []bridgeFileData  `json:"files,omitempty"`
	Action     string            `json:"action,omitempty"` // card_action
}

type bridgeReply struct {
	Type          string `json:"type"`
	SessionKey    string `json:"session_key"`
	SessionID     string `json:"session_id,omitempty"`
	ReplyCtx      string `json:"reply_ctx,omitempty"`
	Content       string `json:"content,omitempty"`
	Format        string `json:"format,omitempty"`
	Images        any    `json:"images,omitempty"`
	Image         any    `json:"image,omitempty"`
	Buttons       any    `json:"buttons,omitempty"`
	Card          any    `json:"card,omitempty"`
	Delta         string `json:"delta,omitempty"`
	FullText      string `json:"full_text,omitempty"`
	Done          bool   `json:"done,omitempty"`
	PreviewHandle string `json:"preview_handle,omitempty"`
	RefID         string `json:"ref_id,omitempty"`
}

func (s *Server) bridgePersistOutbound(state *bridgeProxyState, raw []byte) (outboxProject string, outboxID string, _ error) {
	var base bridgeBase
	if err := json.Unmarshal(raw, &base); err != nil {
		return "", "", nil
	}
	switch base.Type {
	case "frontend_connect":
		var fc bridgeFrontendConnect
		if err := json.Unmarshal(raw, &fc); err != nil {
			return "", "", nil
		}
		state.setFromFrontendConnect(fc)
		return "", "", nil
	case "message":
		var m bridgeMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", "", nil
		}
		project := strings.TrimSpace(m.Project)
		if project == "" {
			p, _, _, _ := state.snapshot()
			project = p
		}
		if project == "" {
			return "", "", nil
		}
		obID, err := s.persistBridgeUserMessage(project, m, true)
		if err != nil {
			return "", "", err
		}
		return project, obID, nil
	case "card_action":
		// Persist as a user message event so history remains meaningful after
		// refresh.
		var m bridgeMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", "", nil
		}
		project := strings.TrimSpace(m.Project)
		if project == "" {
			p, _, _, _ := state.snapshot()
			project = p
		}
		if project == "" {
			return "", "", nil
		}
		if strings.TrimSpace(m.SessionKey) == "" {
			return "", "", nil
		}
		// Store action as plain text for now.
		m.Content = "[card_action] " + strings.TrimSpace(m.Action)
		m.Images = nil
		m.Files = nil
		_, err := s.persistBridgeUserMessage(project, m, false)
		return "", "", err
	}
	return "", "", nil
}

func (s *Server) bridgePersistInbound(state *bridgeProxyState, raw []byte) error {
	var base bridgeBase
	if err := json.Unmarshal(raw, &base); err != nil {
		return nil
	}
	switch base.Type {
	case "reply", "image", "buttons", "card", "file", "reply_stream", "update_message", "delete_message", "preview_start":
		var r bridgeReply
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil
		}
		project, _, _, _ := state.snapshot()
		if project == "" {
			return nil
		}
		return s.persistBridgeAssistantEvent(project, base.Type, r, raw)
	}
	return nil
}

func stableJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return strings.TrimSpace(string(raw))
	}
	b, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(b)
}

func (s *Server) persistBridgeUserMessage(project string, m bridgeMessage, createOutbox bool) (string, error) {
	if err := store.ValidateSegment("project", project); err != nil {
		return "", err
	}
	sessionKey := strings.TrimSpace(m.SessionKey)
	if sessionKey == "" {
		return "", fmt.Errorf("session_key is required")
	}
	sessionID, err := s.ensureClientSessionForBridge(project, sessionKey, strings.TrimSpace(m.SessionID))
	if err != nil {
		return "", err
	}

	var atts []store.Attachment
	for _, img := range m.Images {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(img.Data))
		if err != nil {
			return "", fmt.Errorf("invalid image base64")
		}
		_, att, err := s.storeSaveImage(core.ImageAttachment{
			MimeType: strings.TrimSpace(img.MimeType),
			Data:     b,
			FileName: strings.TrimSpace(img.FileName),
		})
		if err != nil {
			return "", err
		}
		atts = append(atts, att)
	}
	for _, f := range m.Files {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(f.Data))
		if err != nil {
			return "", fmt.Errorf("invalid file base64")
		}
		_, att, err := s.storeSaveFile(core.FileAttachment{
			MimeType: strings.TrimSpace(f.MimeType),
			Data:     b,
			FileName: strings.TrimSpace(f.FileName),
		})
		if err != nil {
			return "", err
		}
		atts = append(atts, att)
	}

	content := strings.TrimSpace(m.Content)
	_, err = s.store.AppendMessage(project, sessionID, store.Message{
		Role:        store.RoleUser,
		Content:     content,
		Attachments: atts,
	})
	if err != nil {
		return "", err
	}
	if !createOutbox {
		return "", nil
	}

	payloadBytes, err := json.Marshal(outboxPayloadV1Send{
		Kind:        outboxPayloadKindV1Send,
		SessionKey:  sessionKey,
		SessionID:   sessionID,
		Message:     content,
		Attachments: atts,
	})
	if err != nil {
		return "", err
	}
	obID := ""
	if strings.TrimSpace(m.MsgID) != "" {
		obID = strings.TrimSpace(m.MsgID)
	}
	ob, err := s.store.CreateOutboxItem(store.CreateOutboxItemInput{
		ID:          obID,
		Project:     project,
		SessionID:   sessionID,
		SessionKey:  sessionKey,
		Payload:     payloadBytes,
		NextRetryAt: time.Now().UTC(),
	})
	if err != nil {
		// If we reconnected and replayed the same user message, the outbox item
		// may already exist. Treat that as idempotent success so the bridge can
		// continue forwarding.
		if strings.Contains(strings.ToLower(err.Error()), "already exists") {
			return strings.TrimSpace(obID), nil
		}
		return "", err
	}
	return ob.ID, nil
}

func (s *Server) persistBridgeAssistantEvent(project, typ string, r bridgeReply, raw []byte) error {
	if err := store.ValidateSegment("project", project); err != nil {
		return err
	}
	sessionKey := strings.TrimSpace(r.SessionKey)
	if sessionKey == "" {
		return nil
	}
	sessionID, err := s.ensureClientSessionForBridge(project, sessionKey, strings.TrimSpace(r.SessionID))
	if err != nil {
		return err
	}

	atts, err := s.persistBridgeReplyAttachments(typ, r, raw)
	if err != nil {
		return err
	}

	content := strings.TrimSpace(r.Content)
	switch typ {
	case "card", "buttons":
		if content == "" {
			content = "[" + typ + "] " + stableJSON(raw)
		}
	case "reply_stream":
		// Persist only the final assembled text to avoid spamming history with
		// deltas.
		if !r.Done {
			return nil
		}
		if strings.TrimSpace(r.FullText) != "" {
			content = strings.TrimSpace(r.FullText)
		} else if content == "" {
			content = "[reply_stream] " + stableJSON(raw)
		}
	case "preview_start", "update_message", "delete_message":
		if content == "" {
			content = "[" + typ + "] " + stableJSON(raw)
		}
	case "file":
		if content == "" && len(atts) > 0 {
			names := make([]string, 0, len(atts))
			for _, a := range atts {
				if strings.TrimSpace(a.FileName) != "" {
					names = append(names, strings.TrimSpace(a.FileName))
				}
			}
			if len(names) > 0 {
				content = "[file] " + strings.Join(names, ", ")
			} else {
				content = "[file]"
			}
		}
		if content == "" {
			content = "[file]"
		}
	case "image":
		if content == "" && len(atts) > 0 {
			content = "[image]"
		}
		if content == "" {
			content = "[image] " + stableJSON(raw)
		}
	}
	_, err = s.store.AppendMessage(project, sessionID, store.Message{
		Role:        store.RoleAssistant,
		Content:     content,
		Attachments: atts,
	})
	return err
}

func (s *Server) persistBridgeReplyAttachments(typ string, r bridgeReply, rawJSON []byte) ([]store.Attachment, error) {
	var imgAny any
	if r.Images != nil {
		imgAny = r.Images
	} else if r.Image != nil {
		imgAny = r.Image
	} else {
		imgAny = nil
	}

	items := make([]any, 0, 4)
	if imgAny != nil {
		switch v := imgAny.(type) {
		case []any:
			items = append(items, v...)
		default:
			items = append(items, v)
		}
	}

	var out []store.Attachment
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			return nil, err
		}
		var img bridgeImageData
		if err := json.Unmarshal(b, &img); err != nil {
			return nil, err
		}
		data := strings.TrimSpace(img.Data)
		if data == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("invalid image base64")
		}
		_, att, err := s.storeSaveImage(core.ImageAttachment{
			MimeType: strings.TrimSpace(img.MimeType),
			Data:     decoded,
			FileName: strings.TrimSpace(img.FileName),
		})
		if err != nil {
			return nil, err
		}
		out = append(out, att)
	}

	// Additional "file" type support: accept `files` field that matches the File
	// schema.
	if typ == "file" {
		var ev struct {
			File  *bridgeFileData  `json:"file,omitempty"`
			Files []bridgeFileData `json:"files,omitempty"`
		}
		if err := json.Unmarshal(rawJSON, &ev); err == nil {
			var files []bridgeFileData
			if ev.File != nil {
				files = append(files, *ev.File)
			}
			files = append(files, ev.Files...)
			for _, f := range files {
				b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(f.Data))
				if err != nil {
					return nil, fmt.Errorf("invalid file base64")
				}
				_, att, err := s.storeSaveFile(core.FileAttachment{
					MimeType: strings.TrimSpace(f.MimeType),
					Data:     b,
					FileName: strings.TrimSpace(f.FileName),
				})
				if err != nil {
					return nil, err
				}
				out = append(out, att)
			}
		}
	}
	return out, nil
}

func (s *Server) ensureClientSessionForBridge(project, sessionKey, sessionID string) (string, error) {
	if strings.TrimSpace(sessionID) != "" {
		// Ensure meta exists; tolerate "already exists".
		_, _ = s.store.CreateClientSession(project, store.CreateClientSessionInput{
			ID:         sessionID,
			SessionKey: sessionKey,
			Name:       sessionID,
		})
		return sessionID, nil
	}

	id, err := s.store.ActiveSessionID(project, sessionKey)
	if err == nil && strings.TrimSpace(id) != "" {
		return id, nil
	}

	newID := "web_" + uuid.NewString()
	created, err := s.store.CreateClientSession(project, store.CreateClientSessionInput{
		ID:         newID,
		SessionKey: sessionKey,
		Name:       newID,
	})
	if err == nil && strings.TrimSpace(created.ID) != "" {
		return created.ID, nil
	}
	// Best-effort: even if create failed due to a race, try to resolve again.
	if id2, err2 := s.store.ActiveSessionID(project, sessionKey); err2 == nil && strings.TrimSpace(id2) != "" {
		return id2, nil
	}
	return "", fmt.Errorf("failed to resolve session id for session_key")
}
