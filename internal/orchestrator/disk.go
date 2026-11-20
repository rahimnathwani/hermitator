package orchestrator

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rahimnathwani/hermitator/internal/media"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// Agents write to the host's own filesystem (an OverlayFS upper layer
// has no quota), which hermitator's database, checkpoints and, on a
// shared server, other people's apps also need. Nothing stops a runaway
// build or log from filling it, so the disk guard measures instead, and
// acts once on each change:
//
//   - An agent whose files pass its limit is stopped, which ends a
//     runaway process, and its turns wait until it is back under. It can
//     still be woken by /sh and the Terminal tab, to clean up, and it is
//     not stopped again for staying over.
//   - When the disk's free space falls under the reserve, every running
//     agent is stopped (not checkpointed: that would write more) and all
//     turns wait until there is a quarter more than the reserve free.
//
// Waiting messages stay queued, never dropped, and each chat is told once
// why its message is waiting.

const defaultAgentDisk = 50 << 30

// diskGuard is what the guard last found; under mu, read by actors.
type diskGuard struct {
	mu   sync.Mutex
	low  bool
	free int64
	// over holds the agents past their limit, with their use.
	over map[string]int64
}

func (m *Manager) diskLimit() int64 {
	switch {
	case m.cfg.AgentDiskLimit < 0:
		return 0
	case m.cfg.AgentDiskLimit == 0:
		return defaultAgentDisk
	}
	return m.cfg.AgentDiskLimit
}

// diskReserve is the free space under which turns stop: the configured
// amount, or the smaller of a tenth of the disk and 20 GiB.
func (m *Manager) diskReserve(total int64) int64 {
	switch {
	case m.cfg.DiskReserve < 0:
		return 0
	case m.cfg.DiskReserve > 0:
		return m.cfg.DiskReserve
	}
	return min(total/10, 20<<30)
}

// diskHold says why an agent's turns must wait, or "" if they need not.
func (m *Manager) diskHold(agentID, name string) string {
	m.disk.mu.Lock()
	defer m.disk.mu.Unlock()
	if m.disk.low {
		return fmt.Sprintf("⏸ The server is almost out of disk space (%s free), so this waits for *%s* until there is room. The host operators have been told.", media.HumanSize(m.disk.free), name)
	}
	if used, ok := m.disk.over[agentID]; ok {
		return fmt.Sprintf("⏸ *%s* is using %s of disk, more than the %s an agent may, so this waits. Free some space with /sh (for example `/sh du -xsh /root /workspace/* | sort -h`) or from its Terminal tab, and it carries on by itself.", name, media.HumanSize(used), media.HumanSize(m.diskLimit()))
	}
	return ""
}

// diskStatus is /status's line about the guard, or "".
func (m *Manager) diskStatus(agentID string) string {
	m.disk.mu.Lock()
	defer m.disk.mu.Unlock()
	if m.disk.low {
		return "⏸ Turns wait: the server is almost out of disk space"
	}
	if used, ok := m.disk.over[agentID]; ok {
		return fmt.Sprintf("⏸ Turns wait: %s of files is over the %s limit; free some with /sh", media.HumanSize(used), media.HumanSize(m.diskLimit()))
	}
	return ""
}

// waitForDisk keeps the queued messages back while there is no room for
// a turn, telling each chat once.
func (a *actor) waitForDisk() bool {
	name := a.id
	if ag, err := a.m.store.Agent(a.ctx, a.id); err == nil {
		name = ag.Name
	}
	why := a.m.diskHold(a.id, name)
	if why == "" {
		return false
	}
	for _, msg := range a.pending {
		k := chatKey(msg)
		if a.diskNoted[k] || msg.Chat == "" {
			continue
		}
		if a.diskNoted == nil {
			a.diskNoted = map[string]bool{}
		}
		a.diskNoted[k] = true
		go chatRef{m: a.m, msg: msg}.send(why)
	}
	return true
}

