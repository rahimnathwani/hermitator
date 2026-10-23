// Package harness drives Claude Code and pi headlessly inside a sandbox
// and translates what they print into turn.Events.
package harness

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	if req.Discard && req.Fork {
		// The fork's ID comes in its events; its transcript goes after them.
		inner := emit
		var forked string
		emit = func(e turn.Event) {
			if e.SessionID != "" && e.SessionID != req.SessionID {
				forked = e.SessionID
			}
			inner(e)
		}
		defer func() { discardSession(req.Harness, forked) }()
	}

	switch req.Harness {
	case turn.Claude:
		return runClaude(ctx, req, workdir, emit)
	case turn.Pi:
		return runPi(ctx, req, workdir, emit)
	default:
		return fmt.Errorf("unknown harness %q", req.Harness)
	}
}

// discardSession deletes a session's transcript.
func discardSession(harness, id string) {
	if id == "" || strings.ContainsAny(id, "/*?[") {
		return
	}
	home, _ := os.UserHomeDir()
	var pattern string
	switch harness {
	case turn.Claude:
		pattern = filepath.Join(home, ".claude", "projects", "*", id+".jsonl")
	case turn.Pi:
		pattern = filepath.Join(home, ".pi", "agent", "sessions", "*", "*_"+id+".jsonl")
	}
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		os.Remove(m)
	}
}

// newSessionID is a random UUID, for a session hermitator starts itself.
func newSessionID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
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
		content := f.Content
		if f.Path == turn.PiModelsPath {
			content = mergePiModels(path, content)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			return nil, err
		}
		before[f.Path] = f.Content
	}
	return before, nil
}

// mergePiModels puts the host's providers into the models.json at path.
// Providers the agent added itself stay; the host's from an earlier turn
// are replaced, so one removed in the panel goes. A file that is not plain
// JSON (pi allows comments) is kept beside it as models.json.bak.
func mergePiModels(path, ours string) string {
	var in struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if json.Unmarshal([]byte(ours), &in) != nil {
		return ours
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return ours
	}
	doc := map[string]json.RawMessage{}
	if json.Unmarshal(old, &doc) != nil {
		os.WriteFile(path+".bak", old, 0o600)
		return ours
	}
	providers := map[string]json.RawMessage{}
	if raw, ok := doc["providers"]; ok && json.Unmarshal(raw, &providers) != nil {
		os.WriteFile(path+".bak", old, 0o600)
		return ours
	}
	if providers == nil { // "providers": null
		providers = map[string]json.RawMessage{}
	}
	for name := range providers {
		if strings.HasPrefix(name, turn.CustomProviderPrefix) {
			delete(providers, name)
		}
	}
	for name, p := range in.Providers {
		providers[name] = p
	}
	doc["providers"], _ = json.Marshal(providers)
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return ours
	}
	return string(b)
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
