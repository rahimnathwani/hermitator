package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/harness"
	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/spool"
	"github.com/rahimnathwani/hermitator/internal/store"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// Pi providers hermitator configures.
const (
	ProviderCodex      = "openai-codex"
	ProviderDeepSeek   = "deepseek"
	ProviderOpenRouter = "openrouter"
)

// PiProviders lists the pi providers offered in the UI, with their
// default models.
var PiProviders = []struct{ ID, Name, Short, DefaultModel string }{
	{ProviderCodex, "ChatGPT subscription (Codex)", "ChatGPT", harness.DefaultModels[ProviderCodex]},
	{ProviderDeepSeek, "DeepSeek API", "DeepSeek", harness.DefaultModels[ProviderDeepSeek]},
	{ProviderOpenRouter, "OpenRouter API", "OpenRouter", harness.DefaultModels[ProviderOpenRouter]},
}

// Secret returns a user's decrypted secret, or "" if it is unset.
func (m *Manager) Secret(ctx context.Context, userID, name string) string {
	ct, err := m.store.Secret(ctx, userID, name)
	if err != nil {
		return ""
	}
	pt, err := m.box.Open(userID, name, ct)
	if err != nil {
		m.log.Error("secret failed to decrypt; was the master key changed?", "user", userID, "secret", name)
		return ""
	}
	return string(pt)
}

// PutSecret encrypts and stores a user's secret. An empty value deletes it.
func (m *Manager) PutSecret(ctx context.Context, userID, name, value string) error {
	if value == "" {
		return m.store.DeleteSecret(ctx, userID, name)
	}
	return m.store.PutSecret(ctx, userID, name, m.box.Seal(userID, name, []byte(value)))
}

type credentials struct {
	env   map[string]string
	files []turn.File
	// names are the user's own secrets, which the agent is told about.
	names []string
	// github is the account git and gh act as, if a token is set.
	github string
	// redact holds every secret value, kept out of what is sent or shown.
	redact []string
}

// credentials gathers what an agent's harness needs to talk to its model,
// or explains (in a chat-ready sentence) what is missing.
func (m *Manager) credentials(ctx context.Context, a *store.Agent) (credentials, string) {
	c := credentials{env: map[string]string{}}
	settings := strings.TrimRight(m.cfg.PublicURL, "/") + "/settings"
	switch a.Harness {
	case store.HarnessClaude:
		tok := m.Secret(ctx, a.UserID, store.SecretClaudeToken)
		if tok == "" {
			return c, "This agent runs Claude Code, but no Claude token is set up. Run `claude setup-token` on your own computer and paste the token at " + settings
		}
		c.env["CLAUDE_CODE_OAUTH_TOKEN"] = tok
	case store.HarnessPi:
		auth := m.Secret(ctx, a.UserID, store.SecretPiAuth)
		if auth != "" {
			c.files = append(c.files, turn.File{Path: turn.PiAuthPath, Content: auth, Sync: true})
		}
		deepseek := m.Secret(ctx, a.UserID, store.SecretDeepSeekKey)
		openrouter := m.Secret(ctx, a.UserID, store.SecretOpenRouterKey)
		if deepseek != "" {
			c.env["DEEPSEEK_API_KEY"] = deepseek
		}
		if openrouter != "" {
			c.env["OPENROUTER_API_KEY"] = openrouter
		}
		switch providerOrDefault(a.PiProvider) {
		case ProviderCodex:
			if !hasProvider(auth, ProviderCodex) {
				return c, "This agent uses pi with a ChatGPT subscription, but ChatGPT is not connected. Connect it at " + settings
			}
		case ProviderDeepSeek:
			if deepseek == "" {
				return c, "This agent uses pi with DeepSeek, but no DeepSeek API key is set. Add one at " + settings
			}
		case ProviderOpenRouter:
			if openrouter == "" {
				return c, "This agent uses pi with OpenRouter, but no OpenRouter API key is set. Add one at " + settings
			}
		}
	}
	m.extraCredentials(ctx, a, &c)
	return c, ""
}

