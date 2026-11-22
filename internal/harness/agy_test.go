package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

// The test binary doubles as a fake `agy acp`: with FAKE_AGY set to a
// scenario, it speaks ACP on stdin and stdout the way agy 1.5 was seen to,
// and logs each request it gets to FAKE_AGY_LOG.
func TestMain(m *testing.M) {
	if scenario := os.Getenv("FAKE_AGY"); scenario != "" {
		fakeAgy(scenario)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeAgy(scenario string) {
	logf, _ := os.OpenFile(os.Getenv("FAKE_AGY_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	reply := func(id int, result any) { send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
	update := func(u map[string]any) {
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "agy-1", "update": u}})
	}
	text := func(s string) {
		update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": s}})
	}
	usage := func(n int) { update(map[string]any{"sessionUpdate": "usage_update", "used": n, "size": 1048576}) }
	tool := func(kind, id, status string) {
		update(map[string]any{"sessionUpdate": kind, "toolCallId": id, "name": "run_command", "title": "make test",
			"status": status, "args": map[string]any{"CommandLine": "make test"}})
	}
	tokens := map[string]any{"inputTokens": 1000, "outputTokens": 100, "cachedReadTokens": 500, "cachedWriteTokens": 0, "thoughtTokens": 40}
	prompts := 0
	var firstPrompt int
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<24)
	for in.Scan() {
		logf.Write(append(append([]byte{}, in.Bytes()...), '\n'))
		var m struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.Unmarshal(in.Bytes(), &m)
		switch m.Method {
		case "initialize":
			reply(m.ID, map[string]any{"protocolVersion": 1})
		case "session/new":
			reply(m.ID, map[string]any{"sessionId": "agy-1", "models": map[string]any{"currentModelId": "gemini-4-neon-high"}})
		case "session/load":
			// agy replays the conversation before it answers.
			text("an old answer")
			usage(50)
			reply(m.ID, map[string]any{"models": map[string]any{"currentModelId": "gemini-4-neon-high"}})
		case "session/set_config_option":
			reply(m.ID, map[string]any{})
		case "session/cancel":
		case "session/prompt":
			prompts++
			switch scenario {
			case "turn":
				usage(100)
				text("Running the tests.")
				tool("tool_call", "t1", "in_progress")
				tool("tool_call_update", "t1", "completed")
				usage(500)
				text("All ")
				text("done.")
				usage(600)
				reply(m.ID, map[string]any{"stopReason": "end_turn", "usage": tokens})
			case "background":
				// The command outlives the prompt; agy answers it, and goes
				// on by itself once the command ends.
				tool("tool_call", "t1", "in_progress")
				text("Waiting for it to finish.")
				usage(200)
				reply(m.ID, map[string]any{"stopReason": "end_turn", "usage": tokens})
				time.Sleep(time.Second)
				tool("tool_call_update", "t1", "completed")
				time.Sleep(500 * time.Millisecond)
				text("It passed.")
				usage(300)
			case "steer":
				if prompts == 1 {
					firstPrompt = m.ID
					text("Working on it.")
					continue
				}
				// A prompt mid-turn joins it; the first answers at once.
				reply(firstPrompt, map[string]any{"stopReason": "end_turn"})
				text("Blue it is.")
				usage(400)
				reply(m.ID, map[string]any{"stopReason": "end_turn", "usage": tokens})
			}
		}
	}
}

// agyRun runs a turn against the fake and returns its events and the
// requests the fake got.
func agyRun(t *testing.T, scenario string, req *turn.Request) ([]turn.Event, []map[string]any) {
	t.Helper()
	return agyRunIn(t, t.TempDir(), scenario, req)
}

// agyRunIn is agyRun with a home directory already set up.
func agyRunIn(t *testing.T, home, scenario string, req *turn.Request) ([]turn.Event, []map[string]any) {
	t.Helper()
	t.Setenv("HOME", home)
	log := filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv("HERMITATOR_AGY_BIN", os.Args[0])
	t.Setenv("FAKE_AGY", scenario)
	t.Setenv("FAKE_AGY_LOG", log)
	quiet := agyQuiet
	agyQuiet = 300 * time.Millisecond
	t.Cleanup(func() { agyQuiet = quiet })
	if req.Workdir == "" {
		req.Workdir = t.TempDir()
	}
	req.Harness = turn.Agy
	var events []turn.Event
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Run(ctx, req, func(e turn.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	data, _ := os.ReadFile(log)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			got = append(got, m)
		}
	}
	return events, got
}

func lastEvent(events []turn.Event) turn.Event {
	if len(events) == 0 {
		return turn.Event{}
	}
	return events[len(events)-1]
}

func requests(got []map[string]any, method string) []map[string]any {
	var out []map[string]any
	for _, m := range got {
		if m["method"] == method {
			p, _ := m["params"].(map[string]any)
			out = append(out, p)
		}
	}
	return out
}

