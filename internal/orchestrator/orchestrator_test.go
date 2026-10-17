package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/secrets"
	"github.com/rahimnathwani/hermitator/internal/store"
)

var guestBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hermitator-guest-")
	if err != nil {
		panic(err)
	}
	guestBinary = filepath.Join(dir, "hermitator-guest")
	build := exec.Command("go", "build", "-o", guestBinary, "../../cmd/hermitator-guest")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		panic("building the guest: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeClaude stands in for Claude Code. It echoes the prompt, and when
// asked to, sends a file with whatsapp-send the way an agent would.
const fakeClaude = `#!/bin/sh
read -r line
sid=""
while [ $# -gt 0 ]; do
  case "$1" in --session-id|--resume) sid="$2"; echo "$1" >> "$HOME/modes" ;; esac
  shift
done
[ -n "$CLAUDE_CODE_OAUTH_TOKEN" ] || { echo "no token" >&2; exit 1; }
text=$(printf '%s' "$line" | sed -n 's/.*"type":"text","text":"\([^"]*\)".*/\1/p')
echo '{"type":"system","subtype":"init","session_id":"'"$sid"'","model":"fake"}'
case "$text" in
  *"send a file"*)
    echo "report body" > /tmp/hermitator-test-report.txt
    whatsapp-send /tmp/hermitator-test-report.txt "the report" >/dev/null
    echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"whatsapp-send report.txt"}}]}}'
    ;;
  *"sleep"*) sleep 30 ;;
esac
echo '{"type":"result","subtype":"success","is_error":false,"result":"**echo:** '"$text"'","session_id":"'"$sid"'","total_cost_usd":0.01}'
`

type sent struct{ kind, chat, body string }

type recorder struct {
	mu  sync.Mutex
	log []sent
	ch  chan sent
}

func (r *recorder) add(s sent) {
	r.mu.Lock()
	r.log = append(r.log, s)
	r.mu.Unlock()
	r.ch <- s
}
func (r *recorder) SendText(_ context.Context, _, chat, text string) error {
	r.add(sent{"text", chat, text})
	return nil
}
func (r *recorder) SendFile(_ context.Context, _, chat, name string, data []byte, caption string) error {
	r.add(sent{"file", chat, caption + "|" + string(data)})
	return nil
}
func (r *recorder) React(_ context.Context, _, chat, _, _, emoji string) error {
	r.add(sent{"react", chat, emoji})
	return nil
}
func (r *recorder) Typing(context.Context, string, string, bool) error { return nil }

// wait returns the next event of the given kind.
func (r *recorder) wait(t *testing.T, kind string) sent {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case s := <-r.ch:
			if s.kind == kind {
				return s
			}
		case <-deadline:
			r.mu.Lock()
			defer r.mu.Unlock()
			t.Fatalf("timed out waiting for %s; got %+v", kind, r.log)
		}
	}
}

type env struct {
	m     *Manager
	st    *store.Store
	rec   *recorder
	user  *store.User
	agent *store.Agent
	acct  *store.Account
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	root := t.TempDir()

	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "claude"), []byte(fakeClaude), 0o755)
	t.Setenv("HERMITATOR_CLAUDE_BIN", filepath.Join(bin, "claude"))

	images := filepath.Join(root, "images")
	os.MkdirAll(filepath.Join(images, "universal-test", "rootfs"), 0o755)
	os.Symlink("universal-test", filepath.Join(images, "universal"))
	tools, err := sandbox.InstallTools(filepath.Join(root, "tools"), guestBinary, fstest.MapFS{"pi-login.mjs": {Data: []byte("")}})
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(make([]byte, secrets.KeySize))
	rt := &sandbox.Local{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	m := New(Config{
		AgentsDir: filepath.Join(root, "agents"),
		Images:    &sandbox.Images{Dir: images},
		Tools:     tools,
		Subnet:    netip.MustParsePrefix("10.88.0.0/16"),
		Debounce:  50 * time.Millisecond,
		PublicURL: "https://hermitator.example.com",
	}, st, box, rt, log)
	rec := &recorder{ch: make(chan sent, 100)}
	m.SetMessenger(rec)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}

	user := &store.User{Email: "a@example.com", PasswordHash: "x", Role: store.RoleUser}
	st.CreateUser(ctx, user)
	agent := &store.Agent{Name: "bot", Harness: store.HarnessClaude}
	if err := m.CreateAgent(ctx, user, agent, "universal"); err != nil {
		t.Fatal(err)
	}
	acct := &store.Account{UserID: user.ID, JID: "1:2@s.whatsapp.net", Status: store.AccountConnected}
	st.CreateAccount(ctx, acct)

	t.Cleanup(func() {
		if spec, err := m.Spec(agent); err == nil {
			rt.Stop(context.Background(), spec)
		}
		cancel()
		st.Close()
	})
	return &env{m: m, st: st, rec: rec, user: user, agent: agent, acct: acct}
}