// extraCredentials adds the user's own secrets, GitHub and their commit
// identity to a turn, and lists every value to redact.
func (m *Manager) extraCredentials(ctx context.Context, a *store.Agent, c *credentials) {
	times, _ := m.store.SecretTimes(ctx, a.UserID)
	for name := range times {
		key, ok := strings.CutPrefix(name, store.SecretEnvPrefix)
		if !ok {
			continue
		}
		if v := m.Secret(ctx, a.UserID, name); v != "" {
			c.env[key] = v
			c.names = append(c.names, key)
		}
	}
	sort.Strings(c.names)
	if tok := m.Secret(ctx, a.UserID, store.SecretGitHubToken); tok != "" {
		c.env["GH_TOKEN"], c.env["GITHUB_TOKEN"] = tok, tok
		// git over https asks this helper, which answers from the
		// environment: the token is never written to the sandbox's disk.
		c.env["GIT_CONFIG_COUNT"] = "1"
		c.env["GIT_CONFIG_KEY_0"] = "credential.https://github.com.helper"
		c.env["GIT_CONFIG_VALUE_0"] = `!f() { test "$1" = get && echo username=x-access-token && echo "password=$GH_TOKEN"; }; f`
		c.github = "GitHub"
		if u, err := m.store.User(ctx, a.UserID); err == nil && u.GitHubLogin != "" {
			c.github = "@" + u.GitHubLogin
		}
	}
	if u, err := m.store.User(ctx, a.UserID); err == nil && u.GitName != "" && u.GitEmail != "" {
		for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
			c.env[k] = u.GitName
		}
		for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
			c.env[k] = u.GitEmail
		}
	}
	for k, v := range c.env {
		if !strings.HasPrefix(k, "GIT_") {
			c.redact = append(c.redact, v)
		}
	}
	for _, f := range c.files {
		c.redact = append(c.redact, secretStrings(f.Content)...)
	}
}

// secretStrings picks the long string values out of a JSON credentials
// file: tokens, not field names or short settings.
func secretStrings(doc string) []string {
	var v any
	if json.Unmarshal([]byte(doc), &v) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case string:
			if len(x) >= 20 {
				out = append(out, x)
			}
		}
	}
	walk(v)
	return out
}

// redactor replaces secret values in text with a marker.
type redactor []string

func newRedactor(values []string) redactor {
	var r redactor
	for _, v := range values {
		if len(v) >= 8 { // shorter ones would hit ordinary words
			r = append(r, v)
		}
	}
	// Longest first, so a secret that contains another is caught whole.
	sort.Slice(r, func(i, j int) bool { return len(r[i]) > len(r[j]) })
	return r
}

func (r redactor) apply(s string) string {
	for _, v := range r {
		s = strings.ReplaceAll(s, v, "[secret removed]")
	}
	return s
}

func providerOrDefault(p string) string {
	if p == "" {
		return ProviderCodex
	}
	return p
}

func hasProvider(authJSON, provider string) bool {
	var auth map[string]json.RawMessage
	if json.Unmarshal([]byte(authJSON), &auth) != nil {
		return false
	}
	_, ok := auth[provider]
	return ok
}

// syncFile stores credential files the harness rewrote during a turn
// (pi refreshes its OAuth tokens in place).
func (m *Manager) syncFile(ctx context.Context, userID string, ev turn.Event) {
	if ev.Path != turn.PiAuthPath || !json.Valid([]byte(ev.Content)) {
		return
	}
	if err := m.PutSecret(ctx, userID, store.SecretPiAuth, ev.Content); err != nil {
		m.log.Error("store refreshed pi credentials", "user", userID, "err", err)
	}
}

