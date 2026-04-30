package core

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxImageAttachments     = 4
	maxImageAttachmentBytes = 5 * 1024 * 1024
)

var allowedImageMimeTypes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/webp": {},
	"image/gif":  {},
}

// MergeEnv returns base env with entries from extra overriding same-key entries.
// This prevents duplicate keys (e.g. two PATH entries) which cause the override
// to be silently ignored on Linux (getenv returns the first match).
func MergeEnv(base, extra []string) []string {
	keys := make(map[string]bool, len(extra))
	for _, e := range extra {
		if k, _, ok := strings.Cut(e, "="); ok {
			keys[k] = true
		}
	}
	merged := make([]string, 0, len(base)+len(extra))
	for _, e := range base {
		if k, _, ok := strings.Cut(e, "="); ok && keys[k] {
			continue
		}
		merged = append(merged, e)
	}
	return append(merged, extra...)
}

// CheckAllowFrom logs a security warning at startup when allow_from is not
// configured (defaults to permit-all). Platforms should call this during init.
func CheckAllowFrom(platform, allowFrom string) {
	if strings.TrimSpace(allowFrom) == "" {
		slog.Warn("allow_from is not set — all users are permitted. "+
			"Set allow_from in config to restrict access.",
			"platform", platform)
	}
}

// RedactToken replaces a secret token in text with [REDACTED] to prevent
// token leakage in logs or error messages.
func RedactToken(text, token string) string {
	if token == "" || text == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "[REDACTED]")
}

// AllowList checks whether a user ID is permitted based on a comma-separated
// allow_from string. Returns true if allowFrom is empty or "*" (allow all),
// or if the userID is in the list. Comparison is case-insensitive.
func AllowList(allowFrom, userID string) bool {
	allowFrom = strings.TrimSpace(allowFrom)
	if allowFrom == "" || allowFrom == "*" {
		return true
	}
	for _, id := range strings.Split(allowFrom, ",") {
		if strings.EqualFold(strings.TrimSpace(id), userID) {
			return true
		}
	}
	return false
}

// ImageAttachment represents an image sent by the user.
type ImageAttachment struct {
	MimeType string // e.g. "image/png", "image/jpeg"
	Data     []byte // raw image bytes
	FileName string // original filename (optional)
}

func validateImageAttachmentCount(count int) error {
	if count > maxImageAttachments {
		return fmt.Errorf("too many images: %d > %d", count, maxImageAttachments)
	}
	return nil
}

func imageAttachmentFromBase64(mimeType, data, fileName string) (ImageAttachment, error) {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if _, ok := allowedImageMimeTypes[mimeType]; !ok {
		return ImageAttachment{}, fmt.Errorf("unsupported image mime type %q", mimeType)
	}

	data = stripDataURLPrefix(strings.TrimSpace(data))
	if data == "" {
		return ImageAttachment{}, fmt.Errorf("image data is required")
	}
	if len(data) > base64.StdEncoding.EncodedLen(maxImageAttachmentBytes) {
		return ImageAttachment{}, fmt.Errorf("image is too large: encoded payload exceeds %d bytes", maxImageAttachmentBytes)
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return ImageAttachment{}, fmt.Errorf("invalid image base64: %w", err)
	}
	if len(decoded) > maxImageAttachmentBytes {
		return ImageAttachment{}, fmt.Errorf("image is too large: %d > %d bytes", len(decoded), maxImageAttachmentBytes)
	}
	return ImageAttachment{
		MimeType: mimeType,
		Data:     decoded,
		FileName: strings.TrimSpace(fileName),
	}, nil
}

func stripDataURLPrefix(data string) string {
	if !strings.HasPrefix(data, "data:") {
		return data
	}
	if idx := strings.IndexByte(data, ','); idx >= 0 {
		return data[idx+1:]
	}
	return data
}

// HistoryImageAttachment is the JSON-safe representation of an image stored in
// conversation history. Data is base64 text, never raw bytes.
type HistoryImageAttachment struct {
	MimeType string `json:"mime_type,omitempty"`
	Data     string `json:"data,omitempty"`
	FileName string `json:"file_name,omitempty"`
	Size     int    `json:"size,omitempty"`
}

// HistoryImagesFromAttachments converts runtime image bytes into a stable,
// JSON-safe history representation.
func HistoryImagesFromAttachments(images []ImageAttachment) []HistoryImageAttachment {
	if len(images) == 0 {
		return nil
	}
	out := make([]HistoryImageAttachment, 0, len(images))
	for _, img := range images {
		size := len(img.Data)
		entry := HistoryImageAttachment{
			MimeType: strings.TrimSpace(img.MimeType),
			FileName: strings.TrimSpace(img.FileName),
			Size:     size,
		}
		if size > 0 {
			entry.Data = base64.StdEncoding.EncodeToString(img.Data)
		}
		out = append(out, entry)
	}
	return out
}

// FileAttachment represents a file (PDF, doc, spreadsheet, etc.) sent by the user.
type FileAttachment struct {
	MimeType string // e.g. "application/pdf", "text/plain"
	Data     []byte // raw file bytes
	FileName string // original filename
}

