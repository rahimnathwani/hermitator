package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

// claudeEvent is the subset of `claude -p --output-format stream-json`
// output that hermitator reads.
type claudeEvent struct {
	Type      string  `json:"type"`
	Subtype   string  `json:"subtype"`
	SessionID string  `json:"session_id"`
	Model     string  `json:"model"`
	Result    string  `json:"result"`
	IsError   bool    `json:"is_error"`
	CostUSD   float64 `json:"total_cost_usd"`
	Message   struct {
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

type claudeContent struct {
	Type   string        `json:"type"`
	Text   string        `json:"text,omitempty"`
	Source *claudeSource `json:"source,omitempty"`
}

type claudeSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// claudeSessionExists reports whether Claude Code has a transcript for id.
// Claude keys transcripts by working directory, so look in all of them.
func claudeSessionExists(id string) bool {
	home, _ := os.UserHomeDir()
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", id+".jsonl"))
	return len(matches) > 0
}

func runClaude(ctx context.Context, req *turn.Request, workdir string, emit Emit) error {
	args := []string{"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		// The gVisor sandbox is the permission boundary.
		"--dangerously-skip-permissions",
		// Nobody is at a screen to answer these; the agent asks in its
		// reply instead, and the user answers in the chat.
		"--disallowedTools", "AskUserQuestion,EnterPlanMode,ExitPlanMode",
	}
	if req.SessionID != "" {
		if claudeSessionExists(req.SessionID) {
			args = append(args, "--resume", req.SessionID)
		} else {
			args = append(args, "--session-id", req.SessionID)
		}
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}

	var content []claudeContent
	for _, img := range req.Images {
		data, err := readImage(img)
		if err != nil {
			continue
		}
		content = append(content, claudeContent{Type: "image", Source: &claudeSource{Type: "base64", MediaType: img.MIME, Data: data}})
	}
	content = append(content, claudeContent{Type: "text", Text: req.Prompt})
	msg, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": content},
	})

	env := map[string]string{
		// Claude Code refuses to skip permissions as root unless it is
		// told it is inside a sandbox, which it is.
		"IS_SANDBOX":          "1",
		"DISABLE_AUTOUPDATER": "1",
	}
	for k, v := range req.Env {
		env[k] = v
	}
	var drop []string
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != "" {
		// An API key in the environment would silently take precedence
		// over the subscription token.
		drop = append(drop, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")
	}

	cmd := exec.CommandContext(ctx, Getenv("HERMITATOR_CLAUDE_BIN", "claude"), args...)
	cmd.Dir = workdir
	cmd.Env = environ(env, drop...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	stderr := &tail{n: 4096}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claude: %w", err)
	}
	stdin.Write(append(msg, '\n'))
	stdin.Close()

	var result *turn.Event
	sessionID := req.SessionID
	readLines(stdout, func(line []byte) {
		var ev claudeEvent
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" {
				if ev.SessionID != "" {
					sessionID = ev.SessionID
				}
				emit(turn.Event{Type: turn.EventStarted, SessionID: sessionID, Model: ev.Model})
			}
		case "assistant":
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					if c.Text != "" {
						emit(turn.Event{Type: turn.EventText, Text: c.Text})
					}
				case "tool_use":
					emit(turn.Event{Type: turn.EventTool, Tool: c.Name, Detail: summarize(c.Name, c.Input)})
				}
			}
		case "result":
			if ev.SessionID != "" {
				sessionID = ev.SessionID
			}
			text := ev.Result
			if text == "" && ev.IsError {
				text = "Claude Code stopped: " + ev.Subtype
			}
			result = &turn.Event{Type: turn.EventResult, Text: text, SessionID: sessionID, CostUSD: ev.CostUSD, IsError: ev.IsError}
		}
	})
	waitErr := cmd.Wait()
	if result == nil {
		msg := stderr.String()
		if msg == "" && waitErr != nil {
			msg = waitErr.Error()
		}
		if msg == "" {
			msg = "Claude Code exited without a result"
		}
		if ctx.Err() != nil {
			msg = "turn aborted"
		}
		emit(turn.Event{Type: turn.EventError, Text: msg, SessionID: sessionID})
		return nil
	}
	emit(*result)
	return nil
}
