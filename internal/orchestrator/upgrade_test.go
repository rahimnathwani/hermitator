package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
