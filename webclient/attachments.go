package webclient

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/webclient/internal/store"
)

func (s *Server) storeSaveImage(img core.ImageAttachment) (store.AttachmentMeta, store.Attachment, error) {
	mime := strings.TrimSpace(img.MimeType)
	name := strings.TrimSpace(img.FileName)
	if name == "" {
		name = "image" + extFromMime(mime)
	}
	meta, err := s.store.SaveAttachment(store.AttachmentMeta{
		FileName: name,
		MimeType: mime,
	}, bytes.NewReader(img.Data))
	if err != nil {
		return store.AttachmentMeta{}, store.Attachment{}, err
	}
	att := store.Attachment{
		ID:       meta.ID,
		Kind:     "image",
		FileName: meta.FileName,
		MimeType: meta.MimeType,
		Size:     meta.Size,
		URL:      s.store.AttachmentURL(meta.ID),
	}
	return meta, att, nil
}

func (s *Server) storeSaveFile(file core.FileAttachment) (store.AttachmentMeta, store.Attachment, error) {
	mime := strings.TrimSpace(file.MimeType)
	if mime == "" {
		mime = "application/octet-stream"
	}
	name := strings.TrimSpace(file.FileName)
	if name == "" {
		name = "file"
	}
	meta, err := s.store.SaveAttachment(store.AttachmentMeta{
		FileName: name,
		MimeType: mime,
	}, bytes.NewReader(file.Data))
	if err != nil {
		return store.AttachmentMeta{}, store.Attachment{}, err
	}
	att := store.Attachment{
		ID:       meta.ID,
		Kind:     "file",
		FileName: meta.FileName,
		MimeType: meta.MimeType,
		Size:     meta.Size,
		URL:      s.store.AttachmentURL(meta.ID),
	}
	return meta, att, nil
}

func extFromMime(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "":
		return ".png"
	default:
		// Keep a stable suffix even for unknown MIME types.
		return fmt.Sprintf(".%s", strings.ReplaceAll(strings.ReplaceAll(mime, "/", "_"), "+", "_"))
	}
}

