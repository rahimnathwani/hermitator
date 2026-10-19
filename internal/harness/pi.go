package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

// piEvent is the subset of `pi --mode rpc` output that hermitator reads.
type piEvent struct {
	Type     string         `json:"type"`
	ID       string         `json:"id"`
	Command  string         `json:"command"`
	Success  *bool          `json:"success"`
	Error    string         `json:"error"`
	Method   string         `json:"method"`
	ToolName string         `json:"toolName"`
	Args     map[string]any `json:"args"`
	Aborted  bool           `json:"aborted"`
	// Data is a command's answer; compact's has these.
	Data *struct {
		TokensBefore         int `json:"tokensBefore"`
		EstimatedTokensAfter int `json:"estimatedTokensAfter"`
		Usage                struct {
			Cost struct {
				Total float64 `json:"total"`
			} `json:"cost"`
		} `json:"usage"`
	} `json:"data"`
	Message *struct {
		Role         string `json:"role"`
		StopReason   string `json:"stopReason"`
		ErrorMessage string `json:"errorMessage"`
		Model        string `json:"model"`
		Content      []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			Input      int `json:"input"`
			CacheRead  int `json:"cacheRead"`
			CacheWrite int `json:"cacheWrite"`
			Cost       struct {
				Total float64 `json:"total"`
			} `json:"cost"`
		} `json:"usage"`
	} `json:"message"`
}

type piImage struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// Extension dialogs that block until the RPC client answers. Nobody is at
// a keyboard, so hermitator cancels them.
var piDialogs = map[string]bool{"select": true, "confirm": true, "input": true, "editor": true}

