package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/media"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// LinkCodeTTL is how long a /link code stays valid.
const LinkCodeTTL = 24 * time.Hour

// LinkCode returns an agent's current /link code, minting one if needed.
func (m *Manager) LinkCode(ctx context.Context, agentID string, fresh bool) (string, time.Time, error) {
	if !fresh {
		if code, exp, err := m.store.LinkCode(ctx, agentID); err == nil {
			return code, exp, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", time.Time{}, err
		}
	}
	code := ids.LinkCode()
	if err := m.store.PutLinkCode(ctx, agentID, code, LinkCodeTTL); err != nil {
		return "", time.Time{}, err
	}
	return code, time.Now().Add(LinkCodeTTL), nil
}

// HandleMessage routes one incoming chat message.
//
// The owner is whoever the linked account belongs to: messages the account
// itself sends (when it is the owner's own number) and messages from the
// owner's other numbers (when it is a number dedicated to agents). Only
// the owner can run commands.
//
// A linked chat sends its agent everything. An unlinked direct chat from
// one of the owner's numbers goes to the account's fallback agent, if it
// has one. Everything else, including strangers messaging an agent's
// number, is ignored.
func (m *Manager) HandleMessage(ctx context.Context, msg Message) {
	acct, err := m.store.Account(ctx, msg.AccountID)
	if err != nil {
		return
	}
	reply := func(text string) { chatRef{m: m, msg: msg}.send(text) }
	owner := msg.FromMe || m.store.IsOwnerNumber(ctx, acct.UserID, msg.SenderPhone)

	cmd, isCmd := ParseCommand(msg.Text)
	isCmd = isCmd && msg.Media == nil
	if isCmd && cmd.Name == "link" {
		// A valid code proves its sender can see the agent's page, so it
		// is accepted from anyone; guessing is throttled.
		m.link(ctx, acct, msg, cmd.Arg, owner, reply)
		return
	}

	route, err := m.store.Route(ctx, acct.ID, msg.Chat)
	var agentID string
	switch {
	case err == nil:
		agentID = route.AgentID
	case errors.Is(err, store.ErrNotFound):
		if acct.FallbackAgentID == "" || msg.FromMe || msg.IsGroup || !owner {
			return
		}
		agentID = acct.FallbackAgentID
		route = nil
	default:
		m.log.Error("route lookup", "err", err)
		return
	}
	msg.Owner = owner

	if isCmd && owner {
		m.command(ctx, acct, agentID, route, msg, cmd, reply)
		return
	}
	if strings.TrimSpace(msg.Text) == "" && msg.Media == nil {
		return
	}
	if route != nil {
		ag, err := m.store.Agent(ctx, agentID)
		if err != nil {
			return
		}
		if !triggers(route, ag, msg) {
			// Kept for the agent's next turn; nothing wakes up.
			m.hold(ctx, ag, msg)
			return
		}
	}
	m.actor(agentID).do(func(a *actor) { a.enqueue(msg) })
}

// Wrong /link codes allowed per sender per hour before hermitator stops
// answering them.
const linkAttempts = 5

