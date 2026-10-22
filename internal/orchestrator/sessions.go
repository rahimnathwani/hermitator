package orchestrator

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/rahimnathwani/hermitator/internal/store"
)

// Conversations: /new no longer loses the old one. /sessions lists them,
// /resume N goes back to one, and /fork branches off the current one so
// both can be continued. Each chat has its own, and these see only the
// chat they are sent in.

func (m *Manager) sessionsCommand(ctx context.Context, ag *store.Agent, k store.ThreadKey, reply func(string)) {
	list, _ := m.store.Sessions(ctx, k, 10)
	if len(list) == 0 {
		reply("*" + ag.Name + "* has not had a conversation in this chat yet.")
		return
	}
	th, _ := m.store.Thread(ctx, k)
	loc := m.location(ctx, ag)
	var b strings.Builder
	fmt.Fprintf(&b, "*%s's conversations*\n", ag.Name)
	for i, s := range list {
		current := ""
		if th != nil && s.SessionID == th.SessionID {
			current = " (current)"
		}
		other := ""
		if s.Harness != ag.Harness {
			other = ", " + harnessName(s.Harness)
		}
		first := clip(strings.Join(strings.Fields(s.FirstPrompt), " "), 70)
		fmt.Fprintf(&b, "\n%d.%s %s\n    %s, %d turn%s%s", i+1, current, first, s.LastAt.In(loc).Format("2 Jan 15:04"), s.Turns, plural(s.Turns), other)
	}
	b.WriteString("\n\n/resume N goes back to one; /fork branches off the current one.")
	reply(b.String())
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (m *Manager) resumeCommand(ctx context.Context, ag *store.Agent, k store.ThreadKey, arg string, reply func(string)) {
	list, _ := m.store.Sessions(ctx, k, 10)
	th, err := m.store.Thread(ctx, k)
	if err != nil {
		reply("⚠️ " + err.Error())
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || n < 1 || n > len(list) {
		reply("Which one? /sessions lists them with their numbers; then /resume N.")
		return
	}
	s := list[n-1]
	switch {
	case s.SessionID == th.SessionID:
		reply("That is the current conversation already.")
		return
	case s.Harness != ag.Harness:
		reply(fmt.Sprintf("That conversation was with %s; /switch %s first.", harnessName(s.Harness), s.Harness))
		return
	}
	err = m.actor(ag.ID).call(func(a *actor) error {
		if a.turn != nil {
			return ErrBusy
		}
		a.forgetThread(k)
		if err := m.store.SetThreadSessionCost(ctx, k, s.SessionID, s.CostTotal); err != nil {
			return err
		}
		// Its size is not known until its next turn says.
		return m.store.SetThreadContext(ctx, k, 0)
	})
	if err == ErrBusy {
		reply("*" + ag.Name + "* is busy. Send /stop first, or wait for it to finish.")
		return
	} else if err != nil {
		reply("⚠️ " + err.Error())
		return
	}
	reply("↩️ Back to: " + clip(strings.Join(strings.Fields(s.FirstPrompt), " "), 100) + ". The next message carries on from there.")
}

func (m *Manager) forkCommand(ctx context.Context, ag *store.Agent, k store.ThreadKey, reply func(string)) {
	th, err := m.store.Thread(ctx, k)
	switch {
	case ag.Harness != store.HarnessClaude:
		reply("/fork works with Claude Code; pi agents can start afresh with /new and go back with /resume.")
		return
	case err != nil || th.SessionID == "":
		reply("There is no conversation to branch off yet.")
		return
	}
	m.actor(ag.ID).do(func(a *actor) {
		if a.forkNext == nil {
			a.forkNext = map[store.ThreadKey]bool{}
		}
		a.forkNext[k] = true
	})
	reply("🌿 Your next message branches off this conversation. The current one stays as it is, under /sessions.")
}
