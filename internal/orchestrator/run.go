package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/media"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/spool"
	"github.com/rahimnathwani/hermitator/internal/store"
	"github.com/rahimnathwani/hermitator/internal/turn"
	"github.com/rahimnathwani/hermitator/internal/wamd"
)

// maxMessage is the longest single WhatsApp message hermitator sends.
// WhatsApp allows far more, but long walls of text are unreadable on a
// phone.
const maxMessage = 3500

const systemPrompt = `You are running inside hermitator: an isolated Linux machine (a gVisor sandbox) that is yours to use. %s

- Your final reply is delivered as a WhatsApp message, so keep it short and readable on a phone. WhatsApp formatting is *bold*, _italic_, ~strikethrough~ and ` + "```code```" + `. Do not use tables or headings.
- Files the user sends you are saved under %s; their paths appear in the message.
- To send the user a file (a screenshot, chart, PDF, archive...), run: whatsapp-send <path> [caption]
- /workspace is your working directory. The whole filesystem persists between conversations, including anything you install with apt, npm, pip or uv. Run as root; there is no sudo prompt.
- When nobody is talking to you the machine is suspended. Background processes you leave running (dev servers, databases) are frozen and continue when the next message arrives.
- Chromium and Playwright are installed for screenshots and browser automation.`

func (m *Manager) agentPrompt(spec *sandbox.Spec, last Message) string {
	who := "The user talks to you over WhatsApp."
	switch {
	case last.AccountID == "":
		who = "The user talks to you through hermitator's web console; their messages normally arrive over WhatsApp."
	case last.IsGroup && last.ChatName != "":
		who = fmt.Sprintf("You are in the WhatsApp group %q; each message is prefixed with its sender's name.", last.ChatName)
	case last.SenderName != "" && !last.FromMe:
		who = fmt.Sprintf("You are talking to %s over WhatsApp.", last.SenderName)
	}
	return fmt.Sprintf(systemPrompt, who, path.Join(m.rt.GuestShared(spec), turn.InboxDir))
}

