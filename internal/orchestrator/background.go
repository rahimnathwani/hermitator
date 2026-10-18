package orchestrator

import (
	"github.com/rahimnathwani/hermitator/internal/turn"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// maxBackgroundPerHour caps what an agent sends between turns, so a
// runaway loop cannot get its WhatsApp number banned for spam.
const maxBackgroundPerHour = 20

// background delivers what an awake agent's background jobs queue after
// its turn has ended: files and messages go to the chat it last talked
// in, and schedule requests are answered.
func (m *Manager) background(spec *sandbox.Spec, ag *store.Agent) {
	last, err := m.store.LastTurn(m.ctx, ag.ID)
	if err != nil || last.AccountID == "" {
		return
	}
	chat := chatRef{m: m, msg: Message{AccountID: last.AccountID, Chat: last.ChatJID}}
	m.handleRequests(m.ctx, spec, ag, chat)
	if !outboxPending(spec) {
		return
	}
	creds, problem := m.credentials(m.ctx, ag)
	if problem != "" {
		m.extraCredentials(m.ctx, ag, &creds)
	}
	chat.redact = newRedactor(creds.redact)
	a := m.actor(ag.ID)
	a.outbox.Lock()
	now := time.Now()
	recent := a.bgSent[:0]
	for _, t := range a.bgSent {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	a.bgSent = recent
	full := len(recent) >= maxBackgroundPerHour
	a.outbox.Unlock()
	if full {
		return // they wait in the outbox for the next turn or the next hour
	}
	if n := m.deliverOutbox(m.ctx, spec, chat); n > 0 {
		a.outbox.Lock()
		for range n {
			a.bgSent = append(a.bgSent, now)
		}
		a.outbox.Unlock()
	}
}

// outboxPending reports whether anything waits in the agent's outbox.
func outboxPending(spec *sandbox.Spec) bool {
	root, err := spec.Shared()
	if err != nil {
		return false
	}
	defer root.Close()
	dir, err := root.Open(turn.OutboxDir)
	if err != nil {
		return false
	}
	defer dir.Close()
	names, _ := dir.Readdirnames(-1)
	for _, n := range names {
		if strings.HasSuffix(n, ".meta.json") && !strings.HasPrefix(n, ".") {
			return true
		}
	}
	return false
}
