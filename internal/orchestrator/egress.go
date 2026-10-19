package orchestrator

import (
	"context"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/rahimnathwani/hermitator/internal/egress"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/store"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// Keeping credentials out of sandboxes. With the egress proxy on for a
// user, their agents hold placeholders; the proxy swaps in the real
// values on the way to each credential's own hosts, so an agent that is
// tricked into sending its environment somewhere sends nothing of use.

// builtinCredential is a credential hermitator knows the hosts of.
type builtinCredential struct {
	secret string   // store name
	env    []string // variables it reaches the sandbox as
	prefix string   // keeps the placeholder looking like the real thing
	hosts  []string
}

var builtinCredentials = []builtinCredential{
	{store.SecretClaudeToken, []string{"CLAUDE_CODE_OAUTH_TOKEN"}, "sk-ant-oat01-", []string{"api.anthropic.com"}},
	{store.SecretDeepSeekKey, []string{"DEEPSEEK_API_KEY"}, "sk-", []string{"api.deepseek.com"}},
	{store.SecretOpenRouterKey, []string{"OPENROUTER_API_KEY"}, "sk-or-v1-", []string{"openrouter.ai"}},
	{store.SecretGitHubToken, []string{"GH_TOKEN", "GITHUB_TOKEN"}, "gho_", []string{"github.com", "api.github.com", "uploads.github.com", "codeload.github.com"}},
}

// egressRouter is a runtime that can send an agent's HTTPS through the
// egress proxy.
type egressRouter interface {
	SetEgress(ctx context.Context, ip netip.Addr, on bool) error
}

// Egress says whether the egress proxy runs on this host.
func (m *Manager) Egress() bool { return len(m.cfg.EgressKey) > 0 }

// usePlaceholders swaps the turn's credentials for placeholders when the
// owner keeps them out of the sandbox and the sandbox trusts the proxy.
func (m *Manager) usePlaceholders(ctx context.Context, ag *store.Agent, spec *sandbox.Spec, c *credentials) {
	if !m.Egress() || !spec.TrustsEgress() {
		return
	}
	u, err := m.store.User(ctx, ag.UserID)
	if err != nil || !u.Egress {
		return
	}
	if err := m.refreshCodex(ctx, u.ID); err != nil {
		m.log.Warn("ChatGPT token refresh", "user", u.ID, "err", err)
	}
	for _, s := range m.egressSecrets(ctx, u) {
		for k, v := range c.env {
			if v == s.Value {
				c.env[k] = s.Placeholder
			}
		}
	}
	for i, f := range c.files {
		if f.Path == turn.PiAuthPath {
			c.files[i].Content = m.sandboxPiAuth(u.ID, f.Content)
		}
	}
	c.placeholders = true
}

type egressCache struct {
	mu      sync.Mutex
	entries map[string]egressEntry
}

type egressEntry struct {
	at      time.Time
	secrets []egress.Secret
}

// EgressSecrets is the proxy's Resolver: the secrets of the agent at an
// address, or none for an agent whose owner has the proxy off.
func (m *Manager) EgressSecrets(ctx context.Context, from netip.Addr) ([]egress.Secret, bool) {
	ag, err := m.store.AgentByIP(ctx, from.String())
	if err != nil {
		return nil, false
	}
	u, err := m.store.User(ctx, ag.UserID)
	if err != nil {
		return nil, false
	}
	if !u.Egress {
		return nil, true
	}
	m.refreshCodex(ctx, u.ID)
	return m.egressSecrets(ctx, u), true
}

func (m *Manager) egressSecrets(ctx context.Context, u *store.User) []egress.Secret {
	m.egress.mu.Lock()
	defer m.egress.mu.Unlock()
	if e, ok := m.egress.entries[u.ID]; ok && time.Since(e.at) < 30*time.Second {
		return e.secrets
	}
	var out []egress.Secret
	for _, b := range builtinCredentials {
		if v := m.Secret(ctx, u.ID, b.secret); v != "" {
			out = append(out, egress.Secret{Name: b.secret, Value: v, Hosts: b.hosts,
				Placeholder: egress.Placeholder(m.cfg.EgressKey, u.ID, b.secret, b.prefix)})
		}
	}
	out = append(out, m.codexSecrets(ctx, u.ID)...)
	hosts, _ := m.store.SecretHosts(ctx, u.ID)
	for name, hs := range hosts {
		if v := m.Secret(ctx, u.ID, store.SecretEnvPrefix+name); v != "" && len(hs) > 0 {
			out = append(out, egress.Secret{Name: name, Value: v, Hosts: hs,
				Placeholder: egress.Placeholder(m.cfg.EgressKey, u.ID, store.SecretEnvPrefix+name, "")})
		}
	}
	if m.egress.entries == nil {
		m.egress.entries = map[string]egressEntry{}
	}
	m.egress.entries[u.ID] = egressEntry{time.Now(), out}
	return out
}

// ForgetSecrets drops the proxy's cached secrets for a user, after a
// credential changes.
func (m *Manager) ForgetSecrets(userID string) {
	m.egress.mu.Lock()
	delete(m.egress.entries, userID)
	m.egress.mu.Unlock()
}

// SetEgress turns the egress proxy on or off for a user's agents. Agents
// that booted before the proxy existed do not trust it yet; idle ones are
// stopped so their next message boots them afresh (files are kept), and
// until then their turns carry real credentials as before. It reports how
// many agents still wait for that.
func (m *Manager) SetEgress(ctx context.Context, userID string, on bool) (waiting int, err error) {
	if !m.Egress() && on {
		return 0, ErrNoEgress
	}
	if err := m.store.SetEgress(ctx, userID, on); err != nil {
		return 0, err
	}
	m.ForgetSecrets(userID)
	agents, _ := m.store.Agents(ctx, userID)
	for _, ag := range agents {
		m.routeEgress(ctx, ag, on)
		if !on {
			continue
		}
		spec, err := m.Spec(ag)
		if err != nil || spec.TrustsEgress() {
			continue
		}
		err = m.actor(ag.ID).call(func(a *actor) error {
			if a.turn != nil || a.holds > 0 {
				return ErrBusy
			}
			if a.state == sandbox.Running {
				m.scrubCredentials(ctx, spec)
			}
			if err := m.rt.Stop(ctx, spec); err != nil {
				return err
			}
			a.setState(sandbox.Stopped)
			return nil
		})
		if err != nil {
			waiting++
		}
	}
	return waiting, nil
}

func (m *Manager) routeEgress(ctx context.Context, ag *store.Agent, on bool) {
	r, ok := m.rt.(egressRouter)
	if !ok || ag.IP == "" {
		return
	}
	if ip, err := netip.ParseAddr(ag.IP); err == nil {
		if err := r.SetEgress(ctx, ip, on); err != nil {
			m.log.Warn("egress route", "agent", ag.ID, "err", err)
		}
	}
}

// ValidHost reports whether h looks like a host name a secret may go to.
func ValidHost(h string) bool {
	if len(h) == 0 || len(h) > 253 || strings.ContainsAny(h, "/:@ ") || !strings.Contains(h, ".") {
		return false
	}
	for _, part := range strings.Split(h, ".") {
		if part == "" || len(part) > 63 {
			return false
		}
	}
	return true
}