// runTurn runs one turn for a batch of messages and delivers the reply.
// It reports whether the sandbox was left running.
func (m *Manager) runTurn(a *actor, t *activeTurn, batch []Message) bool {
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.MaxTurn)
	defer cancel()
	last := batch[len(batch)-1]
	chat := chatRef{m: m, msg: last}

	agent, err := m.store.Agent(ctx, a.id)
	if err != nil {
		m.log.Error("turn for unknown agent", "agent", a.id, "err", err)
		return false
	}
	log := m.log.With("agent", agent.ID, "turn", t.id)

	chat.react("⏳")
	stopTyping := chat.typing(ctx)
	defer stopTyping()

	fail := func(text string) bool {
		chat.send(text)
		chat.react("❌")
		m.hub.publish(a.id, UIEvent{Kind: "turn_end", TurnID: t.id, Status: store.TurnError, Output: text})
		return false
	}

	creds, problem := m.credentials(ctx, agent)
	if problem != "" {
		return fail(problem)
	}

	spec, err := m.Spec(agent)
	if err != nil {
		log.Error("spec", "err", err)
		return fail("⚠️ This agent's sandbox is misconfigured: " + err.Error())
	}
	woke := time.Now()
	wake, err := m.rt.Wake(ctx, spec)
	if err != nil {
		log.Error("wake", "err", err)
		return fail("⚠️ Could not start the sandbox: " + err.Error())
	}
	wakeMS := int(time.Since(woke).Milliseconds())
	a.do(func(a *actor) { a.setState(sandbox.Running) })

	prompt, images := m.buildPrompt(ctx, spec, batch)
	if agent.SessionID == "" {
		agent.SessionID = ids.UUID()
		m.store.SetAgentSession(ctx, agent.ID, agent.SessionID)
	}

	rec := &store.Turn{
		ID: t.id, AgentID: agent.ID, AccountID: last.AccountID, ChatJID: last.Chat,
		Harness: agent.Harness, Input: clip(prompt, 4000), Wake: wake, WakeMS: wakeMS,
	}
	if err := m.store.CreateTurn(ctx, rec); err != nil {
		log.Error("record turn", "err", err)
	}
	m.hub.publish(agent.ID, UIEvent{Kind: "turn_start", TurnID: t.id, Input: rec.Input})
	log.Info("turn started", "harness", agent.Harness, "wake", wake, "wake_ms", wakeMS, "messages", len(batch))

	req := turn.Request{
		Harness:      agent.Harness,
		SessionID:    agent.SessionID,
		Prompt:       prompt,
		Images:       images,
		Model:        agent.Model,
		Provider:     agent.PiProvider,
		SystemPrompt: m.agentPrompt(spec, last),
		Env:          creds.env,
		Files:        creds.files,
	}
	stdin, _ := json.Marshal(req)
	client := spec.Spool()
	if err := client.Submit(spool.Job{ID: t.id, Argv: []string{"hermitator-guest", "turn"}, Stdin: stdin}); err != nil {
		return m.finish(ctx, chat, rec, nil, "⚠️ Could not hand the turn to the sandbox: "+err.Error(), false)
	}
	t.submitted(&client)
	defer client.Remove(t.id)

	// Deliver files as soon as the agent queues them.
	outboxDone := make(chan struct{})
	outboxStopped := make(chan struct{})
	go func() {
		defer close(outboxStopped)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-outboxDone:
				return
			case <-tick.C:
				m.deliverOutbox(ctx, spec, chat)
			}
		}
	}()

	// If the sandbox dies mid-turn the job never writes an exit record, so
	// watch the sandbox too.
	followCtx, stopFollow := context.WithCancel(ctx)
	var died atomic.Bool
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-followCtx.Done():
				return
			case <-tick.C:
				if st, err := m.rt.State(followCtx, spec); err == nil && st != sandbox.Running {
					died.Store(true)
					stopFollow()
					return
				}
			}
		}
	}()

	var result *turn.Event
	exit, err := client.Follow(followCtx, t.id, func(line []byte) {
		var ev turn.Event
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		switch ev.Type {
		case turn.EventTool:
			rec.Tools++
		case turn.EventFile:
			m.syncFile(ctx, agent.UserID, ev)
			return // never publish credentials
		case turn.EventResult, turn.EventError:
			e := ev
			result = &e
		}
		m.hub.publish(agent.ID, UIEvent{Kind: "event", TurnID: t.id, Event: &ev})
	})
	stopFollow()
	close(outboxDone)
	<-outboxStopped
	m.deliverOutbox(ctx, spec, chat)

	switch {
	case died.Load():
		return m.finish(ctx, chat, rec, nil, "⚠️ The sandbox stopped in the middle of the turn (out of memory?).", false)
	case errors.Is(err, context.DeadlineExceeded):
		client.Kill(t.id)
		return m.finish(ctx, chat, rec, nil, fmt.Sprintf("⚠️ The turn ran for over %s and was stopped.", m.cfg.MaxTurn), true)
	case err != nil:
		return m.finish(ctx, chat, rec, nil, "⚠️ Lost track of the turn: "+err.Error(), true)
	case t.wasAborted():
		rec.Status = store.TurnAborted
		return m.finish(ctx, chat, rec, result, "", true)
	case result == nil:
		detail := strings.TrimSpace(client.Stderr(t.id, 600))
		if detail == "" {
			detail = exit.String()
		}
		return m.finish(ctx, chat, rec, nil, "⚠️ The agent exited without replying: "+detail, true)
	}
	if result.SessionID != "" && result.SessionID != agent.SessionID {
		m.store.SetAgentSession(ctx, agent.ID, result.SessionID)
	}
	return m.finish(ctx, chat, rec, result, "", true)
}

