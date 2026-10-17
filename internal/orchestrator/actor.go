package orchestrator

import (
	"context"
	"sync"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/spool"
)

// actor owns one agent's runtime state. Its fields are only touched on
// its own goroutine; other goroutines send it closures with do and call.
type actor struct {
	m      *Manager
	id     string
	inbox  chan func(*actor)
	ctx    context.Context
	cancel context.CancelFunc

	pending    []Message
	debounce   *time.Timer
	turn       *activeTurn
	holds      int
	lastActive time.Time
	state      string
}

func (a *actor) loop(ctx context.Context) {
	a.ctx = ctx
	for {
		var fire <-chan time.Time
		if a.debounce != nil {
			fire = a.debounce.C
		}
		select {
		case <-ctx.Done():
			return
		case f := <-a.inbox:
			f(a)
		case <-fire:
			a.debounce = nil
			a.startTurn()
		}
	}
}

// do runs f on the actor's goroutine without waiting for it.
func (a *actor) do(f func(*actor)) {
	select {
	case a.inbox <- f:
	case <-a.m.ctx.Done():
	}
}

// call runs f on the actor's goroutine and waits for its result. It must
// not be used from the actor's own goroutine.
func (a *actor) call(f func(*actor) error) error {
	done := make(chan error, 1)
	a.do(func(a *actor) { done <- f(a) })
	select {
	case err := <-done:
		return err
	case <-a.m.ctx.Done():
		return a.m.ctx.Err()
	}
}

func (a *actor) enqueue(msg Message) {
	a.pending = append(a.pending, msg)
	a.lastActive = time.Now()
	if a.debounce != nil {
		a.debounce.Stop()
	}
	a.debounce = time.NewTimer(a.m.cfg.Debounce)
}

func (a *actor) startTurn() {
	if a.turn != nil || len(a.pending) == 0 {
		return
	}
	batch := a.pending
	a.pending = nil
	t := &activeTurn{id: ids.New()}
	a.turn = t
	go func() {
		ok := a.m.runTurn(a, t, batch)
		a.do(func(a *actor) {
			a.turn = nil
			a.lastActive = time.Now()
			if ok {
				a.setState(sandbox.Running)
			}
			if a.debounce == nil {
				a.startTurn()
			}
		})
	}()
}

func (a *actor) abort() bool {
	if a.turn == nil {
		return false
	}
	a.turn.kill(a.m)
	return true
}

func (a *actor) reap() {
	if a.turn != nil || a.holds > 0 || a.state != sandbox.Running {
		return
	}
	ag, err := a.m.store.Agent(a.ctx, a.id)
	if err != nil {
		return
	}
	if time.Since(a.lastActive) < ag.IdleTimeout {
		return
	}
	if st, err := a.suspend(a.ctx); err != nil {
		a.m.log.Error("suspend idle agent", "agent", a.id, "err", err)
	} else {
		a.m.log.Info("suspended idle agent", "agent", a.id, "state", st, "idle", time.Since(a.lastActive).Round(time.Second).String())
	}
}

// suspend frees the agent's memory unless something is using it.
func (a *actor) suspend(ctx context.Context) (string, error) {
	if a.turn != nil || a.holds > 0 {
		return a.state, ErrBusy
	}
	ag, err := a.m.store.Agent(ctx, a.id)
	if err != nil {
		return "", err
	}
	spec, err := a.m.Spec(ag)
	if err != nil {
		return "", err
	}
	if a.state == sandbox.Running {
		a.m.scrubCredentials(ctx, spec)
	}
	st, err := a.m.rt.Suspend(ctx, spec)
	a.setState(st)
	return st, err
}

func (a *actor) setState(st string) {
	if st == "" || st == a.state {
		return
	}
	a.state = st
	a.m.store.SetAgentState(a.m.ctx, a.id, st)
	a.m.hub.publish(a.id, UIEvent{Kind: "state", State: st})
}

// activeTurn is shared between the actor (which may abort it) and the
// goroutine running it.
type activeTurn struct {
	id string

	mu      sync.Mutex
	client  *spool.Client
	aborted bool
}

func (t *activeTurn) kill(m *Manager) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.aborted = true
	if t.client != nil {
		t.client.Kill(t.id)
	}
}

// submitted records where the turn's job went, killing it at once if an
// abort arrived first.
func (t *activeTurn) submitted(c *spool.Client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.client = c
	if t.aborted {
		c.Kill(t.id)
	}
}

func (t *activeTurn) wasAborted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.aborted
}