// scrubCredentials removes credential files from a sandbox before it is
// suspended, so plaintext tokens do not sit in its upper layer or in a
// checkpoint image.
func (m *Manager) scrubCredentials(ctx context.Context, spec *sandbox.Spec) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := spec.Spool()
	_, exit, err := client.Run(ctx, spool.Job{ID: "scrub-" + ids.New(), Argv: []string{"sh", "-c", `rm -f "$HOME/.pi/agent/auth.json"`}})
	if err != nil || !exit.OK() {
		m.log.Warn("scrub credentials", "agent", spec.ID, "err", err, "exit", exit.String())
	}
}

// Terminal wakes an agent and returns an interactive shell inside it, with
// the owner's credentials available. done must be called when the
// terminal closes.
func (m *Manager) Terminal(ctx context.Context, agentID string) (cmd *exec.Cmd, done func(), err error) {
	ag, err := m.store.Agent(ctx, agentID)
	if err != nil {
		return nil, nil, err
	}
	spec, err := m.Spec(ag)
	if err != nil {
		return nil, nil, err
	}
	release, err := m.Hold(ctx, agentID)
	if err != nil {
		return nil, nil, err
	}
	env := []string{"IS_SANDBOX=1", "TERM=xterm-256color", "COLORTERM=truecolor"}
	for name, envVar := range map[string]string{
		store.SecretClaudeToken:   "CLAUDE_CODE_OAUTH_TOKEN",
		store.SecretDeepSeekKey:   "DEEPSEEK_API_KEY",
		store.SecretOpenRouterKey: "OPENROUTER_API_KEY",
	} {
		if v := m.Secret(ctx, ag.UserID, name); v != "" {
			env = append(env, envVar+"="+v)
		}
	}
	if auth := m.Secret(ctx, ag.UserID, store.SecretPiAuth); auth != "" {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		spec.Spool().Run(wctx, spool.Job{ID: "creds-" + ids.New(), Stdin: []byte(auth),
			Argv: []string{"sh", "-c", `mkdir -p "$HOME/.pi/agent" && umask 077 && cat > "$HOME/.pi/agent/auth.json"`}})
		cancel()
	}
	cmd, err = m.rt.Shell(ctx, spec, env)
	if err != nil {
		release()
		return nil, nil, err
	}
	done = func() {
		// Pick up anything changed from the terminal, such as a pi /login.
		rctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		out, exit, err := spec.Spool().Run(rctx, spool.Job{ID: "creds-" + ids.New(),
			Argv: []string{"sh", "-c", `cat "$HOME/.pi/agent/auth.json" 2>/dev/null`}})
		if err == nil && exit.OK() && json.Valid(out) && len(strings.TrimSpace(string(out))) > 2 {
			if current := m.Secret(rctx, ag.UserID, store.SecretPiAuth); strings.TrimSpace(current) != strings.TrimSpace(string(out)) {
				m.PutSecret(rctx, ag.UserID, store.SecretPiAuth, strings.TrimSpace(string(out)))
			}
		}
		release()
	}
	return cmd, done, nil
}

// LoginEvent reports progress of a ChatGPT (Codex) login.
type LoginEvent struct {
	Type            string `json:"type"` // device_code, info, done, error
	UserCode        string `json:"user_code,omitempty"`
	VerificationURI string `json:"verification_uri,omitempty"`
	Message         string `json:"message,omitempty"`
}

// ErrNoAgent is returned when a user needs an agent for something.
var ErrNoAgent = errors.New("create an agent first")