// finish records the outcome of a turn, then delivers it.
func (m *Manager) finish(ctx context.Context, chat chatRef, rec *store.Turn, result *turn.Event, failure string, running bool) bool {
	var deliver func()
	switch {
	case rec.Status == store.TurnAborted:
		if result != nil {
			rec.Output = result.Text
		}
		deliver = func() { chat.react("⏹") }
	case failure != "":
		rec.Status, rec.Output = store.TurnError, failure
		deliver = func() { chat.send(failure); chat.react("❌") }
	case result.Type == turn.EventError || result.IsError:
		rec.Status, rec.Output = store.TurnError, result.Text
		deliver = func() { chat.sendAgent("⚠️ " + result.Text); chat.react("❌") }
	default:
		rec.Status, rec.Output, rec.CostUSD = store.TurnOK, result.Text, result.CostUSD
		deliver = func() {
			if strings.TrimSpace(result.Text) == "" {
				chat.send("_(the agent finished without a reply)_")
			} else {
				chat.sendAgent(result.Text)
			}
			chat.react("✅")
		}
	}
	if !rec.StartedAt.IsZero() {
		m.store.FinishTurn(ctx, rec)
		m.store.TouchAgent(ctx, rec.AgentID)
		m.hub.publish(rec.AgentID, UIEvent{Kind: "turn_end", TurnID: rec.ID, Status: rec.Status, Output: clip(rec.Output, 4000)})
		m.log.Info("turn finished", "agent", rec.AgentID, "turn", rec.ID, "status", rec.Status, "tools", rec.Tools,
			"duration", rec.Duration().Round(time.Millisecond), "cost_usd", rec.CostUSD)
	}
	deliver()
	return running
}

// buildPrompt turns a batch of chat messages into one prompt, saving any
// attachments into the sandbox's inbox.
func (m *Manager) buildPrompt(ctx context.Context, spec *sandbox.Spec, batch []Message) (string, []turn.Image) {
	senders := map[string]bool{}
	for _, msg := range batch {
		senders[msg.Sender] = true
	}
	labelled := len(senders) > 1 || batch[len(batch)-1].IsGroup
	var parts []string
	var images []turn.Image
	for i, msg := range batch {
		var lines []string
		if msg.Media != nil {
			note, img := m.saveMedia(ctx, spec, msg, i)
			lines = append(lines, note)
			if img != nil {
				images = append(images, *img)
			}
		}
		if t := strings.TrimSpace(msg.Text); t != "" {
			lines = append(lines, t)
		}
		if len(lines) == 0 {
			continue
		}
		body := strings.Join(lines, "\n")
		if labelled {
			name := msg.SenderName
			if name == "" {
				name = "someone"
			}
			body = "[" + name + "] " + body
		}
		parts = append(parts, body)
	}
	return strings.Join(parts, "\n\n"), images
}

func (m *Manager) saveMedia(ctx context.Context, spec *sandbox.Spec, msg Message, seq int) (string, *turn.Image) {
	md := msg.Media
	data, err := md.Fetch(ctx)
	if err != nil {
		return fmt.Sprintf("[The user sent a %s that could not be downloaded: %v]", md.Kind, err), nil
	}
	ts := msg.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	name := media.FileName(md.FileName, md.MIME, ts, seq)
	host := filepath.Join(spec.InboxDir(), name)
	if err := os.WriteFile(host, data, 0o644); err != nil {
		return fmt.Sprintf("[The user sent a %s that could not be saved: %v]", md.Kind, err), nil
	}
	guest := path.Join(m.rt.GuestShared(spec), turn.InboxDir, name)
	size := media.HumanSize(int64(len(data)))

	switch {
	case md.Kind == "image" && media.InlineImage(md.MIME):
		return "[Image attached, also saved at " + guest + "]", &turn.Image{Path: path.Join(turn.InboxDir, name), MIME: md.MIME}
	case md.Kind == "audio" && m.stt != nil && m.stt.Available():
		text, err := m.stt.Transcribe(ctx, host)
		if err != nil {
			m.log.Warn("transcription failed", "err", err)
			return fmt.Sprintf("[Voice note saved at %s (%s); transcription failed]", guest, size), nil
		}
		return fmt.Sprintf("[Voice note, transcribed: %q. Audio saved at %s]", text, guest), nil
	case md.Kind == "audio":
		return fmt.Sprintf("[Audio saved at %s (%s, %s)]", guest, md.MIME, size), nil
	default:
		return fmt.Sprintf("[File saved at %s (%s, %s)]", guest, md.MIME, size), nil
	}
}

