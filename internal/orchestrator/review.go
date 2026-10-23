package orchestrator

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/spool"
	"github.com/rahimnathwani/hermitator/internal/store"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// Reviewing before sleep. Every few turns with a user message, an agent
// about to sleep looks back over the conversation, in a copy of it that is
// thrown away afterwards, and adds what will still matter to the memory
// file its harness reads at the start of every conversation. It runs in
// the same window as compaction, before it, while the conversation is
// still cached, with the last turn's system prompt so the cache is hit.
// It only adds; a line that looks wrong is put to the user instead.
// Adapted from Hermes's background memory review.

// reviewAfter is how many turns with a user message earn a review.
const reviewAfter = 10

// memoryFiles are where each harness keeps instructions it reads at the
// start of every conversation.
var memoryFiles = map[string]string{
	store.HarnessClaude: "~/.claude/CLAUDE.md",
	store.HarnessPi:     "~/.pi/agent/AGENTS.md",
}

const reviewPrompt = `[hermitator: memory review. This is a copy of the conversation, made before you sleep; it is thrown away afterwards, and the user sees nothing of it unless you reply to them below.]

Look back over the conversation for what will still matter in later conversations, and add it to %[1]s under the heading "## Remembered" (add the heading at the end if it is not there). That file is read at the start of every conversation.

- Save facts about the people you work for and their preferences, decisions and their reasons, how their projects, machines and accounts are set up, and corrections to how you should work.
- Write each as one short line, a dated declarative fact ("- %[2]s: staging listens on port 8443; deploy with make deploy"), not an instruction to yourself.
- Do not save what the file already says, progress on a task or anything else that will be stale within a week (the transcript keeps that, and hermitator-guest recall finds it), failures caused by something temporary, or secrets.
- Only add lines; never change or remove one. Keep the Remembered section under 3,000 characters: if it is full, add nothing.

Then reply with just NO_REPLY, unless a line already in the file now looks wrong or out of date: then reply to the user in a few words, quoting those lines and asking whether you should remove them.`

// reviewable reports whether a turn's messages include one from a person,
// not only scheduled runs and job reports.
func reviewable(batch []Message) bool {
	for _, msg := range batch {
		if msg.TaskID == "" && msg.JobName == "" {
			return true
		}
	}
	return false
}

// reviewFirst starts a review of a conversation instead of suspending the
// agent, if one is due; it reports whether it did.
func (a *actor) reviewFirst(ag *store.Agent, th *store.Thread) bool {
	system := a.lastSystem[th.ThreadKey]
	if th.ReviewTurns < reviewAfter || th.SessionID == "" || system == "" || memoryFiles[ag.Harness] == "" ||
		time.Since(th.LastAt) > compactWarm {
		return false
	}
	return a.beforeSleep(ag, "review-", func(t *activeTurn) { a.m.review(t, ag, th, system) })
}

func (m *Manager) review(t *activeTurn, ag *store.Agent, th *store.Thread, system string) {
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
	defer cancel()
	// Counted as tried whatever happens, or a failure would be retried on
	// every reaper tick.
	m.store.ResetReviewTurns(ctx, th.ThreadKey)
	log := m.log.With("agent", ag.ID, "turn", t.id)
	spec, err := m.Spec(ag)
	if err != nil {
		return
	}
	creds, problem := m.credentials(ctx, ag)
	if problem != "" {
		log.Info("not reviewing before sleep", "reason", problem)
		return
	}
	m.usePlaceholders(ctx, ag, spec, &creds)
	loc := m.location(ctx, ag)
	env := map[string]string{"TZ": loc.String()}
	for k, v := range creds.env {
		env[k] = v
	}
	req := turn.Request{Harness: ag.Harness, SessionID: th.SessionID, Fork: true, Discard: true,
		Prompt: fmt.Sprintf(reviewPrompt, memoryFiles[ag.Harness], time.Now().In(loc).Format("2006-01-02")),
		Model:  cmp.Or(ag.Model, creds.model), Effort: ag.Effort, Provider: ag.PiProvider, SystemPrompt: system, Env: env, Files: creds.files,
		MaxCostUSD: m.allowance(ctx, ag), SessionCostUSD: th.SessionCost}
	stdin, _ := json.Marshal(req)
	rec := &store.Turn{ID: t.id, AgentID: ag.ID, Harness: ag.Harness, SessionID: th.SessionID,
		Input: "[Looking back over the conversation for things to remember, before sleeping]"}
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
		case turn.EventTool:
			rec.Tools++
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
		rec.Status, rec.Output = store.TurnError, "the review did not finish"
	case result.Type == turn.EventError || result.IsError:
		rec.Status, rec.Output, rec.CostUSD = store.TurnError, redact.apply(result.Text), reviewCost(ag, th, result)
	default:
		rec.Status, rec.Output, rec.CostUSD = store.TurnOK, redact.apply(result.Text), reviewCost(ag, th, result)
	}
	if t.wasAborted() {
		rec.Status = store.TurnAborted
	}
	m.store.FinishTurn(ctx, rec)
	log.Info("reviewed before sleep", "status", rec.Status, "cost_usd", rec.CostUSD)
	if reply := strings.Trim(rec.Output, " \t\n.*`"); rec.Status == store.TurnOK && reply != noReply && reply != "" {
		chat := chatRef{m: m, msg: Message{AccountID: th.AccountID, Chat: th.ChatJID}, prefix: "🧠 *Memory*\n\n", redact: redact}
		chat.sendAgent(rec.Output)
	}
}

// reviewCost is what a review cost. Claude Code reports a running total,
// which a fork carries over from the session it copied.
func reviewCost(ag *store.Agent, th *store.Thread, result *turn.Event) float64 {
	if ag.Harness == store.HarnessClaude && result.CostUSD >= th.SessionCost {
		return result.CostUSD - th.SessionCost
	}
	return result.CostUSD
}
