package codex

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const codexGeneratedImageMaxBytes = 50 << 20

func collectCodexGeneratedImages(codexHome, sessionID string, after time.Time, delivered map[string]struct{}) []core.ImageAttachment {
	codexHome = strings.TrimSpace(codexHome)
	sessionID = strings.TrimSpace(sessionID)
	if codexHome == "" || sessionID == "" {
		return nil
	}
	pattern := filepath.Join(codexHome, "generated_images", sessionID, "*")
	matches, _ := filepath.Glob(pattern)
	sort.Strings(matches)

	var out []core.ImageAttachment
	for _, path := range matches {
		if delivered != nil {
			if _, ok := delivered[path]; ok {
				continue
			}
		}
		img, ok := loadCodexGeneratedImage(path, after)
		if !ok {
			continue
		}
		out = append(out, img)
		if delivered != nil {
			delivered[path] = struct{}{}
		}
	}
	return out
}

func loadCodexGeneratedImage(path string, after time.Time) (core.ImageAttachment, bool) {
	info, err := os.Stat(path)
	if err != nil || info == nil || !info.Mode().IsRegular() {
		return core.ImageAttachment{}, false
	}
	if !after.IsZero() && info.ModTime().Before(after.Add(-2*time.Second)) {
		return core.ImageAttachment{}, false
	}
	if info.Size() <= 0 || info.Size() > codexGeneratedImageMaxBytes {
		return core.ImageAttachment{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) != info.Size() || len(data) > codexGeneratedImageMaxBytes {
		return core.ImageAttachment{}, false
	}
	mimeType := codexGeneratedImageMime(path, data)
	if mimeType == "" {
		return core.ImageAttachment{}, false
	}
	return core.ImageAttachment{
		MimeType: mimeType,
		Data:     data,
		FileName: filepath.Base(path),
	}, true
}

func codexGeneratedImageMime(path string, data []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	}
	if len(data) == 0 {
		return ""
	}
	sniff := data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	switch mimeType := http.DetectContentType(sniff); mimeType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return mimeType
	default:
		return ""
	}
}

func resolveCodexHomeForGeneratedImages(extraEnv []string) string {
	codexHome, err := resolveCodexHome(extraEnv)
	if err != nil {
		return ""
	}
	return codexHome
}
