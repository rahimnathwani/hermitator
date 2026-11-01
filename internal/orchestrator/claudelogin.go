package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rahimnathwani/hermitator/internal/guestfiles"
	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/spool"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// A Claude token from the panel: `claude setup-token` runs in one of the
// user's sandboxes (internal/guestfiles/claude-login.py drives it), the
// panel shows the sign-in link it prints, and the code the sign-in page
// gives back is pasted into the panel and handed to it as a file in
// /shared. The token it prints is stored like a pasted one. As with the
// ChatGPT login, the token passes through the sandbox on its way, so the
// agent could see it; it is the user's own agent, which gets the token on
// every turn anyway.

// claudeLoginDir is where, under /shared, codes for logins wait.
const claudeLoginDir = "login"

// claudeLogins are logins waiting for their code, by login ID.
type claudeLogins struct {
	mu      sync.Mutex
	pending map[string]claudeLogin
}

type claudeLogin struct {
	userID string
	spec   *sandbox.Spec
}

// ErrNoLogin is returned for a code sent to a login that is not waiting.
var ErrNoLogin = errors.New("that sign-in has ended; start again")

// ClaudeLogin starts `claude setup-token` in one of the user's sandboxes.
// The first event carries the sign-in URL and the login's ID, to send the
// code to with ClaudeLoginCode. The channel closes when the login ends;
// cancelling ctx ends it.
func (m *Manager) ClaudeLogin(ctx context.Context, userID string) (<-chan LoginEvent, error) {
	ag, err := m.loginAgent(ctx, userID)
	if err != nil {
		return nil, err
	}
	spec, err := m.Spec(ag)
	if err != nil {
		return nil, err
	}
	release, err := m.Hold(ctx, ag.ID)
	if err != nil {
		return nil, err
	}
	script, err := guestfiles.FS.ReadFile("claude-login.py")
	if err != nil {
		release()
		return nil, err
	}
	login := ids.New()
	jobID := "claude-login-" + login
	codeFile := path.Join(m.rt.GuestShared(spec), claudeLoginDir, login)
	client := spec.Spool()
	// The helper goes in on stdin rather than from the sandbox's tools
	// bundle: a sandbox keeps the bundle it booted with, so one that booted
	// before the helper existed would not have it. The helper reads the
	// code from codeFile and never reads stdin itself.
	job := spool.Job{
		ID:    jobID,
		Argv:  []string{"python3", "-", codeFile},
		Stdin: script,
	}
	// The directory must exist before the code is written, and an agent may
	// have put something else there.
	removeShared(spec, path.Join(claudeLoginDir, login))
	if err := client.Submit(job); err != nil {
		release()
		return nil, err
	}
	m.claude.mu.Lock()
	if m.claude.pending == nil {
		m.claude.pending = map[string]claudeLogin{}
	}
	m.claude.pending[login] = claudeLogin{userID: userID, spec: spec}
	m.claude.mu.Unlock()

	ch := make(chan LoginEvent, 8)
	// Counted in bg, so Wait sees it out; it ends with the manager too.
	m.bg.Add(1)
	go func() {
		defer m.bg.Done()
		defer close(ch)
		defer release()
		// Once the helper has an answer, no further code is taken.
		forget := func() {
			m.claude.mu.Lock()
			delete(m.claude.pending, login)
			m.claude.mu.Unlock()
		}
		defer func() {
			forget()
			removeShared(spec, path.Join(claudeLoginDir, login), path.Join(claudeLoginDir, login+".part"))
		}()
		lctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		defer cancel()
		defer context.AfterFunc(m.ctx, cancel)()
		var finished bool
		send := func(ev LoginEvent) {
			select {
			case ch <- ev:
			case <-lctx.Done():
			}
		}
		_, err := client.Follow(lctx, jobID, func(line []byte) {
			var ev struct {
				Type    string `json:"type"`
				URL     string `json:"url"`
				Token   string `json:"token"`
				Message string `json:"message"`
			}
			if json.Unmarshal(line, &ev) != nil {
				return
			}
			switch ev.Type {
			case "url":
				send(LoginEvent{Type: "url", VerificationURI: ev.URL, Login: login})
			case "error":
				finished = true
				forget()
				send(LoginEvent{Type: "error", Message: ev.Message})
			case "token":
				finished = true
				forget()
				tok := strings.TrimSpace(ev.Token)
				if !strings.HasPrefix(tok, "sk-ant-") {
					send(LoginEvent{Type: "error", Message: "setup-token printed something that is not a Claude token"})
					return
				}
				if err := m.PutSecret(lctx, userID, store.SecretClaudeToken, tok); err != nil {
					send(LoginEvent{Type: "error", Message: err.Error()})
					return
				}
				m.ForgetSecrets(userID)
				m.store.ClearNotice(lctx, userID, ClaudeExpiryNotice)
				send(LoginEvent{Type: "done", Message: "Claude is connected."})
			}
		})
		// Read before Cancel or Remove, which delete the job's stderr.
		detail := strings.TrimSpace(client.Stderr(jobID, 300))
		if err != nil {
			client.Cancel(jobID, 15*time.Second)
		} else {
			client.Remove(jobID)
		}
		if !finished {
			msg := "the sign-in did not finish"
			if detail != "" {
				msg += ": " + detail
			}
			send(LoginEvent{Type: "error", Message: msg})
		}
	}()
	return ch, nil
}

