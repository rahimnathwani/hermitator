package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
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
// Only the account owner's own messages can run commands. A chat bound
// to an agent sends it everything; an unbound direct chat goes to the
// account's fallback agent if there is one, but only for messages from
// other people, so the agent does not answer the owner's own
// conversations with friends.
func (m *Manager) HandleMessage(ctx context.Context, msg Message) {
	acct, err := m.store.Account(ctx, msg.AccountID)
	if err != nil {
		return
	}
	reply := func(text string) { chatRef{m: m, msg: msg}.send(text) }

	cmd, isCmd := ParseCommand(msg.Text)
	isCmd = isCmd && msg.FromMe && msg.Media == nil
	if isCmd && cmd.Name == "link" {
		m.link(ctx, acct, msg, cmd.Arg, reply)
		return
	}

	route, err := m.store.Route(ctx, acct.ID, msg.Chat)
	var agentID string
	switch {
	case err == nil:
		agentID = route.AgentID
		if route.RequireMention && !msg.Mentioned && !msg.FromMe {
			return
		}
	case errors.Is(err, store.ErrNotFound):
		if acct.FallbackAgentID == "" || msg.FromMe || msg.IsGroup {
			return
		}
		agentID = acct.FallbackAgentID
	default:
		m.log.Error("route lookup", "err", err)
		return
	}

	if isCmd {
		m.command(ctx, acct, agentID, route, msg, cmd, reply)
		return
	}
	if strings.TrimSpace(msg.Text) == "" && msg.Media == nil {
		return
	}
	m.actor(agentID).do(func(a *actor) { a.enqueue(msg) })
}

func (m *Manager) link(ctx context.Context, acct *store.Account, msg Message, code string, reply func(string)) {
	invalid := "That link code is not valid. Codes expire after a day; get a fresh one from the agent's page in hermitator."
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		reply("Usage: /link CODE. You will find the code on the agent's page in hermitator.")
		return
	}
	agentID, err := m.store.ConsumeLinkCode(ctx, code)
	if err != nil {
		reply(invalid)
		return
	}
	ag, err := m.store.Agent(ctx, agentID)
	if err != nil || ag.UserID != acct.UserID {
		reply(invalid)
		return
	}
	if err := m.store.PutRoute(ctx, &store.Route{AccountID: acct.ID, ChatJID: msg.Chat, ChatName: msg.ChatName, AgentID: ag.ID}); err != nil {
		reply("⚠️ Could not link this chat: " + err.Error())
		return
	}
	m.store.Audit(ctx, acct.UserID, acct.UserID, "route.link", fmt.Sprintf("agent=%s chat=%s", ag.ID, msg.Chat))
	reply(fmt.Sprintf("✅ Linked this chat to *%s* (%s). Send a message to wake it up; /help lists commands.", ag.Name, harnessName(ag.Harness)))
}

func (m *Manager) command(ctx context.Context, acct *store.Account, agentID string, route *store.Route, msg Message, cmd Command, reply func(string)) {
	ag, err := m.store.Agent(ctx, agentID)
	if err != nil {
		return
	}
	switch cmd.Name {
	case "help":
		reply(helpText)
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
			ag.Harness = target
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
