package broker

import (
	"sync"

	"github.com/chenhg5/cc-connect/webclient/internal/store"
)

// Hub is a minimal in-memory pub/sub for per-session realtime events (SSE).
// Durable history always comes from store; the Hub is best-effort.
type Hub struct {
	mu     sync.RWMutex
	closed bool
	subs   map[string]map[chan store.Message]struct{}
}

func NewHub() *Hub {
	return &Hub{
		subs: make(map[string]map[chan store.Message]struct{}),
	}
}

func key(project, session string) string { return project + "\n" + session }

func (h *Hub) Subscribe(project, session string) (<-chan store.Message, func()) {
	ch := make(chan store.Message, 16)
	k := key(project, session)

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(ch)
		return ch, func() {}
	}
	m := h.subs[k]
	if m == nil {
		m = make(map[chan store.Message]struct{})
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

func (h *Hub) Publish(project, session string, msg store.Message) {
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
	// Copy channels under read lock, then publish without holding it.
	chs := make([]chan store.Message, 0, len(m))
	for ch := range m {
		chs = append(chs, ch)
	}
	h.mu.RUnlock()

	for _, ch := range chs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (h *Hub) Close() {
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

