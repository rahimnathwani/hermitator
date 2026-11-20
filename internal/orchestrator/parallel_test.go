package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rahimnathwani/hermitator/internal/store"
)

// working waits until the agent runs n turns at once.
func (e *env) working(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); len(e.m.Status(e.agent.ID).Turns) < n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never ran %d turns at once", n)
		}
	}
}

// textIn returns the next text sent to chat.
func (e *env) textIn(t *testing.T, chat string) sent {
	t.Helper()
	for {
		if got := e.rec.wait(t, "text"); got.chat == chat {
			return got
		}
	}
}

func (e *env) key(chat string) store.ThreadKey {
	return store.ThreadKey{AgentID: e.agent.ID, AccountID: e.acct.ID, ChatJID: chat}
}

// A long turn in one chat no longer holds up another chat: each runs in
// its own conversation, and the later one is told the other is going on.
func TestChatsRunAtTheSameTime(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	one, two := "one@s.whatsapp.net", "two@s.whatsapp.net"
	e.link(t, one)
	e.link(t, two)
	e.m.HandleMessage(ctx, e.msg(one, "go quiet", true))
	e.working(t, 1)
	e.m.HandleMessage(ctx, e.msg(two, "quick one", true))
	for {
		got := e.rec.wait(t, "text")
		if got.chat == one {
			t.Fatalf("the second chat waited for the first: %+v", got)
		}
		if got.chat == two {
			if !strings.Contains(got.body, "quick one") {
				t.Fatalf("reply in the second chat = %q", got.body)
			}
			break
		}
	}
	if _, ok := e.m.Status(e.agent.ID).In(e.key(one)); !ok {
		t.Fatal("the first chat's turn should still be running")
	}
	e.idle(t)

	e.rec.mu.Lock()
	for _, s := range e.rec.log {
		if s.kind == "react" && s.chat == two && s.body == queuedReaction {
			t.Error("the second chat's message was marked as waiting")
		}
	}
	e.rec.mu.Unlock()

	spec, _ := e.m.Spec(e.agent)
	prompts, _ := os.ReadFile(filepath.Join(spec.Dir, "home", "prompts"))
	for _, line := range strings.Split(string(prompts), "\n") {
		told := strings.Contains(line, "you are also working on something else in another of your conversations")
		switch {
		case strings.Contains(line, "go quiet") && told:
			t.Errorf("the first turn was told of another: %s", line)
		case strings.Contains(line, "quick one") && !told:
			t.Errorf("the second turn was not told of the first: %s", line)
		}
	}
	turns, _ := e.st.Turns(ctx, e.agent.ID, 5)
	for _, tr := range turns {
		if strings.Contains(tr.Prompt, "From hermitator") {
			t.Errorf("the note was kept with the turn, for /retry: %q", tr.Prompt)
		}
	}
}

// One conversation still runs one turn at a time, whichever chat it is
// continued from: two harness processes on a session would corrupt it.
func TestOneTurnPerConversation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	one := "one@s.whatsapp.net"
	e.link(t, one)
	e.m.HandleMessage(ctx, e.msg(one, "go quiet", true))
	e.working(t, 1)
	e.m.Prompt(e.key(one), "from the console")
	time.Sleep(300 * time.Millisecond) // well past the debounce
	if st := e.m.Status(e.agent.ID); len(st.Turns) != 1 || st.Queued != 1 {
		t.Fatalf("%d turns running and %d queued; the console's should wait", len(st.Turns), st.Queued)
	}
	e.idle(t)
	if turns, _ := e.st.Turns(ctx, e.agent.ID, 5); len(turns) != 2 {
		t.Fatalf("%d turns; the console's should have run after the first", len(turns))
	}
}

