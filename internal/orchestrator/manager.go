package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/secrets"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// Config holds the orchestrator's settings.
type Config struct {
	// AgentsDir holds one directory per agent.
	AgentsDir string
	Images    *sandbox.Images
	// Tools is the guest bundle new boots mount.
	Tools  string
	Subnet netip.Prefix
	// Debounce is how long to wait for follow-up messages (a caption
	// typed after a photo, say) before starting a turn.
	Debounce time.Duration
	// ReapInterval is how often idle sandboxes are looked for.
	ReapInterval time.Duration
	// MaxTurn bounds a single turn.
	MaxTurn time.Duration
	// RetryNoticeDelay is how long to wait between attempts to tell chats
	// that a restart interrupted their turn.
	RetryNoticeDelay time.Duration
	// ShellTimeout bounds a /sh command.
	ShellTimeout time.Duration
	// StallAfter is how long a turn may go without any sign of progress
	// before the chat is told, once.
	StallAfter time.Duration
	// ResendInterval is how often texts that failed to send are retried.
	ResendInterval time.Duration
	// EgressKey derives credential placeholders; set when the egress
	// proxy runs on this host.
	EgressKey []byte
	// PublicURL is the web UI's address, used in replies.
	PublicURL string
	// CompactAbove overrides the conversation size, in tokens, compacted
	// before an agent sleeps.
	CompactAbove int
	// PreviewDomain is where preview links live; empty turns them off.
	PreviewDomain string
	// PreviewKey derives preview links' secrets.
	PreviewKey []byte
	// PreviewLoopback sends preview requests to 127.0.0.1, for the
	// unsafe-local runtime, where agents' servers run on the host.
	PreviewLoopback bool
}

// Manager is the orchestrator.
type Manager struct {
	cfg   Config
	store *store.Store
	box   *secrets.Box
	rt    sandbox.Runtime
	log   *slog.Logger
	hub   hub

	msgr   Messenger
	egress egressCache
	codex  codexState
	// bg counts background deliveries in flight, for Wait.
	bg  sync.WaitGroup
	stt Transcriber

	ctx    context.Context
	mu     sync.Mutex
	actors map[string]*actor
	links  linkGuard
}

// New creates a Manager. Call Start before handing it messages.
func New(cfg Config, st *store.Store, box *secrets.Box, rt sandbox.Runtime, log *slog.Logger) *Manager {
	if cfg.Debounce == 0 {
		cfg.Debounce = 1500 * time.Millisecond
	}
	if cfg.ReapInterval == 0 {
		cfg.ReapInterval = 15 * time.Second
	}
	if cfg.MaxTurn == 0 {
		cfg.MaxTurn = 2 * time.Hour
	}
	if cfg.RetryNoticeDelay == 0 {
		cfg.RetryNoticeDelay = 15 * time.Second
	}
	if cfg.ShellTimeout == 0 {
		cfg.ShellTimeout = 2 * time.Minute
	}
	if cfg.StallAfter == 0 {
		cfg.StallAfter = 10 * time.Minute
	}
	if cfg.ResendInterval == 0 {
		cfg.ResendInterval = 10 * time.Second
	}
	return &Manager{cfg: cfg, store: st, box: box, rt: rt, log: log, actors: map[string]*actor{}, msgr: nopMessenger{}}
}

// SetMessenger connects the manager to the chat gateway.
func (m *Manager) SetMessenger(msgr Messenger) { m.msgr = msgr }

// SetTranscriber enables voice note transcription.
func (m *Manager) SetTranscriber(t Transcriber) { m.stt = t }

// Start reconciles stored agent state with the runtime and starts the
// reaper. Sandboxes that were running before a restart keep running and
// are put to sleep once they have been idle for their timeout.
func (m *Manager) Start(ctx context.Context) error {
	m.ctx = ctx
	cut, err := m.store.InterruptRunningTurns(ctx)
	if err != nil {
		return err
	}
	if len(cut) > 0 {
		m.log.Warn("marked turns interrupted by restart", "count", len(cut))
		go m.tellInterrupted(cut)
	}
	go m.scheduler()
	go m.resender()
	agents, err := m.store.Agents(ctx, "")
	if err != nil {
		return err
	}
	var dirs []string
	for _, a := range agents {
		m.renumber(ctx, a)
		spec, err := m.Spec(a)
		if err != nil {
			m.log.Warn("agent has no usable spec", "agent", a.ID, "err", err)
			continue
		}
		dirs = append(dirs, spec.Dir)
		st, err := m.rt.State(ctx, spec)
		if err != nil {
			m.log.Warn("state", "agent", a.ID, "err", err)
			continue
		}
		if st != a.State {
			m.store.SetAgentState(ctx, a.ID, st)
		}
		if st == sandbox.Running {
			m.actor(a.ID)
		}
		cleanSpool(spec)
	}
	if m.cfg.Tools != "" {
		sandbox.PruneTools(filepath.Dir(m.cfg.Tools), m.cfg.Tools, dirs)
	}
	go m.reapLoop(ctx)
	return nil
}

