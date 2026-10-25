package orchestrator

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/spool"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// The agent's browser: a headless Chromium inside the sandbox, started by
// `hermitator-guest browser`, which the owner drives from the panel's
// Browser tab to sign in to sites for the agent. Its cookies are real
// credentials in the agent's own files, unlike the placeholders the egress
// proxy swaps, so the agent can use them however it likes.

// BrowserPath is the panel page for an agent's browser.
func BrowserPath(agentID string) string { return "/agents/" + agentID + "/browser" }

// Browser wakes an agent, starts its browser unless it runs, and returns
// the address of its DevTools forwarder. The agent stays awake until
// release is called.
func (m *Manager) Browser(ctx context.Context, agentID string) (addr string, release func(), err error) {
	ag, err := m.store.Agent(ctx, agentID)
	if err != nil {
		return "", nil, err
	}
	ip, err := netip.ParseAddr(ag.IP)
	if err != nil {
		return "", nil, err
	}
	if m.cfg.PreviewLoopback {
		ip = netip.MustParseAddr("127.0.0.1")
	}
	release, err = m.Hold(ctx, agentID)
	if err != nil {
		return "", nil, err
	}
	if err := m.browserJob(ctx, agentID); err != nil {
		release()
		return "", nil, err
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(turn.BrowserPort)), release, nil
}

// ForgetBrowser closes an agent's browser and deletes its profile, with
// every cookie and saved login in it.
func (m *Manager) ForgetBrowser(ctx context.Context, agentID string) error {
	release, err := m.Hold(ctx, agentID)
	if err != nil {
		return err
	}
	defer release()
	return m.browserJob(ctx, agentID, "--forget")
}

func (m *Manager) browserJob(ctx context.Context, agentID string, args ...string) error {
	ag, err := m.store.Agent(ctx, agentID)
	if err != nil {
		return err
	}
	spec, err := m.Spec(ag)
	if err != nil {
		return err
	}
	jctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	id := "browser-" + ids.New()
	client := spec.Spool()
	if err := client.Submit(spool.Job{ID: id, Argv: append([]string{"hermitator-guest", "browser"}, args...)}); err != nil {
		return err
	}
	// Not Run, which deletes stderr, where the guest says what went wrong.
	exit, err := client.Follow(jctx, id, func([]byte) {})
	if err != nil {
		client.Cancel(id, 15*time.Second)
		return err
	}
	defer client.Remove(id)
	if !exit.OK() {
		msg := strings.TrimSpace(client.Stderr(id, 600))
		if strings.HasPrefix(msg, "usage: hermitator-guest init") {
			// A sandbox restored from a checkpoint keeps the guest it
			// booted with.
			return errors.New("this sandbox is still running a hermitator-guest from before the browser existed; it gets the new one at its next cold boot, when it sleeps with nothing running in the background")
		}
		if msg == "" {
			msg = "the browser command failed: " + exit.String()
		}
		return errors.New(strings.TrimPrefix(msg, "browser: "))
	}
	return nil
}
