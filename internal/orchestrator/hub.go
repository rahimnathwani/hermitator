package orchestrator

import "sync"

// hub fans UI events out to subscribers, per agent. Slow subscribers miss
// events rather than slowing the agent down.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan UIEvent]struct{}
}

func (h *hub) subscribe(agentID string) (<-chan UIEvent, func()) {
	ch := make(chan UIEvent, 64)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[string]map[chan UIEvent]struct{}{}
	}
	if h.subs[agentID] == nil {
		h.subs[agentID] = map[chan UIEvent]struct{}{}
	}
	h.subs[agentID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[agentID], ch)
		h.mu.Unlock()
	}
}

func (h *hub) publish(agentID string, ev UIEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[agentID] {
		select {
		case ch <- ev:
		default:
		}
	}
}