// /stop in a chat stops that chat's turn and leaves another chat's;
// /status says what is running where.
func TestStopOnlyThisChat(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	one, two := "one@s.whatsapp.net", "two@s.whatsapp.net"
	e.link(t, one)
	e.link(t, two)
	e.m.HandleMessage(ctx, e.msg(one, "go quiet", true))
	e.m.HandleMessage(ctx, e.msg(two, "go quiet", true))
	e.working(t, 2)

	e.m.HandleMessage(ctx, e.msg(two, "/status", true))
	if got := e.textIn(t, two); !strings.Contains(got.body, "State: working for") || !strings.Contains(got.body, ", and in 1 other chat") {
		t.Fatalf("/status = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.msg(two, "/stop", true))
	if got := e.textIn(t, two); got.body != "⏹ Stopping the current turn." {
		t.Fatalf("/stop = %q", got.body)
	}
	for deadline := time.Now().Add(10 * time.Second); len(e.m.Status(e.agent.ID).Turns) != 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the stopped turn never ended")
		}
	}
	if _, ok := e.m.Status(e.agent.ID).In(e.key(one)); !ok {
		t.Fatal("/stop in one chat stopped the other's turn")
	}
	e.m.HandleMessage(ctx, e.msg(two, "/status", true))
	if got := e.textIn(t, two); !strings.Contains(got.body, "State: working in 1 other chat, not this one") {
		t.Fatalf("/status with nothing here = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.msg(two, "/stop", true))
	if got := e.textIn(t, two); got.body != "Nothing is running in this chat. *bot* is working in 1 other chat; /stop all stops that too." {
		t.Fatalf("/stop with nothing here = %q", got.body)
	}
	if got := e.textIn(t, one); !strings.Contains(got.body, "go quiet") {
		t.Fatalf("the other chat's reply = %q", got.body)
	}
	e.idle(t)
}

// An owner with a spending limit gets one turn at a time, since each
// turn is given all that is left under it.
func TestSpendingLimitRunsOneAtATime(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	e.st.SetCaps(ctx, e.user.ID, 10, 0)
	one, two := "one@s.whatsapp.net", "two@s.whatsapp.net"
	e.link(t, one)
	e.link(t, two)
	e.m.HandleMessage(ctx, e.msg(one, "go quiet", true))
	e.working(t, 1)
	e.m.HandleMessage(ctx, e.msg(two, "me next", true))
	for {
		if got := e.rec.wait(t, "react"); got.chat == two {
			if got.body != queuedReaction {
				t.Fatalf("first reaction in the second chat = %q, want it to wait", got.body)
			}
			break
		}
	}
	e.idle(t)
}

// What a turn's agent sends, and asks of the host, is handled by that
// turn, for its own chat: another turn running beside it leaves it
// alone. It matters most for the web console, which has no chat to route
// by, so another turn would send its files to the wrong chat.
func TestOutboxStaysWithItsTurn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	spec, _ := e.m.Spec(e.agent)
	shared := spec.SharedDir()
	os.MkdirAll(filepath.Join(shared, "outbox"), 0o755)
	os.MkdirAll(filepath.Join(shared, "requests"), 0o755)
	os.WriteFile(filepath.Join(shared, "outbox", "1.meta.json"), []byte(`{"text":"from the console's turn","turn":"console-turn"}`), 0o644)
	os.WriteFile(filepath.Join(shared, "requests", "1.json"), []byte(`{"op":"poll","turn":"console-turn","question":"Deploy?","options":["Yes","No"]}`), 0o644)

	a := e.m.actor(e.agent.ID)
	a.call(func(a *actor) error {
		a.hold(&activeTurn{id: "console-turn", key: store.ThreadKey{AgentID: e.agent.ID}})
		return nil
	})
	chat := chatRef{m: e.m, msg: e.msg("one@s.whatsapp.net", "", true)}
	skip := a.othersLive("chat-turn")
	if n := e.m.deliverOutbox(ctx, spec, chat, skip); n != 0 {
		t.Fatalf("delivered %d of another running turn's messages", n)
	}
	e.m.handleRequests(ctx, spec, e.agent, chat, skip)
	if _, err := os.Stat(filepath.Join(shared, "requests", "1.json")); err != nil {
		t.Fatalf("answered another running turn's request: %v", err)
	}
	// The turn itself handles them.
	if n := e.m.deliverOutbox(ctx, spec, chat, a.othersLive("console-turn")); n != 1 {
		t.Fatalf("the turn's own message: %d delivered", n)
	}
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	for _, s := range e.rec.log {
		if s.kind == "poll" {
			t.Fatalf("sent another turn's poll: %+v", s)
		}
	}
}
