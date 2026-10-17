// Package harness drives Claude Code and pi headlessly inside a sandbox
// and translates what they print into turn.Events.
package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

// Emit receives events as the harness produces them.
type Emit func(turn.Event)

// DefaultModels is used for pi when an agent names a provider but no model.
var DefaultModels = map[string]string{
	"openai-codex": "gpt-5.6-terra",
	"deepseek":     "deepseek-v4-pro",
	"openrouter":   "~anthropic/claude-sonnet-latest",
}

// Run executes one turn and reports it through emit. It returns an error
// only when the harness could not run at all; a turn that ran and failed
// is reported as a result event with IsError set.
func Run(ctx context.Context, req *turn.Request, emit Emit) error {
	workdir := req.Workdir
	if workdir == "" {
		workdir = Getenv("HERMITATOR_WORKSPACE", turn.Workspace)
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	before, err := writeFiles(req.Files)
	if err != nil {
		return fmt.Errorf("write files: %w", err)
	}
	defer syncFiles(req.Files, before, emit)

	switch req.Harness {
	case turn.Claude:
		return runClaude(ctx, req, workdir, emit)
	case turn.Pi:
		return runPi(ctx, req, workdir, emit)
	default:
		return fmt.Errorf("unknown harness %q", req.Harness)
	}
}

// Getenv returns the environment variable key, or fallback when unset.
func Getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func expandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return path
}

// writeFiles writes the request's files and returns what each one held,
// keyed by the path as the request named it.
func writeFiles(files []turn.File) (map[string]string, error) {
	before := map[string]string{}
	for _, f := range files {
		path := expandHome(f.Path)
		mode := os.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o600
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(f.Content), mode); err != nil {
			return nil, err
		}
		before[f.Path] = f.Content
	}
	return before, nil
}

// syncFiles reports Sync files whose contents changed during the turn.
func syncFiles(files []turn.File, before map[string]string, emit Emit) {
	for _, f := range files {
		if !f.Sync {
			continue
		}
		data, err := os.ReadFile(expandHome(f.Path))
		if err != nil || string(data) == before[f.Path] {
			continue
		}
		emit(turn.Event{Type: turn.EventFile, Path: f.Path, Content: string(data)})
	}
}

// environ builds the harness environment: the guest's own environment,
// overridden by the request.
func environ(extra map[string]string, drop ...string) []string {
	skip := map[string]bool{}
	for _, k := range drop {
		skip[k] = true
	}
	for k := range extra {
		skip[k] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !skip[k] {
			env = append(env, kv)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func readImage(img turn.Image) (string, error) {
	path := img.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(Getenv("HERMITATOR_SHARED", turn.SharedDir), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// readLines calls fn for every newline-terminated line of r. Harnesses can
// print very long lines (whole file contents in tool results), so this
// does not use bufio.Scanner and its token limit.
func readLines(r io.Reader, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			fn(bytes.TrimRight(line, "\r\n"))
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// tail keeps the last n bytes written to it, for error reports.
type tail struct {
	n   int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.n {
		t.buf = t.buf[len(t.buf)-t.n:]
	}
	return len(p), nil
}

func (t *tail) String() string { return strings.TrimSpace(string(t.buf)) }

// summarize turns a tool's input into a one-line description for logs.
func summarize(tool string, input map[string]any) string {
	keys := []string{"command", "file_path", "path", "pattern", "url", "query", "description", "prompt", "skill"}
	for _, k := range keys {
		if v, ok := input[k].(string); ok && v != "" {
			return clip(v)
		}
	}
	for _, v := range input {
		if s, ok := v.(string); ok && s != "" {
			return clip(s)
		}
	}
	return ""
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		return string(r[:119]) + "…"
	}
	return s
}
