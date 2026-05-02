package webclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/webclient/internal/store"
	"github.com/gorilla/websocket"
)

// adapterClient is a long-lived external Bridge adapter connection.
//
// It registers as platform=webclient, receives bridge outbound frames, persists
// durable messages into the local store, persists transient progress into
// run_events, and forwards user messages from /api/v1/projects/{project}/send
// to the upstream bridge as "message" / "card_action".
type adapterClient struct {
	srv *Server

	started atomic.Bool
	stopCh  chan struct{}
	doneCh  chan struct{}

	readyOnce sync.Once
	readyCh   chan struct{}

	preview *previewManager

	connMu sync.RWMutex
	conn   *websocket.Conn
	// serializes writes on conn
	writeMu sync.Mutex

	sendCh chan sendItem

	mapMu          sync.RWMutex
	sessionProject map[string]string // key: session_key+"\n"+session_id -> project
	activeRun      map[string]runRef // key: session_key+"\n"+session_id -> run
}

type sendItem struct {
	raw  []byte
	done chan error // optional; when non-nil, writer signals write result then closes it
}

type runRef struct {
	RunID         string
	UserMessageID string
	UpdatedAt     time.Time
}

func newAdapterClient(s *Server) *adapterClient {
	return &adapterClient{
		srv:            s,
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
		readyCh:        make(chan struct{}),
		preview:        newPreviewManager(),
		sendCh:         make(chan sendItem, 256),
		sessionProject: make(map[string]string),
		activeRun:      make(map[string]runRef),
	}
}

func (c *adapterClient) Start() {
	if !c.started.CompareAndSwap(false, true) {
		return
	}
	go c.loop()
}

func (c *adapterClient) Stop() {
	if !c.started.Load() {
		return
	}
	select {
	case <-c.stopCh:
		// already stopped
	default:
		close(c.stopCh)
	}
	<-c.doneCh
}

