package orchestrator

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/spool"
	"github.com/rahimnathwani/hermitator/internal/store"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// actor owns one agent's runtime state. Its fields are only touched on
// its own goroutine; other goroutines send it closures with do and call.
type actor struct {
	m      *Manager
	id     string
	inbox  chan func(*actor)
	ctx    context.Context
	cancel context.CancelFunc

	pending  []Message
	debounce *time.Timer
	turn     *activeTurn
	holds    int
	// awake is the owner's /awake, one of the holds.
	awake      *ownerHold
	lastActive time.Time
	state      string

	// outbox serialises outbox delivery between a turn and the background
	// tick, which run outside the actor; bgSent and wakes, under it,
	// rate-limit what the agent sends between turns and the turns its
	// background jobs start.
	outbox sync.Mutex
	bgSent []time.Time
	wakes  []time.Time
	// inBackground keeps one background delivery at a time per agent.
	inBackground atomic.Bool

	// todos is the agent's own task list from its latest turn, for /plan
	// in the chat that turn answered.
	todos       []turn.Todo
	todosAt     time.Time
	todosThread store.ThreadKey
	// forkNext makes a chat's next turn branch off its conversation.
	forkNext map[store.ThreadKey]bool
	// lastSystem is the system prompt of each conversation's last turn: a
	// review before sleep reuses it, to read the conversation from the
	// cache.
	lastSystem map[store.ThreadKey]string
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
	if t := a.turn; t != nil && t.steerTo != "" && t.steerTo == chatKey(msg) && msg.steerable() {
		if msg.ID != "" {
			t.messages = append(t.messages, msg.ID)
		}
		t.steerSeq++
		t.steering.Add(1)
		seq := t.steerSeq
		go func() {
			defer t.steering.Done()
			a.m.steer(a.id, t, seq, msg)
		}()
		a.lastActive = time.Now()
		return
	}
	if a.turn != nil && msg.ID != "" {
		// Behind a turn it may wait a while; say it arrived.
		msg.Queued = true
		go chatRef{m: a.m, msg: msg}.react(queuedReaction)
	}
	a.pending = append(a.pending, msg)
	a.lastActive = time.Now()
	if a.debounce != nil {
		a.debounce.Stop()
	}
	a.debounce = time.NewTimer(a.m.cfg.Debounce)
}

// queuedReaction marks a message waiting for the turn in progress.
const queuedReaction = "📥"

func (a *actor) startTurn() {
	if a.turn != nil || len(a.pending) == 0 {
		return
	}
	// One turn answers one chat, in its conversation; messages from other
	// chats wait their turn.
	first := chatKey(a.pending[0])
	var batch, rest []Message
	for _, msg := range a.pending {
		if chatKey(msg) == first {
			batch = append(batch, msg)
		} else {
			rest = append(rest, msg)
		}
	}
	a.pending = rest
	t := &activeTurn{id: ids.New(), started: time.Now()}
	for _, msg := range batch {
		if msg.TaskID != "" {
			t.tasks = append(t.tasks, msg.TaskID)
		}
		if msg.ID != "" {
			t.messages = append(t.messages, msg.ID)
		}
	}
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

// hasTask reports whether a run of the task is queued or running.
func (a *actor) hasTask(id string) bool {
	for _, msg := range a.pending {
		if msg.TaskID == id {
			return true
		}
	}
	return a.turn != nil && slices.Contains(a.turn.tasks, id)
}

func (a *actor) abort() bool {
	if a.turn == nil {
		return false
	}
	a.turn.kill(a.m)
	return true
}

func (a *actor) reap() {
	if a.turn != nil || a.state != sandbox.Running {
		return
	}
	ag, err := a.m.store.Agent(a.ctx, a.id)
	if err != nil {
		return
	}
	spec, err := a.m.Spec(ag)
	if err != nil {
		return
	}
	// Between turns, deliver what background jobs send, and answer their
	// requests; outside the actor, since sending takes a while.
	if a.m.ctx.Err() == nil && a.inBackground.CompareAndSwap(false, true) {
		a.m.bg.Add(1)
		go func() {
			defer a.m.bg.Done()
			defer a.inBackground.Store(false)
			a.m.background(spec, ag)
		}()
	}
	// A hold (/awake, the terminal) keeps it up, but what its background
	// jobs send still goes out.
	if a.holds > 0 || time.Since(a.lastActive) < ag.IdleTimeout || spec.Holding(time.Now()) {
		return
	}
	if a.sleepWork(ag) {
		return // suspended once it is done
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
	// tasks are the scheduled tasks whose runs this turn answers.
	tasks []string
	// started is when the turn began; progress is its latest sign of
	// life and the step it was on, for the stall warning and /status.
	started  time.Time
	progress atomic.Pointer[stallState]
	// messages are the IDs of the chat messages the turn answers, those
	// that joined it included; a ❌ on one of them stops it.
	messages []string

	mu      sync.Mutex
	client  *spool.Client
	aborted bool

	// steerTo, set by the actor once a Claude turn is running, is the chat
	// whose new messages go into the running turn rather than the queue.
	steerTo  string
	steerSeq int
	steering sync.WaitGroup
}

// forgetThread drops what the actor kept about a conversation that is
// starting afresh.
func (a *actor) forgetThread(k store.ThreadKey) {
	if a.todosThread == k {
		a.todos = nil
	}
	delete(a.forkNext, k)
	delete(a.lastSystem, k)
}

// forgetThreads drops what the actor kept about every conversation.
func (a *actor) forgetThreads() {
	a.todos, a.forkNext, a.lastSystem = nil, nil, nil
}

func chatKey(m Message) string {
	return m.AccountID + "\x00" + m.Chat + "\x00" + m.ThreadAccount + "\x00" + m.ThreadChat
}

// steerable is a plain message: one that changes nothing about how the
// turn runs, so it can join a turn already under way.
func (m Message) steerable() bool {
	return m.Prompt == "" && m.Model == "" && m.Effort == "" && m.TaskName == "" && m.JobName == ""
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