// SaveFilesToDisk saves file attachments to workDir/.cc-connect/attachments/
// and returns the list of absolute file paths. Agents can reference these paths
// in their prompts so the CLI can read them with built-in tools.
func SaveFilesToDisk(workDir string, files []FileAttachment) []string {
	if len(files) == 0 {
		return nil
	}
	attachDir := filepath.Join(workDir, ".cc-connect", "attachments")
	if err := os.MkdirAll(attachDir, 0o755); err != nil {
		slog.Warn("SaveFilesToDisk: mkdir failed", "dir", attachDir, "error", err)
	}

	var paths []string
	for i, f := range files {
		fname := f.FileName
		if fname == "" {
			fname = fmt.Sprintf("file_%d_%d", time.Now().UnixMilli(), i)
		}
		fpath := filepath.Join(attachDir, fname)
		if err := os.WriteFile(fpath, f.Data, 0o644); err != nil {
			slog.Error("SaveFilesToDisk: write failed", "error", err)
			continue
		}
		paths = append(paths, fpath)
		slog.Debug("SaveFilesToDisk: file saved", "path", fpath, "name", f.FileName, "mime", f.MimeType, "size", len(f.Data))
	}
	return paths
}

// AppendFileRefs appends file path references to a prompt string.
func AppendFileRefs(prompt string, filePaths []string) string {
	if len(filePaths) == 0 {
		return prompt
	}
	if prompt == "" {
		prompt = "Please analyze the attached file(s)."
	}
	return prompt + "\n\n(Files saved locally, please read them: " + strings.Join(filePaths, ", ") + ")"
}

// AudioAttachment represents a voice/audio message sent by the user.
type AudioAttachment struct {
	MimeType string // e.g. "audio/amr", "audio/ogg", "audio/mp4"
	Data     []byte // raw audio bytes
	Format   string // short format hint: "amr", "ogg", "m4a", "mp3", "wav", etc.
	Duration int    // duration in seconds (if known)
}

// LocationAttachment represents a geographical location sent by the user.
type LocationAttachment struct {
	Latitude             float64 // latitude coordinate
	Longitude            float64 // longitude coordinate
	HorizontalAccuracy   float64 // accuracy radius in meters (optional)
	LivePeriod           int     // time period for live location updates in seconds (optional)
	Heading              int     // direction of movement in degrees (optional)
	ProximityAlertRadius int     // maximum distance for proximity alerts in meters (optional)
}

// Message represents a unified incoming message from any platform.
type Message struct {
	SessionKey   string // unique key for user context, e.g. "feishu:{chatID}:{userID}"
	SessionID    string // optional cc-connect conversation ID under SessionKey
	Platform     string
	MessageID    string // platform message ID for tracing
	UserID       string
	UserName     string
	ChatName     string // human-readable chat/group name (optional)
	Content      string
	Images       []ImageAttachment   // attached images (if any)
	Files        []FileAttachment    // attached files (if any)
	Audio        *AudioAttachment    // voice message (if any)
	Location     *LocationAttachment // geographical location (if any)
	ExtraContent string              // platform-enriched content (e.g. location text, reply quote) prepended for the agent
	ChannelKey   string              // platform-provided channel identifier for workspace binding (optional)
	ReplyCtx     any                 // platform-specific context needed for replying
	FromVoice    bool                // true if message originated from voice transcription
	ModeOverride string              // if set, temporarily override agent permission mode for this message
}

// EventType distinguishes different kinds of agent output.
type EventType string

const (
	EventText              EventType = "text"               // intermediate or final text
	EventToolUse           EventType = "tool_use"           // tool invocation info
	EventToolResult        EventType = "tool_result"        // tool execution result
	EventResult            EventType = "result"             // final aggregated result
	EventError             EventType = "error"              // error occurred
	EventPermissionRequest EventType = "permission_request" // agent requests permission via stdio protocol
	EventThinking          EventType = "thinking"           // thinking/processing status
)

// UserQuestion represents a structured question from AskUserQuestion.
type UserQuestion struct {
	Question    string               `json:"question"`
	Header      string               `json:"header"`
	Options     []UserQuestionOption `json:"options"`
	MultiSelect bool                 `json:"multiSelect"`
}

// UserQuestionOption is one choice in a UserQuestion.
type UserQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Event represents a single piece of agent output streamed back to the engine.
type Event struct {
	Type         EventType
	Content      string
	ToolName     string         // populated for EventToolUse, EventPermissionRequest
	ToolInput    string         // human-readable summary of tool input
	ToolInputRaw map[string]any // raw tool input (for EventPermissionRequest, used in allow response)
	ToolResult   string         // populated for EventToolResult
	ToolStatus   string         // optional status for EventToolResult (e.g. completed/failed)
	ToolExitCode *int           // optional exit code for EventToolResult
	ToolSuccess  *bool          // optional success flag for EventToolResult
	SessionID    string         // agent-managed session ID for conversation continuity
	RequestID    string         // unique request ID for EventPermissionRequest
	Questions    []UserQuestion // populated when ToolName == "AskUserQuestion"
	Done         bool
	Error        error
	InputTokens  int // token usage from agent result events
	OutputTokens int
}

// HistoryEntry is one turn in a conversation.
type HistoryEntry struct {
	Role      string                   `json:"role"` // "user" or "assistant"
	Content   string                   `json:"content"`
	Images    []HistoryImageAttachment `json:"images,omitempty"`
	Timestamp time.Time                `json:"timestamp"`
}

func historyEntryMap(h HistoryEntry, includeTimestamp bool) map[string]any {
	entry := map[string]any{
		"role":    h.Role,
		"content": h.Content,
	}
	if len(h.Images) > 0 {
		entry["images"] = h.Images
	}
	if includeTimestamp {
		entry["timestamp"] = h.Timestamp
	}
	return entry
}

// AgentSessionInfo describes one session as reported by the agent backend.
type AgentSessionInfo struct {
	ID           string
	Summary      string
	MessageCount int
	ModifiedAt   time.Time
	GitBranch    string
}
