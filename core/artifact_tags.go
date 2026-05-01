package core

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

type artifactTagKind string

const (
	artifactTagMedia artifactTagKind = "MEDIA"
	artifactTagFile  artifactTagKind = "FILE"
)

type artifactReference struct {
	Kind artifactTagKind
	Path string
}

var artifactTagLineRe = regexp.MustCompile(`(?i)^\s*(MEDIA|FILE)\s*:\s*(.+?)\s*$`)

func extractArtifactReferences(text string) (string, []artifactReference) {
	if text == "" {
		return text, nil
	}

	lines := strings.SplitAfter(text, "\n")
	cleaned := make([]string, 0, len(lines))
	refs := make([]artifactReference, 0, 1)
	for _, line := range lines {
		body := strings.TrimRight(line, "\r\n")
		matches := artifactTagLineRe.FindStringSubmatch(body)
		if len(matches) != 3 {
			cleaned = append(cleaned, line)
			continue
		}
		path := normalizeArtifactTagPath(matches[2])
		if path == "" {
			cleaned = append(cleaned, line)
			continue
		}
		refs = append(refs, artifactReference{
			Kind: artifactTagKind(strings.ToUpper(matches[1])),
			Path: path,
		})
	}

	return strings.TrimRight(strings.Join(cleaned, ""), "\n\r "), refs
}

func normalizeArtifactTagPath(raw string) string {
	path := strings.TrimSpace(raw)
	path = strings.Trim(path, "`")
	if len(path) >= 2 {
		if (path[0] == '"' && path[len(path)-1] == '"') ||
			(path[0] == '\'' && path[len(path)-1] == '\'') ||
			(path[0] == '<' && path[len(path)-1] == '>') {
			path = strings.TrimSpace(path[1 : len(path)-1])
		}
	}
	if strings.HasPrefix(strings.ToLower(path), "file://") {
		u, err := url.Parse(path)
		if err != nil {
			return ""
		}
		if u.Host != "" && u.Host != "localhost" {
			return ""
		}
		unescaped, err := url.PathUnescape(u.Path)
		if err != nil {
			return ""
		}
		path = unescaped
	}
	if !filepath.IsAbs(path) {
		return ""
	}
	return filepath.Clean(path)
}

func artifactReferenceToOutboxItem(ref artifactReference) (OutboxItem, error) {
	if ref.Path == "" {
		return OutboxItem{}, fmt.Errorf("empty artifact path")
	}
	data, err := readOutboxFileNoFollow(ref.Path, -1, maxOutboxAttachmentBytes)
	if err != nil {
		return OutboxItem{}, fmt.Errorf("read artifact %q: %w", ref.Path, err)
	}
	item := outboxItemFromData(ref.Path, filepath.Base(ref.Path), data)
	if ref.Kind == artifactTagFile && item.Image != nil {
		item.File = &FileAttachment{
			MimeType: item.MimeType,
			Data:     item.Image.Data,
			FileName: item.Image.FileName,
		}
		item.Image = nil
	}
	return item, nil
}

func (e *Engine) deliverArtifactReferencesToState(ctx context.Context, state *interactiveState, refs []artifactReference) {
	for _, ref := range refs {
		item, err := artifactReferenceToOutboxItem(ref)
		if err != nil {
			slog.Warn("artifact tag: load failed", "path", ref.Path, "kind", ref.Kind, "error", err)
			continue
		}
		if err := e.deliverOutboxItemToState(ctx, state, item); err != nil {
			slog.Warn("artifact tag: deliver failed", "path", ref.Path, "kind", ref.Kind, "error", err)
		}
	}
}
