package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// waitText returns the next text containing want.
func (e *env) waitText(t *testing.T, want string) sent {
	t.Helper()
	for {
		if got := e.rec.wait(t, "text"); strings.Contains(got.body, want) {
			return got
		}
	}
}

func (e *env) waitState(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ag, _ := e.st.Agent(context.Background(), e.agent.ID)
		if ag.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent is %s, want %s", ag.State, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgentOverItsDiskLimitIsStoppedAndItsTurnsWait(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chat := "self@s.whatsapp.net"
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	e.st.AddOwnerNumber(ctx, e.user.ID, "15550001111")
	e.link(t, chat)
	e.m.HandleMessage(ctx, e.msg(chat, "hello", true))
	e.waitText(t, "*echo:* hello")
	e.idle(t)
	e.waitState(t, sandbox.Running)

	// A runaway fills the agent's files past its 1 GiB limit; its owner
	// hears directly.
	e.used.Store(3 << 30)
	if got := e.waitText(t, "was stopped"); got.chat != "15550001111@s.whatsapp.net" {
		t.Fatalf("owner told in %s", got.chat)
	}
	e.waitState(t, sandbox.Stopped)

	// A message waits rather than waking it, and is not dropped.
	e.m.HandleMessage(ctx, e.msg(chat, "are you there", true))
	if got := e.waitText(t, "⏸"); !strings.Contains(got.body, "more than the 1024.0 MB") || !strings.Contains(got.body, "/sh") {
		t.Fatalf("wait notice = %q", got.body)
	}
	time.Sleep(300 * time.Millisecond)
	if ag, _ := e.st.Agent(ctx, e.agent.ID); ag.State != sandbox.Stopped {
		t.Fatalf("a waiting message woke the agent: %s", ag.State)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/status", true))
	if got := e.waitText(t, "*bot*"); !strings.Contains(got.body, "Turns wait") {
		t.Fatalf("status does not say turns wait: %q", got.body)
	}
	// A second message in the same chat is not told again.
	e.m.HandleMessage(ctx, e.msg(chat, "hello?", true))

	// /sh still wakes it, to clean up; staying over does not stop it again.
	e.m.HandleMessage(ctx, e.msg(chat, "/sh echo cleaning", true))
	e.waitText(t, "cleaning")
	time.Sleep(200 * time.Millisecond)
	if ag, _ := e.st.Agent(ctx, e.agent.ID); ag.State != sandbox.Running {
		t.Fatalf("agent stopped again while still over its limit, during clean-up: %s", ag.State)
	}

	// Back under: the waiting messages run, as one turn.
	e.used.Store(100 << 20)
	got := e.waitText(t, "*echo:*")
	if !strings.Contains(got.body, "are you there") || !strings.Contains(got.body, "hello?") {
		t.Fatalf("waiting messages ran as %q", got.body)
	}
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	n := 0
	for _, s := range e.rec.log {
		if s.chat == chat && strings.HasPrefix(s.body, "⏸") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the chat was told %d times that its messages wait", n)
	}
}

func TestLowDiskStopsAgentsAndTurnsWaitForRoom(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chat := "self@s.whatsapp.net"
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	e.link(t, chat)
	e.m.HandleMessage(ctx, e.msg(chat, "hello", true))
	e.waitText(t, "*echo:* hello")
	e.idle(t)

	// Under the 1 GiB reserve: running agents stop, without a checkpoint.
	e.free.Store(500 << 20)
	e.waitState(t, sandbox.Stopped)
	e.m.HandleMessage(ctx, e.msg(chat, "still there?", true))
	if got := e.waitText(t, "⏸"); !strings.Contains(got.body, "almost out of disk") {
		t.Fatalf("wait notice = %q", got.body)
	}

	// Just over the reserve is not enough room to start again.
	e.free.Store(1<<30 + 1<<20)
	time.Sleep(300 * time.Millisecond)
	if ag, _ := e.st.Agent(ctx, e.agent.ID); ag.State != sandbox.Stopped {
		t.Fatalf("turns started again at the reserve's edge: %s", ag.State)
	}

	e.free.Store(10 << 30)
	if got := e.waitText(t, "*echo:*"); !strings.Contains(got.body, "still there?") {
		t.Fatalf("waiting message ran as %q", got.body)
	}
}

func TestDiskReserveDefault(t *testing.T) {
	m := &Manager{}
	if got := m.diskReserve(100 << 30); got != 10<<30 {
		t.Fatalf("reserve on a 100 GiB disk = %d", got)
	}
	if got := m.diskReserve(4 << 40); got != 20<<30 {
		t.Fatalf("reserve on a 4 TiB disk = %d", got)
	}
	m.cfg.DiskReserve = -1
	if got := m.diskReserve(4 << 40); got != 0 {
		t.Fatalf("a negative reserve should turn it off, got %d", got)
	}
	if m.diskLimit() != defaultAgentDisk {
		t.Fatal("no default agent limit")
	}
}