// CodexLogin runs pi's device-code login for ChatGPT inside one of the
// user's sandboxes and stores the resulting credential. The returned
// channel closes when the login ends.
func (m *Manager) CodexLogin(ctx context.Context, userID string) (<-chan LoginEvent, error) {
	agents, err := m.store.Agents(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(agents) == 0 {
		return nil, ErrNoAgent
	}
	// Prefer a sandbox that is already awake, then the most recently used.
	ag := agents[0]
	for _, a := range agents[1:] {
		awake, agAwake := a.State == sandbox.Running, ag.State == sandbox.Running
		if (awake && !agAwake) || (awake == agAwake && a.LastActive.After(ag.LastActive)) {
			ag = a
		}
	}
	spec, err := m.Spec(ag)
	if err != nil {
		return nil, err
	}
	release, err := m.Hold(ctx, ag.ID)
	if err != nil {
		return nil, err
	}

	jobID := "login-" + ids.New()
	client := spec.Spool()
	sum := sha256.Sum256([]byte("hermitator-device:" + userID))
	deviceID := hex.EncodeToString(sum[:16])
	job := spool.Job{
		ID:   jobID,
		Argv: []string{"node", path.Join(m.rt.GuestTools(spec), "lib", "pi-login.mjs"), ProviderCodex},
		Env:  []string{"HERMITATOR_DEVICE_ID=" + deviceID},
	}
	if err := client.Submit(job); err != nil {
		release()
		return nil, err
	}

	ch := make(chan LoginEvent, 8)
	go func() {
		defer close(ch)
		defer release()
		lctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		defer cancel()
		var finished bool
		send := func(ev LoginEvent) {
			select {
			case ch <- ev:
			case <-lctx.Done():
			}
		}
		_, err := client.Follow(lctx, jobID, func(line []byte) {
			var ev struct {
				Type            string          `json:"type"`
				UserCode        string          `json:"user_code"`
				VerificationURI string          `json:"verification_uri"`
				Message         string          `json:"message"`
				Credential      json.RawMessage `json:"credential"`
			}
			if json.Unmarshal(line, &ev) != nil {
				return
			}
			switch ev.Type {
			case "device_code":
				send(LoginEvent{Type: "device_code", UserCode: ev.UserCode, VerificationURI: ev.VerificationURI})
			case "info":
				send(LoginEvent{Type: "info", Message: ev.Message})
			case "error":
				finished = true
				send(LoginEvent{Type: "error", Message: ev.Message})
			case "credential":
				finished = true
				if err := m.mergePiCredential(lctx, userID, ProviderCodex, ev.Credential); err != nil {
					send(LoginEvent{Type: "error", Message: err.Error()})
					return
				}
				send(LoginEvent{Type: "done", Message: "ChatGPT is connected."})
			}
		})
		if err != nil {
			// The browser left or the code expired; stop the login.
			client.Cancel(jobID, 15*time.Second)
		} else {
			client.Remove(jobID)
		}
		if !finished {
			msg := "the login did not finish"
			if detail := strings.TrimSpace(client.Stderr(jobID, 300)); detail != "" {
				msg += ": " + detail
			}
			send(LoginEvent{Type: "error", Message: msg})
		}
	}()
	return ch, nil
}

func (m *Manager) mergePiCredential(ctx context.Context, userID, provider string, cred json.RawMessage) error {
	auth := map[string]json.RawMessage{}
	if current := m.Secret(ctx, userID, store.SecretPiAuth); current != "" {
		if err := json.Unmarshal([]byte(current), &auth); err != nil {
			return fmt.Errorf("stored pi credentials are corrupt: %w", err)
		}
	}
	auth[provider] = cred
	b, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return err
	}
	return m.PutSecret(ctx, userID, store.SecretPiAuth, string(b))
}

// HasPiProvider reports whether a user's pi credentials include provider.
func (m *Manager) HasPiProvider(ctx context.Context, userID, provider string) bool {
	return hasProvider(m.Secret(ctx, userID, store.SecretPiAuth), provider)
}

// DisconnectPiProvider removes one provider from a user's pi credentials.
func (m *Manager) DisconnectPiProvider(ctx context.Context, userID, provider string) error {
	current := m.Secret(ctx, userID, store.SecretPiAuth)
	if current == "" {
		return nil
	}
	auth := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(current), &auth); err != nil {
		return m.PutSecret(ctx, userID, store.SecretPiAuth, "")
	}
	delete(auth, provider)
	if len(auth) == 0 {
		return m.PutSecret(ctx, userID, store.SecretPiAuth, "")
	}
	b, _ := json.MarshalIndent(auth, "", "  ")
	return m.PutSecret(ctx, userID, store.SecretPiAuth, string(b))
}