func (m *Manager) diskLoop(ctx context.Context) {
	if m.cfg.DiskInterval < 0 {
		return
	}
	t := time.NewTicker(m.cfg.DiskInterval)
	defer t.Stop()
	for {
		m.checkDisk(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// checkDisk measures, and acts on what changed since the last time.
func (m *Manager) checkDisk(ctx context.Context) {
	agents, err := m.store.Agents(ctx, "")
	if err != nil {
		return
	}
	byID := map[string]*store.Agent{}
	for _, ag := range agents {
		byID[ag.ID] = ag
	}

	var becameLow, recovered bool
	if free, total, err := m.cfg.DiskFree(m.cfg.AgentsDir); err != nil {
		m.log.Warn("disk guard: free space", "err", err)
	} else if reserve := m.diskReserve(total); reserve > 0 {
		m.disk.mu.Lock()
		was := m.disk.low
		if was {
			m.disk.low = free < reserve+reserve/4
		} else {
			m.disk.low = free < reserve
		}
		m.disk.free = free
		becameLow, recovered = m.disk.low && !was, was && !m.disk.low
		m.disk.mu.Unlock()
		if becameLow {
			m.log.Error("disk guard: free space under the reserve; stopping agents", "free", free, "reserve", reserve)
		}
	}

	// Only a running agent's files can grow, and only one over its limit
	// can come back under.
	var newlyOver, back []string
	if limit := m.diskLimit(); limit > 0 {
		m.disk.mu.Lock()
		over := m.disk.over
		m.disk.mu.Unlock()
		measured := map[string]int64{}
		for _, ag := range agents {
			if ag.State != sandbox.Running {
				continue
			}
			spec, err := m.Spec(ag)
			if err != nil {
				continue
			}
			used, err := m.cfg.DiskUsed(spec)
			if err != nil {
				continue
			}
			measured[ag.ID] = used
		}
		m.disk.mu.Lock()
		if m.disk.over == nil {
			m.disk.over = map[string]int64{}
		}
		for id, used := range measured {
			_, was := over[id]
			switch {
			case used > limit:
				m.disk.over[id] = used
				if !was {
					newlyOver = append(newlyOver, id)
				}
			case was:
				delete(m.disk.over, id)
				back = append(back, id)
			}
		}
		// Agents deleted since.
		for id := range m.disk.over {
			if byID[id] == nil {
				delete(m.disk.over, id)
			}
		}
		m.disk.mu.Unlock()
	}

	if becameLow {
		var stopped []string
		for _, ag := range agents {
			if ag.State == sandbox.Running {
				m.diskStop(ag)
				stopped = append(stopped, ag.Name)
			}
		}
		m.NotifyOperators(ctx, m.lowDiskNote(agents, stopped))
	}
	for _, id := range newlyOver {
		ag := byID[id]
		m.log.Warn("disk guard: agent over its limit; stopping it", "agent", id, "used", m.diskUsedOf(id))
		m.diskStop(ag)
		m.notifyUser(ctx, ag.UserID, fmt.Sprintf("⏸ *%s* has %s of files, more than the %s an agent may, so it was stopped: its files are kept, but background processes ended, and its turns wait until it is back under. Free some space with /sh or from its Terminal tab.",
			ag.Name, media.HumanSize(m.diskUsedOf(id)), media.HumanSize(m.diskLimit())))
	}
	if recovered {
		m.NotifyOperators(ctx, "✅ The server has disk space again; agents' turns carry on.")
	}
	if recovered {
		m.mu.Lock()
		for id := range m.actors {
			back = append(back, id)
		}
		m.mu.Unlock()
	}
	slices.Sort(back)
	for _, id := range slices.Compact(back) {
		m.actor(id).do(func(a *actor) {
			a.diskNoted = nil
			if a.debounce == nil {
				a.startTurn()
			}
		})
	}
}

func (m *Manager) diskUsedOf(id string) int64 {
	m.disk.mu.Lock()
	defer m.disk.mu.Unlock()
	return m.disk.over[id]
}

// diskStop stops an agent's sandbox, after its turn if one is running.
func (m *Manager) diskStop(ag *store.Agent) {
	m.actor(ag.ID).do(func(a *actor) {
		if a.busy() {
			a.stopAfterTurn = true
			a.abort()
			return
		}
		a.stopForDisk()
	})
}

func (a *actor) stopForDisk() {
	ag, err := a.m.store.Agent(a.ctx, a.id)
	if err != nil {
		return
	}
	spec, err := a.m.Spec(ag)
	if err != nil {
		return
	}
	if a.state == sandbox.Running {
		a.m.scrubCredentials(a.ctx, spec)
	}
	if err := a.m.rt.Stop(a.ctx, spec); err != nil {
		a.m.log.Error("disk guard: stop agent", "agent", a.id, "err", err)
		return
	}
	a.setState(sandbox.Stopped)
}

// lowDiskNote tells the host operators what happened and which agents
// take the most.
func (m *Manager) lowDiskNote(agents []*store.Agent, stopped []string) string {
	m.disk.mu.Lock()
	free := m.disk.free
	m.disk.mu.Unlock()
	type use struct {
		name string
		used int64
	}
	var uses []use
	for _, ag := range agents {
		if spec, err := m.Spec(ag); err == nil {
			if used, err := m.cfg.DiskUsed(spec); err == nil {
				uses = append(uses, use{ag.Name, used})
			}
		}
	}
	sort.Slice(uses, func(i, j int) bool { return uses[i].used > uses[j].used })
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ The server has only %s of disk free, under hermitator's reserve, so agents' turns wait until there is room.", media.HumanSize(free))
	if len(stopped) > 0 {
		fmt.Fprintf(&b, " Stopped, with their files kept: %s.", strings.Join(stopped, ", "))
	}
	if len(uses) > 0 {
		b.WriteString("\nThe largest agents:")
		for _, u := range uses[:min(5, len(uses))] {
			fmt.Fprintf(&b, "\n%s: %s", u.name, media.HumanSize(u.used))
		}
	}
	b.WriteString("\n/host shows the disk.")
	return b.String()
}

func statfsFree(dir string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return int64(uint64(st.Bavail) * bs), int64(uint64(st.Blocks) * bs), nil
}