// linkGuard throttles wrong /link codes per sender.
type linkGuard struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func (g *linkGuard) blocked(sender string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	var recent []time.Time
	for _, t := range g.failures[sender] {
		if time.Since(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if g.failures == nil {
		g.failures = map[string][]time.Time{}
	}
	g.failures[sender] = recent
	return len(recent) >= linkAttempts
}

func (g *linkGuard) fail(sender string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failures == nil {
		g.failures = map[string][]time.Time{}
	}
	g.failures[sender] = append(g.failures[sender], time.Now())
}

func (m *Manager) link(ctx context.Context, acct *store.Account, msg Message, code string, owner bool, reply func(string)) {
	who := msg.Sender
	if !owner && m.links.blocked(who) {
		return
	}
	invalid := "That link code is not valid. Codes expire after a day; get a fresh one from the agent's page in hermitator."
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		if owner {
			reply("Usage: /link CODE. You will find the code on the agent's page in hermitator.")
		}
		return
	}
	agentID, err := m.store.ConsumeLinkCode(ctx, code)
	var ag *store.Agent
	if err == nil {
		ag, err = m.store.Agent(ctx, agentID)
	}
	if err != nil || ag.UserID != acct.UserID {
		if !owner {
			m.links.fail(who)
		}
		reply(invalid)
		return
	}
	route := &store.Route{AccountID: acct.ID, ChatJID: msg.Chat, ChatName: msg.ChatName, AgentID: ag.ID,
		OwnerTrigger: store.TriggerAll, OthersTrigger: store.TriggerNone}
	if msg.IsGroup && msg.GroupSize > 2 {
		// In a group with other people, the owner talks to them too.
		route.OwnerTrigger = store.TriggerMentions
	}
	if err := m.store.PutRoute(ctx, route); err != nil {
		reply("⚠️ Could not link this chat: " + err.Error())
		return
	}
	m.store.Audit(ctx, acct.UserID, acct.UserID, "route.link", fmt.Sprintf("agent=%s chat=%s", ag.ID, msg.Chat))
	text := fmt.Sprintf("✅ Linked this chat to *%s* (%s). %s Change that with /listen or on the agent's page; /help lists commands.",
		ag.Name, harnessName(ag.Harness), describeTriggers(route, ag.Name))
	if !msg.FromMe && msg.SenderPhone != "" && !m.store.IsOwnerNumber(ctx, acct.UserID, msg.SenderPhone) {
		// Whoever had the code is the owner, writing from their own phone.
		if err := m.store.AddOwnerNumber(ctx, acct.UserID, msg.SenderPhone); err == nil {
			m.store.Audit(ctx, acct.UserID, acct.UserID, "number.add", "+"+msg.SenderPhone+" via /link")
			text += fmt.Sprintf("\n\nI will treat +%s as you from now on: its messages can run commands. You can change that in hermitator's settings.", msg.SenderPhone)
		}
	}
	reply(text)
}

func (m *Manager) command(ctx context.Context, acct *store.Account, agentID string, route *store.Route, msg Message, cmd Command, reply func(string)) {
	ag, err := m.store.Agent(ctx, agentID)
	if err != nil {
		return
	}
	switch cmd.Name {
	case "help":
		reply(helpText)
	case "retry":
		m.retry(ctx, agentID, msg, reply)
	case "sh":
		go m.shell(agentID, cmd.Arg, reply)
	case "unlink":
		if route == nil {
			reply("This chat is not linked; it reaches *" + ag.Name + "* as the account's fallback agent. Change that in hermitator.")
			return
		}
		m.store.DeleteRoute(ctx, acct.ID, msg.Chat)
		m.store.Audit(ctx, acct.UserID, acct.UserID, "route.unlink", fmt.Sprintf("agent=%s chat=%s", ag.ID, msg.Chat))
		reply("Unlinked this chat from *" + ag.Name + "*.")
	case "sleep":
		state, err := m.Sleep(ctx, agentID)
		switch {
		case errors.Is(err, ErrBusy):
			reply("*" + ag.Name + "* is busy. Send /stop first, or wait for it to finish.")
		case err != nil:
			reply("⚠️ Could not suspend: " + err.Error())
		case state == sandbox.Checkpointed:
			reply("😴 *" + ag.Name + "* is checkpointed to disk with its background processes. It uses no memory until the next message.")
		default:
			reply("😴 *" + ag.Name + "* is stopped. It uses no memory until the next message.")
		}
	case "stop":
		if m.Abort(agentID) {
			reply("⏹ Stopping the current turn.")
		} else {
			reply("Nothing is running.")
		}
	case "new":
		if err := m.NewSession(ctx, agentID); err != nil {
			reply("*" + ag.Name + "* is busy. Send /stop first, or wait for it to finish.")
			return
		}
		reply("🧹 The next message starts a fresh conversation. Files in the sandbox are untouched.")
	case "status":
		reply(m.statusText(ctx, ag))
	case "listen":
		m.listen(ctx, route, ag, cmd.Arg, reply)
	case "model":
		m.modelCommand(ctx, ag, cmd.Arg, reply)
	case "effort":
		m.effortCommand(ctx, ag, cmd.Arg, reply)
	case "once":
		model, effort, text := parseOnce(ag, cmd.Arg)
		if text == "" {
			reply("Usage: /once [MODEL] [EFFORT]: MESSAGE, for example /once opus max: review this design")
			return
		}
		once := msg
		once.Text, once.Model, once.Effort = text, model, effort
		// Asking explicitly wakes the agent whatever the chat's settings.
		m.actor(agentID).do(func(a *actor) { a.enqueue(once) })
	case "switch":
		target := strings.ToLower(cmd.Arg)
		if target != store.HarnessClaude && target != store.HarnessPi {
			reply("Usage: /switch claude or /switch pi")
			return
		}
		if target == ag.Harness {
			reply("*" + ag.Name + "* already uses " + harnessName(target) + ".")
			return
		}
		err := m.actor(agentID).call(func(a *actor) error {
			if a.turn != nil {
				return ErrBusy
			}
			prev := *ag
			ag.Harness = target
			fitHarness(&prev, ag)
			if err := m.store.UpdateAgentSettings(ctx, ag); err != nil {
				return err
			}
			return m.store.SetAgentSession(ctx, ag.ID, "")
		})
		if err != nil {
			reply("Could not switch: " + err.Error())
			return
		}
		m.store.Audit(ctx, acct.UserID, acct.UserID, "agent.switch", fmt.Sprintf("agent=%s harness=%s", ag.ID, target))
		reply("🔀 *" + ag.Name + "* now uses " + harnessName(target) + ", starting a fresh conversation in the same workspace.")
	}
}

func (m *Manager) statusText(ctx context.Context, ag *store.Agent) string {
	st := m.Status(ag.ID)
	state := st.State
	if state == "" {
		state = ag.State
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*%s* · %s", ag.Name, harnessName(ag.Harness))
	if ag.Harness == store.HarnessPi {
		fmt.Fprintf(&b, " (%s)", providerOrDefault(ag.PiProvider))
	}
	fmt.Fprintf(&b, "\nModel: %s", describeModel(ag))
	fmt.Fprintf(&b, "\nState: %s", describeState(state, st.Busy))
	if st.Queued > 0 {
		fmt.Fprintf(&b, ", %d message(s) queued", st.Queued)
	}
	if spec, err := m.Spec(ag); err == nil {
		if size, err := dirSize(spec.Dir + "/upper"); err == nil {
			fmt.Fprintf(&b, "\nDisk: %s of changes on top of %s", media.HumanSize(size), ag.Image)
		}
	}
	if !ag.LastActive.IsZero() {
		fmt.Fprintf(&b, "\nLast active: %s ago", time.Since(ag.LastActive).Round(time.Second))
	}
	if turns, err := m.store.Turns(ctx, ag.ID, 1); err == nil && len(turns) == 1 {
		t := turns[0]
		fmt.Fprintf(&b, "\nLast turn: %s, %d tool calls, %s", t.Status, t.Tools, t.Duration().Round(time.Second))
		if t.Model != "" {
			fmt.Fprintf(&b, " on %s", t.Model)
		}
	}
	return b.String()
}

func describeState(state string, busy bool) string {
	switch {
	case busy:
		return "working"
	case state == sandbox.Running:
		return "awake, idle"
	case state == sandbox.Checkpointed:
		return "checkpointed (0 MB RAM)"
	default:
		return "stopped (0 MB RAM)"
	}
}

func harnessName(h string) string {
	if h == store.HarnessPi {
		return "pi"
	}
	return "Claude Code"
}

// dirSize adds up the sizes of regular files under dir.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total, err
}