// renumber moves an agent whose address is outside the configured subnet,
// as happens when HERMITATOR_SUBNET changes. A sandbox that is running or
// checkpointed holds its old address in memory, so it is stopped first.
func (m *Manager) renumber(ctx context.Context, a *store.Agent) {
	if ip, err := netip.ParseAddr(a.IP); err == nil && m.cfg.Subnet.Contains(ip) {
		return
	}
	if spec, err := m.Spec(a); err == nil {
		if st, _ := m.rt.State(ctx, spec); st != sandbox.Stopped {
			m.log.Warn("stopping agent to move it to the new subnet; running processes are lost", "agent", a.ID, "state", st)
			m.rt.Stop(ctx, spec)
		}
	}
	ip, err := m.store.ReassignAgentIP(ctx, a.ID, m.cfg.Subnet)
	if err != nil {
		m.log.Error("renumber agent", "agent", a.ID, "err", err)
		return
	}
	m.log.Info("agent moved to the configured subnet", "agent", a.ID, "from", a.IP, "to", ip)
	a.IP = ip
	if u, err := m.store.User(ctx, a.UserID); err == nil && u.Egress {
		m.routeEgress(ctx, a, true)
	}
}

// cleanSpool removes finished jobs a previous process never collected.
func cleanSpool(spec *sandbox.Spec) {
	spec.Spool().Sweep()
}

func (m *Manager) reapLoop(ctx context.Context) {
	t := time.NewTicker(m.cfg.ReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.mu.Lock()
			actors := make([]*actor, 0, len(m.actors))
			for _, a := range m.actors {
				actors = append(actors, a)
			}
			m.mu.Unlock()
			for _, a := range actors {
				a.do(func(a *actor) { a.reap() })
			}
		}
	}
}

// actor returns the actor for an agent, starting it if needed.
func (m *Manager) actor(agentID string) *actor {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.actors[agentID]; a != nil {
		return a
	}
	ctx, cancel := context.WithCancel(m.ctx)
	a := &actor{m: m, id: agentID, inbox: make(chan func(*actor), 256), cancel: cancel, lastActive: time.Now()}
	if ag, err := m.store.Agent(ctx, agentID); err == nil {
		a.state = ag.State
	}
	m.actors[agentID] = a
	go a.loop(ctx)
	return a
}

func (m *Manager) dropActor(agentID string) {
	m.mu.Lock()
	a := m.actors[agentID]
	delete(m.actors, agentID)
	m.mu.Unlock()
	if a != nil {
		a.cancel()
	}
}

// Subscribe streams UI events for an agent.
func (m *Manager) Subscribe(agentID string) (<-chan UIEvent, func()) {
	return m.hub.subscribe(agentID)
}

// Spec builds the sandbox description for an agent.
func (m *Manager) Spec(a *store.Agent) (*sandbox.Spec, error) {
	img, err := m.cfg.Images.Resolve(a.Image)
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(a.IP)
	if err != nil {
		return nil, fmt.Errorf("agent %s has a bad address %q", a.ID, a.IP)
	}
	return &sandbox.Spec{
		ID:       a.ID,
		Dir:      filepath.Join(m.cfg.AgentsDir, a.ID),
		Image:    img,
		IP:       ip,
		MemoryMB: a.MemoryMB,
		Tools:    m.cfg.Tools,
	}, nil
}

// ErrNoEgress is returned when the egress proxy does not run here.
var ErrNoEgress = errors.New("the egress proxy is not running on this host")

// ErrQuota is returned when a user already has as many agents as allowed.
var ErrQuota = errors.New("agent limit reached")

