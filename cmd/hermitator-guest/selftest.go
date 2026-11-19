package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/rahimnathwani/hermitator/internal/harness"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// selftest checks a newly built image before any agent uses it, with no
// network and no credentials: that Claude Code and pi still take the
// flags and speak the protocols internal/harness drives them with, and
// that `claude setup-token` still shows a sign-in link claude-login.py
// can find. Image updates move both harnesses every week or so, and a
// change to either would otherwise first show up as every agent's turns
// failing.
func selftest() int {
	failed := 0
	for _, c := range selfChecks {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := c.run(ctx)
		cancel()
		if err != nil {
			failed++
			fmt.Printf("FAIL %s: %v\n", c.name, err)
		} else {
			fmt.Printf("ok   %s\n", c.name)
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

var selfChecks = []struct {
	name string
	run  func(context.Context) error
}{
	{"Claude Code turn", checkClaudeTurn},
	{"pi turn", checkPiTurns},
	{"Claude sign-in link", checkClaudeLogin},
}

// selfTurn runs one turn through the harness in a throwaway home and
// returns its events.
func selfTurn(ctx context.Context, req turn.Request) ([]turn.Event, error) {
	dir, err := os.MkdirTemp("", "hermitator-selftest-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	req.Prompt = "Reply with the word ok."
	req.SessionID = selftestID()
	req.Workdir = dir
	req.SystemPrompt = "This is hermitator's image check."
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	req.Env["HOME"] = dir
	var events []turn.Event
	err = harness.Run(ctx, &req, func(e turn.Event) { events = append(events, e) })
	return events, err
}

func checkClaudeTurn(ctx context.Context) error {
	events, err := selfTurn(ctx, turn.Request{
		Harness: turn.Claude,
		// Every flag runClaude can pass, so a renamed one fails here.
		Model:      "sonnet",
		Effort:     "low",
		MaxCostUSD: 0.01,
		Env: map[string]string{
			// Nothing listens there, so the turn ends in an API error
			// after Claude Code has started and reported in stream-json,
			// which is what is being checked.
			"ANTHROPIC_API_KEY":       "sk-ant-api03-hermitator-image-check",
			"ANTHROPIC_BASE_URL":      "http://127.0.0.1:9",
			"CLAUDE_CODE_MAX_RETRIES": "0",
		},
	})
	if err != nil {
		return err
	}
	return claudeTurnOK(events)
}

func claudeTurnOK(events []turn.Event) error {
	started := slices.ContainsFunc(events, func(e turn.Event) bool {
		return e.Type == turn.EventStarted && e.SessionID != ""
	})
	ended := slices.ContainsFunc(events, func(e turn.Event) bool { return e.Type == turn.EventResult })
	switch {
	case started && ended:
		return nil
	case !started:
		return fmt.Errorf("no init event in its stream-json output: %s", lastError(events))
	default:
		return fmt.Errorf("started but sent no result: %s", lastError(events))
	}
}

// checkPiTurns runs a turn for each provider hermitator has a default
// model for, so a model pi no longer knows is caught too.
func checkPiTurns(ctx context.Context) error {
	providers := make([]string, 0, len(harness.DefaultModels))
	for p := range harness.DefaultModels {
		providers = append(providers, p)
	}
	slices.Sort(providers)
	for _, p := range providers {
		events, err := selfTurn(ctx, turn.Request{Harness: turn.Pi, Provider: p, Effort: "low"})
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if err := piTurnOK(events); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

func piTurnOK(events []turn.Event) error {
	for _, e := range events {
		// With no key pi answers the prompt command with an error, which
		// means it parsed its flags and its RPC input. If it somehow had
		// a key, the request fails for want of a network and still ends
		// in a result.
		if e.Type == turn.EventResult || e.Type == turn.EventError && strings.HasPrefix(e.Text, "pi rejected the prompt") {
			return nil
		}
	}
	return errors.New(lastError(events))
}

func lastError(events []turn.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == turn.EventError && events[i].Text != "" {
			return events[i].Text
		}
	}
	return "no error reported"
}

// claudeLoginScript is where the tools bundle has claude-login.py.
func claudeLoginScript() string {
	return filepath.Join(harness.Getenv("HERMITATOR_TOOLS", turn.ToolsDir), "lib", "claude-login.py")
}

func checkClaudeLogin(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "hermitator-selftest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// The code file never appears: the check stops at the link.
	cmd := exec.CommandContext(ctx, "python3", claudeLoginScript(), filepath.Join(dir, "code"))
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
	}()
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		var ev struct{ Type, URL, Message string }
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "url":
			if !strings.HasPrefix(ev.URL, "https://") {
				return fmt.Errorf("link %q is not https", ev.URL)
			}
			return nil
		case "error":
			return errors.New(ev.Message)
		}
	}
	if ctx.Err() != nil {
		return errors.New("no sign-in link in time")
	}
	return errors.New("claude-login.py ended without a sign-in link")
}

func selftestID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
