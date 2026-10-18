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