// CreateAgent pins the current version of the requested image family and
// stores the agent. The sandbox is created on first wake.
func (m *Manager) CreateAgent(ctx context.Context, owner *store.User, a *store.Agent, family string) error {
	n, err := m.store.CountAgents(ctx, owner.ID)
	if err != nil {
		return err
	}
	if n >= owner.MaxAgents {
		return ErrQuota
	}
	img, err := m.cfg.Images.Current(family)
	if err != nil {
		return err
	}
	a.UserID = owner.ID
	a.Image = img
	if err := m.store.CreateAgent(ctx, a, m.cfg.Subnet); err != nil {
		return err
	}
	if owner.Egress {
		m.routeEgress(ctx, a, true)
	}
	return nil
}

// DeleteAgent tears down an agent's sandbox and all of its files.
func (m *Manager) DeleteAgent(ctx context.Context, agentID string) error {
	a, err := m.store.Agent(ctx, agentID)
	if err != nil {
		return err
	}
	m.actor(agentID).call(func(a *actor) error {
		if a.turn != nil {
			a.turn.kill(a.m)
		}
		return nil
	})
	m.dropActor(agentID)
	// Its address may go to another agent, which must not inherit its route.
	m.routeEgress(ctx, a, false)
	if spec, err := m.Spec(a); err == nil {
		if err := m.rt.Destroy(ctx, spec); err != nil {
			return err
		}
	} else {
		os.RemoveAll(filepath.Join(m.cfg.AgentsDir, agentID))
	}
	return m.store.DeleteAgent(ctx, agentID)
}

// Status is a snapshot of an agent's activity.
type Status struct {
	State      string
	Busy       bool
	Queued     int
	Holds      int
	LastActive time.Time
}

// Status reports what an agent is doing.
func (m *Manager) Status(agentID string) Status {
	var st Status
	m.mu.Lock()
	a := m.actors[agentID]
	m.mu.Unlock()
	if a == nil {
		return st
	}
	a.call(func(a *actor) error {
		st = Status{State: a.state, Busy: a.turn != nil, Queued: len(a.pending), Holds: a.holds, LastActive: a.lastActive}
		return nil
	})
	return st
}

// Prompt queues a message typed in the web UI.
func (m *Manager) Prompt(agentID, text string) {
	msg := Message{Text: text, FromMe: true, Owner: true, Time: time.Now(), SenderName: "web"}
	m.actor(agentID).do(func(a *actor) { a.enqueue(msg) })
}

// Wake boots or restores an agent's sandbox without running a turn.
func (m *Manager) Wake(ctx context.Context, agentID string) error {
	release, err := m.Hold(ctx, agentID)
	if err != nil {
		return err
	}
	release()
	return nil
}

// Sleep suspends an agent now.
func (m *Manager) Sleep(ctx context.Context, agentID string) (string, error) {
	var state string
	err := m.actor(agentID).call(func(a *actor) error {
		var err error
		state, err = a.suspend(ctx)
		return err
	})
	return state, err
}

// Abort stops an agent's running turn. It reports whether one was running.
func (m *Manager) Abort(agentID string) bool {
	var aborted bool
	m.actor(agentID).call(func(a *actor) error {
		aborted = a.abort()
		return nil
	})
	return aborted
}

// ErrBusy is returned when an agent is in the middle of a turn.
var ErrBusy = errors.New("the agent is in the middle of a turn")

// NewSession makes the agent's next turn start a fresh conversation.
func (m *Manager) NewSession(ctx context.Context, agentID string) error {
	return m.actor(agentID).call(func(a *actor) error {
		if a.turn != nil {
			return ErrBusy
		}
		a.todos, a.forkNext = nil, false
		return m.store.SetAgentSession(ctx, agentID, "")
	})
}

// Hold wakes an agent and keeps it awake until release is called. The web
// terminal and the Codex login hold their agent while they are open.
func (m *Manager) Hold(ctx context.Context, agentID string) (release func(), err error) {
	ag, err := m.store.Agent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	spec, err := m.Spec(ag)
	if err != nil {
		return nil, err
	}
	a := m.actor(agentID)
	a.call(func(a *actor) error { a.holds++; return nil })
	release = sync.OnceFunc(func() {
		a.do(func(a *actor) {
			a.holds--
			a.lastActive = time.Now()
		})
	})
	if _, err := m.rt.Wake(ctx, spec); err != nil {
		release()
		return nil, err
	}
	a.do(func(a *actor) { a.setState(sandbox.Running) })
	return release, nil
}

