package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

func TestRecall(t *testing.T) {
	home, sharedDir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HERMITATOR_SHARED", sharedDir)
	write := func(path, body string) {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".claude", "projects", "-workspace", "s1.jsonl"), strings.Join([]string{
		`{"type":"user","timestamp":"2026-10-01T09:00:00Z","message":{"role":"user","content":"Deploy the shop to staging"}}`,
		`{"type":"user","isMeta":true,"timestamp":"2026-10-01T09:00:01Z","message":{"role":"user","content":"<local-command-caveat>port 8443 noise</local-command-caveat>"}}`,
		`{"type":"assistant","timestamp":"2026-10-01T09:01:00Z","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"tool_use","name":"Bash","input":{"command":"make deploy"}}]}}`,
		`{"type":"user","timestamp":"2026-10-01T09:02:00Z","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"error E4471: port 8443 in use"}]}]}}`,
		`{"type":"assistant","timestamp":"2026-10-01T09:03:00Z","message":{"content":[{"type":"text","text":"The deploy failed: port 8443 is taken by the old server."}]}}`,
		`{"type":"user","isCompactSummary":true,"timestamp":"2026-10-02T09:00:00Z","message":{"content":"Summary: tried to deploy."}}`,
		`not json`,
	}, "\n")+"\n")
	write(filepath.Join(home, ".pi", "agent", "sessions", "--workspace--", "s2.jsonl"), strings.Join([]string{
		`{"type":"session","version":3,"timestamp":"2026-10-05T10:00:00Z"}`,
		`{"type":"message","timestamp":"2026-10-05T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Which port did staging use?"}]}}`,
		`{"type":"message","timestamp":"2026-10-05T10:00:02Z","message":{"role":"assistant","content":[{"type":"toolCall","name":"bash","arguments":{"command":"grep port deploy.toml"}}]}}`,
		`{"type":"message","timestamp":"2026-10-05T10:00:03Z","message":{"role":"toolResult","toolName":"bash","content":[{"type":"text","text":"port = 8443"}]}}`,
		`{"type":"compaction","timestamp":"2026-10-06T10:00:00Z","summary":"Staging listens on port 8443."}`,
	}, "\n")+"\n")
	write(filepath.Join(sharedDir, "chats", "Sam-abc123.md"), "2026-10-07 18:30 Sam: let's move staging\n    to port 9443 next week\n2026-10-07 18:31 you (the agent): Noted.\n")

	run := func(args ...string) (string, int) {
		f, _ := os.CreateTemp(t.TempDir(), "out")
		code := recall(args, f)
		f.Close()
		b, _ := os.ReadFile(f.Name())
		return string(b), code
	}

	out, code := run("port", "8443")
	if code != 0 {
		t.Fatalf("search failed:\n%s", out)
	}
	for _, want := range []string{"tool result: error E4471: port 8443 in use", "you: The deploy failed: port 8443", "tool result: port = 8443", "summary: Staging listens on port 8443."} {
		if !strings.Contains(out, want) {
			t.Errorf("search lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "noise") {
		t.Errorf("search found Claude Code's own meta messages:\n%s", out)
	}
	// Newest first: pi's compaction summary, then its tool result.
	if strings.Index(out, "Staging listens") > strings.Index(out, "port = 8443") {
		t.Errorf("not newest first:\n%s", out)
	}
	if out, _ := run("9443"); !strings.Contains(out, "Sam: let's move staging to port 9443 next week") {
		t.Errorf("chat history not searched, or its continuation lost:\n%s", out)
	}
	if _, code := run("no-such-thing"); code != 1 {
		t.Errorf("a search that finds nothing exited %d", code)
	}

	ref := regexp.MustCompile(`([0-9a-f]{6}:\d+)  \S+ \S+  tool result: error E4471`).FindStringSubmatch(out)
	if ref == nil {
		t.Fatalf("no reference for the deploy error in:\n%s", out)
	}
	shown, code := run("--show", ref[1], "--around", "1")
	if code != 0 || !strings.Contains(shown, "> "+ref[1]) || !strings.Contains(shown, "tool: Bash {\"command\":\"make deploy\"}") ||
		!strings.Contains(shown, "you: The deploy failed") || strings.Contains(shown, "Deploy the shop") {
		t.Fatalf("show %s:\n%s", ref[1], shown)
	}

	list, _ := run()
	if strings.Count(list, "messages") != 3 || !strings.Contains(list, "Deploy the shop to staging") {
		t.Fatalf("list:\n%s", list)
	}
}

func TestSnippet(t *testing.T) {
	text := strings.Repeat("a ", 200) + "needle é " + strings.Repeat("b ", 200)
	s := snippet(text, regexp.MustCompile("(?i)NEEDLE"), 60)
	if !strings.Contains(s, "needle é") || !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") || len([]rune(s)) > 70 {
		t.Fatalf("snippet = %q", s)
	}
}

// From a turn, recall keeps to that chat's conversations and history
// file; --all reaches every chat's.
func TestRecallKeepsToTheChat(t *testing.T) {
	home, sharedDir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HERMITATOR_SHARED", sharedDir)
	write := func(path, body string) {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	claude := func(text string) string {
		return `{"type":"user","timestamp":"2026-10-01T09:00:00Z","message":{"role":"user","content":"` + text + `"}}` + "\n"
	}
	write(filepath.Join(home, ".claude", "projects", "-workspace", "sa.jsonl"), claude("plan in this chat"))
	write(filepath.Join(home, ".claude", "projects", "-workspace", "sb.jsonl"), claude("plan in the other chat"))
	write(filepath.Join(home, ".pi", "agent", "sessions", "--workspace--", "2026-10-05T10-00-00-000Z_sp.jsonl"),
		`{"type":"message","timestamp":"2026-10-05T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"older plan in this chat"}]}}`+"\n")
	write(filepath.Join(sharedDir, "chats", "this-1.md"), "2026-10-07 18:30 Sam: plan in this chat's history\n")
	write(filepath.Join(sharedDir, "chats", "other-2.md"), "2026-10-07 18:30 Kim: plan in the other chat's history\n")
	t.Setenv(turn.EnvSessions, "sa,sp")
	t.Setenv(turn.EnvHistory, "/shared/chats/this-1.md")

	run := func(args ...string) string {
		f, _ := os.CreateTemp(t.TempDir(), "out")
		recall(args, f)
		f.Close()
		b, _ := os.ReadFile(f.Name())
		return string(b)
	}
	out := run("plan")
	for _, want := range []string{"plan in this chat", "older plan in this chat", "plan in this chat's history"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "other chat") {
		t.Errorf("reached another chat:\n%s", out)
	}
	if out := run("--all", "plan", "other"); !strings.Contains(out, "plan in the other chat") || !strings.Contains(out, "other chat's history") {
		t.Errorf("--all did not reach the other chat:\n%s", out)
	}
	if out := run("Kim"); !strings.Contains(out, "--all searches your other chats") {
		t.Errorf("a miss in this chat does not point at --all:\n%s", out)
	}
}
