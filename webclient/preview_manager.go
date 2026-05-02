package webclient

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// previewManager generates preview handles and keeps a small in-memory mapping
// from preview ref_id -> preview_handle for debugging and correlation.
//
// The bridge protocol requires adapters to ack preview_start quickly; this
// manager is intentionally simple and does not depend on browser availability.
type previewManager struct {
	seq uint64

	mu    sync.Mutex
	byRef map[string]string
}

func newPreviewManager() *previewManager {
	return &previewManager{byRef: make(map[string]string)}
}

func (m *previewManager) newHandle() string {
	n := atomic.AddUint64(&m.seq, 1)
	// uuid makes collisions practically impossible; keep handle short-ish and stable.
	return fmt.Sprintf("webclient-prev-%d-%s", n, uuid.NewString())
}

func (m *previewManager) HandlePreviewStart(refID string) string {
	refID = strings.TrimSpace(refID)
	h := m.newHandle()
	m.mu.Lock()
	if refID != "" {
		m.byRef[refID] = h
	}
	// best-effort pruning
	if len(m.byRef) > 2048 {
		// Drop all; this is only a correlation cache.
		clear(m.byRef)
	}
	m.mu.Unlock()
	return h
}

func (m *previewManager) Lookup(refID string) (string, bool) {
	refID = strings.TrimSpace(refID)
	if refID == "" {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.byRef[refID]
	return h, ok
}

// nowUTC exists for tests (override via package var if needed).
var nowUTC = func() time.Time { return time.Now().UTC() }
