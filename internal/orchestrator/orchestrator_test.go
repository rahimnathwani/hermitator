package orchestrator

import (
	"context"
	"errors"
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
	"github.com/rahimnathwani/hermitator/internal/turn"
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
// It runs under bash, not sh: the steering case needs read -t, which
// dash (Linux's sh) lacks.
const fakeClaude = `#!/usr/bin/env bash
read -r line
printf '%s\n' "$@" > "$HOME/last-args"
sid=""
while [ $# -gt 0 ]; do
  case "$1" in --session-id|--resume) sid="$2"; echo "$1" >> "$HOME/modes" ;; esac
  # Like Claude Code, keep a transcript, so the next turn resumes it.
  [ "$1" = "--session-id" ] && mkdir -p "$HOME/.claude/projects/w" && touch "$HOME/.claude/projects/w/$2.jsonl"
  shift
done
[ -n "$CLAUDE_CODE_OAUTH_TOKEN" ] || { echo "no token" >&2; exit 1; }
text=$(printf '%s' "$line" | sed -n 's/.*"type":"text","text":"\([^"]*\)".*/\1/p' | tr -d '\\')
echo '{"type":"system","subtype":"init","session_id":"'"$sid"'","model":"fake"}'
case "$text" in
  *"send a file"*)
    echo "report body" > /tmp/hermitator-test-report.txt
    whatsapp-send /tmp/hermitator-test-report.txt "the report" >/dev/null
    echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"whatsapp-send report.txt"}}]}}'
    ;;
  *"sleep"*) sleep 30 ;;
  *"go quiet"*)
    printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"npm test"}}]}}'
    sleep 4 ;;
  *"ask me"*)
    hermitator-guest ask "Deploy to production?" Yes No "Staging first" >> "$HOME/ask.out" 2>&1 ;;
  *"leak it"*)
    text="key=$STRIPE_KEY gh=$GH_TOKEN author=$GIT_AUTHOR_NAME helper=$GIT_CONFIG_KEY_0" ;;
  *"steer me"*)
    # Like Claude Code with stdin left open: take in what arrives while
    # working, then answer once.
    n=0
    while read -t 2 -r more; do n=$((n + 1)); done
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"steered '"$n"'","session_id":"'"$sid"'","total_cost_usd":0.01}'
    exit 0 ;;
  *"plan it"*)
    printf '%s\n' '{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"five_hour":{"utilization":0.95,"resetsAt":'"$(( $(date +%s) + 3600 ))"'},"seven_day":{"utilization":0.40,"resetsAt":'"$(( $(date +%s) + 86400 ))"'}}}}'
    printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TodoWrite","input":{"todos":[{"content":"Fix the navbar","status":"completed"},{"content":"Run the tests","status":"in_progress"}]}}]}}'
    ;;
  *"schedule it"*)
    hermitator-guest schedule --in 1h --name "CI check" check that CI is green >> "$HOME/schedule.out" 2>&1
    ;;
  *"in the background"*)
    (sleep 1; whatsapp-send --text "background job done") >/dev/null 2>&1 &
    ;;
  *"say nothing"*)
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"'"$sid"'","total_cost_usd":0.001}'
    exit 0 ;;
  *"flaky"*)
    # Fails the first time only, like a transient upstream error.
    if [ ! -f "$HOME/flaked" ]; then
      touch "$HOME/flaked"
      printf '%s\n' '{"type":"result","subtype":"error","is_error":true,"result":"upstream overloaded","session_id":"'"$sid"'"}'
      exit 0
    fi ;;
esac
# Like Claude Code, report the session's running total: a cent per turn.
total=$(wc -l < "$HOME/modes" | tr -d ' ')
# printf, not echo: some shells' echo expands the \n escapes in the JSON.
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"**echo:** '"$text"'","session_id":"'"$sid"'","total_cost_usd":0.0'"$total"'}'
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
func (r *recorder) SendPoll(_ context.Context, _, chat, question string, options []string) error {
	r.add(sent{"poll", chat, question + "|" + strings.Join(options, "|")})
	return nil
}
func (r *recorder) React(_ context.Context, _, chat, _, _, emoji string) error {
	r.add(sent{"react", chat, emoji})
	return nil
}
func (r *recorder) Typing(context.Context, string, string, bool) error { return nil }
func (r *recorder) MarkRead(_ context.Context, _, chat, _, id string) error {
	r.add(sent{"read", chat, id})
	return nil
}

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
		AgentsDir:    filepath.Join(root, "agents"),
		Images:       &sandbox.Images{Dir: images},
		Tools:        tools,
		Subnet:       netip.MustParsePrefix("10.88.0.0/16"),
		Debounce:     50 * time.Millisecond,
		ReapInterval: 100 * time.Millisecond,
		PublicURL:    "https://hermitator.example.com",
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
		cancel()
		m.Wait()
		if spec, err := m.Spec(agent); err == nil {
			rt.Stop(context.Background(), spec)
		}
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
	// Claude reported running totals of 0.01 and 0.02; each turn cost 0.01.
	for _, tr := range turns {
		if tr.CostUSD < 0.0099 || tr.CostUSD > 0.0101 {
			t.Fatalf("turn cost = %v, want 0.01 per turn", tr.CostUSD)
		}
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

// from builds a message the way the gateway does for someone else
// writing to the linked account.
func (e *env) from(phone, text string) Message {
	m := e.msg(phone+"@s.whatsapp.net", text, false)
	m.Sender, m.SenderPhone = phone+"@s.whatsapp.net", phone
	return m
}

// quiet fails if hermitator sent anything to chat.
func (e *env) quiet(t *testing.T, chat string) {
	t.Helper()
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	for _, s := range e.rec.log {
		if s.chat == chat {
			t.Fatalf("hermitator answered a chat it should have ignored: %+v", s)
		}
	}
}

func TestAgentNumber(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	const me, stranger = "447700900123", "15550001111"

	// Strangers messaging the agent's number get nothing, commands included.
	e.m.HandleMessage(ctx, e.from(stranger, "hi, who is this?"))
	e.m.HandleMessage(ctx, e.from(stranger, "/status"))

	// The owner links their chat with the agent's number from their own phone.
	code, _, _ := e.m.LinkCode(ctx, e.agent.ID, false)
	e.m.HandleMessage(ctx, e.from(me, "/link "+code))
	got := e.rec.wait(t, "text")
	if !strings.Contains(got.body, "Linked this chat") || !strings.Contains(got.body, "+"+me) {
		t.Fatalf("link reply = %q", got.body)
	}
	if !e.st.IsOwnerNumber(ctx, e.user.ID, me) {
		t.Fatal("the number that used the code was not recorded as the owner's")
	}

	// Now commands from that number are hermitator's, not the agent's.
	e.m.HandleMessage(ctx, e.from(me, "/status"))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "*bot*") {
		t.Fatalf("status = %q", got.body)
	}
	// And ordinary messages reach the agent, with a read receipt.
	e.m.HandleMessage(ctx, e.from(me, "hello agent"))
	e.rec.wait(t, "read")
	if got := e.rec.wait(t, "text"); got.body != "*echo:* hello agent" {
		t.Fatalf("reply = %q", got.body)
	}
	e.quiet(t, stranger+"@s.whatsapp.net")
}

func TestFallbackOnlyServesTheOwner(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	e.st.AddOwnerNumber(ctx, e.user.ID, "447700900123")
	e.st.SetAccountFallback(ctx, e.acct.ID, e.agent.ID)

	e.m.HandleMessage(ctx, e.from("15550001111", "who is this?"))
	group := e.from("447700900123", "hello group")
	group.Chat, group.IsGroup = "123@g.us", true
	e.m.HandleMessage(ctx, group)
	e.m.HandleMessage(ctx, e.msg("friend@s.whatsapp.net", "see you at 6", true)) // sent by the account itself
	e.m.HandleMessage(ctx, e.from("447700900123", "are you there?"))

	got := e.rec.wait(t, "text")
	if got.chat != "447700900123@s.whatsapp.net" || !strings.Contains(got.body, "are you there?") {
		t.Fatalf("first reply went to %s: %q", got.chat, got.body)
	}
	e.quiet(t, "15550001111@s.whatsapp.net")
	e.quiet(t, "123@g.us")
	e.quiet(t, "friend@s.whatsapp.net")
}

func TestLinkGuessingIsThrottled(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	const guesser = "15550001111"
	for i := 0; i < linkAttempts+3; i++ {
		e.m.HandleMessage(ctx, e.from(guesser, "/link ZZZZZ"))
	}
	time.Sleep(200 * time.Millisecond)
	e.rec.mu.Lock()
	replies := len(e.rec.log)
	e.rec.mu.Unlock()
	if replies != linkAttempts {
		t.Fatalf("got %d replies to wrong codes, want %d", replies, linkAttempts)
	}
	// Even a correct code is ignored once the sender is throttled.
	code, _, _ := e.m.LinkCode(ctx, e.agent.ID, false)
	e.m.HandleMessage(ctx, e.from(guesser, "/link "+code))
	if _, err := e.st.Route(ctx, e.acct.ID, guesser+"@s.whatsapp.net"); err == nil {
		t.Fatal("a throttled sender linked a chat")
	}
}

func TestPersonalNumber(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.st.SetAccountKind(ctx, e.acct.ID, store.KindPersonal)
	e.st.SetAccountFallback(ctx, e.acct.ID, e.agent.ID)
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")

	// Friends writing to the owner, and the owner writing to friends, are
	// none of hermitator's business.
	e.m.HandleMessage(ctx, e.from("15550001111", "dinner tonight?"))
	e.m.HandleMessage(ctx, e.msg("15550001111@s.whatsapp.net", "sure", true))
	// Message yourself, linked by the owner, works.
	e.link(t, "self@s.whatsapp.net")
	e.m.HandleMessage(ctx, e.msg("self@s.whatsapp.net", "hello", true))
	if got := e.rec.wait(t, "text"); got.chat != "self@s.whatsapp.net" {
		t.Fatalf("reply went to %s", got.chat)
	}
	e.quiet(t, "15550001111@s.whatsapp.net")
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

// inGroup is a message in a group chat.
func (e *env) inGroup(phone, text string) Message {
	m := e.from(phone, text)
	m.Chat, m.IsGroup, m.GroupSize, m.ChatName = "team@g.us", true, 3, "team"
	m.SenderName = "+" + phone
	return m
}

func TestHeldMessagesWaitForATrigger(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	const me, friend = "447700900123", "15550001111"
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	e.st.AddOwnerNumber(ctx, e.user.ID, me)

	code, _, _ := e.m.LinkCode(ctx, e.agent.ID, false)
	e.m.HandleMessage(ctx, e.inGroup(me, "/link "+code))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "mentioning @bot") {
		t.Fatalf("link reply does not explain the group default: %q", got.body)
	}
	route, _ := e.st.Route(ctx, e.acct.ID, "team@g.us")
	if route.OwnerTrigger != store.TriggerMentions || route.OthersTrigger != store.TriggerNone {
		t.Fatalf("group defaults = %s/%s", route.OwnerTrigger, route.OthersTrigger)
	}

	// Chatter between people is held. Nothing wakes, nothing is answered.
	e.m.HandleMessage(ctx, e.inGroup(friend, "lunch at noon?"))
	e.m.HandleMessage(ctx, e.inGroup(me, "sounds good"))
	e.m.HandleMessage(ctx, e.inGroup(friend, "@bot are you there?")) // others: none, so held too
	time.Sleep(300 * time.Millisecond)
	if n := e.st.HeldCount(ctx, e.acct.ID, "team@g.us"); n != 3 {
		t.Fatalf("held %d messages, want 3", n)
	}
	if st := e.m.Status(e.agent.ID); st.State == sandbox.Running || st.Busy {
		t.Fatalf("a held message woke the agent: %+v", st)
	}
	if turns, _ := e.st.Turns(ctx, e.agent.ID, 10); len(turns) != 0 {
		t.Fatalf("held messages started %d turns", len(turns))
	}

	// Mentioning the agent starts a turn that carries the held messages.
	e.m.HandleMessage(ctx, e.inGroup(me, "@bot what did they say?"))
	got := e.rec.wait(t, "text")
	for _, want := range []string{"Earlier messages", "lunch at noon?", "sounds good", "what did they say?"} {
		if !strings.Contains(got.body, want) {
			t.Fatalf("reply lacks %q:\n%s", want, got.body)
		}
	}
	if n := e.st.HeldCount(ctx, e.acct.ID, "team@g.us"); n != 0 {
		t.Fatalf("%d messages still held after delivery", n)
	}
	e.idle(t)

	// Replying to the agent counts as a mention.
	reply := e.inGroup(me, "and tomorrow?")
	reply.Mentioned = true
	e.m.HandleMessage(ctx, reply)
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "and tomorrow?") {
		t.Fatalf("reply-to-agent did not trigger: %q", got.body)
	}
	e.idle(t)

	// /listen opens the chat to everyone.
	e.m.HandleMessage(ctx, e.inGroup(me, "/listen others all"))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "every message from other people") {
		t.Fatalf("/listen reply = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.inGroup(friend, "hello bot"))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "[+"+friend+"] hello bot") {
		t.Fatalf("friend's message was not labelled and answered: %q", got.body)
	}
}

func TestEachChatGetsItsOwnTurn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	e.link(t, "one@s.whatsapp.net")
	e.link(t, "two@s.whatsapp.net")
	e.m.HandleMessage(ctx, e.msg("one@s.whatsapp.net", "first chat", true))
	e.m.HandleMessage(ctx, e.msg("two@s.whatsapp.net", "second chat", true))
	replies := map[string]string{}
	for len(replies) < 2 {
		got := e.rec.wait(t, "text")
		replies[got.chat] = got.body
	}
	if !strings.Contains(replies["one@s.whatsapp.net"], "first chat") || strings.Contains(replies["one@s.whatsapp.net"], "second") ||
		!strings.Contains(replies["two@s.whatsapp.net"], "second chat") {
		t.Fatalf("replies crossed chats: %+v", replies)
	}
}

func TestMentionsName(t *testing.T) {
	for text, want := range map[string]bool{
		"@bot hi":         true,
		"hey @Bot, look":  true,
		"@bottle of wine": false,
		"email bot@x.com": false,
		"no mention":      false,
		"@bot":            true,
	} {
		if got := mentionsName(text, "bot"); got != want {
			t.Errorf("mentionsName(%q) = %v", text, got)
		}
	}
}

func TestUpgradeImage(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	e.link(t, "self@s.whatsapp.net")
	e.m.HandleMessage(ctx, e.msg("self@s.whatsapp.net", "hello", true))
	e.rec.wait(t, "text")
	e.idle(t)

	if _, err := e.m.UpgradeImage(ctx, e.agent.ID); !errors.Is(err, ErrUpToDate) {
		t.Fatalf("upgrade with nothing newer: %v", err)
	}
	// A newer image appears.
	images := e.m.cfg.Images.Dir
	os.MkdirAll(filepath.Join(images, "universal-test.1", "rootfs"), 0o755)
	os.Remove(filepath.Join(images, "universal"))
	os.Symlink("universal-test.1", filepath.Join(images, "universal"))

	ag, _ := e.st.Agent(ctx, e.agent.ID)
	if got := e.m.NewerImage(ag); got != "universal-test.1" {
		t.Fatalf("NewerImage = %q", got)
	}
	spec, _ := e.m.Spec(ag)
	os.WriteFile(filepath.Join(spec.Dir, "home", "keep.txt"), []byte("mine"), 0o644)
	got, err := e.m.UpgradeImage(ctx, e.agent.ID)
	if err != nil || got != "universal-test.1" {
		t.Fatalf("UpgradeImage = %q, %v", got, err)
	}
	ag, _ = e.st.Agent(ctx, e.agent.ID)
	if ag.Image != "universal-test.1" || ag.State != sandbox.Stopped {
		t.Fatalf("after upgrade: image %s, state %s", ag.Image, ag.State)
	}
	if b, err := os.ReadFile(filepath.Join(spec.Dir, "home", "keep.txt")); err != nil || string(b) != "mine" {
		t.Fatal("the agent's files did not survive the upgrade")
	}
	// It still works afterwards.
	e.m.HandleMessage(ctx, e.msg("self@s.whatsapp.net", "still there?", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "still there?") {
		t.Fatalf("reply after upgrade = %q", got.body)
	}
}

func TestAgentsMoveWhenTheSubnetChanges(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if e.agent.IP != "10.88.0.2" {
		t.Fatalf("agent starts at %s", e.agent.IP)
	}
	cfg := e.m.cfg
	cfg.Subnet = netip.MustParsePrefix("10.231.0.0/16")
	m2 := New(cfg, e.st, e.m.box, &sandbox.Local{}, e.m.log)
	if err := m2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ag, _ := e.st.Agent(ctx, e.agent.ID)
	if ag.IP != "10.231.0.2" {
		t.Fatalf("after the subnet changed the agent is at %s", ag.IP)
	}
}

func (e *env) lastArgs(t *testing.T) string {
	t.Helper()
	spec, _ := e.m.Spec(e.agent)
	b, _ := os.ReadFile(filepath.Join(spec.Dir, "home", "last-args"))
	return string(b)
}

func TestModelAndEffort(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	say := func(text string) string {
		e.m.HandleMessage(ctx, e.msg(chat, text, true))
		return e.rec.wait(t, "text").body
	}

	if got := say("/model"); !strings.Contains(got, "default model") || !strings.Contains(got, "opus") {
		t.Fatalf("/model = %q", got)
	}
	if got := say("/model Opus"); !strings.Contains(got, "uses opus") {
		t.Fatalf("/model opus = %q", got)
	}
	if got := say("/effort extreme"); !strings.Contains(got, "does not take") {
		t.Fatalf("/effort extreme = %q", got)
	}
	if got := say("/effort high"); !strings.Contains(got, "opus at high effort") {
		t.Fatalf("/effort high = %q", got)
	}
	if got := say("/model claude-nonsense-9"); !strings.Contains(got, "claude-nonsense-9") || strings.Contains(got, "do not recognise") {
		t.Fatalf("full model IDs should pass without a warning: %q", got)
	}
	if got := say("/model gpt-4"); !strings.Contains(got, "do not recognise") {
		t.Fatalf("unknown model got no warning: %q", got)
	}
	say("/model opus")

	say("hello")
	e.idle(t)
	if a := e.lastArgs(t); !strings.Contains(a, "--model\nopus") || !strings.Contains(a, "--effort\nhigh") {
		t.Fatalf("turn args:\n%s", a)
	}
	turns, _ := e.st.Turns(ctx, e.agent.ID, 1)
	if turns[0].Model != "fake" {
		t.Fatalf("turn did not record the model the harness reported: %q", turns[0].Model)
	}

	// /once applies to one turn only.
	if got := say("/once haiku max: quick one"); !strings.Contains(got, "quick one") {
		t.Fatalf("/once reply = %q", got)
	}
	e.idle(t)
	if a := e.lastArgs(t); !strings.Contains(a, "--model\nhaiku") || !strings.Contains(a, "--effort\nmax") {
		t.Fatalf("/once args:\n%s", a)
	}
	say("back to normal")
	e.idle(t)
	if a := e.lastArgs(t); !strings.Contains(a, "--model\nopus") || !strings.Contains(a, "--effort\nhigh") {
		t.Fatalf("args after /once:\n%s", a)
	}

	// Switching harness drops settings that mean nothing to pi.
	say("/switch pi")
	ag, _ := e.st.Agent(ctx, e.agent.ID)
	if ag.Model != "" || ag.Effort != "high" {
		t.Fatalf("after /switch pi: model %q effort %q", ag.Model, ag.Effort)
	}
}

func TestParseOnce(t *testing.T) {
	claude := &store.Agent{Harness: store.HarnessClaude}
	cases := []struct{ in, model, effort, text string }{
		{"opus max: review this design", "opus", "max", "review this design"},
		{"high: think hard", "", "high", "think hard"},
		{"Opus review this", "opus", "", "review this"},
		{"max opus what now", "opus", "max", "what now"},
		{"check this: the logs", "", "", "check this: the logs"},
		{"just a question", "", "", "just a question"},
		{"claude-opus-5-5 low: hi", "claude-opus-5-5", "low", "hi"},
	}
	for _, c := range cases {
		m, e, text := parseOnce(claude, c.in)
		if m != c.model || e != c.effort || text != c.text {
			t.Errorf("parseOnce(%q) = %q, %q, %q; want %q, %q, %q", c.in, m, e, text, c.model, c.effort, c.text)
		}
	}
	pi := &store.Agent{Harness: store.HarnessPi, PiProvider: ProviderDeepSeek}
	if m, e, text := parseOnce(pi, "deepseek-flash minimal: hi"); m != "deepseek-flash" || e != "minimal" || text != "hi" {
		t.Errorf("pi parseOnce = %q, %q, %q", m, e, text)
	}
}

func TestRetryAfterAFailure(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	say := func(text string) { e.m.HandleMessage(ctx, e.msg(chat, text, true)) }

	say("/retry")
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "nothing to retry") {
		t.Fatalf("/retry with no turns = %q", got.body)
	}
	say("flaky thing")
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "upstream overloaded") {
		t.Fatalf("failure = %q", got.body)
	}
	e.idle(t)
	say("/retry")
	got := e.rec.wait(t, "text")
	if !strings.Contains(got.body, "flaky thing") || !strings.Contains(got.body, "retry") {
		t.Fatalf("retried reply = %q", got.body)
	}
	e.idle(t)
	say("/retry")
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "went through") {
		t.Fatalf("/retry after success = %q", got.body)
	}
}

func TestSilentTurnIsAFailure(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	e.m.HandleMessage(ctx, e.msg(chat, "say nothing", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "without replying") || !strings.Contains(got.body, "/retry") {
		t.Fatalf("silent turn = %q", got.body)
	}
	if got := e.rec.wait(t, "react"); got.body != "❌" {
		t.Fatalf("silent turn reaction = %q", got.body)
	}
	e.idle(t)
	turns, _ := e.st.Turns(ctx, e.agent.ID, 1)
	if turns[0].Status != store.TurnError || turns[0].CostUSD == 0 {
		t.Fatalf("silent turn recorded as %s costing %v", turns[0].Status, turns[0].CostUSD)
	}
}

func TestRestartTellsTheChat(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	e.m.cfg.RetryNoticeDelay = 10 * time.Millisecond
	left := &store.Turn{AgentID: e.agent.ID, AccountID: e.acct.ID, ChatJID: chat, Harness: store.HarnessClaude, Prompt: "deploy the site"}
	e.st.CreateTurn(ctx, left)
	cut, _ := e.st.InterruptRunningTurns(ctx)
	go e.m.tellInterrupted(cut)
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "restarted") || !strings.Contains(got.body, "/retry") {
		t.Fatalf("restart notice = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/retry", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "deploy the site") || !strings.Contains(got.body, "restart") {
		t.Fatalf("retry of an interrupted turn = %q", got.body)
	}
}

func TestShellCommand(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	e.m.HandleMessage(ctx, e.msg(chat, "/sh echo hello; echo oops >&2; exit 3", true))
	got := e.rec.wait(t, "text")
	if !strings.Contains(got.body, "```hello\noops```") || !strings.Contains(got.body, "3") {
		t.Fatalf("/sh reply = %q", got.body)
	}
	if turns, _ := e.st.Turns(ctx, e.agent.ID, 1); len(turns) != 0 {
		t.Fatal("/sh ran a model turn")
	}
	// Someone else in the chat cannot run commands.
	e.m.HandleMessage(ctx, e.from("15550009999", "/sh id"))
	time.Sleep(300 * time.Millisecond)
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	for _, s := range e.rec.log {
		if strings.Contains(s.body, "uid=") {
			t.Fatal("a stranger ran /sh")
		}
	}
}

func TestScheduledTasks(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)

	// The agent schedules a task; hermitator tells the owner itself.
	e.m.HandleMessage(ctx, e.msg(chat, "please schedule it", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "📅 Scheduled by bot: CI check") {
		t.Fatalf("schedule notice = %q", got.body)
	}
	e.rec.wait(t, "text") // the agent's own reply
	e.idle(t)
	list, _ := e.st.Tasks(ctx, e.agent.ID)
	if len(list) != 1 || list[0].ChatJID != chat || list[0].CreatedBy != store.TaskByAgent || time.Until(list[0].NextRun) < 50*time.Minute {
		t.Fatalf("tasks = %+v", list)
	}
	task := list[0]

	// /tasks lists it; when it comes due, the reply is headed with its name.
	e.m.HandleMessage(ctx, e.msg(chat, "/tasks", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "1. CI check (once, next") {
		t.Fatalf("/tasks = %q", got.body)
	}
	e.st.SetTaskPaused(ctx, task.ID, false, time.Now().Add(-time.Second))
	e.m.runDueTasks(time.Now())
	if got := e.rec.wait(t, "text"); !strings.HasPrefix(got.body, "⏰ *CI check*") {
		t.Fatalf("task reply = %q", got.body)
	}
	e.idle(t)
	if after, _ := e.st.Task(ctx, task.ID); after.Runs != 1 || !after.NextRun.IsZero() {
		t.Fatalf("a one-off after running: runs %d, next %v", after.Runs, after.NextRun)
	}

	// The owner can set one up by asking, and pause and delete it by chat.
	cron := &store.Task{AgentID: e.agent.ID, AccountID: e.acct.ID, ChatJID: chat, Name: "daily", Prompt: "p", Cron: "0 9 * * *", NextRun: time.Now().Add(time.Hour), CreatedBy: store.TaskByOwner}
	e.st.CreateTask(ctx, cron)
	e.m.HandleMessage(ctx, e.msg(chat, "/tasks pause 1", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "Paused: daily") {
		t.Fatalf("/tasks pause = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/tasks delete 1", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "Deleted: daily") {
		t.Fatalf("/tasks delete = %q", got.body)
	}
	if list, _ := e.st.Tasks(ctx, e.agent.ID); len(list) != 0 {
		t.Fatalf("tasks left: %+v", list)
	}
}

func TestScheduleValidation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chat := chatRef{m: e.m, msg: Message{AccountID: e.acct.ID, Chat: "self@s.whatsapp.net"}}
	for _, req := range []turn.HostRequest{
		{Op: "schedule", Cron: "* * * * *", Prompt: "too often"},
		{Op: "schedule", Cron: "0 9 30 2 *", Prompt: "never"},
		{Op: "schedule", In: "-1h", Prompt: "past"},
		{Op: "schedule", In: "500d", Prompt: "bad duration"},
		{Op: "schedule", In: "1h"},
	} {
		if _, err := e.m.newTask(ctx, e.agent, chat, req, store.TaskByAgent); err == nil {
			t.Errorf("accepted %+v", req)
		}
	}
	if _, err := e.m.newTask(ctx, e.agent, chatRef{m: e.m}, turn.HostRequest{In: "1h", Prompt: "p"}, store.TaskByAgent); err == nil {
		t.Error("scheduled a task with no chat to run in")
	}
}

func TestSpendingCaps(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	say := func(text string) { e.m.HandleMessage(ctx, e.msg(chat, text, true)) }

	// Each fake turn costs a cent; 80% of 1.2 cents is crossed by the first.
	e.st.SetCaps(ctx, e.user.ID, 0.012, 0)
	say("first")
	e.rec.wait(t, "text") // the reply
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "of today's $0.01 spending limit") {
		t.Fatalf("80%% warning = %q", got.body)
	}
	e.idle(t)
	say("second")
	e.rec.wait(t, "text")
	e.idle(t)
	say("third")
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "spending limit is reached") {
		t.Fatalf("over the cap = %q", got.body)
	}
	e.idle(t)
	if turns, _ := e.st.Turns(ctx, e.agent.ID, 10); len(turns) != 2 {
		t.Fatalf("%d turns ran; the cap should have stopped the third", len(turns))
	}
}

func TestBackgroundMessages(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	e.m.HandleMessage(ctx, e.msg(chat, "run it in the background", true))
	e.rec.wait(t, "text") // the turn's own reply
	e.idle(t)
	if got := e.rec.wait(t, "text"); got.body != "background job done" || got.chat != chat {
		t.Fatalf("background message = %+v", got)
	}
}

func TestUsageAndPlan(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	say := func(text string) string {
		e.m.HandleMessage(ctx, e.msg(chat, text, true))
		return e.rec.wait(t, "text").body
	}
	if got := say("/plan"); !strings.Contains(got, "has not made a task list") {
		t.Fatalf("/plan before any = %q", got)
	}
	if got := say("plan it"); !strings.Contains(got, "5-hour allowance is 95% used") {
		t.Fatalf("90%% warning = %q", got)
	}
	e.rec.wait(t, "text") // the reply
	e.idle(t)
	if got := say("/usage"); !strings.Contains(got, "5-hour: 95% used") || !strings.Contains(got, "weekly: 40% used") || !strings.Contains(got, "Today: $0.01") {
		t.Fatalf("/usage = %q", got)
	}
	if got := say("/plan"); !strings.Contains(got, "✅ Fix the navbar") || !strings.Contains(got, "▶️ Run the tests") {
		t.Fatalf("/plan = %q", got)
	}
	// A second report over 90% does not warn again.
	say("plan it")
	e.idle(t)
	e.rec.mu.Lock()
	warnings := 0
	for _, s := range e.rec.log {
		if strings.Contains(s.body, "allowance is") {
			warnings++
		}
	}
	e.rec.mu.Unlock()
	if warnings != 1 {
		t.Fatalf("%d usage warnings, want 1", warnings)
	}
}

func TestSessionsResumeFork(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	say := func(text string) string {
		e.m.HandleMessage(ctx, e.msg(chat, text, true))
		return e.rec.wait(t, "text").body
	}
	say("first topic")
	e.idle(t)
	ag, _ := e.st.Agent(ctx, e.agent.ID)
	first := ag.SessionID
	say("/new")
	say("second topic")
	e.idle(t)
	if got := say("/sessions"); !strings.Contains(got, "1. (current) second topic") || !strings.Contains(got, "2. first topic") {
		t.Fatalf("/sessions = %q", got)
	}
	if got := say("/resume 2"); !strings.Contains(got, "Back to: first topic") {
		t.Fatalf("/resume = %q", got)
	}
	if ag, _ := e.st.Agent(ctx, e.agent.ID); ag.SessionID != first {
		t.Fatalf("session after /resume = %s, want %s", ag.SessionID, first)
	}
	if got := say("/fork"); !strings.Contains(got, "branches off") {
		t.Fatalf("/fork = %q", got)
	}
	say("a branch")
	e.idle(t)
	if a := e.lastArgs(t); !strings.Contains(a, "--resume\n"+first) || !strings.Contains(a, "--fork-session") {
		t.Fatalf("forked turn args:\n%s", a)
	}
	say("after the branch")
	e.idle(t)
	if a := e.lastArgs(t); strings.Contains(a, "--fork-session") {
		t.Fatal("forked again on the following turn")
	}
}

func TestSteeringARunningTurn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	e.m.HandleMessage(ctx, e.msg(chat, "steer me", true))
	deadline := time.Now().Add(5 * time.Second)
	for !e.m.Status(e.agent.ID).Busy && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	e.m.HandleMessage(ctx, e.msg(chat, "actually make it blue", true))
	if got := e.rec.wait(t, "text"); got.body != "*echo:* steered 1" && !strings.Contains(got.body, "steered 1") {
		t.Fatalf("reply = %q, want the steered message taken in", got.body)
	}
	e.idle(t)
	if turns, _ := e.st.Turns(ctx, e.agent.ID, 5); len(turns) != 1 {
		t.Fatalf("%d turns; the second message should have joined the first", len(turns))
	}
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	seen := false
	for _, s := range e.rec.log {
		seen = seen || (s.kind == "react" && s.body == "👀")
	}
	if !seen {
		t.Fatal("the steered message was not acknowledged with 👀")
	}
}

func TestSecretsReachTheAgentButNotTheChat(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	e.m.PutSecret(ctx, e.user.ID, store.SecretEnvPrefix+"STRIPE_KEY", "sk_live_abcdefgh12345")
	e.m.PutSecret(ctx, e.user.ID, store.SecretGitHubToken, "github_pat_0123456789abcdef")
	e.st.SetGitIdentity(ctx, e.user.ID, "Ada Lovelace", "ada@example.com")
	e.st.SetGitHubLogin(ctx, e.user.ID, "ada")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	e.m.HandleMessage(ctx, e.msg(chat, "leak it", true))
	got := e.rec.wait(t, "text").body
	if strings.Contains(got, "sk_live") || strings.Contains(got, "github_pat") {
		t.Fatalf("a secret reached the chat: %q", got)
	}
	if !strings.Contains(got, "key=[secret removed]") || !strings.Contains(got, "gh=[secret removed]") ||
		!strings.Contains(got, "author=Ada Lovelace") || !strings.Contains(got, "helper=credential.https://github.com.helper") {
		t.Fatalf("reply = %q", got)
	}
	e.idle(t)
	if turns, _ := e.st.Turns(ctx, e.agent.ID, 1); strings.Contains(turns[0].Output, "sk_live") {
		t.Fatal("a secret was stored in the turn's output")
	}
	if a := e.lastArgs(t); !strings.Contains(a, "secrets as environment variables: STRIPE_KEY") || !strings.Contains(a, "GitHub as @ada") ||
		!strings.Contains(a, "authored as Ada Lovelace <ada@example.com>") || strings.Contains(a, "sk_live") {
		t.Fatalf("system prompt does not describe the credentials right:\n%s", a)
	}
}

func TestRedactor(t *testing.T) {
	r := newRedactor([]string{"short", "abcdefgh", "abcdefgh-longer-secret"})
	if got := r.apply("x abcdefgh-longer-secret y abcdefgh z short"); got != "x [secret removed] y [secret removed] z short" {
		t.Fatalf("apply = %q", got)
	}
	if got := secretStrings(`{"openai-codex":{"access":"eyJhbGciOiJSUzI1NiJ9.payload.sig","type":"oauth","expires":123}}`); len(got) != 1 {
		t.Fatalf("secretStrings = %q", got)
	}
}

func TestStallWarning(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.cfg.StallAfter = 1500 * time.Millisecond
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	e.m.HandleMessage(ctx, e.msg(chat, "go quiet", true))
	got := e.rec.wait(t, "text")
	if !strings.Contains(got.body, "No sign of progress") || !strings.Contains(got.body, "Bash npm test") {
		t.Fatalf("stall warning = %q", got.body)
	}
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "echo") {
		t.Fatalf("after the warning, wanted the reply; got %q", got.body)
	}
}

func TestWorkAtRisk(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	spec, _ := e.m.Spec(e.agent)
	repo := filepath.Join(spec.Dir, "workspace", "shop")
	os.MkdirAll(repo, 0o755)
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a"), 0o644)
	git("add", "a.txt")
	git("commit", "-q", "-m", "one")
	os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b"), 0o644)
	got, err := e.m.WorkAtRisk(ctx, e.agent.ID)
	if err != nil || len(got) != 1 || got[0] != (RepoAtRisk{"/workspace/shop", 1, 1}) {
		t.Fatalf("WorkAtRisk = %+v, %v", got, err)
	}
}

func TestAgentAsksWithAPoll(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	e.m.HandleMessage(ctx, e.msg(chat, "ask me", true))
	if got := e.rec.wait(t, "poll"); got.chat != chat || got.body != "Deploy to production?|Yes|No|Staging first" {
		t.Fatalf("poll = %+v", got)
	}
	e.idle(t)
	bad := []turn.HostRequest{
		{Op: "poll", Question: "Q?", Options: []string{"only one"}},
		{Op: "poll", Question: "Q?", Options: []string{"same", "same"}},
		{Op: "poll", Question: "", Options: []string{"a", "b"}},
	}
	for _, req := range bad {
		if a := e.m.poll(ctx, chatRef{m: e.m, msg: Message{AccountID: e.acct.ID, Chat: chat}}, req); a.OK {
			t.Errorf("sent a poll for %+v", req)
		}
	}
}

func TestPlaceholdersInsteadOfCredentials(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.cfg.EgressKey = []byte("egress test key")
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	e.m.PutSecret(ctx, e.user.ID, store.SecretEnvPrefix+"STRIPE_KEY", "sk_live_abcdefgh12345")
	e.st.SetSecretHosts(ctx, e.user.ID, "STRIPE_KEY", []string{"api.stripe.com"})
	e.m.PutSecret(ctx, e.user.ID, store.SecretGitHubToken, "github_pat_0123456789abcdef")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)

	// Off: real values, as before (and redacted from the reply).
	e.m.HandleMessage(ctx, e.msg(chat, "leak it", true))
	if got := e.rec.wait(t, "text").body; strings.Contains(got, "hermitator-") {
		t.Fatalf("placeholders with the proxy off: %q", got)
	}
	e.idle(t)

	// On, but this sandbox booted before it trusted the proxy: still real.
	if _, err := e.m.SetEgress(ctx, e.user.ID, true); err != nil {
		t.Fatal(err)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "leak it", true))
	if got := e.rec.wait(t, "text").body; strings.Contains(got, "hermitator-") {
		t.Fatalf("placeholders in a sandbox that does not trust the proxy: %q", got)
	}
	e.idle(t)

	// Once it trusts the proxy, the agent holds placeholders only.
	spec, _ := e.m.Spec(e.agent)
	os.WriteFile(filepath.Join(spec.Dir, "config.json"), []byte(`{"process":{"env":["NODE_EXTRA_CA_CERTS=/opt/hermitator/lib/egress-ca.crt"]}}`), 0o644)
	e.m.HandleMessage(ctx, e.msg(chat, "leak it", true))
	got := e.rec.wait(t, "text").body
	if !strings.Contains(got, "key=hermitator-") || !strings.Contains(got, "gh=gho_hermitator-") {
		t.Fatalf("reply = %q, want placeholders", got)
	}
	e.idle(t)

	// The proxy maps them back, each to its own hosts.
	ag, _ := e.st.Agent(ctx, e.agent.ID)
	secrets, ok := e.m.EgressSecrets(ctx, netip.MustParseAddr(ag.IP))
	if !ok {
		t.Fatal("the proxy does not know the agent")
	}
	want := map[string]string{"STRIPE_KEY": "sk_live_abcdefgh12345", store.SecretGitHubToken: "github_pat_0123456789abcdef", store.SecretClaudeToken: "tok"}
	for _, s := range secrets {
		if want[s.Name] != s.Value || !strings.Contains(got, s.Placeholder) && s.Name != store.SecretClaudeToken {
			t.Errorf("secret %s: value %q, placeholder %q in reply: %v", s.Name, s.Value, s.Placeholder, strings.Contains(got, s.Placeholder))
		}
		delete(want, s.Name)
	}
	if len(want) != 0 {
		t.Fatalf("the proxy lacks %v", want)
	}
	if _, ok := e.m.EgressSecrets(ctx, netip.MustParseAddr("10.88.9.9")); ok {
		t.Fatal("the proxy served an address that is no agent's")
	}
}