// outboxMeta mirrors what whatsapp-send writes.
type outboxMeta struct {
	File    string `json:"file"`
	Caption string `json:"caption"`
}

// deliverOutbox sends every file the agent has queued.
func (m *Manager) deliverOutbox(ctx context.Context, spec *sandbox.Spec, chat chatRef) {
	dir := spec.OutboxDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	sent := filepath.Join(dir, "sent")
	os.MkdirAll(sent, 0o755)
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		metaPath := filepath.Join(dir, name)
		raw, err := os.ReadFile(metaPath)
		var meta outboxMeta
		if err == nil {
			err = json.Unmarshal(raw, &meta)
		}
		file := filepath.Join(dir, filepath.Base(meta.File))
		if err != nil || meta.File == "" {
			os.Rename(metaPath, filepath.Join(sent, name))
			continue
		}
		if err := chat.sendFile(file, meta.Caption); err != nil {
			m.log.Warn("send outbox file", "agent", spec.ID, "file", meta.File, "err", err)
			chat.send(fmt.Sprintf("⚠️ Could not send %s: %v", displayName(meta.File), err))
		}
		os.Rename(file, filepath.Join(sent, filepath.Base(meta.File)))
		os.Rename(metaPath, filepath.Join(sent, name))
		m.hub.publish(spec.ID, UIEvent{Kind: "event", Event: &turn.Event{Type: "sent", Text: meta.File}})
	}
}

// chatRef is where a turn's output goes. Turns started from the web UI
// have no chat; their output is only shown there.
type chatRef struct {
	m   *Manager
	msg Message
}

func (c chatRef) ok() bool { return c.msg.AccountID != "" && c.msg.Chat != "" }

// send delivers a message hermitator wrote, already in WhatsApp format.
func (c chatRef) send(text string) {
	if !c.ok() || strings.TrimSpace(text) == "" {
		return
	}
	for _, part := range wamd.Split(text, maxMessage) {
		if err := c.m.msgr.SendText(c.m.ctx, c.msg.AccountID, c.msg.Chat, part); err != nil {
			c.m.log.Warn("send reply", "chat", c.msg.Chat, "err", err)
			return
		}
	}
}

// sendAgent delivers the agent's Markdown, converted for WhatsApp.
func (c chatRef) sendAgent(text string) { c.send(wamd.Format(text)) }

func (c chatRef) sendFile(path, caption string) error {
	if !c.ok() {
		return nil
	}
	return c.m.msgr.SendFile(c.m.ctx, c.msg.AccountID, c.msg.Chat, path, caption)
}

func (c chatRef) react(emoji string) {
	if !c.ok() || c.msg.ID == "" {
		return
	}
	if err := c.m.msgr.React(c.m.ctx, c.msg.AccountID, c.msg.Chat, c.msg.Sender, c.msg.ID, emoji); err != nil {
		c.m.log.Debug("react", "err", err)
	}
}

// typing shows the typing indicator until the returned func is called.
func (c chatRef) typing(ctx context.Context) func() {
	if !c.ok() {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(8 * time.Second)
		defer tick.Stop()
		for {
			c.m.msgr.Typing(ctx, c.msg.AccountID, c.msg.Chat, true)
			select {
			case <-done:
				c.m.msgr.Typing(c.m.ctx, c.msg.AccountID, c.msg.Chat, false)
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	var once bool
	return func() {
		if !once {
			once = true
			close(done)
		}
	}
}

// displayName strips the timestamp whatsapp-send prefixes to file names.
func displayName(file string) string {
	if _, rest, ok := strings.Cut(file, "-"); ok && rest != "" {
		return rest
	}
	return file
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