func runPi(ctx context.Context, req *turn.Request, workdir string, emit Emit) error {
	args := []string{"--mode", "rpc"}
	sessionID := req.SessionID
	switch {
	case req.Fork && req.SessionID != "":
		// pi copies the history into a new session with the ID it is given.
		sessionID = newSessionID()
		args = append(args, "--fork", req.SessionID, "--session-id", sessionID)
	case req.SessionID != "":
		args = append(args, "--session-id", req.SessionID)
	}
	model := req.Model
	if model == "" {
		model = DefaultModels[req.Provider]
	}
	if req.Provider != "" && model != "" {
		args = append(args, "--provider", req.Provider)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if req.Effort != "" {
		args = append(args, "--thinking", req.Effort)
	}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}

	var images []piImage
	for _, img := range req.Images {
		data, err := readImage(img)
		if err != nil {
			continue
		}
		images = append(images, piImage{Type: "image", Data: data, MimeType: img.MIME})
	}
	prompt, _ := json.Marshal(map[string]any{"id": "turn", "type": "prompt", "message": req.Prompt, "images": images})
	if req.Compact {
		compact := map[string]any{"id": "turn", "type": "compact"}
		if req.Prompt != "" {
			compact["customInstructions"] = req.Prompt
		}
		prompt, _ = json.Marshal(compact)
	}

	cmd := exec.CommandContext(ctx, Getenv("HERMITATOR_PI_BIN", "pi"), args...)
	cmd.Dir = workdir
	cmd.Env = environ(req.Env)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	stderr := &tail{n: 4096}
	cmd.Stderr = stderr
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start pi: %w", err)
	}
	stdin := &lockedWriter{w: stdinPipe}
	stdin.writeLine(prompt)

	// On abort, ask pi to stop cleanly before the context kills it.
	stopAbort := context.AfterFunc(ctx, func() {
		stdin.writeLine([]byte(`{"type":"abort"}`))
	})
	defer stopAbort()

	var (
		lastText  string
		lastError string
		cost      float64
		modelUsed string
		settled   bool
		failed    string
		context   int
		compacted string
		// overBudget is set once the turn has spent its MaxCostUSD.
		overBudget bool
	)
	emit(turn.Event{Type: turn.EventStarted, SessionID: sessionID})
	readLines(stdout, func(line []byte) {
		var ev piEvent
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		switch ev.Type {
		case "response":
			if ev.Command == "prompt" && ev.Success != nil && !*ev.Success {
				failed = ev.Error
				stdin.Close()
			}
			if ev.Command == "compact" && ev.Success != nil {
				if *ev.Success && ev.Data != nil {
					cost += ev.Data.Usage.Cost.Total
					context = ev.Data.EstimatedTokensAfter
					compacted = fmt.Sprintf("compacted %d tokens to about %d", ev.Data.TokensBefore, ev.Data.EstimatedTokensAfter)
				} else {
					failed = ev.Error
				}
				settled = true
				stdin.Close()
			}
		case "extension_ui_request":
			if piDialogs[ev.Method] {
				resp, _ := json.Marshal(map[string]any{"type": "extension_ui_response", "id": ev.ID, "cancelled": true})
				stdin.writeLine(resp)
			}
		case "tool_execution_start":
			emit(turn.Event{Type: turn.EventTool, Tool: ev.ToolName, Detail: summarize(ev.ToolName, ev.Args)})
		case "message_end":
			m := ev.Message
			if m == nil || m.Role != "assistant" {
				return
			}
			cost += m.Usage.Cost.Total
			if req.MaxCostUSD > 0 && cost >= req.MaxCostUSD && !overBudget {
				overBudget = true
				stdin.writeLine([]byte(`{"type":"abort"}`))
			}
			if u := m.Usage; u.Input+u.CacheRead+u.CacheWrite > 0 {
				context = u.Input + u.CacheRead + u.CacheWrite
			}
			if m.Model != "" {
				modelUsed = m.Model
			}
			if m.StopReason == "error" || m.StopReason == "aborted" {
				lastError = m.ErrorMessage
			}
			var sb strings.Builder
			for _, c := range m.Content {
				if c.Type == "text" {
					sb.WriteString(c.Text)
				}
			}
			if text := strings.TrimSpace(sb.String()); text != "" {
				lastText = text
				emit(turn.Event{Type: turn.EventText, Text: text})
			}
		case "agent_settled":
			settled = true
			// Closing stdin is pi's signal for an orderly shutdown.
			stdin.Close()
		}
	})
	waitErr := cmd.Wait()

	switch {
	case req.Compact && failed != "":
		emit(turn.Event{Type: turn.EventError, Text: "pi did not compact: " + failed, SessionID: sessionID})
	case req.Compact && compacted != "":
		emit(turn.Event{Type: turn.EventResult, Text: compacted, SessionID: sessionID, CostUSD: cost, ContextTokens: context})
	case failed != "":
		emit(turn.Event{Type: turn.EventError, Text: "pi rejected the prompt: " + failed, SessionID: sessionID})
	case overBudget:
		emit(turn.Event{Type: turn.EventResult, Text: lastText, SessionID: sessionID, CostUSD: cost, Model: modelUsed,
			ContextTokens: context, IsError: true, StopReason: turn.StopBudget})
	case !settled:
		msg := stderr.String()
		if ctx.Err() != nil {
			msg = "turn aborted"
		} else if msg == "" && waitErr != nil {
			msg = waitErr.Error()
		} else if msg == "" {
			msg = "pi exited before the turn finished"
		}
		emit(turn.Event{Type: turn.EventError, Text: msg, SessionID: sessionID})
	default:
		ev := turn.Event{Type: turn.EventResult, Text: lastText, SessionID: sessionID, CostUSD: cost, Model: modelUsed, ContextTokens: context}
		if lastText == "" && lastError != "" {
			ev.Text, ev.IsError = lastError, true
		}
		emit(ev)
	}
	return nil
}

// lockedWriter serialises writes to pi's stdin, which the event loop and
// the abort hook both use, and tolerates writes after close.
type lockedWriter struct {
	mu     sync.Mutex
	w      io.WriteCloser
	closed bool
}

func (l *lockedWriter) writeLine(b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.w.Write(append(b, '\n'))
	}
}

func (l *lockedWriter) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		l.w.Close()
	}
}