// ClaudeLoginCode hands the code from Claude's sign-in page to a waiting
// login of the user's.
func (m *Manager) ClaudeLoginCode(ctx context.Context, userID, login, code string) error {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 1024 || strings.ContainsAny(code, "\r\n") {
		return errors.New("paste the code exactly as the sign-in page shows it")
	}
	m.claude.mu.Lock()
	p, ok := m.claude.pending[login]
	m.claude.mu.Unlock()
	if !ok || p.userID != userID {
		return ErrNoLogin
	}
	// Written whole and renamed into place: the helper takes the file the
	// moment it appears.
	name := path.Join(claudeLoginDir, login)
	if err := writeShared(p.spec, name+".part", []byte(code)); err != nil {
		return err
	}
	root, err := p.spec.Shared()
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Rename(name+".part", name)
}

// ClaudeExpiryNotice prefixes the kinds of notice that warn a Claude token
// is about to stop working; saving a new token clears them.
const ClaudeExpiryNotice = "claude-expiry"

// claudeTokenLife is how long a token from `claude setup-token` lasts. A
// pasted token may have been made before it was saved, so warnings say
// "around".
const claudeTokenLife = 365 * 24 * time.Hour

// claudeWarnings are how long before expiry the owner is told, shortest
// first; 0 is the notice that it has probably expired.
var claudeWarnings = []time.Duration{0, 3 * 24 * time.Hour, 30 * 24 * time.Hour}

// noticeLoop sends the owner warnings that need no agent, such as an
// expiring Claude token, once an hour.
func (m *Manager) noticeLoop() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	first := time.After(time.Minute)
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-first:
		case <-t.C:
		}
		m.warnClaudeExpiry(m.ctx, time.Now())
	}
}

func (m *Manager) warnClaudeExpiry(ctx context.Context, now time.Time) {
	users, _ := m.store.Users(ctx)
	for _, u := range users {
		if u.IsAdmin() || u.Suspended {
			continue
		}
		times, _ := m.store.SecretTimes(ctx, u.ID)
		saved, ok := times[store.SecretClaudeToken]
		if !ok || saved.IsZero() {
			continue
		}
		expires := saved.Add(claudeTokenLife)
		left := expires.Sub(now)
		// The tightest warning that applies; sending it covers the looser
		// ones too, so a token found late is warned about once.
		i := slices.IndexFunc(claudeWarnings, func(w time.Duration) bool { return left <= w })
		if i < 0 {
			continue
		}
		kind := fmt.Sprintf("%s-%dd", ClaudeExpiryNotice, int(claudeWarnings[i].Hours()/24))
		if m.store.NoticeSent(ctx, u.ID, kind) {
			continue
		}
		settings := strings.TrimRight(m.cfg.PublicURL, "/") + "/settings#claude"
		var text string
		if left <= 0 {
			text = fmt.Sprintf("⚠️ Your Claude token was saved in hermitator on %s. Tokens from claude setup-token last a year, so it has probably stopped working, and your Claude Code agents with it. Sign in again from Settings, which takes a minute from your phone: %s",
				saved.In(u.Location()).Format("2 January 2006"), settings)
		} else {
			text = fmt.Sprintf("⏳ Your Claude token was saved in hermitator on %s. Tokens from claude setup-token last a year, so it stops working around %s, in %s. Sign in again from Settings before then, which takes a minute from your phone: %s",
				saved.In(u.Location()).Format("2 January 2006"), expires.In(u.Location()).Format("2 January 2006"), count(int(left.Hours()/24)+1, "day"), settings)
		}
		if !m.notifyUser(ctx, u.ID, text) {
			continue
		}
		for _, w := range claudeWarnings[i:] {
			m.store.MarkNotice(ctx, u.ID, fmt.Sprintf("%s-%dd", ClaudeExpiryNotice, int(w.Hours()/24)))
		}
	}
}
