package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

// Antigravity's CLI runs as `agy acp`, an Agent Client Protocol server:
// JSON-RPC 2.0 over stdin and stdout, one message per line. What follows
// was worked out against agy 1.5 on a Linux host, and differs from the
// other harnesses in ways the turn has to allow for:
//
//   - session/new picks the session ID; hermitator's own is only a guess
//     until agy has made one. session/load replays the whole conversation
//     as updates before it answers, and "succeeds" for an ID agy has never
//     seen, failing only at the prompt; so a session is loaded only when
//     its transcript is on disk.
//   - A prompt sent while a turn runs joins it: the earlier prompt answers
//     at once with end_turn and the later one carries the turn on.
//   - A command that runs longer than ten seconds is moved to the
//     background, the prompt answers end_turn, and when the command ends
//     agy goes on by itself, streaming more of the turn with no prompt to
//     answer. So the turn ends only once no prompt is waiting, no tool
//     call is still running, and the model has finished a step since the
//     last one did.
//   - The model resets to agy's default on every load, so it is set on
//     every turn, with the thinking level.
//   - Prompt results report tokens but no cost; the cost is estimated from
//     turn.Prices.
const agyBin = "agy"

// agyQuiet is how long agy must have said nothing, once nothing is
// pending, before the turn is taken to be over.
var agyQuiet = 3 * time.Second

// agyFollowUpWait bounds the wait for agy to go on after a background
// command ends: it has always done so within seconds, but a turn must
// not hang if it ever does not.
var agyFollowUpWait = 2 * time.Minute

// agyConversations is where agy keeps a transcript per session.
const agyConversations = "~/.gemini/antigravity-cli/conversations"

