package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
- When nobody is talking to you the machine is suspended. Background processes you leave running (dev servers, databases) are frozen and continue when the next message arrives. Start them with their output redirected to a file, for example: nohup npm run dev > /tmp/dev.log 2>&1 &. The shell you start them from goes away at the end of your turn.
- For screenshots and browser automation there is Playwright for Python (from playwright.sync_api import sync_playwright) and for Node (require("playwright")), plus a chromium command for one-off captures. After taking a screenshot, look at it before sending it, so you know the page really loaded.
- Claude Code and pi come with this machine and are updated for you. Do not update or reinstall them yourself (npm install -g, claude update, pi update self): a copy you install hides every later update. Plugins, extensions, skills and MCP servers are yours to install.
- To do something later, run: hermitator-guest schedule --at 9am|--in 2h|--cron "0 9 * * 1-5" --name "short name" PROMPT. At that time PROMPT reaches you here as a message, in this conversation, and your reply goes to this chat. Times are the owner's (%s; TZ is set to it, so date shows their time). hermitator-guest tasks lists tasks, hermitator-guest unschedule ID cancels one. Schedule only what the user asked for or agreed to; hermitator tells them about every task you set.
- To report on background work after your turn has ended, have the job finish with whatsapp-send --text "..." (or send a file). A background job freezes when the machine sleeps; to keep it awake until the job ends, start it as: nohup hermitator-guest hold -- sh -c 'CMD > /tmp/job.log 2>&1; whatsapp-send --text "done: ..."' >/dev/null 2>&1 &
- When you need the user to choose between a few options, ask with a poll they can tap: hermitator-guest ask "Deploy to production?" "Yes" "No" "Staging first". Their choice arrives as their next message. Use it for real decisions, not for every question.
- A table, chart or diagram wider than a phone screen is unreadable in WhatsApp. Render it to an image instead (HTML or Mermaid through Playwright), look at it, and send it with whatsapp-send.
- Each message starts you afresh in the same conversation, so anything you install or configure is loaded from the next message, not this one; say so when you install something. Add MCP servers for the whole machine (claude mcp add --scope user ...), not in a project .mcp.json, which needs approval you cannot give here. A server that signs in through a browser cannot sign in from here: tell the user to open your Terminal tab on the hermitator page, run claude, and sign in with /mcp.`

func (m *Manager) agentPrompt(spec *sandbox.Spec, loc *time.Location, last Message) string {
	who := "The user talks to you over WhatsApp."
	switch {
	case last.AccountID == "":
		who = "The user talks to you through hermitator's web console; their messages normally arrive over WhatsApp."
	case last.IsGroup && last.ChatName != "":
		who = fmt.Sprintf("You are in the WhatsApp group %q; each message is prefixed with its sender's name.", last.ChatName)
	case last.SenderName != "" && !last.FromMe:
		who = fmt.Sprintf("You are talking to %s over WhatsApp.", last.SenderName)
	}
	prompt := fmt.Sprintf(systemPrompt, who, path.Join(m.rt.GuestShared(spec), turn.InboxDir), loc)
	if m.Previews() {
		prompt += "\n- To let the user open a web server you run (a dev server, a demo), have it listen on 0.0.0.0 (vite --host, next dev -H 0.0.0.0) and run: hermitator-guest preview PORT. It prints a link to send them; it works from their phone, and opening it wakes this machine. Anyone with the link can see the page, so offer it for things meant to be seen."
	}
	return prompt
}

// runTurn runs one turn for a batch of messages and delivers the reply.
// It reports whether the sandbox was left running.
func (m *Manager) runTurn(a *actor, t *activeTurn, batch []Message) bool {
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.MaxTurn)
	defer cancel()
	last := batch[len(batch)-1]
	chat := chatRef{m: m, msg: last}
	if last.TaskName != "" {
		chat.prefix = "⏰ *" + last.TaskName + "*\n\n"
	}

	agent, err := m.store.Agent(ctx, a.id)
	if err != nil {
		m.log.Error("turn for unknown agent", "agent", a.id, "err", err)
		return false
	}
	log := m.log.With("agent", agent.ID, "turn", t.id)

	for _, msg := range batch {
		chatRef{m: m, msg: msg}.markRead()
	}
	chat.react("⏳")
	stopTyping := chat.typing(ctx)
	defer stopTyping()
	m.hub.publish(a.id, UIEvent{Kind: "turn_start", TurnID: t.id, Input: summarizeBatch(batch)})

	fail := func(text string) bool {
		chat.send(text)
		chat.react("❌")
		m.hub.publish(a.id, UIEvent{Kind: "turn_end", TurnID: t.id, Status: store.TurnError, Output: text})
		if !chat.ok() {
			m.recordFailed(ctx, t.id, agent, batch, text)
		}
		return false
	}

	if over := m.overCap(ctx, agent); over != "" {
		return fail(over)
	}
	creds, problem := m.credentials(ctx, agent)
	if problem != "" {
		return fail(problem)
	}
	chat.redact = newRedactor(creds.redact)

	spec, err := m.Spec(agent)
	if err != nil {
		log.Error("spec", "err", err)
		return fail("⚠️ This agent's sandbox is misconfigured: " + err.Error())
	}
	m.usePlaceholders(ctx, agent, spec, &creds)
	woke := time.Now()
	wake, err := m.rt.Wake(ctx, spec)
	if err != nil {
		log.Error("wake", "err", err)
		return fail("⚠️ Could not start the sandbox: " + err.Error())
	}
	wakeMS := int(time.Since(woke).Milliseconds())
	a.do(func(a *actor) { a.setState(sandbox.Running) })

	prompt, images := m.buildPrompt(ctx, spec, batch)
	var held []*store.HeldMessage
	if last.AccountID != "" {
		held, _ = m.store.Held(ctx, last.AccountID, last.Chat)
	}
	if context := m.heldContext(ctx, spec, held); context != "" {
		prompt = context + "\n\n" + prompt
	}
	if agent.SessionID == "" {
		agent.SessionID = ids.UUID()
		m.store.SetAgentSession(ctx, agent.ID, agent.SessionID)
	}
	var fork bool
	a.call(func(a *actor) error { fork, a.forkNext = a.forkNext, false; return nil })

	rec := &store.Turn{
		ID: t.id, AgentID: agent.ID, AccountID: last.AccountID, ChatJID: last.Chat,
		Harness: agent.Harness, SessionID: agent.SessionID, Input: clip(prompt, 4000), Prompt: prompt, Wake: wake, WakeMS: wakeMS,
	}
	if err := m.store.CreateTurn(ctx, rec); err != nil {
		log.Error("record turn", "err", err)
	}
	log.Info("turn started", "harness", agent.Harness, "wake", wake, "wake_ms", wakeMS, "messages", len(batch))

	// The agent's clock shows the owner's time, which schedules use too.
	loc := m.location(ctx, agent)
	env := map[string]string{"TZ": loc.String()}
	for k, v := range creds.env {
		env[k] = v
	}
	// /once overrides the agent's model and effort for this turn.
	model, effort := agent.Model, agent.Effort
	for _, msg := range batch {
		if msg.Model != "" {
			model = msg.Model
		}
		if msg.Effort != "" {
			effort = msg.Effort
		}
	}
	req := turn.Request{
		Harness:      agent.Harness,
		SessionID:    agent.SessionID,
		Prompt:       prompt,
		Images:       images,
		Model:        model,
		Effort:       effort,
		Fork:         fork,
		Provider:     agent.PiProvider,
		SystemPrompt: m.agentPrompt(spec, loc, last) + credentialsNote(creds) + m.historyNote(ctx, spec, agent, last),
		SteerID:      t.id,
		Env:          env,
		Files:        creds.files,
	}
	m.writeTasks(ctx, spec, agent)
	stdin, _ := json.Marshal(req)
	client := spec.Spool()
	if err := client.Submit(spool.Job{ID: t.id, Argv: []string{"hermitator-guest", "turn"}, Stdin: stdin}); err != nil {
		return m.finish(ctx, chat, rec, nil, "⚠️ Could not hand the turn to the sandbox: "+err.Error(), false)
	}
	t.submitted(&client)
	if agent.Harness == store.HarnessClaude {
		a.do(func(a *actor) {
			if a.turn == t {
				t.steerTo = chatKey(last)
			}
		})
	}
	if len(held) > 0 {
		// The agent has them now; they are read, and no longer held.
		ids := make([]string, len(held))
		for i, h := range held {
			ids[i] = h.ID
			chatRef{m: m, msg: Message{AccountID: h.AccountID, Chat: h.ChatJID, Sender: h.Sender, ID: h.MessageID}}.markRead()
		}
		m.store.Release(ctx, ids)
	}

	// Deliver files as soon as the agent queues them.
	var filesSent atomic.Int32
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
				filesSent.Add(int32(m.deliverOutbox(ctx, spec, chat)))
				m.handleRequests(ctx, spec, agent, chat)
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

	// A quiet turn may be a slow build, so this only ever warns, once.
	var progress atomic.Pointer[stallState]
	progress.Store(&stallState{at: time.Now()})
	go func() {
		tick := time.NewTicker(m.cfg.StallAfter / 10)
		defer tick.Stop()
		warned := false
		for {
			select {
			case <-followCtx.Done():
				return
			case <-tick.C:
				p := progress.Load()
				if !warned && time.Since(p.at) >= m.cfg.StallAfter {
					warned = true
					last := ""
					if p.doing != "" {
						last = " (last step: " + chat.redact.apply(clip(p.doing, 120)) + ")"
					}
					chat.send(fmt.Sprintf("⏳ No sign of progress for %s%s. It may just be slow; /stop ends the turn.", humanize(time.Since(p.at)), last))
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
		doing := progress.Load().doing
		if ev.Type == turn.EventTool {
			doing = strings.TrimSpace(ev.Tool + " " + ev.Detail)
		}
		progress.Store(&stallState{at: time.Now(), doing: doing})
		if ev.Model != "" {
			rec.Model = ev.Model
		}
		ev.Text, ev.Detail = chat.redact.apply(ev.Text), chat.redact.apply(ev.Detail)
		switch ev.Type {
		case turn.EventTool:
			rec.Tools++
		case turn.EventFile:
			m.syncFile(ctx, agent.UserID, ev)
			return // never publish credentials
		case turn.EventUsage:
			m.recordUsage(ctx, agent, ev.Windows, chat)
			return
		case turn.EventReply:
			chat.sendAgent(ev.Text)
		case turn.EventTodos:
			todos := ev.Todos
			a.do(func(a *actor) { a.todos, a.todosAt = todos, time.Now() })
		case turn.EventResult, turn.EventError:
			e := ev
			result = &e
		}
		m.hub.publish(agent.ID, UIEvent{Kind: "event", TurnID: t.id, Event: &ev})
	})
	stopFollow()
	// Messages from now on queue for the next turn; ones that reached the
	// spool but not the harness join them.
	a.call(func(a *actor) error { t.steerTo = ""; return nil })
	t.steering.Wait()
	for _, data := range client.Unsteered(t.id) {
		var s turn.Steer
		if json.Unmarshal(data, &s) == nil && s.Text != "" {
			again := Message{AccountID: last.AccountID, Chat: last.Chat, Sender: last.Sender, Owner: true, Time: time.Now(), Prompt: s.Text}
			a.do(func(a *actor) { a.enqueue(again) })
		}
	}
	close(outboxDone)
	<-outboxStopped
	filesSent.Add(int32(m.deliverOutbox(ctx, spec, chat)))
	m.handleRequests(ctx, spec, agent, chat)
	if err != nil && !died.Load() {
		client.Cancel(t.id, 15*time.Second)
	} else {
		client.Remove(t.id)
	}

	if result != nil && result.Type == turn.EventResult {
		m.accountSession(ctx, agent, result, prompt)
	}

	switch {
	case died.Load():
		return m.finish(ctx, chat, rec, nil, "⚠️ The sandbox stopped in the middle of the turn (out of memory?).", false)
	case errors.Is(err, context.DeadlineExceeded):
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
	case result.Type == turn.EventResult && !result.IsError && strings.TrimSpace(result.Text) == "" && filesSent.Load() == 0:
		// Said nothing and sent nothing: from the chat, indistinguishable
		// from a turn that failed, so it counts as one.
		rec.CostUSD = result.CostUSD
		return m.finish(ctx, chat, rec, nil, "⚠️ The agent finished without replying. Send /retry to try again.", true)
	}
	return m.finish(ctx, chat, rec, result, "", true)
}

// accountSession records which session the turn continued and turns the
// harness's cost figure into this turn's own cost.
//
// Claude Code reports a running total for the whole session, restored
// when a session is resumed, so the turn's cost is the increase since the
// last total. A total lower than the last one means Claude started over
// and the figure stands on its own. pi reports per turn already.
func (m *Manager) accountSession(ctx context.Context, ag *store.Agent, result *turn.Event, prompt string) {
	sid := result.SessionID
	if sid == "" {
		sid = ag.SessionID
	}
	m.store.SessionTurn(ctx, ag.ID, sid, ag.Harness, clip(prompt, 200))
	if ag.Harness != store.HarnessClaude {
		if sid != ag.SessionID {
			m.store.SetAgentSession(ctx, ag.ID, sid)
		}
		return
	}
	total := result.CostUSD
	if sid == ag.SessionID && total >= ag.SessionCost {
		result.CostUSD = total - ag.SessionCost
	}
	m.store.SetAgentSessionCost(ctx, ag.ID, sid, total)
	m.store.SetSessionCost(ctx, ag.ID, sid, total)
}

// finish records the outcome of a turn, then delivers it.
func (m *Manager) finish(ctx context.Context, chat chatRef, rec *store.Turn, result *turn.Event, failure string, running bool) bool {
	if result != nil {
		// What is stored and shown is as clean as what is sent.
		r := *result
		r.Text = chat.redact.apply(r.Text)
		result = &r
	}
	var deliver func()
	switch {
	case rec.Status == store.TurnAborted:
		if result != nil {
			rec.Output, rec.CostUSD = result.Text, result.CostUSD
		}
		deliver = func() { chat.react("⏹") }
	case failure != "":
		rec.Status, rec.Output = store.TurnError, failure
		deliver = func() { chat.send(failure); chat.react("❌") }
	case result.Type == turn.EventError || result.IsError:
		rec.Status, rec.Output, rec.CostUSD = store.TurnError, result.Text, result.CostUSD
		deliver = func() { chat.sendAgent("⚠️ " + result.Text); chat.react("❌") }
	default:
		rec.Status, rec.Output, rec.CostUSD = store.TurnOK, result.Text, result.CostUSD
		deliver = func() {
			// No text is fine when the agent's answer was the files it sent.
			if strings.TrimSpace(result.Text) != "" {
				chat.sendAgent(result.Text)
			}
			chat.react("✅")
		}
	}
	if !rec.StartedAt.IsZero() {
		m.store.FinishTurn(ctx, rec)
		defer m.capWarning(ctx, chat, rec)
		m.store.TouchAgent(ctx, rec.AgentID)
		m.hub.publish(rec.AgentID, UIEvent{Kind: "turn_end", TurnID: rec.ID, Status: rec.Status, Output: clip(rec.Output, 4000)})
		m.log.Info("turn finished", "agent", rec.AgentID, "turn", rec.ID, "status", rec.Status, "tools", rec.Tools,
			"duration", rec.Duration().Round(time.Millisecond).String(), "cost_usd", rec.CostUSD)
	}
	deliver()
	return running
}

// summarizeBatch is a short description of what arrived, for the UI.
func summarizeBatch(batch []Message) string {
	var parts []string
	for _, msg := range batch {
		if msg.Media != nil {
			parts = append(parts, "["+msg.Media.Kind+"]")
		}
		if t := strings.TrimSpace(msg.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return clip(strings.Join(parts, "\n"), 4000)
}

// buildPrompt turns a batch of chat messages into one prompt, saving any
// attachments into the sandbox's inbox.
func (m *Manager) buildPrompt(ctx context.Context, spec *sandbox.Spec, batch []Message) (string, []turn.Image) {
	// Messages are labelled with their sender whenever there is anyone but
	// the owner to tell apart. That also means nobody else's message can
	// start with a slash and be taken as a harness command.
	senders := map[string]bool{}
	labelled := batch[len(batch)-1].IsGroup
	for _, msg := range batch {
		senders[msg.Sender] = true
		labelled = labelled || !msg.Owner
	}
	labelled = labelled || len(senders) > 1
	var parts []string
	var images []turn.Image
	for i, msg := range batch {
		if msg.Prompt != "" {
			parts = append(parts, msg.Prompt)
			continue
		}
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
	if err := writeShared(spec, path.Join(turn.InboxDir, name), data); err != nil {
		return fmt.Sprintf("[The user sent a %s that could not be saved: %v]", md.Kind, err), nil
	}
	guest := path.Join(m.rt.GuestShared(spec), turn.InboxDir, name)
	size := media.HumanSize(int64(len(data)))

	switch {
	case md.Kind == "image" && media.InlineImage(md.MIME):
		return "[Image attached, also saved at " + guest + "]", &turn.Image{Path: path.Join(turn.InboxDir, name), MIME: md.MIME}
	case md.Kind == "audio" && m.stt != nil && m.stt.Available():
		text, err := m.transcribe(ctx, spec.ID, data, name)
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

// transcribe works on a private copy: the copy in the inbox belongs to the
// agent, which could swap it for something else.
func (m *Manager) transcribe(ctx context.Context, agentID string, data []byte, name string) (string, error) {
	f, err := os.CreateTemp("", "hermitator-audio-*"+filepath.Ext(name))
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	vocabulary := ""
	if ag, err := m.store.Agent(ctx, agentID); err == nil {
		vocabulary = ag.Vocabulary
	}
	return m.stt.Transcribe(ctx, f.Name(), vocabulary)
}

// maxOutboxFile is the largest file an agent can send.
const maxOutboxFile = 100 << 20

func writeShared(spec *sandbox.Spec, name string, data []byte) error {
	root, err := spec.Shared()
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	return root.WriteFile(name, data, 0o644)
}

func readShared(spec *sandbox.Spec, name string) ([]byte, error) {
	root, err := spec.Shared()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return readRegular(root, name)
}

// deliverOutbox sends every file the agent has queued.
func (m *Manager) deliverOutbox(ctx context.Context, spec *sandbox.Spec, chat chatRef) (sentFiles int) {
	// A turn and the background tick may both look; only one at a time
	// may, or a file could go out twice.
	a := m.actor(spec.ID)
	a.outbox.Lock()
	defer a.outbox.Unlock()
	root, err := spec.Shared()
	if err != nil {
		return 0
	}
	defer root.Close()
	dir, err := root.Open(turn.OutboxDir)
	if err != nil {
		return 0
	}
	names, _ := dir.Readdirnames(-1)
	dir.Close()
	root.MkdirAll(turn.OutboxDir+"/sent", 0o755)
	in := func(n string) string { return turn.OutboxDir + "/" + n }
	sent := func(n string) string { return turn.OutboxDir + "/sent/" + n }
	for _, name := range names {
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		var meta turn.OutboxMeta
		raw, err := root.ReadFile(in(name))
		if err == nil {
			err = json.Unmarshal(raw, &meta)
		}
		if err == nil && meta.File == "" && strings.TrimSpace(meta.Text) != "" {
			chat.sendAgent(clip(meta.Text, maxMessage))
			sentFiles++
			root.Rename(in(name), sent(name))
			continue
		}
		file := filepath.Base(meta.File)
		if err != nil || meta.File == "" || file == "." || file == "/" {
			root.Rename(in(name), sent(name))
			continue
		}
		data, err := readRegular(root, in(file))
		if err == nil {
			err = chat.sendFile(displayName(file), data, chat.redact.apply(meta.Caption))
		}
		if err != nil {
			m.log.Warn("send outbox file", "agent", spec.ID, "file", file, "err", err)
			chat.send(fmt.Sprintf("⚠️ Could not send %s: %v", displayName(file), err))
		} else {
			sentFiles++
		}
		root.Rename(in(file), sent(file))
		root.Rename(in(name), sent(name))
		m.hub.publish(spec.ID, UIEvent{Kind: "event", Event: &turn.Event{Type: "sent", Text: file}})
	}
	return sentFiles
}

// readRegular reads a regular file of at most maxOutboxFile bytes.
func readRegular(root *os.Root, name string) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if fi.Size() > maxOutboxFile {
		return nil, fmt.Errorf("it is %d MB; the limit is %d MB", fi.Size()>>20, maxOutboxFile>>20)
	}
	return io.ReadAll(io.LimitReader(f, maxOutboxFile))
}

// chatRef is where a turn's output goes. Turns started from the web UI
// have no chat; their output is only shown there.
type chatRef struct {
	m   *Manager
	msg Message
	// prefix starts the agent's messages: a scheduled task's name.
	prefix string
	// redact keeps secret values out of what the agent sends.
	redact redactor
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
func (c chatRef) sendAgent(text string) {
	text = c.redact.apply(text)
	c.send(c.prefix + wamd.Format(text))
	if c.ok() {
		c.m.recordHistory(c.msg.AccountID, c.msg.Chat, "you (the agent)", text, time.Now())
	}
}

func (c chatRef) sendFile(name string, data []byte, caption string) error {
	if !c.ok() {
		return nil
	}
	return c.m.msgr.SendFile(c.m.ctx, c.msg.AccountID, c.msg.Chat, name, data, caption)
}

func (c chatRef) react(emoji string) {
	if !c.ok() || c.msg.ID == "" {
		return
	}
	if err := c.m.msgr.React(c.m.ctx, c.msg.AccountID, c.msg.Chat, c.msg.Sender, c.msg.ID, emoji); err != nil {
		c.m.log.Debug("react", "err", err)
	}
}

func (c chatRef) markRead() {
	if c.ok() && c.msg.ID != "" && !c.msg.FromMe {
		c.m.msgr.MarkRead(c.m.ctx, c.msg.AccountID, c.msg.Chat, c.msg.Sender, c.msg.ID)
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
	if stamp, rest, ok := strings.Cut(file, "-"); ok && rest != "" && strings.Trim(stamp, "0123456789") == "" {
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

// tellInterrupted tells each chat whose turn a restart cut short, once
// WhatsApp is back: until then sends fail, so it tries for a few minutes.
func (m *Manager) tellInterrupted(turns []*store.Turn) {
	const text = "⚠️ hermitator restarted while working on your last message, so it stopped partway. Send /retry to run it again."
	pending := map[[2]string]bool{}
	for _, t := range turns {
		if t.AccountID != "" && t.ChatJID != "" {
			pending[[2]string{t.AccountID, t.ChatJID}] = true
		}
	}
	for try := 0; try < 12 && len(pending) > 0; try++ {
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(m.cfg.RetryNoticeDelay):
		}
		for chat := range pending {
			if err := m.msgr.SendText(m.ctx, chat[0], chat[1], text); err == nil {
				delete(pending, chat)
			}
		}
	}
}

// retry runs the last turn in this chat again, if it did not succeed.
func (m *Manager) retry(ctx context.Context, agentID string, msg Message, reply func(string)) {
	last, err := m.store.LastChatTurn(ctx, agentID, msg.AccountID, msg.Chat)
	switch {
	case errors.Is(err, store.ErrNotFound):
		reply("There is nothing to retry in this chat yet.")
		return
	case err != nil:
		reply("⚠️ Could not look up the last message: " + err.Error())
		return
	case last.Status == store.TurnRunning:
		reply("It is still working on that one.")
		return
	case last.Status == store.TurnOK:
		reply("The last message here went through, so there is nothing to retry.")
		return
	case last.Prompt == "":
		reply("That message is from before hermitator kept enough to retry it. Please send it again.")
		return
	}
	why := "failed"
	switch last.Status {
	case store.TurnInterrupted:
		why = "was cut short by a restart"
	case store.TurnAborted:
		why = "was stopped"
	}
	again := msg
	again.Text, again.Media = "", nil
	again.Prompt = "(The user asked to retry this message; the previous attempt " + why +
		". Check what was already done before repeating any step.)\n\n" + last.Prompt
	m.actor(agentID).do(func(a *actor) { a.enqueue(again) })
}

// steer passes a message to the running turn: the harness reads it from
// the spool and takes it in as it works.
func (m *Manager) steer(agentID string, t *activeTurn, seq int, msg Message) {
	ctx := m.ctx
	ag, err := m.store.Agent(ctx, agentID)
	if err != nil {
		return
	}
	spec, err := m.Spec(ag)
	if err != nil {
		return
	}
	chat := chatRef{m: m, msg: msg}
	chat.markRead()
	text, _ := m.buildPrompt(ctx, spec, []Message{msg})
	if text == "" {
		return
	}
	text = "[Sent while you were working on the above:]\n" + text
	data, _ := json.Marshal(turn.Steer{Text: text})
	if err := spec.Spool().Steer(t.id, seq, data); err != nil {
		m.log.Warn("steer", "agent", agentID, "err", err)
		return
	}
	chat.react("👀")
	m.hub.publish(agentID, UIEvent{Kind: "steer", TurnID: t.id, Input: summarizeBatch([]Message{msg})})
}

// credentialsNote tells the agent about the secrets and accounts it has.
func credentialsNote(c credentials) string {
	var b strings.Builder
	if len(c.names) > 0 {
		fmt.Fprintf(&b, "\n- The owner has given you these secrets as environment variables: %s. Use them by name in commands; never print their values or send them anywhere they are not meant for. hermitator removes them from your replies.", strings.Join(c.names, ", "))
	}
	if c.github != "" {
		fmt.Fprintf(&b, "\n- git and gh can reach GitHub as %s: GH_TOKEN is set, and git over https uses it.", c.github)
	}
	if c.env["GIT_AUTHOR_NAME"] != "" {
		fmt.Fprintf(&b, "\n- Your commits are authored as %s <%s>.", c.env["GIT_AUTHOR_NAME"], c.env["GIT_AUTHOR_EMAIL"])
	}
	return b.String()
}

// stallState is when a turn last showed progress, and what it was doing.
type stallState struct {
	at    time.Time
	doing string
}

// historyNote points the agent at its earlier conversations and, where
// the chat keeps one, at its history file.
func (m *Manager) historyNote(ctx context.Context, spec *sandbox.Spec, ag *store.Agent, last Message) string {
	var b strings.Builder
	switch ag.Harness {
	case store.HarnessClaude:
		b.WriteString("\n- Transcripts of your earlier conversations with this owner, from before a /new, are in ~/.claude/projects/ (one .jsonl per conversation); search them when someone refers to something from before.")
	case store.HarnessPi:
		b.WriteString("\n- Transcripts of your earlier conversations with this owner, from before a /new, are in ~/.pi/agent/sessions/; search them when someone refers to something from before.")
	}
	if last.AccountID != "" {
		if route, err := m.store.Route(ctx, last.AccountID, last.Chat); err == nil && route.History {
			fmt.Fprintf(&b, "\n- Everything said in this chat since its history was turned on, other people's messages included, is in %s; grep it when someone refers to something said before.",
				path.Join(m.rt.GuestShared(spec), historyFile(route)))
		}
	}
	return b.String()
}

// recordFailed records a turn that could not run and had no chat to say
// so in, so one nobody was watching (from the web console, or a new
// agent's setup) still shows on the agent's page why it did not run.
func (m *Manager) recordFailed(ctx context.Context, id string, agent *store.Agent, batch []Message, why string) {
	var input []string
	for _, msg := range batch {
		input = append(input, msg.Text)
	}
	prompt := strings.Join(input, "\n")
	rec := &store.Turn{ID: id, AgentID: agent.ID, Harness: agent.Harness, Input: clip(prompt, 4000), Prompt: prompt}
	if m.store.CreateTurn(ctx, rec) == nil {
		rec.Status, rec.Output = store.TurnError, why
		m.store.FinishTurn(ctx, rec)
	}
}
