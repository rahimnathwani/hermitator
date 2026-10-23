package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rahimnathwani/hermitator/internal/store"
)

// reply is msg sent as a WhatsApp reply to quoted, the way the gateway
// hands it over.
func reply(msg Message, quoted string) Message {
	msg.QuotedID, msg.QuotedText, msg.Text = "q-1", quoted, QuoteText(quoted, msg.Text)
	return msg
}

// Replying is how a phone points at a message, so a command sent as a
// reply must still be a command, not text for the agent.
func TestCommandsWorkAsReplies(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)

	e.m.HandleMessage(ctx, reply(e.msg(chat, "/status", true), "**echo:** earlier\nanswer"))
	if got := e.rec.wait(t, "text"); !strings.HasPrefix(got.body, "*bot*") {
		t.Fatalf("/status as a reply = %q", got.body)
	}
	if turns, _ := e.st.Turns(ctx, e.agent.ID, 1); len(turns) != 0 {
		t.Fatal("a command sent as a reply ran a turn")
	}

	// /once keeps the quote, so the agent knows what "this" is.
	e.m.HandleMessage(ctx, reply(e.msg(chat, "/once opus: explain this", true), "the earlier answer"))
	e.rec.wait(t, "text")
	e.idle(t)
	spec, _ := e.m.Spec(e.agent)
	prompts, _ := os.ReadFile(filepath.Join(spec.Dir, "home", "prompts"))
	if !strings.Contains(string(prompts), `\u003e the earlier answer\n\nexplain this`) {
		t.Fatalf("prompt = %s", prompts)
	}

	// Text that merely starts with a slash after a quote is still for the agent.
	e.m.HandleMessage(ctx, reply(e.msg(chat, "/workspace is full, see above", true), "disk report"))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "/workspace is full") {
		t.Fatalf("reply = %q", got.body)
	}
}

// /in, /at and /every set up tasks without a model call, and /tasks run
// starts one outside its schedule.
func TestScheduleFromChat(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)

	e.m.HandleMessage(ctx, e.msg(chat, "/in 2h check whether the deploy finished", true))
	if got := e.rec.wait(t, "text"); !strings.HasPrefix(got.body, "📅 Scheduled: check whether the deploy finished (once, next") {
		t.Fatalf("/in = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/at 2099-01-02 09:30 wish me a happy new year", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "Not scheduled: that is more than a year away") {
		t.Fatalf("/at far away = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/every 0 9 * * 1-5: triage new issues", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "triage new issues (cron 0 9 * * 1-5, next") {
		t.Fatalf("/every = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/every * * * * *: spam", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "Not scheduled") || !strings.Contains(got.body, "Usage: /every") {
		t.Fatalf("/every too often = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/in soon do it", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, `"soon" is not a duration`) {
		t.Fatalf("/in soon = %q", got.body)
	}
	tasks, _ := e.st.Tasks(ctx, e.agent.ID)
	var in *store.Task
	n := 0
	for i, task := range tasks {
		if task.Cron == "" {
			in, n = task, i+1
		}
	}
	if len(tasks) != 2 || in == nil || in.CreatedBy != store.TaskByOwner || time.Until(in.NextRun).Round(time.Hour) != 2*time.Hour {
		t.Fatalf("tasks = %+v", in)
	}
	if turns, _ := e.st.Turns(ctx, e.agent.ID, 1); len(turns) != 0 {
		t.Fatal("scheduling ran a model turn")
	}
	if days("3d") != "72h" || days("90m") != "90m" || days("d") != "d" {
		t.Fatal("days")
	}

	e.m.HandleMessage(ctx, e.msg(chat, fmt.Sprintf("/tasks run %d", n), true))
	if got := e.rec.wait(t, "text"); got.body != "▶️ Running now: check whether the deploy finished." {
		t.Fatalf("/tasks run = %q", got.body)
	}
	if got := e.rec.wait(t, "text"); !strings.HasPrefix(got.body, "⏰ *check whether the deploy finished*") {
		t.Fatalf("task reply = %q", got.body)
	}
	e.idle(t)
	spec, _ := e.m.Spec(e.agent)
	prompts, _ := os.ReadFile(filepath.Join(spec.Dir, "home", "prompts"))
	if !strings.Contains(string(prompts), "outside its schedule") {
		t.Fatalf("prompt = %s", prompts)
	}
	if again, _ := e.st.Task(ctx, in.ID); !again.NextRun.Equal(in.NextRun) {
		t.Fatal("/tasks run moved the next run")
	}
}

// /get sends a sandbox file to the chat at once; a folder goes as a zip.
func TestGetFile(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	spec, _ := e.m.Spec(e.agent)
	ws := filepath.Join(spec.Dir, "workspace")
	os.MkdirAll(filepath.Join(ws, "site", "css"), 0o755)
	os.WriteFile(filepath.Join(ws, "report.txt"), []byte("the report"), 0o644)
	os.WriteFile(filepath.Join(ws, "site", "css", "a.css"), []byte("body{}"), 0o644)

	e.m.HandleMessage(ctx, e.msg(chat, "/get report.txt", true))
	if got := e.rec.wait(t, "file"); got.body != "|the report" {
		t.Fatalf("/get file = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/get site", true))
	if got := e.rec.wait(t, "file"); !strings.HasPrefix(got.body, "|PK") {
		t.Fatalf("/get folder = %.20q", got.body)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/get nope.txt", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "nope.txt") || !strings.Contains(got.body, "exit") {
		t.Fatalf("/get missing = %q", got.body)
	}
	if turns, _ := e.st.Turns(ctx, e.agent.ID, 1); len(turns) != 0 {
		t.Fatal("/get ran a model turn")
	}
}

// /git says which repositories have work that exists nowhere else.
func TestGitCommand(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	spec, _ := e.m.Spec(e.agent)
	ws := filepath.Join(spec.Dir, "workspace")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	shop := filepath.Join(ws, "shop")
	os.MkdirAll(shop, 0o755)
	git(shop, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(shop, "a.txt"), []byte("a"), 0o644)
	git(shop, "add", "a.txt")
	git(shop, "commit", "-q", "-m", "Add the navbar")
	os.WriteFile(filepath.Join(shop, "b.txt"), []byte("b"), 0o644)

	e.m.HandleMessage(ctx, e.msg(chat, "/git", true))
	got := e.rec.wait(t, "text")
	if !strings.Contains(got.body, "*/workspace/shop* (main): 1 uncommitted file, no remote, 1 commit") || !strings.Contains(got.body, ": Add the navbar") {
		t.Fatalf("/git = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/git shop", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "?? b.txt") || !strings.Contains(got.body, "Add the navbar") {
		t.Fatalf("/git shop = %q", got.body)
	}
	if formatRepos("") != "No git repositories under /workspace." {
		t.Fatal("no repos")
	}
}
