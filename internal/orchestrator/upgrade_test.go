package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rahimnathwani/hermitator/internal/sandbox"
)

func TestUpgradeFromTheChat(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chat := "self@s.whatsapp.net"
	e.link(t, chat)

	e.m.HandleMessage(ctx, e.msg(chat, "/upgrade", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "already runs the newest image") {
		t.Fatalf("/upgrade with nothing newer = %q", got.body)
	}

	// A new image arrives.
	images := e.m.cfg.Images.Dir
	os.MkdirAll(filepath.Join(images, "universal-test2", "rootfs"), 0o755)
	os.Remove(filepath.Join(images, "universal"))
	os.Symlink("universal-test2", filepath.Join(images, "universal"))

	e.m.HandleMessage(ctx, e.msg(chat, "/upgrade", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "/upgrade yes") || !strings.Contains(got.body, "universal-test to universal-test2") {
		t.Fatalf("/upgrade = %q", got.body)
	}
	if ag, _ := e.st.Agent(ctx, e.agent.ID); ag.Image != "universal-test" {
		t.Fatal("upgraded without being told yes")
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/upgrade all", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "Upgrading bot stops its sandbox") {
		t.Fatalf("/upgrade all = %q", got.body)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/upgrade yes", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "now runs universal-test2") {
		t.Fatalf("/upgrade yes = %q", got.body)
	}
	if ag, _ := e.st.Agent(ctx, e.agent.ID); ag.Image != "universal-test2" {
		t.Fatalf("image after upgrade = %s", ag.Image)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/upgrade all yes", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "All your agents run the newest image") {
		t.Fatalf("/upgrade all yes when current = %q", got.body)
	}
}

// A checkpointed sandbox keeps the guest tools it booted with, so the
// owner needs a way to make it boot cold.
func TestRestartFromTheChat(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chat := "self@s.whatsapp.net"
	e.link(t, chat)
	if err := e.m.Wake(ctx, e.agent.ID); err != nil {
		t.Fatal(err)
	}
	spec, _ := e.m.Spec(e.agent)

	e.m.HandleMessage(ctx, e.msg(chat, "/restart", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "/restart yes") || !strings.Contains(got.body, "file is kept") {
		t.Fatalf("/restart = %q", got.body)
	}
	if st, _ := e.m.rt.State(ctx, spec); st != sandbox.Running {
		t.Fatalf("restarted without being told yes: %s", st)
	}

	release, err := e.m.Hold(ctx, e.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.m.HandleMessage(ctx, e.msg(chat, "/reboot yes", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "busy") {
		t.Fatalf("/restart yes while held = %q", got.body)
	}
	release()

	e.m.HandleMessage(ctx, e.msg(chat, "/restart yes", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "boots fresh") {
		t.Fatalf("/restart yes = %q", got.body)
	}
	if ag, _ := e.st.Agent(ctx, e.agent.ID); ag.State != sandbox.Stopped {
		t.Fatalf("state after /restart yes = %s", ag.State)
	}
	if st, _ := e.m.rt.State(ctx, spec); st != sandbox.Stopped {
		t.Fatalf("runtime state after /restart yes = %s", st)
	}
}