var agySessionID = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}$`)

// agyConversationExists reports whether agy has a transcript for id, so
// that loading it resumes a conversation rather than an empty one.
func agyConversationExists(id string) bool {
	if !agySessionID.MatchString(id) {
		return false
	}
	_, err := os.Stat(filepath.Join(expandHome(agyConversations), id+".db"))
	return err == nil
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// acpUpdate is the part of a session/update notification hermitator reads.
type acpUpdate struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		Kind    string `json:"sessionUpdate"`
		Content json.RawMessage
		// Tool calls.
		ToolCallID string         `json:"toolCallId"`
		Name       string         `json:"name"`
		Title      string         `json:"title"`
		Status     string         `json:"status"`
		Args       map[string]any `json:"args"`
		// usage_update: the context window's use.
		Used int `json:"used"`
	} `json:"update"`
}

type acpUsage struct {
	InputTokens       int `json:"inputTokens"`
	OutputTokens      int `json:"outputTokens"`
	CachedReadTokens  int `json:"cachedReadTokens"`
	CachedWriteTokens int `json:"cachedWriteTokens"`
	ThoughtTokens     int `json:"thoughtTokens"`
}

// acpClient is one connection to `agy acp`.
type acpClient struct {
	w      *lockedWriter
	mu     sync.Mutex
	nextID int
	calls  map[int]chan rpcMessage
}

func (c *acpClient) send(m rpcMessage) {
	m.JSONRPC = "2.0"
	b, _ := json.Marshal(m)
	c.w.writeLine(b)
}

// start sends a request and returns where its answer will arrive.
func (c *acpClient) start(method string, params any) chan rpcMessage {
	raw, _ := json.Marshal(params)
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcMessage, 1)
	c.calls[id] = ch
	c.mu.Unlock()
	c.send(rpcMessage{ID: &id, Method: method, Params: raw})
	return ch
}

// call sends a request and waits for its answer.
func (c *acpClient) call(ctx context.Context, done <-chan struct{}, method string, params any, out any) error {
	ch := c.start(method, params)
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out != nil {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-done:
		return errors.New("agy exited")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// answered routes a response to its caller; it reports whether m was one.
func (c *acpClient) answered(m rpcMessage) bool {
	if m.ID == nil || m.Method != "" {
		return false
	}
	c.mu.Lock()
	ch, ok := c.calls[*m.ID]
	delete(c.calls, *m.ID)
	c.mu.Unlock()
	if ok {
		ch <- m
	}
	return true
}

func runAgy(ctx context.Context, req *turn.Request, workdir string, emit Emit) error {
	if req.Fork {
		emit(turn.Event{Type: turn.EventError, Text: "Antigravity cannot branch a conversation"})
		return nil
	}
	env := map[string]string{"AGY_CLI_DISABLE_AUTO_UPDATE": "1"}
	for k, v := range req.Env {
		env[k] = v
	}
	cmd := exec.Command(Getenv("HERMITATOR_AGY_BIN", agyBin), "acp", "--dangerously-skip-permissions", "--disable-telemetry")
	cmd.Dir = workdir
	cmd.Env = environ(env)
	cmd.WaitDelay = 10 * time.Second
	stderr := &tail{n: 4096}
	cmd.Stderr = stderr
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start agy: %w", err)
	}
	t := &agyTurn{
		req: req, emit: emit,
		c:       &acpClient{w: &lockedWriter{w: stdinPipe}, calls: map[int]chan rpcMessage{}},
		updates: make(chan acpUpdate, 256),
		exited:  make(chan struct{}),
		tools:   map[string]bool{},
	}
	go t.read(stdout)
	failure := t.run(ctx, workdir)
	t.c.w.Close()
	stopped := make(chan struct{})
	go func() {
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			cmd.Process.Signal(syscall.SIGTERM)
		}
	}()
	cmd.Wait()
	close(stopped)
	t.finish(ctx, failure, stderr.String())
	return nil
}

// agyTurn is the state of one turn.
type agyTurn struct {
	req     *turn.Request
	emit    Emit
	c       *acpClient
	updates chan acpUpdate
	exited  chan struct{}

	sessionID string
	model     string
	// prompts are the prompts not yet answered.
	prompts int
	// tools are the tool calls still running, by ID.
	tools map[string]bool
	// lastUpdate is when agy last said anything; toolDone is when a tool
	// call last finished, and stepDone when a model step last did (a
	// usage_update closes each).
	lastUpdate, toolDone, stepDone time.Time
	// segment is the text of the current step: since the last tool call,
	// finished command, answered prompt or message sent in. lastText is
	// the last such text that was not empty, which is the reply.
	segment  strings.Builder
	lastText string
	context  int
	cost     float64
	// unpriced is set when a model's tokens could not be priced.
	unpriced   bool
	overBudget bool
	errorText  string
	cancelled  bool
}

// read takes in agy's output until it closes.
func (t *agyTurn) read(r io.Reader) {
	defer close(t.exited)
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var m rpcMessage
			if json.Unmarshal(line, &m) == nil && !t.c.answered(m) {
				t.handle(m)
			}
		}
		if err != nil {
			return
		}
	}
}

func (t *agyTurn) handle(m rpcMessage) {
	switch {
	case m.Method == "session/update":
		var u acpUpdate
		if json.Unmarshal(m.Params, &u) == nil {
			t.updates <- u
		}
	case m.Method == "session/request_permission" && m.ID != nil:
		// --dangerously-skip-permissions should mean none arrive; nobody
		// could answer one, and the sandbox is the boundary.
		var p struct {
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		json.Unmarshal(m.Params, &p)
		choice := ""
		for _, o := range p.Options {
			if strings.HasPrefix(o.Kind, "allow") && (choice == "" || o.Kind == "allow_always") {
				choice = o.OptionID
			}
		}
		result, _ := json.Marshal(map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": choice}})
		if choice == "" {
			result, _ = json.Marshal(map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
		}
		t.c.send(rpcMessage{ID: m.ID, Result: result})
	case m.ID != nil:
		// Nothing else the client offers (files, terminals) is turned on.
		t.c.send(rpcMessage{ID: m.ID, Error: &rpcError{Code: -32601, Message: "not supported by hermitator"}})
	}
}

// run drives the turn; it returns why it could not, or "".
func (t *agyTurn) run(ctx context.Context, workdir string) string {
	var initialized struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	err := t.c.call(ctx, t.exited, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false},
	}, &initialized)
	if err != nil {
		return "agy did not start: " + err.Error()
	}
	session := map[string]any{"cwd": workdir, "mcpServers": []any{}}
	if t.req.SystemPrompt != "" {
		session["_meta"] = map[string]any{"antigravity": map[string]any{"systemPrompt": map[string]any{"append": t.req.SystemPrompt}}}
	}
	var opened struct {
		SessionID string `json:"sessionId"`
		Models    struct {
			Current string `json:"currentModelId"`
		} `json:"models"`
	}
	if t.req.SessionID != "" && agyConversationExists(t.req.SessionID) {
		session["sessionId"] = t.req.SessionID
		// The replay of the conversation arrives before the answer and is
		// dropped with it.
		err = t.c.call(ctx, t.exited, "session/load", session, &opened)
		t.drain()
		opened.SessionID = t.req.SessionID
	} else {
		if t.req.Compact {
			return "no conversation to compact"
		}
		err = t.c.call(ctx, t.exited, "session/new", session, &opened)
	}
	if err != nil {
		return "agy could not open the conversation: " + err.Error()
	}
	t.sessionID, t.model = opened.SessionID, opened.Models.Current
	for _, opt := range []struct{ id, value string }{{"model", t.req.Model}, {"thought_level", t.req.Effort}} {
		if opt.value == "" {
			continue
		}
		err := t.c.call(ctx, t.exited, "session/set_config_option", map[string]any{"sessionId": t.sessionID, "configId": opt.id, "value": opt.value}, nil)
		if err != nil {
			return fmt.Sprintf("agy does not take %s %q: %v", strings.ReplaceAll(opt.id, "_", " "), opt.value, err)
		}
		if opt.id == "model" {
			t.model = opt.value
		}
	}
	t.emit(turn.Event{Type: turn.EventStarted, SessionID: t.sessionID, Model: t.model})

	var content []map[string]any
	for _, img := range t.req.Images {
		if data, err := readImage(img); err == nil {
			content = append(content, map[string]any{"type": "image", "mimeType": img.MIME, "data": data})
		}
	}
	text := t.req.Prompt
	if t.req.Compact {
		text = strings.TrimSpace("/compact " + t.req.Prompt)
	}
	content = append(content, map[string]any{"type": "text", "text": text})
	answers := make(chan rpcMessage, 16)
	t.prompt(content, answers)

	steered := make(chan string, 16)
	stopSteering := func() {}
	if t.req.SteerID != "" && !t.req.Compact {
		stopSteering = steerInto(filepath.Join(Getenv("HERMITATOR_SPOOL", turn.SpoolDir), "steer", t.req.SteerID), steered)
	}
	defer stopSteering()

	t.lastUpdate = time.Now()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.cancel()
			return ""
		case <-t.exited:
			if t.prompts > 0 || len(t.tools) > 0 {
				return "agy exited before the turn finished"
			}
			return ""
		case u := <-t.updates:
			t.update(u)
		case m := <-answers:
			t.answer(m)
		case s := <-steered:
			t.closeSegment()
			t.prompt([]map[string]any{{"type": "text", "text": s}}, answers)
			t.emit(turn.Event{Type: turn.EventSteered, Text: s})
		case <-tick.C:
			if t.settled() {
				return ""
			}
		}
	}
}

// drain drops the updates a load replayed.
func (t *agyTurn) drain() {
	for {
		select {
		case <-t.updates:
		default:
			return
		}
	}
}

func (t *agyTurn) prompt(content []map[string]any, answers chan<- rpcMessage) {
	t.prompts++
	ch := t.c.start("session/prompt", map[string]any{"sessionId": t.sessionID, "prompt": content})
	go func() {
		select {
		case m := <-ch:
			answers <- m
		case <-t.exited:
		}
	}()
}

func (t *agyTurn) cancel() {
	if t.cancelled {
		return
	}
	t.cancelled = true
	raw, _ := json.Marshal(map[string]any{"sessionId": t.sessionID})
	t.c.send(rpcMessage{Method: "session/cancel", Params: raw})
}

func (t *agyTurn) update(u acpUpdate) {
	t.lastUpdate = time.Now()
	switch up := u.Update; up.Kind {
	case "agent_message_chunk":
		var c struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(up.Content, &c) == nil && c.Type == "text" {
			t.segment.WriteString(c.Text)
		}
	case "tool_call":
		t.closeSegment()
		if up.Status != "completed" && up.Status != "failed" {
			t.tools[up.ToolCallID] = true
		}
		detail := up.Title
		if s := summarize(up.Name, lowerKeys(up.Args)); s != "" {
			detail = s
		}
		t.emit(turn.Event{Type: turn.EventTool, Tool: up.Name, Detail: detail})
	case "tool_call_update":
		if up.Status == "completed" || up.Status == "failed" {
			if t.tools[up.ToolCallID] {
				delete(t.tools, up.ToolCallID)
				t.toolDone = time.Now()
				// What agy says next answers the command's outcome; what
				// it said while waiting ("waiting for it to finish") is
				// not the reply.
				t.closeSegment()
			}
		}
	case "usage_update":
		t.stepDone = time.Now()
		if up.Used > 0 {
			t.context = up.Used
		}
	}
}

// lowerKeys lets summarize find agy's CommandLine and similar arguments.
func lowerKeys(args map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		switch strings.ToLower(k) {
		case "commandline":
			out["command"] = v
		case "absolutepath", "targetfile":
			out["path"] = v
		default:
			out[strings.ToLower(k)] = v
		}
	}
	return out
}

func (t *agyTurn) closeSegment() {
	if text := strings.TrimSpace(t.segment.String()); text != "" {
		t.lastText = text
		t.emit(turn.Event{Type: turn.EventText, Text: text})
	}
	t.segment.Reset()
}

func (t *agyTurn) answer(m rpcMessage) {
	t.prompts--
	t.lastUpdate = time.Now()
	t.closeSegment()
	if m.Error != nil {
		t.errorText = m.Error.Message
		return
	}
	var r struct {
		StopReason string    `json:"stopReason"`
		Usage      *acpUsage `json:"usage"`
	}
	json.Unmarshal(m.Result, &r)
	if r.Usage != nil {
		// agy's inputTokens leave out the cached part, and its outputTokens
		// take in the thinking: totalTokens is input + output + cache read.
		cost, ok := turn.Price(t.model, turn.Tokens{
			Input: r.Usage.InputTokens, CacheRead: r.Usage.CachedReadTokens,
			CacheWrite: r.Usage.CachedWriteTokens, Output: r.Usage.OutputTokens,
		})
		t.cost += cost
		t.unpriced = t.unpriced || !ok
	}
	if t.req.MaxCostUSD > 0 && t.cost >= t.req.MaxCostUSD && !t.overBudget {
		t.overBudget = true
		t.cancel()
	}
}

// settled reports whether the turn is over: nothing pending, and quiet.
func (t *agyTurn) settled() bool {
	if t.prompts > 0 || len(t.tools) > 0 || time.Since(t.lastUpdate) < agyQuiet {
		return false
	}
	// After a background command ends, agy takes a model step of its own.
	if t.toolDone.After(t.stepDone) && time.Since(t.toolDone) < agyFollowUpWait {
		return false
	}
	return true
}

func (t *agyTurn) finish(ctx context.Context, failure, stderr string) {
	t.drainInto()
	t.closeSegment()
	ev := turn.Event{Type: turn.EventResult, Text: t.lastText, SessionID: t.sessionID, CostUSD: t.cost, Model: t.model, ContextTokens: t.context}
	switch {
	case failure != "":
		if detail := strings.TrimSpace(stderr); detail != "" && !strings.Contains(failure, "conversation") {
			failure += ": " + detail
		}
		t.emit(turn.Event{Type: turn.EventError, Text: failure, SessionID: t.sessionID})
		return
	case t.overBudget:
		ev.IsError, ev.StopReason = true, turn.StopBudget
	case ctx.Err() != nil:
		t.emit(turn.Event{Type: turn.EventError, Text: "turn aborted", SessionID: t.sessionID, CostUSD: t.cost})
		return
	case t.req.Compact:
		ev.Text = "compacted the conversation"
	case t.lastText == "" && t.errorText != "":
		ev.Text, ev.IsError = t.errorText, true
	}
	t.emit(ev)
}

// drainInto takes in updates that arrived as the turn ended.
func (t *agyTurn) drainInto() {
	for {
		select {
		case u := <-t.updates:
			t.update(u)
		default:
			return
		}
	}
}

// steerInto passes messages from the spool's steer directory to ch, in
// order, until the returned function stops it.
func steerInto(dir string, ch chan<- string) (stop func()) {
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
			sort.Strings(names)
			for _, name := range names {
				data, err := os.ReadFile(name)
				os.Remove(name)
				var s turn.Steer
				if err != nil || json.Unmarshal(data, &s) != nil || strings.TrimSpace(s.Text) == "" {
					continue
				}
				select {
				case ch <- s.Text:
				case <-done:
					return
				}
			}
		}
	}()
	return func() { close(done); <-stopped }
}
