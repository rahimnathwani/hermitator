package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// Chat history: for chats where the owner turns it on, every message,
// the agent's replies included, is appended to a file in the agent's
// /shared/chats, so "as Sam said last week" can be looked up. It also
// keeps what the 200-message cap on held messages would drop, and what
// was said before a /new.

const (
	historyDir = "chats"
	// maxHistory is where a history file is rotated: the older half goes
	// to NAME.1.md, replacing what was there.
	maxHistory = 8 << 20
)

var unsafeName = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)

// historyFile names a chat's history file: its name, made safe, and a
// short hash of its JID so two chats with one name do not share a file.
func historyFile(r *store.Route) string {
	name := r.ChatName
	if name == "" {
		name, _, _ = strings.Cut(r.ChatJID, "@")
	}
	name = strings.Trim(unsafeName.ReplaceAllString(name, "-"), "-.")
	if len(name) > 40 {
		name = name[:40]
	}
	sum := sha256.Sum256([]byte(r.AccountID + "/" + r.ChatJID))
	return path.Join(historyDir, name+"-"+hex.EncodeToString(sum[:3])+".md")
}

// recordHistory appends a message to the chat's history file, if the
// chat keeps one. who is the sender as the agent should see them.
func (m *Manager) recordHistory(accountID, chat, who, text string, at time.Time) {
	route, err := m.store.Route(m.ctx, accountID, chat)
	if err != nil || !route.History {
		return
	}
	ag, err := m.store.Agent(m.ctx, route.AgentID)
	if err != nil {
		return
	}
	spec, err := m.Spec(ag)
	if err != nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	line := fmt.Sprintf("%s %s: %s\n", at.In(m.location(m.ctx, ag)).Format("2006-01-02 15:04"), who,
		strings.ReplaceAll(text, "\n", "\n    "))
	if err := appendShared(spec, historyFile(route), line); err != nil {
		m.log.Warn("chat history", "agent", ag.ID, "err", err)
	}
}

// appendShared appends to a file in the shared directory, through an
// os.Root like everything the host does there.
func appendShared(spec *sandbox.Spec, name, text string) error {
	root, err := spec.Shared()
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	if fi, err := root.Lstat(name); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", name)
		}
		if fi.Size() > maxHistory {
			root.Rename(name, strings.TrimSuffix(name, ".md")+".1.md")
		}
	}
	f, err := root.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// historyText is what the history file records for a message.
func historyText(msg Message) string {
	text := strings.TrimSpace(msg.Text)
	if msg.Media != nil {
		note := "[" + msg.Media.Kind + "]"
		if msg.Media.FileName != "" {
			note = "[" + msg.Media.Kind + ": " + msg.Media.FileName + "]"
		}
		text = strings.TrimSpace(note + " " + text)
	}
	return text
}

func senderLabel(msg Message) string {
	switch {
	case msg.Owner && msg.SenderName != "":
		return msg.SenderName + " (the owner)"
	case msg.Owner:
		return "the owner"
	case msg.SenderName != "":
		return msg.SenderName
	}
	return "someone"
}

// historyCommand handles /history on|off.
func (m *Manager) historyCommand(route *store.Route, arg string, reply func(string)) {
	if route == nil {
		reply("Only linked chats keep a history; this one reaches the agent as the account's fallback.")
		return
	}
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "":
		state := "off"
		if route.History {
			state = "on, in /shared/" + historyFile(route)
		}
		reply("Chat history is " + state + ". /history on keeps every message here in a file the agent can search; /history off stops.")
	case "on":
		m.store.SetRouteHistory(m.ctx, route.AccountID, route.ChatJID, true)
		reply("📒 From now on every message here, other people's included, is kept in the agent's /shared/" + historyFile(route) + ", so it can look things up.")
	case "off":
		m.store.SetRouteHistory(m.ctx, route.AccountID, route.ChatJID, false)
		reply("📒 History stopped. What was kept stays in the agent's files; ask it to delete it if you want it gone.")
	default:
		reply("Use /history on or /history off.")
	}
}
