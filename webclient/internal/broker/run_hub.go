package broker

import (
	"sync"

	"github.com/chenhg5/cc-connect/webclient/internal/store"
)

// RunHub is a minimal in-memory pub/sub for per-session run_event updates (SSE).
// Durable run_events always come from store; the Hub is best-effort.
type RunHub struct {
	mu     sync.RWMutex
	closed bool
	subs   map[string]map[chan store.RunEvent]struct{}
}

func NewRunHub() *RunHub {
	return &RunHub{
		subs: make(map[string]map[chan store.RunEvent]struct{}),
	}
}

func (h *RunHub) Subscribe(project, session string) (<-chan store.RunEvent, func()) {
	ch := make(chan store.RunEvent, 16)
	k := key(project, session)

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(ch)
		return ch, func() {}
	}
	m := h.subs[k]
	if m == nil {
		m = make(map[chan store.RunEvent]struct{})
		h.subs[k] = m
	}
	m[ch] = struct{}{}

	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closed {
			return
		}
		if mm := h.subs[k]; mm != nil {
			if _, ok := mm[ch]; ok {
				delete(mm, ch)
				close(ch)
			}
			if len(mm) == 0 {
				delete(h.subs, k)
			}
		}
	}
	return ch, cancel
}

func (h *RunHub) Publish(project, session string, ev store.RunEvent) {
	k := key(project, session)

	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return
	}
	m := h.subs[k]
	if len(m) == 0 {
		h.mu.RUnlock()
		return
	}
	chs := make([]chan store.RunEvent, 0, len(m))
	for ch := range m {
		chs = append(chs, ch)
	}
	h.mu.RUnlock()

	for _, ch := range chs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (h *RunHub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for _, m := range h.subs {
		for ch := range m {
			close(ch)
		}
	}
	clear(h.subs)
}