func (e *env) msg(chat, text string, fromMe bool) Message {
	return Message{AccountID: e.acct.ID, Chat: chat, Sender: "1@s.whatsapp.net", ID: fmt.Sprint(time.Now().UnixNano()), FromMe: fromMe, Text: text, Time: time.Now()}
}

// idle waits until the agent has wrapped up its turn.
func (e *env) idle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.m.Status(e.agent.ID).Busy {
		if time.Now().After(deadline) {
			t.Fatal("agent never went idle")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) link(t *testing.T, chat string) {
	t.Helper()
	code, _, err := e.m.LinkCode(context.Background(), e.agent.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	e.m.HandleMessage(context.Background(), e.msg(chat, "/link "+strings.ToLower(code), true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "Linked this chat") {
		t.Fatalf("link reply = %q", got.body)
	}
}

func TestTurnEndToEnd(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chat := "self@s.whatsapp.net"

	// Without a token, the agent explains what is missing.
	e.link(t, chat)
	e.m.HandleMessage(ctx, e.msg(chat, "hello", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "claude setup-token") || !strings.Contains(got.body, "https://hermitator.example.com/settings") {
		t.Fatalf("missing-token reply = %q", got.body)
	}
	if got := e.rec.wait(t, "react"); got.body != "❌" {
		t.Fatalf("missing-token reaction = %q", got.body)
	}

	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	e.m.HandleMessage(ctx, e.msg(chat, "hello there", true))
	if got := e.rec.wait(t, "react"); got.body != "⏳" {
		t.Fatalf("first reaction = %q", got.body)
	}
	if got := e.rec.wait(t, "text"); got.body != "*echo:* hello there" {
		t.Fatalf("reply = %q", got.body)
	}
	if got := e.rec.wait(t, "react"); got.body != "✅" {
		t.Fatalf("final reaction = %q", got.body)
	}

	// The agent sends a file mid-turn.
	e.m.HandleMessage(ctx, e.msg(chat, "please send a file", true))
	if got := e.rec.wait(t, "file"); got.body != "the report|report body\n" {
		t.Fatalf("file = %q", got.body)
	}
	e.rec.wait(t, "text")
	e.idle(t)

	turns, _ := e.st.Turns(ctx, e.agent.ID, 10)
	if len(turns) != 2 || turns[0].Status != store.TurnOK || turns[0].Tools != 1 {
		t.Fatalf("turns = %+v", turns)
	}
	ag, _ := e.st.Agent(ctx, e.agent.ID)
	if ag.SessionID == "" || ag.State != sandbox.Running {
		t.Fatalf("agent after turns = %+v", ag)
	}

	// Commands are answered by hermitator without waking a turn.
	e.m.HandleMessage(ctx, e.msg(chat, "/status", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "*bot*") || !strings.Contains(got.body, "awake") {
		t.Fatalf("status = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/sleep", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "stopped") {
		t.Fatalf("sleep = %q", got.body)
	}
	if ag, _ := e.st.Agent(ctx, e.agent.ID); ag.State != sandbox.Stopped {
		t.Fatalf("state after /sleep = %s", ag.State)
	}
}

func TestAbort(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chat := "self@s.whatsapp.net"
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	e.link(t, chat)
	e.m.HandleMessage(ctx, e.msg(chat, "sleep for a while", true))
	e.rec.wait(t, "react") // ⏳
	deadline := time.Now().Add(10 * time.Second)
	for !e.m.Status(e.agent.ID).Busy && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // let the job start in the guest
	e.m.HandleMessage(ctx, e.msg(chat, "/stop", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "Stopping") {
		t.Fatalf("stop reply = %q", got.body)
	}
	if got := e.rec.wait(t, "react"); got.body != "⏹" {
		t.Fatalf("reaction after abort = %q", got.body)
	}
}

func TestRoutingRules(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")

	// Unlinked chats are ignored, and so is a /link from someone else.
	e.m.HandleMessage(ctx, e.msg("friend@s.whatsapp.net", "hi", false))
	code, _, _ := e.m.LinkCode(ctx, e.agent.ID, false)
	e.m.HandleMessage(ctx, e.msg("friend@s.whatsapp.net", "/link "+code, false))

	// With a fallback agent, other people's direct messages reach it, but
	// the owner's own chats and groups do not.
	e.st.SetAccountFallback(ctx, e.acct.ID, e.agent.ID)
	e.m.HandleMessage(ctx, e.msg("me-to-friend@s.whatsapp.net", "see you at 6", true))
	group := e.msg("123@g.us", "hello group", false)
	group.IsGroup = true
	e.m.HandleMessage(ctx, group)
	e.m.HandleMessage(ctx, e.msg("stranger@s.whatsapp.net", "who is this?", false))

	got := e.rec.wait(t, "text")
	if got.chat != "stranger@s.whatsapp.net" || !strings.Contains(got.body, "who is this?") {
		t.Fatalf("first reply went to %s: %q", got.chat, got.body)
	}
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	for _, s := range e.rec.log {
		if s.chat != "stranger@s.whatsapp.net" {
			t.Fatalf("hermitator answered a chat it should have ignored: %+v", s)
		}
	}
	if _, err := e.st.Route(ctx, e.acct.ID, "friend@s.whatsapp.net"); err == nil {
		t.Fatal("someone other than the owner linked a chat")
	}
}

func TestOutboxDoesNotFollowSymlinksOutOfTheSandbox(t *testing.T) {
	e := setup(t)
	spec, err := e.m.Spec(e.agent)
	if err != nil {
		t.Fatal(err)
	}
	outbox := filepath.Join(spec.SharedDir(), "outbox")
	os.MkdirAll(outbox, 0o755)
	secret := filepath.Join(t.TempDir(), "host-secret")
	os.WriteFile(secret, []byte("do not send"), 0o600)
	// What a hostile agent could leave in its outbox.
	os.Symlink(secret, filepath.Join(outbox, "1-loot.txt"))
	os.WriteFile(filepath.Join(outbox, "1-loot.txt.meta.json"), []byte(`{"file":"1-loot.txt"}`), 0o644)

	chat := chatRef{m: e.m, msg: e.msg("self@s.whatsapp.net", "", true)}
	e.m.deliverOutbox(context.Background(), spec, chat)

	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	for _, s := range e.rec.log {
		if s.kind == "file" || strings.Contains(s.body, "do not send") {
			t.Fatalf("sent a host file through a symlink: %+v", s)
		}
	}
}

func TestInboxDoesNotFollowSymlinksOutOfTheSandbox(t *testing.T) {
	e := setup(t)
	spec, _ := e.m.Spec(e.agent)
	os.MkdirAll(spec.SharedDir(), 0o755)
	target := t.TempDir()
	os.Symlink(target, filepath.Join(spec.SharedDir(), "inbox"))
	msg := e.msg("self@s.whatsapp.net", "", true)
	msg.Media = &Media{Kind: "document", MIME: "text/plain", FileName: "payload.txt",
		Fetch: func(context.Context) ([]byte, error) { return []byte("* * * * * root sh -c evil"), nil }}
	note, _ := e.m.saveMedia(context.Background(), spec, msg, 0)
	if !strings.Contains(note, "could not be saved") {
		t.Fatalf("note = %q", note)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("attachment was written outside the sandbox: %v", entries)
	}
}
