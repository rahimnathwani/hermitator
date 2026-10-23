package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// maxAwake caps /awake, like the twelve hours a guest hold lasts.
const maxAwake = 12 * time.Hour

// ownerHold is the owner's /awake: the release of a Hold, and when it ends.
type ownerHold struct {
	release func()
	until   time.Time
	timer   *time.Timer
}

// awakeCommand handles /awake: keeping the sandbox from sleeping, so a
// background job runs on. Only a turn or the agent itself could hold it
// before, or the panel's terminal while it is open.
func (m *Manager) awakeCommand(agentID, arg string, loc *time.Location, reply func(string)) {
	arg = strings.ToLower(strings.TrimSpace(arg))
	a := m.actor(agentID)
	switch arg {
	case "":
		var until time.Time
		a.call(func(a *actor) error {
			if a.awake != nil {
				until = a.awake.until
			}
			return nil
		})
		if until.IsZero() {
			reply("Nothing you set is keeping it awake; it sleeps once idle. /awake 2h keeps it awake for two hours.")
			return
		}
		reply(fmt.Sprintf("☕ Kept awake until %s. /awake off lets it sleep.", until.In(loc).Format("Mon 15:04")))
		return
	case "off", "stop", "0":
		m.setAwake(a, nil)
		reply("😴 It may sleep again once idle.")
		return
	}
	d, err := time.ParseDuration(days(arg))
	if err != nil || d <= 0 {
		reply("Usage: /awake 2h keeps the sandbox awake for two hours, up to 12h; /awake off ends it; /awake shows it.")
		return
	}
	d = min(max(d, time.Minute), maxAwake)
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.ShellTimeout)
	defer cancel()
	release, err := m.Hold(ctx, agentID)
	if err != nil {
		reply("⚠️ Could not wake the sandbox: " + err.Error())
		return
	}
	h := &ownerHold{release: release, until: time.Now().Add(d)}
	h.timer = time.AfterFunc(d, func() {
		var mine bool
		a.call(func(a *actor) error {
			if mine = a.awake == h; mine {
				a.awake = nil
			}
			return nil
		})
		if mine {
			release()
		}
	})
	m.setAwake(a, h)
	reply(fmt.Sprintf("☕ Kept awake until %s, unless hermitator restarts first. /awake off lets it sleep sooner.", h.until.In(loc).Format("Mon 15:04")))
}

// setAwake replaces the owner's hold, releasing the one before.
func (m *Manager) setAwake(a *actor, h *ownerHold) {
	var old *ownerHold
	a.call(func(a *actor) error {
		old, a.awake = a.awake, h
		return nil
	})
	// Release goes through the actor itself, so not from inside call.
	if old != nil {
		old.timer.Stop()
		old.release()
	}
}
