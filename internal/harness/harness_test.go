package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

// fake installs a stand-in harness binary that records its argv and the
// first line of stdin, prints a captured transcript, and waits for stdin
// to close the way the real tools do.
func fake(t *testing.T, envVar, fixture string) (args, stdin func() string) {
	t.Helper()
	dir := t.TempDir()
	fakeDir = dir
	fixturePath, _ := filepath.Abs(filepath.Join("testdata", fixture))
	script := filepath.Join(dir, "fake")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + dir + "/args\n" +
		"head -n 1 > " + dir + "/stdin\n" +
		"cat " + fixturePath + "\n" +
		"cat > " + dir + "/rest\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envVar, script)
	read := func(name string) func() string {
		return func() string {
			b, _ := os.ReadFile(filepath.Join(dir, name))
			return string(b)
		}
	}
	return read("args"), read("stdin")
}

// fakeDir is the last fake's directory; "rest" there holds what it was
// sent after the first line.
var fakeDir string

func run(t *testing.T, req *turn.Request) []turn.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if req.Workdir == "" {
		req.Workdir = t.TempDir()
	}
	var events []turn.Event
	if err := Run(ctx, req, func(e turn.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	return events
}

func last(events []turn.Event) turn.Event {
	if len(events) == 0 {
		return turn.Event{}
	}
	return events[len(events)-1]
}

func ofType(events []turn.Event, typ string) []turn.Event {
	var out []turn.Event
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func pngFile(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "img.png")
	os.WriteFile(p, []byte("\x89PNG\r\n\x1a\nfake"), 0o600)
	return p
}

func TestClaude(t *testing.T) {
	args, stdin := fake(t, "HERMITATOR_CLAUDE_BIN", "claude-turn.jsonl")
	events := run(t, &turn.Request{
		Harness:      turn.Claude,
		SessionID:    "4a0d6c4e-8a5c-4bb8-9c55-a1d7e1f2c3b4",
		Prompt:       "What colour is this?",
		Images:       []turn.Image{{Path: pngFile(t), MIME: "image/png"}},
		SystemPrompt: "Be brief.",
		Model:        "opus",
		Effort:       "high",
		Env:          map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"},
	})

	res := last(events)
	if res.Type != turn.EventResult || res.IsError || !strings.Contains(res.Text, "red") {
		t.Fatalf("result = %+v", res)
	}
	if res.CostUSD <= 0 || res.SessionID == "" {
		t.Fatalf("result lacks cost or session: %+v", res)
	}
	tools := ofType(events, turn.EventTool)
	if len(tools) != 1 || tools[0].Tool != "Bash" || tools[0].Detail != "echo tool-ok" {
		t.Fatalf("tools = %+v", tools)
	}
	if ofType(events, turn.EventStarted)[0].Model != "claude-opus-5-5" {
		t.Fatalf("started event lacks model: %+v", events[0])
	}

	a := args()
	for _, want := range []string{"--input-format\nstream-json", "--dangerously-skip-permissions", "--model\nopus", "--effort\nhigh", "--disallowedTools\nAskUserQuestion,EnterPlanMode,ExitPlanMode", "--session-id\n4a0d6c4e-8a5c-4bb8-9c55-a1d7e1f2c3b4", "--append-system-prompt\nBe brief."} {
		if !strings.Contains(a, want) {
			t.Errorf("args missing %q:\n%s", want, a)
		}
	}
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Content []map[string]any `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(stdin()), &msg); err != nil {
		t.Fatalf("stdin not JSON: %v", err)
	}
	if msg.Type != "user" || len(msg.Message.Content) != 2 || msg.Message.Content[0]["type"] != "image" {
		t.Fatalf("stdin message = %+v", msg)
	}
}

func TestClaudeExitWithoutResultIsAnError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	os.WriteFile(script, []byte("#!/bin/sh\necho 'Invalid OAuth token' >&2\nexit 1\n"), 0o755)
	t.Setenv("HERMITATOR_CLAUDE_BIN", script)
	events := run(t, &turn.Request{Harness: turn.Claude, Prompt: "hi"})
	if e := last(events); e.Type != turn.EventError || !strings.Contains(e.Text, "Invalid OAuth token") {
		t.Fatalf("last event = %+v", e)
	}
}

func TestPi(t *testing.T) {
	args, stdin := fake(t, "HERMITATOR_PI_BIN", "pi-turn.jsonl")
	events := run(t, &turn.Request{
		Harness:   turn.Pi,
		SessionID: "4a0d6c4e-8a5c-4bb8-9c55-a1d7e1f2c3b4",
		Provider:  "openai-codex",
		Effort:    "minimal",
		Prompt:    "What colour is this?",
		Images:    []turn.Image{{Path: pngFile(t), MIME: "image/png"}},
	})

	res := last(events)
	if res.Type != turn.EventResult || res.IsError || !strings.Contains(res.Text, "Red") {
		t.Fatalf("result = %+v", res)
	}
	if res.CostUSD <= 0 || res.Model == "" {
		t.Fatalf("result lacks cost or model: %+v", res)
	}
	tools := ofType(events, turn.EventTool)
	if len(tools) != 1 || tools[0].Tool != "bash" || tools[0].Detail != "echo tool-ok" {
		t.Fatalf("tools = %+v", tools)
	}
	a := args()
	for _, want := range []string{"--mode\nrpc", "--session-id\n4a0d6c4e", "--provider\nopenai-codex", "--model\n" + DefaultModels["openai-codex"], "--thinking\nminimal"} {
		if !strings.Contains(a, want) {
			t.Errorf("args missing %q:\n%s", want, a)
		}
	}
	var prompt struct {
		Type   string    `json:"type"`
		Images []piImage `json:"images"`
	}
	if err := json.Unmarshal([]byte(stdin()), &prompt); err != nil || prompt.Type != "prompt" || len(prompt.Images) != 1 {
		t.Fatalf("prompt = %+v, %v", prompt, err)
	}
}

func TestFilesAreWrittenAndChangesSyncedBack(t *testing.T) {
	dir := t.TempDir()
	auth := filepath.Join(dir, "agent", "auth.json")
	script := filepath.Join(dir, "pi")
	// A pi that refreshes its token mid-turn, as the real one does.
	os.WriteFile(script, []byte("#!/bin/sh\nhead -n1 >/dev/null\necho '{\"refreshed\":true}' > "+auth+"\necho '{\"type\":\"agent_settled\"}'\ncat >/dev/null\n"), 0o755)
	t.Setenv("HERMITATOR_PI_BIN", script)
	events := run(t, &turn.Request{
		Harness: turn.Pi,
		Prompt:  "hi",
		Files:   []turn.File{{Path: auth, Content: `{"old":true}`, Sync: true}},
	})
	files := ofType(events, turn.EventFile)
	if len(files) != 1 || !strings.Contains(files[0].Content, "refreshed") {
		t.Fatalf("file events = %+v", files)
	}
	if fi, err := os.Stat(auth); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("auth file mode = %v, %v", fi.Mode(), err)
	}
}

func TestSummarize(t *testing.T) {
	cases := []struct {
		in   map[string]any
		want string
	}{
		{map[string]any{"command": "npm   test\n --watch"}, "npm test --watch"},
		{map[string]any{"file_path": "/workspace/a.go", "content": "x"}, "/workspace/a.go"},
		{map[string]any{"limit": 3.0}, ""},
	}
	for _, c := range cases {
		if got := summarize("t", c.in); got != c.want {
			t.Errorf("summarize(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClaudeUsageAndTodos(t *testing.T) {
	fake(t, "HERMITATOR_CLAUDE_BIN", "claude-usage-todos.jsonl")
	events := run(t, &turn.Request{Harness: turn.Claude, Prompt: "hi"})
	usage := ofType(events, turn.EventUsage)
	if len(usage) != 1 || len(usage[0].Windows) != 2 || usage[0].Windows[0].Name != "five_hour" || usage[0].Windows[1].Utilization != 0.64 {
		t.Fatalf("usage = %+v", usage)
	}
	todos := ofType(events, turn.EventTodos)
	if len(todos) != 1 || len(todos[0].Todos) != 3 || todos[0].Todos[1] != (turn.Todo{Content: "Run the tests", Status: "in_progress"}) {
		t.Fatalf("todos = %+v", todos)
	}
}

func TestCompact(t *testing.T) {
	args, stdin := fake(t, "HERMITATOR_PI_BIN", "pi-compact.jsonl")
	res := last(run(t, &turn.Request{Harness: turn.Pi, SessionID: "4a0d6c4e", Provider: "openai-codex", Compact: true, Prompt: "Keep the codeword."}))
	if res.Type != turn.EventResult || res.ContextTokens != 26484 || res.CostUSD < 0.0056 || !strings.Contains(res.Text, "43425") {
		t.Fatalf("pi compact = %+v", res)
	}
	if !strings.Contains(stdin(), `"customInstructions":"Keep the codeword."`) || !strings.Contains(args(), "--session-id\n4a0d6c4e") {
		t.Fatalf("pi was sent %q with %q", stdin(), args())
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".claude", "projects", "w"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "projects", "w", "b2fbb8dd.jsonl"), nil, 0o644)
	args, _ = fake(t, "HERMITATOR_CLAUDE_BIN", "claude-compact.jsonl")
	res = last(run(t, &turn.Request{Harness: turn.Claude, SessionID: "b2fbb8dd", Compact: true, Model: "haiku", Prompt: "Keep the codeword."}))
	if res.Type != turn.EventResult || res.CostUSD != 0.0213 || !strings.Contains(res.Text, "36641") || res.ContextTokens != 0 {
		t.Fatalf("claude compact = %+v", res)
	}
	if a := args(); !strings.HasPrefix(a, "-p\n/compact Keep the codeword.\n--resume\nb2fbb8dd\n") || !strings.Contains(a, "--model\nhaiku") {
		t.Fatalf("claude args:\n%s", a)
	}
	// Nothing to compact without a transcript.
	if res := last(run(t, &turn.Request{Harness: turn.Claude, SessionID: "none", Compact: true})); res.Type != turn.EventError {
		t.Fatalf("compacting a missing conversation = %+v", res)
	}
}

// A turn with a spending limit stops partway at it, in both harnesses.
func TestBudget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".claude", "projects", "w"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "projects", "w", "b2fbb8dd.jsonl"), nil, 0o644)
	args, _ := fake(t, "HERMITATOR_CLAUDE_BIN", "claude-budget.jsonl")
	res := last(run(t, &turn.Request{Harness: turn.Claude, SessionID: "b2fbb8dd", Prompt: "build it", MaxCostUSD: 0.25, SessionCostUSD: 1.5}))
	if res.Type != turn.EventResult || res.StopReason != turn.StopBudget || !res.IsError || res.CostUSD != 1.76 {
		t.Fatalf("claude over budget = %+v", res)
	}
	// Claude Code counts the resumed session's running total against it.
	if a := args(); !strings.Contains(a, "--max-budget-usd\n1.7500\n") {
		t.Fatalf("claude args:\n%s", a)
	}
	run(t, &turn.Request{Harness: turn.Claude, SessionID: "new-one", Prompt: "hi", MaxCostUSD: 0.25, SessionCostUSD: 1.5})
	if a := args(); !strings.Contains(a, "--max-budget-usd\n0.2500\n") {
		t.Fatalf("a new session's budget:\n%s", a)
	}
	run(t, &turn.Request{Harness: turn.Claude, SessionID: "new-one", Prompt: "hi"})
	if strings.Contains(args(), "--max-budget-usd") {
		t.Fatal("a budget with no limit set")
	}

	fake(t, "HERMITATOR_PI_BIN", "pi-budget.jsonl")
	res = last(run(t, &turn.Request{Harness: turn.Pi, SessionID: "p1", Provider: "openai-codex", Prompt: "build it", MaxCostUSD: 0.05}))
	if res.Type != turn.EventResult || res.StopReason != turn.StopBudget || res.CostUSD < 0.0699 || res.CostUSD > 0.0701 || res.Text != "Still building." {
		t.Fatalf("pi over budget = %+v", res)
	}
	if rest, _ := os.ReadFile(filepath.Join(fakeDir, "rest")); !strings.Contains(string(rest), `{"type":"abort"}`) {
		t.Fatalf("pi was not told to stop: %q", rest)
	}
}

func TestClaudeReportsConversationSize(t *testing.T) {
	fake(t, "HERMITATOR_CLAUDE_BIN", "claude-turn.jsonl")
	// The last call of the captured turn read 2 + 9459 + 27004 tokens.
	if res := last(run(t, &turn.Request{Harness: turn.Claude, Prompt: "hi"})); res.ContextTokens != 36465 {
		t.Fatalf("context = %d", res.ContextTokens)
	}
}
