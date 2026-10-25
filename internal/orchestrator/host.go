package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// HostOps is what /host asks of the server itself, outside any agent:
// cmd/hermitator provides it (internal/hostops). Nothing in it takes an
// argument that reaches a shell.
type HostOps interface {
	Version() string
	// Facts are lines about the machine: uptime, load, memory, disk.
	Facts(ctx context.Context) []string
	// Doctor runs `hermitator doctor` and reports whether it passed.
	Doctor(ctx context.Context) (string, bool)
	// Logs returns recent warnings and errors, unredacted.
	Logs(ctx context.Context, n int) (string, error)
	// Restart ends the process for systemd to start again.
	Restart()
}

// SetHost enables /host. Call it before Start, which tells whoever asked
// for a restart that it is over.
func (m *Manager) SetHost(h HostOps) { m.host = h }

// restartNotice is who asked for a restart, so the next process can tell
// them it is back.
type restartNotice struct {
	AccountID string    `json:"account_id"`
	Chat      string    `json:"chat"`
	At        time.Time `json:"at"`
}

const (
	restartNoticeFile = "restart-notice.json"
	// A notice older than this is from a restart that never finished
	// coming back, and is dropped rather than announced late.
	restartNoticeFor = 15 * time.Minute
	defaultLogLines  = 20
	maxLogLines      = 100
)

func (m *Manager) restartNoticePath() string {
	if m.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(m.cfg.StateDir, restartNoticeFile)
}

// hostChat reports whether /host may answer in msg's chat: one linked to
// an agent, a direct chat with the agents' number, or the owner's chat
// with themselves. On the owner's own number, everything they send counts
// as theirs, so without this /host typed in a chat with a friend would
// tell the friend about the server.
func (m *Manager) hostChat(ctx context.Context, acct *store.Account, msg Message) bool {
	if _, err := m.store.Route(ctx, acct.ID, msg.Chat); err == nil {
		return true
	}
	if msg.IsGroup {
		return false
	}
	if acct.Personal() {
		return acct.Phone != "" && msg.Chat == acct.Phone+"@s.whatsapp.net"
	}
	return !msg.FromMe
}

// hostCommand runs /host for a user an admin has made a host operator.
func (m *Manager) hostCommand(ctx context.Context, acct *store.Account, msg Message, arg string, reply func(string)) {
	u, err := m.store.User(ctx, acct.UserID)
	if err != nil {
		return
	}
	if !u.HostOperator {
		reply("/host reports on and restarts the whole server, so only users an admin allows can run it. An admin turns it on for you on the Users page.")
		return
	}
	if m.host == nil {
		reply("/host is not available on this server.")
		return
	}
	sub, rest, _ := strings.Cut(strings.ToLower(strings.TrimSpace(arg)), " ")
	rest = strings.TrimSpace(rest)
	audit := func(detail string) {
		action := "host.status"
		if sub != "" {
			action = "host." + sub
		}
		m.store.Audit(ctx, acct.UserID, acct.UserID, action, detail)
	}
	switch sub {
	case "", "status":
		audit("")
		reply(m.hostStatus(ctx))
	case "doctor":
		audit("")
		go func() {
			text, ok := m.host.Doctor(m.ctx)
			head := "🩺 *All checks pass*"
			if !ok {
				head = "🩺 *Some checks failed*"
			}
			reply(head + "\n\n" + text)
		}()
	case "logs", "log":
		n := defaultLogLines
		if rest != "" {
			v, err := strconv.Atoi(rest)
			if err != nil || v < 1 {
				reply("Usage: /host logs [N], the last N warnings and errors (default 20, at most 100).")
				return
			}
			n = min(v, maxLogLines)
		}
		audit(strconv.Itoa(n))
		go func() {
			text, err := m.host.Logs(m.ctx, n)
			if err != nil {
				reply("⚠️ Could not read the log: " + err.Error())
				return
			}
			reply("📜 *Recent warnings and errors*\n\n" + m.everySecret(m.ctx).apply(redactTokens(text)))
		}()
	case "restart":
		if rest != "yes" {
			busy := m.busyAgents(ctx)
			text := "Restarting hermitator takes a few seconds. Sandboxes keep running, and WhatsApp reconnects by itself."
			switch len(busy) {
			case 0:
			case 1:
				text += fmt.Sprintf(" *%s* is working now; its turn is cut off, and /retry in its chat runs it again.", busy[0])
			default:
				text += fmt.Sprintf(" %d agents are working now (%s); their turns are cut off, and /retry in each chat runs them again.", len(busy), strings.Join(busy, ", "))
			}
			reply(text + "\n\nSend /host restart yes to go ahead.")
			return
		}
		audit("")
		if path := m.restartNoticePath(); path != "" {
			b, _ := json.Marshal(restartNotice{AccountID: msg.AccountID, Chat: msg.Chat, At: time.Now()})
			if err := os.WriteFile(path, b, 0o600); err != nil {
				m.log.Warn("restart notice", "err", err)
			}
		}
		reply("🔁 Restarting. I will say so here when it is back.")
		go func() {
			// Let the reply go out first.
			time.Sleep(time.Second)
			m.host.Restart()
		}()
	default:
		reply(hostHelp)
	}
}

