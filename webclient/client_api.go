package webclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/webclient/internal/store"
	"github.com/google/uuid"
)

// v1JSON/v1Error mirror the management API envelope expected by the copied
// admin frontend: {"ok": true, "data": ...} or {"ok": false, "error": "..."}.
func v1JSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
}

func v1Error(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

func (s *Server) handleV1GetSettings(w http.ResponseWriter, r *http.Request) {
	env, status, err := s.mgmtDo(r.Context(), http.MethodGet, "/api/v1/settings", nil)
	if err != nil || status < 200 || status >= 300 || !env.OK {
		v1Error(w, http.StatusServiceUnavailable, "upstream settings unavailable")
		return
	}
	var data map[string]any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		v1Error(w, http.StatusInternalServerError, "decode upstream settings failed")
		return
	}
	local, err := s.store.GetWebClientSettings()
	if err != nil {
		v1Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["webclient_display"] = map[string]any{
		"run_trace_mode": local.WebClientDisplay.RunTraceMode,
	}
	v1JSON(w, http.StatusOK, data)
}

func (s *Server) handleV1PatchSettings(w http.ResponseWriter, r *http.Request) {
	var updates map[string]any
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		v1Error(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if updates == nil {
		updates = map[string]any{}
	}

	// Extract local-only settings.
	localMode := ""
	if raw, ok := updates["webclient_display"]; ok {
		if m, ok := raw.(map[string]any); ok {
			if v, ok := m["run_trace_mode"].(string); ok {
				localMode = v
			}
		}
	}
	if localMode == "" {
		if v, ok := updates["run_trace_mode"].(string); ok {
			localMode = v
		}
	}
	delete(updates, "webclient_display")
	delete(updates, "run_trace_mode")

	if strings.TrimSpace(localMode) != "" {
		if _, err := s.store.SetRunTraceMode(localMode); err != nil {
			v1Error(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// Forward remaining updates to upstream management settings.
	var out map[string]any
	if len(updates) > 0 {
		env, status, err := s.mgmtDo(r.Context(), http.MethodPatch, "/api/v1/settings", updates)
		if err != nil || status < 200 || status >= 300 || !env.OK {
			v1Error(w, http.StatusBadGateway, "upstream settings save failed")
			return
		}
		_ = json.Unmarshal(env.Data, &out)
	} else {
		// No upstream updates; fetch current upstream settings so we can return a merged view.
		env, status, err := s.mgmtDo(r.Context(), http.MethodGet, "/api/v1/settings", nil)
		if err != nil || status < 200 || status >= 300 || !env.OK {
			v1Error(w, http.StatusBadGateway, "upstream settings fetch failed")
			return
		}
		_ = json.Unmarshal(env.Data, &out)
	}
	if out == nil {
		out = map[string]any{}
	}
	local, _ := s.store.GetWebClientSettings()
	out["webclient_display"] = map[string]any{
		"run_trace_mode": local.WebClientDisplay.RunTraceMode,
	}
	v1JSON(w, http.StatusOK, out)
}

type v1SessionReq struct {
	SessionKey string `json:"session_key"`
	Name       string `json:"name,omitempty"`
}

type v1SendReq struct {
	SessionKey string          `json:"session_key"`
	SessionID  string          `json:"session_id,omitempty"`
	Message    string          `json:"message,omitempty"`
	Action     string          `json:"action,omitempty"`
	ReplyCtx   string          `json:"reply_ctx,omitempty"`
	Images     []v1BridgeImage `json:"images,omitempty"`
}

type v1BridgeImage struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64
	FileName string `json:"file_name,omitempty"`
	Size     int    `json:"size,omitempty"`
}

const outboxPayloadKindV1Send = "v1_send"
const outboxPayloadKindV1CardAction = "v1_card_action"

type outboxPayloadV1Send struct {
	Kind        string             `json:"kind"`
	SessionKey  string             `json:"session_key"`
	SessionID   string             `json:"session_id"`
	Message     string             `json:"message,omitempty"`
	Attachments []store.Attachment `json:"attachments,omitempty"`
}

type outboxPayloadV1CardAction struct {
	Kind       string `json:"kind"`
	SessionKey string `json:"session_key"`
	SessionID  string `json:"session_id"`
	Action     string `json:"action"`
	ReplyCtx   string `json:"reply_ctx,omitempty"`
}

func (s *Server) handleV1ListSessions(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if err := store.ValidateSegment("project", project); err != nil {
		v1Error(w, http.StatusBadRequest, err.Error())
		return
	}

	sessions, activeKeys, err := s.store.ListClientSessions(project)
	if err != nil {
		v1Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if activeKeys == nil {
		activeKeys = map[string]string{}
	}

	agentType := s.bestEffortProjectAgentType(r.Context(), project)
	out := make([]map[string]any, 0, len(sessions))
	for _, meta := range sessions {
		updated := meta.UpdatedAt
		if updated.IsZero() {
			updated = meta.CreatedAt
		}

		var last map[string]any
		if meta.LastMessage != nil {
			preview := meta.LastMessage.Content
			if len(preview) > 200 {
				preview = preview[:200]
			}
			last = map[string]any{
				"role":      meta.LastMessage.Role,
				"content":   preview,
				"timestamp": meta.LastMessage.Timestamp,
			}
			if imgs := v1ImagesFromAttachments(meta.LastMessage.Attachments); len(imgs) > 0 {
				last["images"] = imgs
			}
			if files := v1FilesFromAttachments(meta.LastMessage.Attachments); len(files) > 0 {
				last["files"] = files
			}
			if !meta.LastMessage.Timestamp.IsZero() {
				updated = meta.LastMessage.Timestamp
			}
		}

		out = append(out, map[string]any{
			"id":            meta.ID,
			"session_key":   meta.SessionKey,
			"name":          meta.Name,
			"alias_mode":    "",
			"alias_suffix":  "",
			"platform":      v1PlatformFromSessionKey(meta.SessionKey),
			"agent_type":    firstNonEmpty(strings.TrimSpace(meta.AgentType), agentType),
			"active":        meta.Active,
			"live":          meta.Live,
			"created_at":    meta.CreatedAt,
			"updated_at":    updated,
			"history_count": meta.HistoryCount,
			"last_message":  last,
		})
	}

	v1JSON(w, http.StatusOK, map[string]any{
		"sessions":    out,
		"active_keys": activeKeys,
	})
}

func (s *Server) handleV1CreateSession(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if err := store.ValidateSegment("project", project); err != nil {
		v1Error(w, http.StatusBadRequest, err.Error())
		return
	}

	var req v1SessionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		v1Error(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.SessionKey = strings.TrimSpace(req.SessionKey)
	req.Name = strings.TrimSpace(req.Name)
	if req.SessionKey == "" {
		v1Error(w, http.StatusBadRequest, "session_key is required")
		return
	}
	if req.Name == "" {
		req.Name = "default"
	}

	id, sessionKey, name, createdAt, updatedAt, upstreamOK := s.tryCreateUpstreamSession(r.Context(), project, req.SessionKey, req.Name)
	if !upstreamOK {
		now := time.Now().UTC()
		id = "web_" + uuid.NewString()
		sessionKey = req.SessionKey
		name = req.Name
		createdAt = now
		updatedAt = now
	}

	meta, err := s.store.CreateClientSession(project, store.CreateClientSessionInput{
		ID:         id,
		SessionKey: sessionKey,
		Name:       name,
		CreatedAt:  createdAt,
	})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		v1Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Best-effort: keep updated_at close to upstream.
	if updatedAt.IsZero() {
		updatedAt = meta.UpdatedAt
	}

	v1JSON(w, http.StatusOK, map[string]any{
		"id":          meta.ID,
		"session_key": meta.SessionKey,
		"name":        meta.Name,
		"created_at":  meta.CreatedAt,
		"updated_at":  updatedAt,
	})
}

func (s *Server) handleV1GetSession(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	id := strings.TrimSpace(r.PathValue("id"))
	if err := store.ValidateSegment("project", project); err != nil {
		v1Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if id == "" {
		v1Error(w, http.StatusBadRequest, "id is required")
		return
	}

	histLimit := 50
	if v := r.URL.Query().Get("history_limit"); v != "" {
		if n, err := strconvAtoiPositive(v); err == nil && n > 0 {
			histLimit = n
		}
	}

	detail, err := s.store.GetClientSession(project, id, histLimit)
	if err != nil {
		if err == store.ErrNotFound {
			v1Error(w, http.StatusNotFound, "session not found")
			return
		}
		v1Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	agentType := s.bestEffortProjectAgentType(r.Context(), project)
	activeID, _ := s.store.ActiveSessionID(project, detail.SessionKey)

	history := make([]map[string]any, 0, len(detail.History))
	for i, m := range detail.History {
		seq := i + 1
		runID := strings.TrimSpace(m.RunID)
		userMsgID := strings.TrimSpace(m.UserMessageID)
		if runID == "" {
			runID = strings.TrimSpace(m.ID)
		}
		if userMsgID == "" {
			userMsgID = strings.TrimSpace(m.ID)
		}
		entry := map[string]any{
			"id":              m.ID,
			"seq":             seq,
			"run_id":          runID,
			"user_message_id": userMsgID,
			"role":            m.Role,
			"content":         m.Content,
			"timestamp":       m.Timestamp,
			"created_at":      m.Timestamp,
		}
		if imgs := v1ImagesFromAttachments(m.Attachments); len(imgs) > 0 {
			entry["images"] = imgs
		}
		if files := v1FilesFromAttachments(m.Attachments); len(files) > 0 {
			entry["files"] = files
		}
		history = append(history, entry)
	}

	// Attach run_events for optional "run trace" UI modes. These are not part of
	// the durable history timeline and must never be treated as assistant messages.
	runEventsLimit := 200
	if v := r.URL.Query().Get("run_events_limit"); v != "" {
		if n, err := strconvAtoiPositive(v); err == nil && n > 0 {
			runEventsLimit = n
		}
	}
	runEventsRaw, _ := s.store.ReadRunEvents(project, id, runEventsLimit)
	runEvents := make([]map[string]any, 0, len(runEventsRaw))
	for i, ev := range runEventsRaw {
		seq := ev.Seq
		if seq <= 0 {
			seq = i + 1
		}
		entry := map[string]any{
			"id":              ev.ID,
			"seq":             seq,
			"run_id":          ev.RunID,
			"user_message_id": ev.UserMessageID,
			"session_id":      ev.SessionID,
			"type":            ev.Type,
			"content":         ev.Content,
			"status":          ev.Status,
			"created_at":      ev.CreatedAt,
			"timestamp":       firstNonEmptyTime(ev.Timestamp, ev.CreatedAt),
			"metadata":        ev.Metadata,
		}
		runEvents = append(runEvents, entry)
	}

	updated := detail.UpdatedAt
	if updated.IsZero() && len(detail.History) > 0 {
		updated = detail.History[len(detail.History)-1].Timestamp
	}
	if updated.IsZero() {
		updated = detail.CreatedAt
	}

	var last map[string]any
	if detail.LastMessage != nil {
		preview := detail.LastMessage.Content
		if len(preview) > 200 {
			preview = preview[:200]
		}
		last = map[string]any{
			"role":      detail.LastMessage.Role,
			"content":   preview,
			"timestamp": detail.LastMessage.Timestamp,
		}
		if imgs := v1ImagesFromAttachments(detail.LastMessage.Attachments); len(imgs) > 0 {
			last["images"] = imgs
		}
		if files := v1FilesFromAttachments(detail.LastMessage.Attachments); len(files) > 0 {
			last["files"] = files
		}
	}

	v1JSON(w, http.StatusOK, map[string]any{
		"id":               detail.ID,
		"session_key":      detail.SessionKey,
		"name":             detail.Name,
		"alias_mode":       "",
		"alias_suffix":     "",
		"platform":         v1PlatformFromSessionKey(detail.SessionKey),
		"agent_type":       firstNonEmpty(strings.TrimSpace(detail.AgentType), agentType),
		"agent_session_id": "",
		"active":           strings.TrimSpace(activeID) != "" && activeID == detail.ID,
		"live":             detail.Live,
		"created_at":       detail.CreatedAt,
		"updated_at":       updated,
		"history_count":    detail.HistoryCount,
		"last_message":     last,
		"history":          history,
		"run_events":       runEvents,
	})
}

func (s *Server) handleV1PatchSession(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	id := strings.TrimSpace(r.PathValue("id"))
	if err := store.ValidateSegment("project", project); err != nil {
		v1Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if id == "" {
		v1Error(w, http.StatusBadRequest, "id is required")
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		v1Error(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		v1Error(w, http.StatusBadRequest, "name is required")
		return
	}

	if _, err := s.store.RenameClientSession(project, id, name); err != nil {
		if err == store.ErrNotFound {
			v1Error(w, http.StatusNotFound, "session not found")
			return
		}
		v1Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Best-effort upstream sync (does not change response).
	_ = s.tryPatchUpstreamSession(r.Context(), project, id, name)

	v1JSON(w, http.StatusOK, map[string]any{"id": id, "name": name})
}

func (s *Server) handleV1DeleteSession(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	id := strings.TrimSpace(r.PathValue("id"))
	if err := store.ValidateSegment("project", project); err != nil {
		v1Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if id == "" {
		v1Error(w, http.StatusBadRequest, "id is required")
		return
	}

	_ = s.tryDeleteUpstreamSession(r.Context(), project, id)
	if err := s.store.DeleteClientSession(project, id); err != nil {
		v1Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	v1JSON(w, http.StatusOK, map[string]any{"message": "session deleted"})
}

func (s *Server) handleV1SwitchSession(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if err := store.ValidateSegment("project", project); err != nil {
		v1Error(w, http.StatusBadRequest, err.Error())
		return
	}

	var body struct {
		SessionKey string `json:"session_key"`
		SessionID  string `json:"session_id"`
		Target     string `json:"target,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		v1Error(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	body.SessionKey = strings.TrimSpace(body.SessionKey)
	body.SessionID = strings.TrimSpace(body.SessionID)
	if body.SessionID == "" {
		body.SessionID = strings.TrimSpace(body.Target)
	}
	if body.SessionKey == "" || body.SessionID == "" {
		v1Error(w, http.StatusBadRequest, "session_key and session_id are required")
		return
	}

	_ = s.trySwitchUpstreamSession(r.Context(), project, body.SessionKey, body.SessionID)
	if err := s.store.SetActiveSession(project, body.SessionKey, body.SessionID); err != nil {
		v1Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	v1JSON(w, http.StatusOK, map[string]any{
		"message":           "active session switched",
		"session_key":       body.SessionKey,
		"session_id":        body.SessionID,
		"active_session_id": body.SessionID,
	})
}

func (s *Server) handleV1Send(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if err := store.ValidateSegment("project", project); err != nil {
		v1Error(w, http.StatusBadRequest, err.Error())
		return
	}

	var body v1SendReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		v1Error(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	body.SessionKey = strings.TrimSpace(body.SessionKey)
	body.SessionID = strings.TrimSpace(body.SessionID)
	body.Message = strings.TrimSpace(body.Message)
	body.Action = strings.TrimSpace(body.Action)
	body.ReplyCtx = strings.TrimSpace(body.ReplyCtx)
	if body.SessionKey == "" && body.SessionID == "" {
		v1Error(w, http.StatusBadRequest, "session_key or session_id is required")
		return
	}
	if body.Message == "" && len(body.Images) == 0 && body.Action == "" {
		v1Error(w, http.StatusBadRequest, "message or attachment is required")
		return
	}
	if body.Action != "" && len(body.Images) > 0 {
		v1Error(w, http.StatusBadRequest, "card_action cannot include images")
		return
	}
	if len(body.Images) > 4 {
		v1Error(w, http.StatusBadRequest, "too many images")
		return
	}

	// Resolve session_key when omitted but session_id is known.
	if body.SessionKey == "" && body.SessionID != "" {
		if det, err := s.store.GetClientSession(project, body.SessionID, 0); err == nil {
			body.SessionKey = strings.TrimSpace(det.SessionKey)
		}
	}
	if body.SessionKey == "" {
		v1Error(w, http.StatusBadRequest, "session_key is required")
		return
	}

	// Resolve session_id when omitted by older clients.
	if body.SessionID == "" && body.SessionKey != "" {
		if id, err := s.store.ActiveSessionID(project, body.SessionKey); err == nil && id != "" {
			body.SessionID = id
		}
	}
	if body.SessionID == "" {
		// Create a new session record (and upstream if available) to avoid losing the message.
		id, sessionKey, name, createdAt, _, upstreamOK := s.tryCreateUpstreamSession(r.Context(), project, body.SessionKey, "default")
		if !upstreamOK {
			now := time.Now().UTC()
			id = "web_" + uuid.NewString()
			sessionKey = body.SessionKey
			name = "default"
			createdAt = now
		}
		_, _ = s.store.CreateClientSession(project, store.CreateClientSessionInput{
			ID:         id,
			SessionKey: sessionKey,
			Name:       name,
			CreatedAt:  createdAt,
		})
		body.SessionID = id
	}

	isAction := strings.TrimSpace(body.Action) != ""

	// Persist user message first (including image uploads saved as attachments).
	var msgAttachments []store.Attachment
	var coreImages []core.ImageAttachment
	if !isAction {
		for _, img := range body.Images {
			att, coreAtt, err := s.persistIncomingImage(img)
			if err != nil {
				v1Error(w, http.StatusBadRequest, "invalid images: "+err.Error())
				return
			}
			msgAttachments = append(msgAttachments, att)
			coreImages = append(coreImages, coreAtt)
		}
	}

	userMsgID := uuid.NewString()
	userContent := body.Message
	if isAction {
		userContent = "[card_action] " + strings.TrimSpace(body.Action)
	}
	stored, err := s.store.AppendMessage(project, body.SessionID, store.Message{
		ID:            userMsgID,
		Role:          store.RoleUser,
		Content:       userContent,
		RunID:         userMsgID,
		UserMessageID: userMsgID,
		Attachments:   msgAttachments,
	})
	if err != nil {
		v1Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.events.Publish(project, body.SessionID, stored)

	// Durable outbox record (persist before attempting delivery).
	var payloadBytes []byte
	if isAction {
		payloadBytes, err = json.Marshal(outboxPayloadV1CardAction{
			Kind:       outboxPayloadKindV1CardAction,
			SessionKey: body.SessionKey,
			SessionID:  body.SessionID,
			Action:     strings.TrimSpace(body.Action),
			ReplyCtx:   strings.TrimSpace(body.ReplyCtx),
		})
	} else {
		payloadBytes, err = json.Marshal(outboxPayloadV1Send{
			Kind:        outboxPayloadKindV1Send,
			SessionKey:  body.SessionKey,
			SessionID:   body.SessionID,
			Message:     body.Message,
			Attachments: msgAttachments,
		})
	}
	if err != nil {
		v1Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	ob, err := s.store.CreateOutboxItem(store.CreateOutboxItemInput{
		ID:          userMsgID,
		Project:     project,
		SessionID:   body.SessionID,
		SessionKey:  body.SessionKey,
		Payload:     payloadBytes,
		NextRetryAt: time.Now().UTC(),
	})
	if err != nil {
		v1Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.adapter != nil {
		s.adapter.SetActiveRun(project, body.SessionKey, body.SessionID, userMsgID, userMsgID)
	}
	var deliverErr error
	if isAction {
		deliverErr = s.deliverV1CardActionToUpstreamBridge(r.Context(), project, body.SessionKey, body.SessionID, body.Action, body.ReplyCtx)
	} else {
		deliverErr = s.deliverV1SendToUpstreamBridge(r.Context(), project, body, ob.ID, coreImages)
	}
	if deliverErr != nil {
		// Mark failed before returning error.
		if _, markErr := s.store.MarkOutboxFailed(project, ob.ID, deliverErr.Error(), time.Now().UTC()); markErr != nil {
			v1Error(w, http.StatusInternalServerError, markErr.Error())
			return
		}
		v1Error(w, http.StatusServiceUnavailable, deliverErr.Error())
		return
	}
	if _, err := s.store.MarkOutboxSent(project, ob.ID); err != nil {
		v1Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	data := map[string]any{"message": "message queued"}
	if body.SessionKey != "" {
		data["session_key"] = body.SessionKey
	}
	if body.SessionID != "" {
		data["session_id"] = body.SessionID
	}
	data["outbox_id"] = ob.ID
	v1JSON(w, http.StatusOK, data)
}

func (s *Server) deliverOutboxItem(ctx context.Context, item store.OutboxItem) error {
	if len(item.Payload) == 0 {
		return fmt.Errorf("outbox payload is empty")
	}
	var kind struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(item.Payload, &kind); err != nil {
		return fmt.Errorf("decode outbox payload: %w", err)
	}
	switch strings.TrimSpace(kind.Kind) {
	case outboxPayloadKindV1Send:
		var p outboxPayloadV1Send
		if err := json.Unmarshal(item.Payload, &p); err != nil {
			return fmt.Errorf("decode outbox payload: %w", err)
		}
		body := v1SendReq{
			SessionKey: strings.TrimSpace(p.SessionKey),
			SessionID:  strings.TrimSpace(p.SessionID),
			Message:    strings.TrimSpace(p.Message),
		}
		if body.SessionKey == "" || body.SessionID == "" {
			return fmt.Errorf("outbox payload is missing session_key or session_id")
		}
		coreImages, err := s.coreImagesFromAttachments(p.Attachments)
		if err != nil {
			return err
		}
		if s.adapter != nil {
			s.adapter.SetActiveRun(item.Project, body.SessionKey, body.SessionID, item.ID, item.ID)
		}
		return s.deliverV1SendToUpstreamBridge(ctx, item.Project, body, item.ID, coreImages)

	case outboxPayloadKindV1CardAction:
		var p outboxPayloadV1CardAction
		if err := json.Unmarshal(item.Payload, &p); err != nil {
			return fmt.Errorf("decode outbox payload: %w", err)
		}
		sessionKey := strings.TrimSpace(p.SessionKey)
		sessionID := strings.TrimSpace(p.SessionID)
		action := strings.TrimSpace(p.Action)
		replyCtx := strings.TrimSpace(p.ReplyCtx)
		if sessionKey == "" || sessionID == "" || action == "" {
			return fmt.Errorf("outbox payload is missing session_key/session_id/action")
		}
		if s.adapter != nil {
			s.adapter.SetActiveRun(item.Project, sessionKey, sessionID, item.ID, item.ID)
		}
		return s.deliverV1CardActionToUpstreamBridge(ctx, item.Project, sessionKey, sessionID, action, replyCtx)

	default:
		return fmt.Errorf("unsupported outbox payload kind %q", strings.TrimSpace(kind.Kind))
	}
}

func (s *Server) coreImagesFromAttachments(atts []store.Attachment) ([]core.ImageAttachment, error) {
	if len(atts) == 0 {
		return nil, nil
	}
	out := make([]core.ImageAttachment, 0, len(atts))
	for _, a := range atts {
		if strings.ToLower(strings.TrimSpace(a.Kind)) != "image" {
			continue
		}
		if strings.TrimSpace(a.ID) == "" {
			continue
		}
		meta, path, err := s.store.OpenAttachment(a.ID)
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read attachment %s: %w", a.ID, err)
		}
		out = append(out, core.ImageAttachment{
			MimeType: strings.TrimSpace(meta.MimeType),
			Data:     b,
			FileName: strings.TrimSpace(meta.FileName),
		})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (s *Server) deliverV1SendToUpstreamBridge(ctx context.Context, project string, body v1SendReq, msgID string, coreImages []core.ImageAttachment) error {
	if s.adapter == nil {
		return fmt.Errorf("upstream bridge adapter is not configured")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if err := s.adapter.WaitConnected(waitCtx); err != nil {
		return fmt.Errorf("upstream bridge adapter not ready: %w", err)
	}

	wire := make([]bridgeImageData, 0, len(coreImages))
	for _, img := range coreImages {
		if len(img.Data) == 0 {
			continue
		}
		wire = append(wire, base64EncodeImage(img.MimeType, img.Data, img.FileName))
	}
	return s.adapter.SendBridgeMessage(ctx, project, body, msgID, nil, wire)
}

func (s *Server) deliverV1CardActionToUpstreamBridge(ctx context.Context, project, sessionKey, sessionID, action, replyCtx string) error {
	if s.adapter == nil {
		return fmt.Errorf("upstream bridge adapter is not configured")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if err := s.adapter.WaitConnected(waitCtx); err != nil {
		return fmt.Errorf("upstream bridge adapter not ready: %w", err)
	}
	return s.adapter.SendCardAction(ctx, project, sessionKey, sessionID, action, replyCtx)
}

func (s *Server) fetchUpstreamBridgeConfig(ctx context.Context) (path string, token string, port int, err error) {
	env, status, err := s.mgmtDo(ctx, http.MethodGet, "/api/v1/status", nil)
	if err != nil || status < 200 || status >= 300 || !env.OK {
		return "", "", 0, fmt.Errorf("fetch upstream status failed")
	}
	var data struct {
		Bridge struct {
			Enabled bool   `json:"enabled"`
			Port    int    `json:"port"`
			Path    string `json:"path"`
			Token   string `json:"token"`
		} `json:"bridge"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return "", "", 0, fmt.Errorf("decode upstream status: %w", err)
	}
	if !data.Bridge.Enabled {
		return "", "", 0, fmt.Errorf("upstream bridge is disabled")
	}
	return strings.TrimSpace(data.Bridge.Path), strings.TrimSpace(data.Bridge.Token), data.Bridge.Port, nil
}

func (s *Server) persistIncomingImage(img v1BridgeImage) (store.Attachment, core.ImageAttachment, error) {
	mime := strings.TrimSpace(img.MimeType)
	if mime == "" {
		mime = "image/png"
	}
	switch strings.ToLower(mime) {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		// allowed
	default:
		return store.Attachment{}, core.ImageAttachment{}, fmt.Errorf("unsupported image mime type %q", mime)
	}
	dataB64 := strings.TrimSpace(img.Data)
	if dataB64 == "" {
		return store.Attachment{}, core.ImageAttachment{}, fmt.Errorf("image data is required")
	}
	decoded, err := base64.StdEncoding.DecodeString(stripDataURLPrefix(dataB64))
	if err != nil {
		return store.Attachment{}, core.ImageAttachment{}, fmt.Errorf("invalid image base64: %w", err)
	}
	const maxImageBytes = 5 * 1024 * 1024
	if len(decoded) > maxImageBytes {
		return store.Attachment{}, core.ImageAttachment{}, fmt.Errorf("image is too large: %d > %d bytes", len(decoded), maxImageBytes)
	}
	coreImg := core.ImageAttachment{
		MimeType: mime,
		Data:     decoded,
		FileName: strings.TrimSpace(img.FileName),
	}
	_, att, err := s.storeSaveImage(coreImg)
	if err != nil {
		return store.Attachment{}, core.ImageAttachment{}, err
	}
	return att, coreImg, nil
}

func stripDataURLPrefix(data string) string {
	data = strings.TrimSpace(data)
	if strings.HasPrefix(data, "data:") {
		if i := strings.IndexByte(data, ','); i >= 0 {
			return strings.TrimSpace(data[i+1:])
		}
	}
	return data
}

func v1ImagesFromAttachments(atts []store.Attachment) []map[string]any {
	if len(atts) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(atts))
	for _, a := range atts {
		if strings.ToLower(strings.TrimSpace(a.Kind)) != "image" {
			continue
		}
		out = append(out, map[string]any{
			"id":        a.ID,
			"mime_type": a.MimeType,
			"url":       a.URL,
			"file_name": a.FileName,
			"size":      a.Size,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func v1FilesFromAttachments(atts []store.Attachment) []map[string]any {
	if len(atts) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(atts))
	for _, a := range atts {
		if strings.ToLower(strings.TrimSpace(a.Kind)) != "file" {
			continue
		}
		u := strings.TrimSpace(a.URL)
		if u == "" && strings.TrimSpace(a.ID) != "" {
			u = "/attachments/" + a.ID
		}
		out = append(out, map[string]any{
			"id":        a.ID,
			"kind":      "file",
			"mime_type": a.MimeType,
			"url":       u,
			"file_name": a.FileName,
			"size":      a.Size,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func v1PlatformFromSessionKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if i := strings.IndexByte(key, ':'); i > 0 {
		return key[:i]
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return strings.TrimSpace(b)
}

func firstNonEmptyTime(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}
	return b
}

// --- Upstream helpers ------------------------------------------------------

type mgmtEnvelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

func (s *Server) mgmtDo(ctx context.Context, method, path string, body any) (mgmtEnvelope, int, error) {
	base := strings.TrimSpace(s.opts.ManagementBaseURL)
	if base == "" {
		return mgmtEnvelope{}, 0, fmt.Errorf("management proxy is not configured")
	}
	u, err := url.Parse(base + path)
	if err != nil {
		return mgmtEnvelope{}, 0, err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return mgmtEnvelope{}, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return mgmtEnvelope{}, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := strings.TrimSpace(s.opts.ManagementToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return mgmtEnvelope{}, 0, err
	}
	defer res.Body.Close()
	var env mgmtEnvelope
	_ = json.NewDecoder(res.Body).Decode(&env)
	return env, res.StatusCode, nil
}

func (s *Server) tryCreateUpstreamSession(ctx context.Context, project, sessionKey, name string) (id, outKey, outName string, createdAt, updatedAt time.Time, ok bool) {
	env, status, err := s.mgmtDo(ctx, http.MethodPost, "/api/v1/projects/"+url.PathEscape(project)+"/sessions", map[string]string{
		"session_key": sessionKey,
		"name":        name,
	})
	if err != nil || status < 200 || status >= 300 || !env.OK {
		return "", "", "", time.Time{}, time.Time{}, false
	}
	var data struct {
		ID         string    `json:"id"`
		SessionKey string    `json:"session_key"`
		Name       string    `json:"name"`
		CreatedAt  time.Time `json:"created_at"`
		UpdatedAt  time.Time `json:"updated_at"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return "", "", "", time.Time{}, time.Time{}, false
	}
	id = strings.TrimSpace(data.ID)
	outKey = strings.TrimSpace(data.SessionKey)
	outName = strings.TrimSpace(data.Name)
	if id == "" || outKey == "" {
		return "", "", "", time.Time{}, time.Time{}, false
	}
	createdAt = data.CreatedAt
	updatedAt = data.UpdatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	return id, outKey, outName, createdAt, updatedAt, true
}

func (s *Server) tryPatchUpstreamSession(ctx context.Context, project, id, name string) error {
	_, _, err := s.mgmtDo(ctx, http.MethodPatch, "/api/v1/projects/"+url.PathEscape(project)+"/sessions/"+url.PathEscape(id), map[string]string{
		"name": name,
	})
	return err
}

func (s *Server) tryDeleteUpstreamSession(ctx context.Context, project, id string) error {
	_, _, err := s.mgmtDo(ctx, http.MethodDelete, "/api/v1/projects/"+url.PathEscape(project)+"/sessions/"+url.PathEscape(id), nil)
	return err
}

func (s *Server) trySwitchUpstreamSession(ctx context.Context, project, sessionKey, sessionID string) error {
	_, _, err := s.mgmtDo(ctx, http.MethodPost, "/api/v1/projects/"+url.PathEscape(project)+"/sessions/switch", map[string]string{
		"session_key": sessionKey,
		"session_id":  sessionID,
	})
	return err
}

func (s *Server) bestEffortProjectAgentType(ctx context.Context, project string) string {
	env, status, err := s.mgmtDo(ctx, http.MethodGet, "/api/v1/projects/"+url.PathEscape(project), nil)
	if err != nil || status < 200 || status >= 300 || !env.OK {
		return ""
	}
	var data struct {
		AgentType string `json:"agent_type"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return ""
	}
	return strings.TrimSpace(data.AgentType)
}

func strconvAtoiPositive(v string) (int, error) {
	n := 0
	for _, r := range strings.TrimSpace(v) {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not an int")
		}
		n = n*10 + int(r-'0')
		if n > 1000000 {
			return 0, fmt.Errorf("too large")
		}
	}
	return n, nil
}

// atomicWriteFile writes data to path atomically by writing to a temp file and
// renaming it into place.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename tmp: %w", err)
	}
	return nil
}