func TestAgyTurn(t *testing.T) {
	events, got := agyRun(t, "turn", &turn.Request{Prompt: "run the tests", Model: "gemini-4-argon", Effort: "low", SystemPrompt: "You are on WhatsApp."})
	r := lastEvent(events)
	if r.Type != turn.EventResult || r.Text != "All done." || r.SessionID != "agy-1" || r.Model != "gemini-4-argon" || r.ContextTokens != 600 {
		t.Fatalf("result = %+v", r)
	}
	// 1000 input, 500 cached and 100 out at Argon's price that day.
	want, _ := turn.Price("gemini-4-argon", turn.Tokens{Input: 1000, CacheRead: 500, Output: 100})
	if r.CostUSD != want || want == 0 {
		t.Fatalf("cost = %v, want %v", r.CostUSD, want)
	}
	if events[0].Type != turn.EventStarted || events[0].SessionID != "agy-1" {
		t.Fatalf("first event = %+v", events[0])
	}
	var tool bool
	for _, e := range events {
		tool = tool || (e.Type == turn.EventTool && e.Tool == "run_command" && e.Detail == "make test")
	}
	if !tool {
		t.Fatalf("no tool event in %+v", events)
	}
	newSession := requests(got, "session/new")
	if len(newSession) != 1 || !strings.Contains(mustJSON(newSession[0]), `"append":"You are on WhatsApp."`) {
		t.Fatalf("session/new = %v", newSession)
	}
	set := mustJSON(requests(got, "session/set_config_option"))
	if !strings.Contains(set, `"configId":"model","sessionId":"agy-1","value":"gemini-4-argon"`) || !strings.Contains(set, `"configId":"thought_level","sessionId":"agy-1","value":"low"`) {
		t.Fatalf("config options = %s", set)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A command agy moved to the background ends the prompt early; the turn
// waits for agy to go on, and the reply is what it said then.
func TestAgyWaitsForBackgroundCommands(t *testing.T) {
	events, _ := agyRun(t, "background", &turn.Request{Prompt: "run the suite"})
	if r := lastEvent(events); r.Type != turn.EventResult || r.Text != "It passed." || r.ContextTokens != 300 {
		t.Fatalf("result = %+v", r)
	}
}

// A conversation agy has on disk is loaded, and its replay is not taken
// for this turn's answer; an ID it has no transcript for starts afresh.
func TestAgyLoadsOnlyConversationsItHas(t *testing.T) {
	home := t.TempDir()
	conversations := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	os.MkdirAll(conversations, 0o755)
	os.WriteFile(filepath.Join(conversations, "agy-1.db"), nil, 0o644)
	req := &turn.Request{Prompt: "carry on", SessionID: "agy-1"}
	events, got := agyRunIn(t, home, "turn", req)
	if r := lastEvent(events); r.Text != "All done." || strings.Contains(r.Text, "old answer") || r.SessionID != "agy-1" {
		t.Fatalf("result = %+v", r)
	}
	if len(requests(got, "session/load")) != 1 || len(requests(got, "session/new")) != 0 {
		t.Fatalf("requests = %v", got)
	}

	events, got = agyRun(t, "turn", &turn.Request{Prompt: "hello", SessionID: "chosen-by-hermitator"})
	if r := lastEvent(events); r.SessionID != "agy-1" || len(requests(got, "session/load")) != 0 {
		t.Fatalf("result = %+v, requests %v", r, got)
	}
}

// A message sent mid-turn goes in as another prompt, which carries the
// turn on.
func TestAgySteering(t *testing.T) {
	spool := t.TempDir()
	t.Setenv("HERMITATOR_SPOOL", spool)
	dir := filepath.Join(spool, "steer", "turn-1")
	os.MkdirAll(dir, 0o755)
	go func() {
		time.Sleep(500 * time.Millisecond)
		os.WriteFile(filepath.Join(dir, "1.json"), []byte(`{"text":"make it blue"}`), 0o644)
	}()
	events, got := agyRun(t, "steer", &turn.Request{Prompt: "paint it", SteerID: "turn-1"})
	if r := lastEvent(events); r.Text != "Blue it is." {
		t.Fatalf("result = %+v", r)
	}
	prompts := requests(got, "session/prompt")
	if len(prompts) != 2 || !strings.Contains(mustJSON(prompts[1]), "make it blue") {
		t.Fatalf("prompts = %v", prompts)
	}
	var steered bool
	for _, e := range events {
		steered = steered || (e.Type == turn.EventSteered && e.Text == "make it blue")
	}
	if !steered {
		t.Fatal("no steered event")
	}
}

func TestPrices(t *testing.T) {
	tok := turn.Tokens{Input: 1_000_000, Output: 1_000_000}
	if c, ok := turn.PriceAt("gemini-4-neon-high", tok, time.Now()); !ok || c != 18 {
		t.Fatalf("neon = %v, %v", c, ok)
	}
	if c, _ := turn.PriceAt("gemini-4-argon", tok, time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC)); c != 12 {
		t.Fatalf("argon before December = %v", c)
	}
	if c, _ := turn.PriceAt("gemini-4-argon-low", tok, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)); c != 24 {
		t.Fatalf("argon from December = %v", c)
	}
	if _, ok := turn.PriceAt("gemini-pro-orchestrator-neon", tok, time.Now()); ok {
		t.Fatal("an unpriced model was priced")
	}
}
