package orchestrator

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/spool"
	"github.com/rahimnathwani/hermitator/internal/store"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// Compacting before sleep. Harnesses resume a conversation by sending its
// whole history again, and their providers cache it for an hour at most,
// so the first turn after a long sleep pays full price for all of it:
// for an agent woken every few days, most of what it costs. Compacting
// just before the agent sleeps, while that history is still cached, is
// cheap, and every later wake re-reads a summary instead. The full
// transcript stays on disk, where agents are told to look.

const (
	// compactAbove is the conversation size, in tokens, worth compacting.
	compactAbove = 60_000
	// compactWarm is how soon after its last activity an agent must go
	// to sleep for its history to still be cached (Claude Code caches
	// for an hour).
	compactWarm = 45 * time.Minute
)

// compactFirst starts compacting an agent's conversation instead of
// suspending it, if that is worth doing; it reports whether it did. The
// compaction holds the turn slot, so messages arriving meanwhile wait for
// it, and the next reaper tick suspends the agent.
func (a *actor) compactFirst(ag *store.Agent) bool {
	if ag.ContextTokens < a.m.compactAbove() || ag.SessionID == "" || time.Since(a.lastActive) > compactWarm {
		return false
	}
	if a.m.overCap(a.ctx, ag) != "" {
		return false
	}
	t := &activeTurn{id: "compact-" + ids.New()}
	a.turn = t
	idleSince := a.lastActive
	go func() {
		a.m.compact(a, t, ag)
		a.do(func(a *actor) {
			a.turn = nil
			if len(a.pending) > 0 {
				a.lastActive = time.Now()
				a.startTurn()
			} else {
				// Still idle: the next tick suspends it.
				a.lastActive = idleSince
			}
		})
	}()
	return true
}

func (m *Manager) compactAbove() int {
	if m.cfg.CompactAbove > 0 {
		return m.cfg.CompactAbove
	}
	return compactAbove
}

func (m *Manager) compact(a *actor, t *activeTurn, ag *store.Agent) {
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
	defer cancel()
	log := m.log.With("agent", ag.ID, "turn", t.id)
	spec, err := m.Spec(ag)
	if err != nil {
		return
	}
	creds, problem := m.credentials(ctx, ag)
	if problem != "" {
		return
	}
	m.usePlaceholders(ctx, ag, spec, &creds)
	env := map[string]string{"TZ": m.location(ctx, ag).String()}
	for k, v := range creds.env {
		env[k] = v
	}
	req := turn.Request{Harness: ag.Harness, SessionID: ag.SessionID, Compact: true, Model: ag.Model,
		Provider: ag.PiProvider, Env: env, Files: creds.files}
	stdin, _ := json.Marshal(req)
	rec := &store.Turn{ID: t.id, AgentID: ag.ID, Harness: ag.Harness, SessionID: ag.SessionID,
		Input: "[Compacting the conversation before sleeping, so the next wake re-reads less]"}
	m.store.CreateTurn(ctx, rec)
	client := spec.Spool()
	if err := client.Submit(spool.Job{ID: t.id, Argv: []string{"hermitator-guest", "turn"}, Stdin: stdin}); err != nil {
		rec.Status, rec.Output = store.TurnError, err.Error()
		m.store.FinishTurn(ctx, rec)
		return
	}
	t.submitted(&client)
	var result *turn.Event
	_, err = client.Follow(ctx, t.id, func(line []byte) {
		var ev turn.Event
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		switch ev.Type {
		case turn.EventFile:
			m.syncFile(ctx, ag.UserID, ev)
		case turn.EventResult, turn.EventError:
			e := ev
			result = &e
		}
	})
	client.Remove(t.id)
	redact := newRedactor(creds.redact)
	switch {
	case err != nil || result == nil:
		rec.Status, rec.Output = store.TurnError, "the compaction did not finish"
	case result.Type == turn.EventError:
		rec.Status, rec.Output, rec.CostUSD = store.TurnError, redact.apply(result.Text), result.CostUSD
	default:
		sid := result.SessionID
		if sid == "" {
			sid = ag.SessionID
		}
		m.accountCost(ctx, ag, result, sid)
		rec.Status, rec.Output, rec.CostUSD, rec.ContextTokens = store.TurnOK, result.Text, result.CostUSD, result.ContextTokens
	}
	if t.wasAborted() {
		rec.Status = store.TurnAborted
	}
	m.store.FinishTurn(ctx, rec)
	// Compacted or not, it is not tried again until a turn measures the
	// conversation anew.
	m.store.SetAgentContext(ctx, ag.ID, rec.ContextTokens)
	log.Info("compacted before sleep", "status", rec.Status, "output", rec.Output, "cost_usd", rec.CostUSD)
}
