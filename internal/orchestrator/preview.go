package orchestrator

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// Preview links: https://PORT-AGENT.<preview domain>/ reaches a port in an
// agent's sandbox, waking it if needed, so the owner can open a dev server
// on their phone. Hostnames end up in public certificate logs, so they are
// not the secret: the link carries one, which the proxy turns into a
// cookie for that host. Anyone with the link can view the page until the
// owner turns the agent's links off.

// PreviewParam is the query parameter a preview link's secret travels in.
const PreviewParam = "hermitator_preview"

// ErrNoPreviews is returned when this server has no preview domain.
var ErrNoPreviews = errors.New("preview links are not set up on this server (HERMITATOR_PREVIEW_DOMAIN)")

// Previews says whether preview links are set up.
func (m *Manager) Previews() bool { return m.cfg.PreviewDomain != "" }

// PreviewHost is the hostname of an agent's port.
func (m *Manager) PreviewHost(agentID string, port int) string {
	return fmt.Sprintf("%d-%s.%s", port, agentID, m.cfg.PreviewDomain)
}

// ParsePreviewHost reads the agent and port out of a preview hostname.
func (m *Manager) ParsePreviewHost(host string) (agentID string, port int, ok bool) {
	if m.cfg.PreviewDomain == "" {
		return "", 0, false
	}
	label, ok := strings.CutSuffix(strings.ToLower(host), "."+m.cfg.PreviewDomain)
	if !ok || strings.Contains(label, ".") {
		return "", 0, false
	}
	p, id, ok := strings.Cut(label, "-")
	port, err := strconv.Atoi(p)
	if !ok || err != nil || port < 1 || port > 65535 || id == "" {
		return "", 0, false
	}
	return id, port, true
}

// PreviewSecret is what a preview link must carry.
func (m *Manager) PreviewSecret(p *store.Preview) string {
	mac := hmac.New(sha256.New, m.cfg.PreviewKey)
	fmt.Fprintf(mac, "%s\x00%d\x00%s", p.AgentID, p.Port, p.Nonce)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:18])
}

// PreviewURL is a preview's link, secret included.
func (m *Manager) PreviewURL(p *store.Preview) string {
	return "https://" + m.PreviewHost(p.AgentID, p.Port) + "/?" + PreviewParam + "=" + m.PreviewSecret(p)
}

// NewPreview returns the link to an agent's port, creating it if needed.
func (m *Manager) NewPreview(ctx context.Context, agentID string, port int) (string, error) {
	if !m.Previews() {
		return "", ErrNoPreviews
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("%d is not a port", port)
	}
	p, err := m.store.PutPreview(ctx, agentID, port)
	if err != nil {
		return "", err
	}
	return m.PreviewURL(p), nil
}

// PreviewTarget wakes an agent for a preview request and keeps it awake
// until release; it returns the address to send the request to.
func (m *Manager) PreviewTarget(ctx context.Context, agentID string) (netip.Addr, func(), error) {
	ag, err := m.store.Agent(ctx, agentID)
	if err != nil {
		return netip.Addr{}, nil, err
	}
	ip, err := netip.ParseAddr(ag.IP)
	if err != nil {
		return netip.Addr{}, nil, err
	}
	if m.cfg.PreviewLoopback {
		ip = netip.MustParseAddr("127.0.0.1")
	}
	// A page loads dozens of files at once; while the sandbox runs, each
	// only needs to count as activity, not to ask the runtime.
	a := m.actor(agentID)
	var running bool
	a.call(func(a *actor) error {
		if running = a.state == sandbox.Running; running {
			a.holds++
		}
		return nil
	})
	if running {
		return ip, sync.OnceFunc(func() {
			a.do(func(a *actor) {
				a.holds--
				a.lastActive = time.Now()
			})
		}), nil
	}
	release, err := m.Hold(ctx, agentID)
	return ip, release, err
}

// previewCommand handles /preview PORT and /previews [off].
func (m *Manager) previewCommand(ctx context.Context, agentID string, cmd Command, reply func(string)) {
	if !m.Previews() {
		reply("Preview links are not set up on this server.")
		return
	}
	arg := strings.ToLower(strings.TrimSpace(cmd.Arg))
	if cmd.Name == "preview" && arg != "" && arg != "off" {
		port, err := strconv.Atoi(arg)
		if err != nil {
			reply("Use /preview PORT, such as /preview 3000.")
			return
		}
		link, err := m.NewPreview(ctx, agentID, port)
		if err != nil {
			reply("No preview link: " + err.Error())
			return
		}
		reply("🔗 " + link + "\n\nAnyone with this link can see port " + arg + " while it is open; /previews off turns every link off. The server has to listen on 0.0.0.0, not localhost.")
		return
	}
	if arg == "off" {
		n, _ := m.store.DeletePreviews(ctx, agentID)
		reply(fmt.Sprintf("🔗 Turned off preview links for %d port(s). New ones get new links.", n))
		return
	}
	ps, _ := m.store.Previews(ctx, agentID)
	if len(ps) == 0 {
		reply("No preview links. /preview 3000 makes one for port 3000.")
		return
	}
	var b strings.Builder
	b.WriteString("🔗 Preview links:\n")
	for _, p := range ps {
		fmt.Fprintf(&b, "\n%d: %s", p.Port, m.PreviewURL(p))
	}
	b.WriteString("\n\n/previews off turns them all off.")
	reply(b.String())
}
