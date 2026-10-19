package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	RateLimit struct {
		Windows map[string]struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    int64   `json:"resetsAt"`
		} `json:"unifiedWindows"`
	} `json:"rate_limit_info"`
	// CompactResult and CompactMetadata come with /compact.
	CompactResult   string `json:"compact_result"`
	CompactMetadata struct {
		PreTokens  int `json:"pre_tokens"`
		PostTokens int `json:"post_tokens"`
	} `json:"compact_metadata"`
	Message struct {
		Usage struct {
			Input       int `json:"input_tokens"`
			CacheRead   int `json:"cache_read_input_tokens"`
			CacheCreate int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
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
	if req.Compact {
		return compactClaude(ctx, req, workdir, emit)
	}
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
	if req.Discard && !claudeSessionExists(req.SessionID) {
		// A throwaway look at a conversation that is not there would start
		// a real one.
		emit(turn.Event{Type: turn.EventError, Text: "no conversation to look back at"})
		return nil
	}
	if req.SessionID != "" {
		if claudeSessionExists(req.SessionID) {
			args = append(args, "--resume", req.SessionID)
			if req.Fork {
				args = append(args, "--fork-session")
			}
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
	if req.MaxCostUSD > 0 {
		// Claude Code checks its budget against the session's running
		// total, which a resumed session starts from.
		budget := req.MaxCostUSD
		if slices.Contains(args, "--resume") {
			budget += req.SessionCostUSD
		}
		args = append(args, "--max-budget-usd", strconv.FormatFloat(budget, 'f', 4, 64))
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
	// With steering, stdin stays open for messages sent during the turn,
	// until the first result; Claude Code folds them into its work.
	stopSteering := func() {}
	if req.SteerID != "" {
		stopSteering = steer(filepath.Join(Getenv("HERMITATOR_SPOOL", turn.SpoolDir), "steer", req.SteerID), stdin, emit)
	} else {
		stdin.Close()
	}
	steered := sync.OnceFunc(func() { stopSteering(); stdin.Close() })
	defer steered()

	var result *turn.Event
	sessionID := req.SessionID
	contextTokens := 0
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
			if u := ev.Message.Usage; u.Input+u.CacheRead+u.CacheCreate > 0 {
				contextTokens = u.Input + u.CacheRead + u.CacheCreate
			}
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					if c.Text != "" {
						emit(turn.Event{Type: turn.EventText, Text: c.Text})
					}
				case "tool_use":
					emit(turn.Event{Type: turn.EventTool, Tool: c.Name, Detail: summarize(c.Name, c.Input)})
					if c.Name == "TodoWrite" {
						if todos := todosOf(c.Input); todos != nil {
							emit(turn.Event{Type: turn.EventTodos, Todos: todos})
						}
					}
				}
			}
		case "rate_limit_event":
			var windows []turn.UsageWindow
			for name, w := range ev.RateLimit.Windows {
				windows = append(windows, turn.UsageWindow{Name: name, Utilization: w.Utilization, ResetsAt: w.ResetsAt})
			}
			if len(windows) > 0 {
				sort.Slice(windows, func(i, j int) bool { return windows[i].Name < windows[j].Name })
				emit(turn.Event{Type: turn.EventUsage, Windows: windows})
			}
		case "result":
			if ev.SessionID != "" {
				sessionID = ev.SessionID
			}
			// No more steering; anything already taken in still gets an
			// answer, so an earlier result goes out on its own.
			steered()
			if result != nil && strings.TrimSpace(result.Text) != "" {
				emit(turn.Event{Type: turn.EventReply, Text: result.Text})
			}
			text := ev.Result
			if text == "" && ev.IsError {
				text = "Claude Code stopped: " + ev.Subtype
			}
			result = &turn.Event{Type: turn.EventResult, Text: text, SessionID: sessionID, CostUSD: ev.CostUSD, IsError: ev.IsError, ContextTokens: contextTokens}
			if ev.Subtype == "error_max_budget_usd" {
				result.StopReason = turn.StopBudget
			}
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

// todosOf reads TodoWrite's input: {"todos": [{"content", "status"}...]}.
func todosOf(input map[string]any) []turn.Todo {
	items, ok := input["todos"].([]any)
	if !ok {
		return nil
	}
	todos := []turn.Todo{}
	for _, it := range items {
		m, _ := it.(map[string]any)
		content, _ := m["content"].(string)
		status, _ := m["status"].(string)
		if content != "" {
			todos = append(todos, turn.Todo{Content: content, Status: status})
		}
	}
	return todos
}

// steer feeds messages from dir into a running Claude Code as user
// messages, in order, until the returned function stops it.
func steer(dir string, stdin io.Writer, emit Emit) (stop func()) {
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
			sort.Strings(names)
			for _, name := range names {
				select {
				case <-done:
					return
				default:
				}
				data, err := os.ReadFile(name)
				os.Remove(name)
				var s turn.Steer
				if err != nil || json.Unmarshal(data, &s) != nil || strings.TrimSpace(s.Text) == "" {
					continue
				}
				line, _ := json.Marshal(map[string]any{
					"type":    "user",
					"message": map[string]any{"role": "user", "content": []claudeContent{{Type: "text", Text: s.Text}}},
				})
				if _, err := stdin.Write(append(line, '\n')); err != nil {
					return
				}
				emit(turn.Event{Type: turn.EventSteered, Text: s.Text})
			}
		}
	}()
	return func() { close(done); <-stopped }
}

// compactClaude runs /compact on a session. Claude Code takes slash
// commands as the prompt of a print-mode run, not over stream-json input,
// and adds what follows /compact to its summarising instructions.
func compactClaude(ctx context.Context, req *turn.Request, workdir string, emit Emit) error {
	if req.SessionID == "" || !claudeSessionExists(req.SessionID) {
		emit(turn.Event{Type: turn.EventError, Text: "no conversation to compact"})
		return nil
	}
	command := strings.TrimSpace("/compact " + req.Prompt)
	args := []string{"-p", command, "--resume", req.SessionID, "--output-format", "stream-json", "--verbose"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	env := map[string]string{"IS_SANDBOX": "1", "DISABLE_AUTOUPDATER": "1"}
	for k, v := range req.Env {
		env[k] = v
	}
	var drop []string
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != "" {
		drop = append(drop, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")
	}
	cmd := exec.CommandContext(ctx, Getenv("HERMITATOR_CLAUDE_BIN", "claude"), args...)
	cmd.Dir = workdir
	cmd.Env = environ(env, drop...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	stderr := &tail{n: 4096}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claude: %w", err)
	}
	var outcome string
	var before, after int
	var cost float64
	readLines(stdout, func(line []byte) {
		var ev claudeEvent
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		switch {
		case ev.Type == "system" && ev.CompactResult != "":
			outcome = ev.CompactResult
		case ev.Type == "system" && ev.Subtype == "compact_boundary":
			before, after = ev.CompactMetadata.PreTokens, ev.CompactMetadata.PostTokens
		case ev.Type == "result":
			cost = ev.CostUSD
		}
	})
	cmd.Wait()
	if outcome != "success" {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = "Claude Code did not compact the conversation"
		}
		emit(turn.Event{Type: turn.EventError, Text: msg, SessionID: req.SessionID, CostUSD: cost})
		return nil
	}
	// post_tokens counts the summary, not the system prompt and tools
	// every call also reads, so the size is unknown until the next turn.
	emit(turn.Event{Type: turn.EventResult, Text: fmt.Sprintf("compacted %d tokens to a %d-token summary", before, after),
		SessionID: req.SessionID, CostUSD: cost})
	return nil
}
