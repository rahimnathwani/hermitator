package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rahimnathwani/hermitator/internal/store"
)

type fakeHost struct {
	logs     string
	restarts atomic.Int32
	images   atomic.Int32
}

func (h *fakeHost) Version() string { return "v9.9.9" }
func (h *fakeHost) Facts(context.Context) []string {
	return []string{"hermitator v9.9.9, up 3m", "Disk: 1 GB free"}
}
func (h *fakeHost) Doctor(context.Context) (string, bool) {
	return "✅ runsc\n❌ overlayfs: missing", false
}
func (h *fakeHost) Logs(context.Context, int) (string, error) { return h.logs, nil }
func (h *fakeHost) Restart()                                  { h.restarts.Add(1) }
func (h *fakeHost) ImageStatus() string                       { return "Image: universal-test" }
func (h *fakeHost) UpdateImage(context.Context) (bool, string) {
	return h.images.Add(1) == 1, "a look is already running"
}

func TestHostCommandNeedsAnOperator(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	h := &fakeHost{}
	e.m.SetHost(h)
	const me = "447700900123"
	e.st.AddOwnerNumber(ctx, e.user.ID, me)

	// Without the permission, /host says who can grant it and runs nothing.
	e.m.HandleMessage(ctx, e.from(me, "/host restart yes"))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "Users page") {
		t.Fatalf("reply without permission = %q", got.body)
	}
	time.Sleep(1200 * time.Millisecond)
	if h.restarts.Load() != 0 {
		t.Fatal("a user who is not a host operator restarted the server")
	}

	// Strangers get nothing at all, operator or not.
	e.st.SetHostOperator(ctx, e.user.ID, true)
	e.m.HandleMessage(ctx, e.from("15550001111", "/host"))
	e.quiet(t, "15550001111@s.whatsapp.net")

	// It works in a direct chat linked to no agent.
	e.m.HandleMessage(ctx, e.from(me, "/host"))
	got := e.rec.wait(t, "text")
	for _, want := range []string{"*Host*", "Disk: 1 GB free", "Agents: 1, ", "Image: universal-test"} {
		if !strings.Contains(got.body, want) {
			t.Fatalf("/host = %q, missing %q", got.body, want)
		}
	}

	e.m.HandleMessage(ctx, e.from(me, "/host doctor"))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "Some checks failed") || !strings.Contains(got.body, "overlayfs") {
		t.Fatalf("/host doctor = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.from(me, "/host image"))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "Image: universal-test") || !strings.Contains(got.body, "/host image update") {
		t.Fatalf("/host image = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.from(me, "/host image update"))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "building a new image") {
		t.Fatalf("/host image update = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.from(me, "/host image update"))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "already running") {
		t.Fatalf("second /host image update = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.from(me, "/host frobnicate"))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "/host logs [N]") {
		t.Fatalf("/host with an unknown word = %q", got.body)
	}
	audit, _ := e.st.AuditLog(ctx, 10)
	var actions []string
	for _, a := range audit {
		actions = append(actions, a.Action)
	}
	if !strings.Contains(strings.Join(actions, " "), "host.status") || !strings.Contains(strings.Join(actions, " "), "host.doctor") {
		t.Fatalf("audit = %v", actions)
	}
}

func TestHostLogsAreRedacted(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.st.SetHostOperator(ctx, e.user.ID, true)
	e.m.PutSecret(ctx, e.user.ID, store.SecretEnvPrefix+"STRIPE_KEY", "rk_live_abcdefghijklmnop")
	h := &fakeHost{logs: "WARN task check failed err=rk_live_abcdefghijklmnop\nERROR clone token=ghp_ABCDEFGHIJKLMNOPQRSTUVWX"}
	e.m.SetHost(h)
	const me = "447700900123"
	e.st.AddOwnerNumber(ctx, e.user.ID, me)
	e.m.HandleMessage(ctx, e.from(me, "/host logs 5"))
	got := e.rec.wait(t, "text")
	if strings.Contains(got.body, "rk_live_") || strings.Contains(got.body, "ghp_") || strings.Count(got.body, "[secret removed]") != 2 {
		t.Fatalf("/host logs = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.from(me, "/host logs lots"))
	if got := e.rec.wait(t, "text"); !strings.HasPrefix(got.body, "Usage") {
		t.Fatalf("/host logs lots = %q", got.body)
	}
}

func TestHostRestartAsksFirstAndSaysWhenBack(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.st.SetHostOperator(ctx, e.user.ID, true)
	h := &fakeHost{}
	e.m.SetHost(h)
	e.m.cfg.StateDir = t.TempDir()
	const me = "447700900123"
	e.st.AddOwnerNumber(ctx, e.user.ID, me)
	chat := me + "@s.whatsapp.net"

	e.m.HandleMessage(ctx, e.from(me, "/host restart"))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "/host restart yes") {
		t.Fatalf("/host restart = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.from(me, "/host restart yes"))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "Restarting") {
		t.Fatalf("/host restart yes = %q", got.body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.restarts.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("never restarted")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The next process finds the notice and tells the same chat.
	path := filepath.Join(e.m.cfg.StateDir, restartNoticeFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("no restart notice written")
	}
	e.m.tellRestarted()
	if got := e.rec.wait(t, "text"); got.chat != chat || !strings.Contains(got.body, "back, running v9.9.9") {
		t.Fatalf("after restart = %+v", got)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("restart notice left behind, to be announced again")
	}
}

func TestNotifyOperators(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	other := &store.User{Email: "b@example.com", PasswordHash: "x", Role: store.RoleUser}
	e.st.CreateUser(ctx, other)
	e.st.SetHostOperator(ctx, e.user.ID, true)
	e.st.AddOwnerNumber(ctx, e.user.ID, "447700900123")

	e.m.NotifyOperators(ctx, "new image")
	got := e.rec.wait(t, "text")
	if got.chat != "447700900123@s.whatsapp.net" || got.body != "new image" {
		t.Fatalf("notice = %+v", got)
	}
}

func TestHostStaysOutOfOtherPeoplesChats(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.SetHost(&fakeHost{})
	e.st.SetHostOperator(ctx, e.user.ID, true)
	// The owner's own number, linked like WhatsApp Web.
	own := &store.Account{UserID: e.user.ID, JID: "447700900123:3@s.whatsapp.net", Phone: "447700900123", Status: store.AccountConnected, Kind: store.KindPersonal}
	if err := e.st.CreateAccount(ctx, own); err != nil {
		t.Fatal(err)
	}
	mine := func(chat string) Message {
		m := e.msg(chat, "/host", true)
		m.AccountID = own.ID
		return m
	}

	// Typed in a chat with a friend, or a group: no answer there.
	e.m.HandleMessage(ctx, mine("15550001111@s.whatsapp.net"))
	group := mine("team@g.us")
	group.IsGroup = true
	e.m.HandleMessage(ctx, group)
	// In the chat with themselves: an answer.
	e.m.HandleMessage(ctx, mine("447700900123@s.whatsapp.net"))
	if got := e.rec.wait(t, "text"); got.chat != "447700900123@s.whatsapp.net" || !strings.Contains(got.body, "*Host*") {
		t.Fatalf("self chat = %+v", got)
	}
	e.quiet(t, "15550001111@s.whatsapp.net")
	e.quiet(t, "team@g.us")
}
