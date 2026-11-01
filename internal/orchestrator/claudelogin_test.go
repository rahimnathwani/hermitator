package orchestrator

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rahimnathwani/hermitator/internal/store"
)

// fakeSetupToken stands in for `claude setup-token`: a sign-in link drawn
// with escape sequences, a prompt for the code, and a token for the right
// one.
const fakeSetupToken = `#!/bin/sh
if [ "$1" != setup-token ]; then exec "$HERMITATOR_REAL_CLAUDE" "$@"; fi
printf '\033[2mWelcome to Claude Code\033[0m\n\033[1CBrowser\033[1Cdidn'"'"'t\033[1Copen?\n\nhttps://claude.com/cai/oauth/authorize?code=true&state=abc\n\nPaste code here if prompted > '
read code
if [ "$code" = "good#abc" ]; then
	printf '\nYour OAuth token (valid for 1 year):\n\nsk-ant-oat01-abcdefghijklmnopqrstuvwxyz0123456789\n'
	sleep 1
	exit 0
fi
printf '\nOAuth error: Request failed with status code 400\nPress Enter to retry.\n'
sleep 30
`

func next(t *testing.T, ch <-chan LoginEvent) LoginEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("login ended early")
		}
		return ev
	case <-time.After(30 * time.Second):
		t.Fatal("no login event")
	}
	return LoginEvent{}
}

func TestClaudeLoginFromThePanel(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	e := setup(t)
	ctx := context.Background()
	// Like a sandbox that booted before the helper existed: it keeps that
	// bundle until a cold boot, so the login must not depend on it.
	if err := os.Remove(filepath.Join(e.m.cfg.Tools, "lib", "claude-login.py")); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(fake, []byte(fakeSetupToken), 0o755)
	t.Setenv("HERMITATOR_REAL_CLAUDE", os.Getenv("HERMITATOR_CLAUDE_BIN"))
	t.Setenv("HERMITATOR_CLAUDE_BIN", fake)
	e.st.MarkNotice(ctx, e.user.ID, ClaudeExpiryNotice+"-30d")

	lctx, cancel := context.WithCancel(ctx)
	ch, err := e.m.ClaudeLogin(lctx, e.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	ev := next(t, ch)
	if ev.Type != "url" || ev.Login == "" || ev.VerificationURI != "https://claude.com/cai/oauth/authorize?code=true&state=abc" {
		t.Fatalf("first event = %+v", ev)
	}
	// The panel's stream ends once it has the link, and a phone drops it
	// anyway while the user is on the sign-in page; the login waits on.
	cancel()
	time.Sleep(300 * time.Millisecond)

	// Only the user who started it can answer it.
	other := &store.User{Email: "b@example.com", PasswordHash: "x", Role: store.RoleUser}
	e.st.CreateUser(ctx, other)
	if err := e.m.ClaudeLoginCode(ctx, other.ID, ev.Login, "good#abc"); !errors.Is(err, ErrNoLogin) {
		t.Fatalf("someone else's code: %v", err)
	}
	if err := e.m.ClaudeLoginCode(ctx, e.user.ID, ev.Login, "good#abc\nmore"); !errors.Is(err, ErrCodeFormat) {
		t.Fatalf("a code with a newline in it: %v", err)
	}
	if err := e.m.ClaudeLoginCode(ctx, e.user.ID, ev.Login, " good#abc "); err != nil {
		t.Fatal(err)
	}
	if got := e.m.Secret(ctx, e.user.ID, store.SecretClaudeToken); got != "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz0123456789" {
		t.Fatalf("stored token = %q", got)
	}
	if e.st.NoticeSent(ctx, e.user.ID, ClaudeExpiryNotice+"-30d") {
		t.Fatal("the old token's expiry warning was kept")
	}
	if err := e.m.ClaudeLoginCode(ctx, e.user.ID, ev.Login, "good#abc"); !errors.Is(err, ErrNoLogin) {
		t.Fatalf("a code for a finished login: %v", err)
	}

	// A wrong code is reported without waiting for setup-token to give up.
	ch, err = e.m.ClaudeLogin(ctx, e.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	ev = next(t, ch)
	start := time.Now()
	if err := e.m.ClaudeLoginCode(ctx, e.user.ID, ev.Login, "bad#abc"); err == nil || !strings.Contains(err.Error(), "not accepted") || strings.Contains(err.Error(), "sk-ant") {
		t.Fatalf("wrong code: %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("a refused code took too long to report")
	}
}

func TestClaudeExpiryWarnings(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.st.AddOwnerNumber(ctx, e.user.ID, "447700900123")
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "sk-ant-oat01-x")
	saved := time.Now()
	chat := "447700900123@s.whatsapp.net"

	e.m.warnClaudeExpiry(ctx, saved.Add(200*24*time.Hour))
	e.m.warnClaudeExpiry(ctx, saved.Add(340*24*time.Hour))
	got := e.rec.wait(t, "text")
	if got.chat != chat || !strings.Contains(got.body, "in 25 days") || !strings.Contains(got.body, "/settings#claude") {
		t.Fatalf("month warning = %+v", got)
	}
	e.m.warnClaudeExpiry(ctx, saved.Add(341*24*time.Hour))
	e.m.warnClaudeExpiry(ctx, saved.Add(363*24*time.Hour))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "in 2 days") {
		t.Fatalf("second warning = %q", got.body)
	}
	e.m.warnClaudeExpiry(ctx, saved.Add(400*24*time.Hour))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "probably stopped working") {
		t.Fatalf("after expiry = %q", got.body)
	}
	e.m.warnClaudeExpiry(ctx, saved.Add(401*24*time.Hour))
	time.Sleep(100 * time.Millisecond)
	e.rec.mu.Lock()
	n := len(e.rec.log)
	e.rec.mu.Unlock()
	if n != 3 {
		t.Fatalf("%d warnings, want 3", n)
	}
}
