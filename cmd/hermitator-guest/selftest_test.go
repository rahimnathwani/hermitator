package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

// fakeBin writes an executable POSIX sh script and returns its path.
func fakeBin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSelftestClaudeTurn(t *testing.T) {
	// What Claude Code prints when its API cannot be reached.
	t.Setenv("HERMITATOR_CLAUDE_BIN", fakeBin(t, `cat >/dev/null
echo '{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet"}'
echo '{"type":"result","subtype":"success","is_error":true,"result":"API Error: Connection refused","session_id":"s1"}'
`))
	if err := checkClaudeTurn(context.Background()); err != nil {
		t.Fatalf("a Claude Code that starts and fails to connect should pass: %v", err)
	}

	// A release that drops one of the flags hermitator passes.
	t.Setenv("HERMITATOR_CLAUDE_BIN", fakeBin(t, `for a in "$@"; do
	if [ "$a" = --effort ]; then echo "error: unknown option '--effort'" >&2; exit 1; fi
done
echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo '{"type":"result","subtype":"success","result":"ok","session_id":"s1"}'
`))
	err := checkClaudeTurn(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unknown option '--effort'") {
		t.Fatalf("a dropped flag should fail with Claude Code's own words, got %v", err)
	}

	// A release whose stream-json ends without a result line.
	t.Setenv("HERMITATOR_CLAUDE_BIN", fakeBin(t, `cat >/dev/null
echo '{"type":"system","subtype":"init","session_id":"s1"}'
`))
	if err := checkClaudeTurn(context.Background()); err == nil {
		t.Fatal("a turn with no result passed")
	}
}

func TestSelftestPiTurns(t *testing.T) {
	var seen string
	logFile := filepath.Join(t.TempDir(), "providers")
	t.Setenv("HERMITATOR_PI_BIN", fakeBin(t, `prev=
for a in "$@"; do
	[ "$prev" = --provider ] && echo "$a" >>`+logFile+`
	prev=$a
done
# pi answers as soon as it reads the prompt; its stdin stays open.
read -r line
echo '{"id":"turn","type":"response","command":"prompt","success":false,"error":"No API key found"}'
`))
	if err := checkPiTurns(context.Background()); err != nil {
		t.Fatalf("pi refusing a prompt for want of a key should pass: %v", err)
	}
	b, _ := os.ReadFile(logFile)
	seen = strings.TrimSpace(string(b))
	if seen != "deepseek\nopenai-codex\nopenrouter" {
		t.Fatalf("providers tried: %q", seen)
	}

	t.Setenv("HERMITATOR_PI_BIN", fakeBin(t, `echo "Error: Unknown option: --thinking" >&2; exit 1`))
	err := checkPiTurns(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Unknown option: --thinking") {
		t.Fatalf("a dropped flag should fail with pi's own words, got %v", err)
	}
}

func TestPiTurnOKNeedsARejectionOrAResult(t *testing.T) {
	if err := piTurnOK([]turn.Event{{Type: turn.EventStarted}, {Type: turn.EventError, Text: "pi exited before the turn finished"}}); err == nil {
		t.Fatal("pi exiting early passed")
	}
	if err := piTurnOK([]turn.Event{{Type: turn.EventResult, IsError: true}}); err != nil {
		t.Fatal(err)
	}
}

func TestSelftestClaudeLogin(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	tools := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tools, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../../internal/guestfiles/claude-login.py")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "lib", "claude-login.py"), script, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMITATOR_TOOLS", tools)

	// setup-token prints its link and then waits for a code.
	t.Setenv("HERMITATOR_CLAUDE_BIN", fakeBin(t, `echo "Browser didn't open? Use the url below to sign in"
echo "https://claude.com/cai/oauth/authorize?code=true&client_id=x&state=y"
sleep 30
`))
	if err := checkClaudeLogin(context.Background()); err != nil {
		t.Fatalf("a setup-token that shows its link should pass: %v", err)
	}

	// A release that words or shapes the link differently.
	t.Setenv("HERMITATOR_CLAUDE_BIN", fakeBin(t, `echo "Visit claude.com/login to continue"`))
	if err := checkClaudeLogin(context.Background()); err == nil {
		t.Fatal("a setup-token with no link passed")
	}
}