type nopMessenger struct{}

func (nopMessenger) SendText(context.Context, string, string, string) error { return nil }
func (nopMessenger) SendFile(context.Context, string, string, string, []byte, string) error {
	return nil
}
func (nopMessenger) SendPoll(context.Context, string, string, string, []string) error { return nil }
func (nopMessenger) React(context.Context, string, string, string, string, string) error {
	return nil
}
func (nopMessenger) Typing(context.Context, string, string, bool) error { return nil }
func (nopMessenger) MarkRead(context.Context, string, string, string, string) error {
	return nil
}

// NewerImage returns the newest version of an agent's image family, or ""
// if the agent already runs it.
func (m *Manager) NewerImage(a *store.Agent) string {
	latest, err := m.cfg.Images.Current(sandbox.Family(a.Image))
	if err != nil || latest == a.Image {
		return ""
	}
	return latest
}

// ErrUpToDate is returned when there is no newer image to move to.
var ErrUpToDate = errors.New("the agent already runs the newest image")

// UpgradeImage moves an agent to the newest version of its image. The
// sandbox is stopped (a checkpoint cannot be restored over a different
// image) and boots from the new one next time; every file the agent made
// is kept, since it lives in the agent's own layer.
func (m *Manager) UpgradeImage(ctx context.Context, agentID string) (string, error) {
	var latest string
	err := m.actor(agentID).call(func(a *actor) error {
		if a.turn != nil || a.holds > 0 {
			return ErrBusy
		}
		ag, err := m.store.Agent(ctx, agentID)
		if err != nil {
			return err
		}
		if latest = m.NewerImage(ag); latest == "" {
			return ErrUpToDate
		}
		spec, err := m.Spec(ag)
		if err != nil {
			return err
		}
		if a.state == sandbox.Running {
			m.scrubCredentials(ctx, spec)
		}
		if err := m.rt.Stop(ctx, spec); err != nil {
			return err
		}
		a.setState(sandbox.Stopped)
		return m.store.SetAgentImage(ctx, agentID, latest)
	})
	return latest, err
}

// OwnCopies lists the harnesses an agent installed over the image's.
func (m *Manager) OwnCopies(ag *store.Agent) []sandbox.OwnCopy {
	spec, err := m.Spec(ag)
	if err != nil {
		return nil
	}
	return spec.OwnCopies()
}

// UseImageHarnesses removes an agent's own Claude Code and pi, so it runs
// the image's again. The sandbox stops first, because its upper layer
// cannot change while mounted; files elsewhere are untouched.
func (m *Manager) UseImageHarnesses(ctx context.Context, agentID string) error {
	return m.actor(agentID).call(func(a *actor) error {
		if a.turn != nil || a.holds > 0 {
			return ErrBusy
		}
		ag, err := m.store.Agent(ctx, agentID)
		if err != nil {
			return err
		}
		spec, err := m.Spec(ag)
		if err != nil {
			return err
		}
		if a.state == sandbox.Running {
			m.scrubCredentials(ctx, spec)
		}
		if err := m.rt.Stop(ctx, spec); err != nil {
			return err
		}
		a.setState(sandbox.Stopped)
		return spec.DropOwnCopies()
	})
}

// UpdateAgent saves an agent's settings. Changing the harness starts a
// fresh conversation, because the two harnesses keep incompatible
// transcripts. A new memory limit applies from the next cold boot.
func (m *Manager) UpdateAgent(ctx context.Context, updated *store.Agent) error {
	return m.actor(updated.ID).call(func(a *actor) error {
		current, err := m.store.Agent(ctx, updated.ID)
		if err != nil {
			return err
		}
		switched := current.Harness != updated.Harness
		if switched && a.turn != nil {
			return ErrBusy
		}
		fitHarness(current, updated)
		if err := m.store.UpdateAgentSettings(ctx, updated); err != nil {
			return err
		}
		if switched {
			return m.store.SetAgentSession(ctx, updated.ID, "")
		}
		return nil
	})
}

// Wait blocks until background work the manager started has finished;
// call it after cancelling the context Start was given.
func (m *Manager) Wait() { m.bg.Wait() }