const hostHelp = `*/host*, for the server itself
/host: version, uptime, load, memory, disk, agents and WhatsApp numbers
/host doctor: hermitator doctor's checks
/host logs [N]: the last N warnings and errors (default 20)
/host restart: restart hermitator; sandboxes keep running`

func (m *Manager) hostStatus(ctx context.Context) string {
	var b strings.Builder
	b.WriteString("🖥 *Host*\n")
	for _, line := range m.host.Facts(ctx) {
		b.WriteString("\n" + line)
	}
	agents, _ := m.store.Agents(ctx, "")
	var awake, working int
	for _, a := range agents {
		st := m.Status(a.ID)
		state := st.State
		if state == "" {
			state = a.State
		}
		if state == sandbox.Running {
			awake++
		}
		if st.Busy {
			working++
		}
	}
	fmt.Fprintf(&b, "\nAgents: %d, %d awake, %d working", len(agents), awake, working)
	if fams := m.cfg.Images.Families(); len(fams) > 0 {
		var imgs []string
		for _, f := range fams {
			if cur, err := m.cfg.Images.Current(f); err == nil {
				imgs = append(imgs, cur)
			}
		}
		b.WriteString("\nImage: " + strings.Join(imgs, ", "))
	}
	return b.String()
}

// busyAgents names the agents with a turn running.
func (m *Manager) busyAgents(ctx context.Context) []string {
	agents, _ := m.store.Agents(ctx, "")
	var out []string
	for _, a := range agents {
		if m.Status(a.ID).Busy {
			out = append(out, a.Name)
		}
	}
	return out
}

// tellRestarted tells whoever asked for a restart that this process is
// up. WhatsApp connects after Start, so it keeps trying for a while.
func (m *Manager) tellRestarted() {
	path := m.restartNoticePath()
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	os.Remove(path)
	var n restartNotice
	if json.Unmarshal(b, &n) != nil || time.Since(n.At) > restartNoticeFor || n.AccountID == "" || n.Chat == "" {
		return
	}
	took := time.Since(n.At).Round(time.Second)
	text := "✅ hermitator is back"
	if m.host != nil {
		text += ", running " + m.host.Version()
	}
	text += fmt.Sprintf(", %s after you asked.", took)
	m.goLoop(func() {
		for range 60 {
			if _, err := m.msgr.SendText(m.ctx, n.AccountID, n.Chat, text); err == nil {
				return
			}
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	})
}

// NotifyOperators sends text to every host operator, in the chat each
// one's WhatsApp number would use to reach them.
func (m *Manager) NotifyOperators(ctx context.Context, text string) {
	users, err := m.store.HostOperators(ctx)
	if err != nil {
		return
	}
	for _, u := range users {
		if !m.notifyUser(ctx, u.ID, text) {
			m.log.Info("host operator has no WhatsApp number to tell", "user", u.Email)
		}
	}
}

// notifyUser sends text to a user directly: on their own number's chat
// with itself, or from their agents' number to the first number they
// message it from. It reports whether there was anywhere to send it.
func (m *Manager) notifyUser(ctx context.Context, userID, text string) bool {
	accounts, err := m.store.Accounts(ctx, userID)
	if err != nil {
		return false
	}
	numbers, _ := m.store.OwnerNumbers(ctx, userID)
	for _, a := range accounts {
		if a.Status != store.AccountConnected {
			continue
		}
		var chat string
		switch {
		case a.Personal() && a.Phone != "":
			chat = a.Phone + "@s.whatsapp.net"
		case !a.Personal() && len(numbers) > 0:
			chat = numbers[0] + "@s.whatsapp.net"
		default:
			continue
		}
		chatRef{m: m, msg: Message{AccountID: a.ID, Chat: chat}}.send(text)
		return true
	}
	return false
}

// everySecret redacts every user's stored secrets, for text that may
// mention anyone's, such as the server's log.
func (m *Manager) everySecret(ctx context.Context) redactor {
	users, _ := m.store.Users(ctx)
	var values []string
	for _, u := range users {
		times, _ := m.store.SecretTimes(ctx, u.ID)
		for name := range times {
			v := m.Secret(ctx, u.ID, name)
			if v == "" {
				continue
			}
			values = append(values, v)
			values = append(values, secretStrings(v)...)
		}
	}
	return newRedactor(values)
}

// tokenPattern catches credentials by their shape, for any the store does
// not hold: an agent's own key that ended up in an error message, say.
var tokenPattern = regexp.MustCompile(`\b(sk-ant-[A-Za-z0-9_-]{8,}|sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abprs]-[A-Za-z0-9-]{10,})`)

func redactTokens(s string) string { return tokenPattern.ReplaceAllString(s, "[secret removed]") }