func (c *adapterClient) WaitReady(ctx context.Context) error {
	select {
	case <-c.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitConnected blocks until a live websocket connection is currently available.
// Unlike WaitReady, this requires a present connection (not just "connected once").
func (c *adapterClient) WaitConnected(ctx context.Context) error {
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	for {
		if c.isConnected() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (c *adapterClient) isConnected() bool {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.conn != nil
}

func (c *adapterClient) setConn(conn *websocket.Conn) {
	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
}

func (c *adapterClient) clearConn() {
	c.connMu.Lock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
	c.connMu.Unlock()
}

func (c *adapterClient) key(sessionKey, sessionID string) string {
	return strings.TrimSpace(sessionKey) + "\n" + strings.TrimSpace(sessionID)
}

func (c *adapterClient) TrackSession(project, sessionKey, sessionID string) {
	project = strings.TrimSpace(project)
	sessionKey = strings.TrimSpace(sessionKey)
	sessionID = strings.TrimSpace(sessionID)
	if project == "" || sessionKey == "" || sessionID == "" {
		return
	}
	k := c.key(sessionKey, sessionID)
	c.mapMu.Lock()
	c.sessionProject[k] = project
	c.mapMu.Unlock()
}

func (c *adapterClient) SetActiveRun(project, sessionKey, sessionID, runID, userMessageID string) {
	project = strings.TrimSpace(project)
	sessionKey = strings.TrimSpace(sessionKey)
	sessionID = strings.TrimSpace(sessionID)
	runID = strings.TrimSpace(runID)
	userMessageID = strings.TrimSpace(userMessageID)
	if project == "" || sessionKey == "" || sessionID == "" || runID == "" {
		return
	}
	k := c.key(sessionKey, sessionID)
	now := nowUTC()
	c.mapMu.Lock()
	c.sessionProject[k] = project
	c.activeRun[k] = runRef{RunID: runID, UserMessageID: firstNonEmpty(userMessageID, runID), UpdatedAt: now}
	c.mapMu.Unlock()
}

func (c *adapterClient) activeRunFor(sessionKey, sessionID string) (runRef, bool) {
	k := c.key(sessionKey, sessionID)
	c.mapMu.RLock()
	ref, ok := c.activeRun[k]
	c.mapMu.RUnlock()
	return ref, ok
}

func (c *adapterClient) projectFor(sessionKey, sessionID string) string {
	k := c.key(sessionKey, sessionID)
	c.mapMu.RLock()
	project := strings.TrimSpace(c.sessionProject[k])
	c.mapMu.RUnlock()
	return project
}

func (c *adapterClient) enqueue(raw []byte) error {
	return c.enqueueItem(sendItem{raw: raw})
}

func (c *adapterClient) send(ctx context.Context, raw []byte) error {
	done := make(chan error, 1)
	if err := c.enqueueItem(sendItem{raw: raw, done: done}); err != nil {
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *adapterClient) enqueueItem(it sendItem) error {
	select {
	case <-c.stopCh:
		return fmt.Errorf("webclient adapter stopped")
	default:
	}
	if len(it.raw) == 0 {
		return nil
	}
	if !c.isConnected() {
		return fmt.Errorf("upstream bridge not connected")
	}
	select {
	case c.sendCh <- it:
		return nil
	default:
		return fmt.Errorf("webclient adapter send queue is full")
	}
}

func (c *adapterClient) failPendingSends(err error) {
	if err == nil {
		err = fmt.Errorf("upstream bridge disconnected")
	}
	for {
		select {
		case it := <-c.sendCh:
			if it.done != nil {
				select {
				case it.done <- err:
				default:
				}
				close(it.done)
			}
		default:
			return
		}
	}
}

func (c *adapterClient) SendBridgeMessage(ctx context.Context, project string, body v1SendReq, msgID string, images []store.Attachment, coreImages []bridgeImageData) error {
	project = strings.TrimSpace(project)
	sessionKey := strings.TrimSpace(body.SessionKey)
	sessionID := strings.TrimSpace(body.SessionID)
	if project == "" || sessionKey == "" || sessionID == "" {
		return fmt.Errorf("missing project/session_key/session_id")
	}

	c.TrackSession(project, sessionKey, sessionID)

	wireImages := make([]map[string]any, 0, len(coreImages))
	for _, img := range coreImages {
		if strings.TrimSpace(img.Data) == "" {
			continue
		}
		wireImages = append(wireImages, map[string]any{
			"mime_type": strings.TrimSpace(img.MimeType),
			"data":      strings.TrimSpace(img.Data),
			"file_name": strings.TrimSpace(img.FileName),
		})
	}

	payload := map[string]any{
		"type":        "message",
		"msg_id":      strings.TrimSpace(msgID),
		"session_key": sessionKey,
		"session_id":  sessionID,
		"user_id":     "web-admin",
		"user_name":   "Web Admin",
		"content":     strings.TrimSpace(body.Message),
		"reply_ctx":   sessionKey,
		"project":     project,
	}
	if len(wireImages) > 0 {
		payload["images"] = wireImages
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := c.send(ctx, b); err != nil {
		return err
	}
	_ = images // reserved for future card_action/attachments; outbox already persisted.
	return nil
}

func (c *adapterClient) SendCardAction(ctx context.Context, project, sessionKey, sessionID, action, replyCtx string) error {
	project = strings.TrimSpace(project)
	sessionKey = strings.TrimSpace(sessionKey)
	sessionID = strings.TrimSpace(sessionID)
	action = strings.TrimSpace(action)
	replyCtx = strings.TrimSpace(replyCtx)
	if project == "" || sessionKey == "" || sessionID == "" || action == "" {
		return fmt.Errorf("missing project/session_key/session_id/action")
	}
	if replyCtx == "" {
		replyCtx = sessionKey
	}
	c.TrackSession(project, sessionKey, sessionID)
	payload := map[string]any{
		"type":        "card_action",
		"session_key": sessionKey,
		"session_id":  sessionID,
		"action":      action,
		"reply_ctx":   replyCtx,
		"project":     project,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return c.send(ctx, b)
}

type upstreamBridgeInfo struct {
	wsURL string
	token string
}

func (c *adapterClient) fetchUpstreamInfo(ctx context.Context) (upstreamBridgeInfo, error) {
	path, token, port, err := c.srv.fetchUpstreamBridgeConfig(ctx)
	if err != nil {
		return upstreamBridgeInfo{}, err
	}
	if strings.TrimSpace(token) == "" {
		return upstreamBridgeInfo{}, fmt.Errorf("upstream bridge token is missing")
	}

	base := strings.TrimSpace(c.srv.opts.ManagementBaseURL)
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return upstreamBridgeInfo{}, fmt.Errorf("invalid management_base_url")
	}

	host := baseURL.Host
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil && strings.TrimSpace(h) != "" {
		hostname = h
	}
	if port <= 0 {
		return upstreamBridgeInfo{}, fmt.Errorf("upstream bridge port is missing")
	}

	wsScheme := "ws"
	if strings.EqualFold(baseURL.Scheme, "https") {
		wsScheme = "wss"
	}
	u := &url.URL{
		Scheme: wsScheme,
		Host:   net.JoinHostPort(hostname, fmt.Sprintf("%d", port)),
		Path:   path,
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return upstreamBridgeInfo{wsURL: u.String(), token: token}, nil
}

func (c *adapterClient) loop() {
	defer close(c.doneCh)

	backoff := 500 * time.Millisecond
	for {
		select {
		case <-c.stopCh:
			c.clearConn()
			return
		default:
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		info, err := c.fetchUpstreamInfo(ctx)
		cancel()
		if err != nil {
			time.Sleep(backoff)
			if backoff < 8*time.Second {
				backoff *= 2
			}
			continue
		}

		conn, _, err := websocket.DefaultDialer.Dial(info.wsURL, nil)
		if err != nil {
			time.Sleep(backoff)
			if backoff < 8*time.Second {
				backoff *= 2
			}
			continue
		}

		if err := c.register(conn); err != nil {
			_ = conn.Close()
			time.Sleep(backoff)
			if backoff < 8*time.Second {
				backoff *= 2
			}
			continue
		}

		backoff = 500 * time.Millisecond
		c.setConn(conn)
		c.readyOnce.Do(func() { close(c.readyCh) })
		slog.Info("webclient: upstream bridge adapter connected", "platform", "webclient", "ws", info.wsURL)
		c.startOutboxRecovery(conn)

		// Run until disconnected.
		errCh := make(chan error, 2)
		go func() { errCh <- c.readLoop(conn) }()
		go func() { errCh <- c.writeLoop(conn) }()

		// Periodic keepalive ping (bridge responds with pong).
		pingTicker := time.NewTicker(25 * time.Second)
		var discErr error

	alive:
		for {
			select {
			case <-c.stopCh:
				discErr = fmt.Errorf("stopped")
				_ = conn.Close()
				break alive
			case err := <-errCh:
				discErr = err
				break alive
			case <-pingTicker.C:
				_ = c.enqueue([]byte(`{"type":"ping","ts":` + fmt.Sprintf("%d", time.Now().UnixMilli()) + `}`))
			}
		}
		pingTicker.Stop()

		// Drop any unsent queued frames on disconnect. Durable retry lives in outbox.
		c.failPendingSends(discErr)
		c.clearConn()
		slog.Warn("webclient: upstream bridge adapter disconnected; reconnecting", "platform", "webclient")
	}
}

func (c *adapterClient) startOutboxRecovery(conn *websocket.Conn) {
	// A single server instance may reconnect many times; keep each recovery loop
	// scoped to the current live connection.
	go func() {
		// Initial attempt (do not wait for the first tick).
		for {
			select {
			case <-c.stopCh:
				return
			default:
			}

			c.connMu.RLock()
			same := c.conn == conn && c.conn != nil
			c.connMu.RUnlock()
			if !same {
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			n, err := c.srv.recoverOutboxOnce(ctx, 50)
			cancel()
			if err != nil {
				slog.Warn("webclient: outbox recovery loop failed", "error", err)
				time.Sleep(1 * time.Second)
			}
			if n > 0 {
				// Drain backlog quickly.
				time.Sleep(150 * time.Millisecond)
				continue
			}

			// Nothing due: idle until next poll or disconnect.
			t := time.NewTicker(3 * time.Second)
			select {
			case <-c.stopCh:
				t.Stop()
				return
			case <-t.C:
				t.Stop()
				continue
			}
		}
	}()
}

func (c *adapterClient) register(conn *websocket.Conn) error {
	reg := map[string]any{
		"type":         "register",
		"platform":     "webclient",
		"capabilities": []string{"text", "image", "file", "card", "buttons", "typing", "preview", "update_message", "delete_message", "reconstruct_reply"},
		"metadata": map[string]any{
			"client_kind": "webclient_backend",
			"adapter":     "webclient_backend",
		},
	}
	if err := conn.WriteJSON(reg); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	var ack struct {
		Type  string `json:"type"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := conn.ReadJSON(&ack); err != nil {
		return err
	}
	if ack.Type != "register_ack" || !ack.OK {
		if strings.TrimSpace(ack.Error) != "" {
			return fmt.Errorf("register failed: %s", ack.Error)
		}
		return fmt.Errorf("register failed")
	}
	return nil
}

func (c *adapterClient) writeLoop(conn *websocket.Conn) error {
	for {
		select {
		case <-c.stopCh:
			return nil
		case it := <-c.sendCh:
			c.writeMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := conn.WriteMessage(websocket.TextMessage, it.raw)
			c.writeMu.Unlock()
			if it.done != nil {
				select {
				case it.done <- err:
				default:
				}
				close(it.done)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (c *adapterClient) readLoop(conn *websocket.Conn) error {
	for {
		select {
		case <-c.stopCh:
			return nil
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}

		var base bridgeBase
		if err := json.Unmarshal(raw, &base); err != nil {
			continue
		}
		switch base.Type {
		case "pong", "register_ack":
			continue
		case "reply", "image", "file", "card", "buttons":
			_ = c.persistDurableAssistant(base.Type, raw)
		case "error":
			_ = c.persistDurableError(raw)
		case "preview_start":
			_ = c.handlePreviewStart(raw)
		case "update_message", "delete_message", "reply_stream", "typing_start", "typing_stop":
			_ = c.persistRunEvent(base.Type, raw)
		default:
			// Unknown or unsupported: ignore.
		}
	}
}

func (c *adapterClient) persistDurableError(raw []byte) error {
	var ev struct {
		Type       string `json:"type"`
		Code       string `json:"code"`
		Message    string `json:"message"`
		SessionKey string `json:"session_key"`
		SessionID  string `json:"session_id"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil
	}
	sessionKey := strings.TrimSpace(ev.SessionKey)
	sessionID := strings.TrimSpace(ev.SessionID)
	if sessionKey == "" || sessionID == "" {
		return nil
	}
	project := c.projectFor(sessionKey, sessionID)
	if project == "" {
		if p, ok := c.srv.bestEffortFindProjectForSession(sessionKey, sessionID); ok {
			project = p
			c.TrackSession(project, sessionKey, sessionID)
		}
	}
	if project == "" {
		return nil
	}

	run, ok := c.activeRunFor(sessionKey, sessionID)
	if !ok || strings.TrimSpace(run.RunID) == "" {
		if ref, ok2 := c.srv.bestEffortLastUserRun(project, sessionID); ok2 {
			run = ref
			c.SetActiveRun(project, sessionKey, sessionID, run.RunID, run.UserMessageID)
		}
	}

	msg := strings.TrimSpace(ev.Message)
	code := strings.TrimSpace(ev.Code)
	if code != "" && msg != "" {
		msg = "[error] " + code + ": " + msg
	} else if msg != "" {
		msg = "[error] " + msg
	} else if code != "" {
		msg = "[error] " + code
	} else {
		msg = "[error]"
	}

	stored, err := c.srv.store.AppendMessage(project, sessionID, store.Message{
		Role:          store.RoleAssistant,
		Content:       msg,
		Timestamp:     nowUTC(),
		RunID:         strings.TrimSpace(run.RunID),
		UserMessageID: strings.TrimSpace(run.UserMessageID),
	})
	if err != nil {
		return err
	}
	c.srv.events.Publish(project, sessionID, stored)

	if strings.TrimSpace(run.RunID) != "" {
		rEv := store.RunEvent{
			RunID:         strings.TrimSpace(run.RunID),
			UserMessageID: strings.TrimSpace(run.UserMessageID),
			SessionID:     sessionID,
			Type:          "run_error",
			Status:        "error",
			CreatedAt:     nowUTC(),
			Timestamp:     nowUTC(),
			Metadata: map[string]any{
				"code": strings.TrimSpace(ev.Code),
			},
		}
		if sev, err := c.srv.store.AppendRunEvent(project, sessionID, rEv); err == nil {
			c.srv.runEvts.Publish(project, sessionID, sev)
		}
	}
	return nil
}

func (c *adapterClient) handlePreviewStart(raw []byte) error {
	var r bridgeReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil
	}
	// Persist as run_event first.
	_ = c.persistRunEvent("preview_start", raw)

	refID := strings.TrimSpace(r.RefID)
	handle := c.preview.HandlePreviewStart(refID)
	ack := map[string]any{
		"type":           "preview_ack",
		"ref_id":         refID,
		"preview_handle": handle,
	}
	b, err := json.Marshal(ack)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.send(ctx, b)
}

func (c *adapterClient) persistRunEvent(typ string, raw []byte) error {
	var r bridgeReply
	_ = json.Unmarshal(raw, &r)
	sessionKey := strings.TrimSpace(r.SessionKey)
	sessionID := strings.TrimSpace(r.SessionID)
	if sessionKey == "" || sessionID == "" {
		// typing_start/stop uses reply_ctx not always present; still needs session_key.
		var m struct {
			SessionKey string `json:"session_key"`
			SessionID  string `json:"session_id"`
		}
		_ = json.Unmarshal(raw, &m)
		sessionKey = strings.TrimSpace(m.SessionKey)
		sessionID = strings.TrimSpace(m.SessionID)
	}
	if sessionKey == "" || sessionID == "" {
		return nil
	}

	project := c.projectFor(sessionKey, sessionID)
	if project == "" {
		// Best-effort fallback: scan store metas.
		if p, ok := c.srv.bestEffortFindProjectForSession(sessionKey, sessionID); ok {
			project = p
			c.TrackSession(project, sessionKey, sessionID)
		}
	}
	if project == "" {
		return nil
	}

	run, ok := c.activeRunFor(sessionKey, sessionID)
	if !ok || strings.TrimSpace(run.RunID) == "" {
		if ref, ok2 := c.srv.bestEffortLastUserRun(project, sessionID); ok2 {
			run = ref
			c.SetActiveRun(project, sessionKey, sessionID, run.RunID, run.UserMessageID)
		}
	}

	content := strings.TrimSpace(r.Content)
	metadata := map[string]any{}
	status := "active"
	switch typ {
	case "reply_stream":
		// Keep run_events compact: store only terminal snapshots.
		if !r.Done {
			return nil
		}
		if strings.TrimSpace(r.Delta) != "" {
			metadata["delta"] = r.Delta
		}
		if strings.TrimSpace(r.FullText) != "" {
			metadata["full_text"] = r.FullText
		}
		metadata["done"] = r.Done
		if r.Done {
			status = "completed"
		}
		if strings.TrimSpace(r.PreviewHandle) != "" {
			metadata["preview_handle"] = strings.TrimSpace(r.PreviewHandle)
		}
		// Do not store stream deltas as primary content by default.
		if content == "" && strings.TrimSpace(r.Delta) != "" {
			content = r.Delta
		}
	case "preview_start":
		if strings.TrimSpace(r.RefID) != "" {
			metadata["ref_id"] = strings.TrimSpace(r.RefID)
		}
		if strings.TrimSpace(r.ReplyCtx) != "" {
			metadata["reply_ctx"] = strings.TrimSpace(r.ReplyCtx)
		}
	case "update_message", "delete_message":
		if strings.TrimSpace(r.PreviewHandle) != "" {
			metadata["preview_handle"] = strings.TrimSpace(r.PreviewHandle)
		}
	case "typing_stop":
		status = "completed"
	}

	ev := store.RunEvent{
		RunID:         strings.TrimSpace(run.RunID),
		UserMessageID: strings.TrimSpace(run.UserMessageID),
		SessionID:     sessionID,
		Type:          typ,
		Content:       content,
		Status:        status,
		CreatedAt:     nowUTC(),
		Timestamp:     nowUTC(),
		Metadata:      metadata,
	}
	stored, err := c.srv.store.AppendRunEvent(project, sessionID, ev)
	if err == nil {
		c.srv.runEvts.Publish(project, sessionID, stored)
	}
	return err
}

func (c *adapterClient) persistDurableAssistant(typ string, raw []byte) error {
	var r bridgeReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil
	}
	sessionKey := strings.TrimSpace(r.SessionKey)
	sessionID := strings.TrimSpace(r.SessionID)
	if sessionKey == "" || sessionID == "" {
		return nil
	}
	project := c.projectFor(sessionKey, sessionID)
	if project == "" {
		if p, ok := c.srv.bestEffortFindProjectForSession(sessionKey, sessionID); ok {
			project = p
			c.TrackSession(project, sessionKey, sessionID)
		}
	}
	if project == "" {
		return nil
	}

	run, ok := c.activeRunFor(sessionKey, sessionID)
	if !ok || strings.TrimSpace(run.RunID) == "" {
		if ref, ok2 := c.srv.bestEffortLastUserRun(project, sessionID); ok2 {
			run = ref
			c.SetActiveRun(project, sessionKey, sessionID, run.RunID, run.UserMessageID)
		}
	}

	atts, err := c.srv.persistBridgeReplyAttachments(typ, r, raw)
	if err != nil {
		// Persist text even if attachments failed.
		slog.Warn("webclient: persist attachments failed", "type", typ, "error", err)
	}

	content := strings.TrimSpace(r.Content)
	switch typ {
	case "file":
		if content == "" && len(atts) > 0 {
			content = "[file]"
		}
	case "image":
		if content == "" && len(atts) > 0 {
			content = "[image]"
		}
	case "card", "buttons":
		if content == "" {
			content = "[" + typ + "]"
		}
	}
	if content == "" {
		content = "[" + typ + "]"
	}

	msg := store.Message{
		Role:          store.RoleAssistant,
		Content:       content,
		Timestamp:     nowUTC(),
		RunID:         strings.TrimSpace(run.RunID),
		UserMessageID: strings.TrimSpace(run.UserMessageID),
		Attachments:   atts,
	}
	stored, err := c.srv.store.AppendMessage(project, sessionID, msg)
	if err != nil {
		return err
	}
	c.srv.events.Publish(project, sessionID, stored)

	// Terminal run_event so the UI can compute run completion from run_events.
	if strings.TrimSpace(run.RunID) != "" {
		ev := store.RunEvent{
			RunID:         strings.TrimSpace(run.RunID),
			UserMessageID: strings.TrimSpace(run.UserMessageID),
			SessionID:     sessionID,
			Type:          "run_completed",
			Status:        "completed",
			CreatedAt:     nowUTC(),
			Timestamp:     nowUTC(),
			Metadata: map[string]any{
				"final_type": typ,
			},
		}
		if sev, err := c.srv.store.AppendRunEvent(project, sessionID, ev); err == nil {
			c.srv.runEvts.Publish(project, sessionID, sev)
		}
	}
	return nil
}

// base64EncodeImage converts core.ImageAttachment data to bridge image wire fields.
func base64EncodeImage(mime string, data []byte, fileName string) bridgeImageData {
	return bridgeImageData{
		MimeType: strings.TrimSpace(mime),
		Data:     base64.StdEncoding.EncodeToString(data),
		FileName: strings.TrimSpace(fileName),
	}
}
