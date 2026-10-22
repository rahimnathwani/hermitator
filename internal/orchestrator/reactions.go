package orchestrator

import (
	"context"
	"slices"
	"strings"
)

// stopReactions are the emoji that, from the owner on a message the agent
// is working on or has queued, mean "never mind": tapping one is quicker
// than typing /stop on a phone.
var stopReactions = map[string]bool{"❌": true, "🛑": true, "⛔": true, "🚫": true, "✋": true}

// reaction acts on an owner's reaction to one of the chat's messages. A
// stop emoji on a message the running turn answers aborts the turn; on
// one still waiting for its turn, it withdraws the message. Any other
// reaction means nothing to hermitator.
func (m *Manager) reaction(ctx context.Context, agentID string, msg Message) {
	if !stopReactions[strings.ReplaceAll(strings.TrimSpace(msg.Reaction), "\ufe0f", "")] {
		return
	}
	var withdrawn *Message
	m.actor(agentID).call(func(a *actor) error {
		if t := a.turn; t != nil && slices.Contains(t.messages, msg.ReactsTo) {
			a.abort() // the turn's end reacts ⏹ on its message
			return nil
		}
		for i, p := range a.pending {
			if p.ID == msg.ReactsTo {
				withdrawn = &p
				a.pending = slices.Delete(a.pending, i, i+1)
				break
			}
		}
		return nil
	})
	if withdrawn != nil {
		m.answered(*withdrawn)
		chatRef{m: m, msg: *withdrawn}.react("⏹")
	}
}

// edited takes in the new text of a message. One still waiting for a
// turn, or held as context, is changed in place, so the agent reads
// what the user meant; one a running turn is already working on is told
// about, as a message of its own; one already answered is left alone.
func (m *Manager) edited(ctx context.Context, agentID string, msg Message) {
	text := strings.TrimSpace(msg.Text)
	var replaced, working bool
	a := m.actor(agentID)
	a.call(func(a *actor) error {
		for i := range a.pending {
			if a.pending[i].ID == msg.Edits {
				a.pending[i].Text = text
				replaced = true
			}
		}
		working = a.turn != nil && slices.Contains(a.turn.messages, msg.Edits)
		return nil
	})
	if replaced {
		return
	}
	if ok, _ := m.store.UpdateHeldText(ctx, msg.AccountID, msg.Edits, clip(text, 4000)); ok {
		return
	}
	if working && text != "" {
		again := msg
		again.Edits, again.Text = "", "(The user edited their earlier message to read:) "+text
		a.do(func(a *actor) { a.enqueue(again) })
	}
}
