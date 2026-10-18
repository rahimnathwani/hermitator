package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/rahimnathwani/hermitator/internal/store"
)

// Spending caps, in the API-equivalent dollars turns are costed in. They
// matter most for API keys, and for anything that runs unattended, such
// as scheduled tasks; on a subscription they guard against runaways.

// capWindow is the start of today and of this month in the user's zone.
func capWindow(u *store.User, now time.Time) (day, month time.Time) {
	loc := u.Location()
	now = now.In(loc)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc),
		time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
}

// overCap says why a turn may not start, or "" if it may.
func (m *Manager) overCap(ctx context.Context, ag *store.Agent) string {
	u, err := m.store.User(ctx, ag.UserID)
	if err != nil || (u.DailyCap <= 0 && u.MonthlyCap <= 0) {
		return ""
	}
	day, month := capWindow(u, time.Now())
	if u.DailyCap > 0 {
		if spent, _ := m.store.SpentSince(ctx, u.ID, day); spent >= u.DailyCap {
			return fmt.Sprintf("💸 Today's spending limit is reached: $%.2f of $%.2f. Raise it in hermitator's Settings, or wait until tomorrow.", spent, u.DailyCap)
		}
	}
	if u.MonthlyCap > 0 {
		if spent, _ := m.store.SpentSince(ctx, u.ID, month); spent >= u.MonthlyCap {
			return fmt.Sprintf("💸 This month's spending limit is reached: $%.2f of $%.2f. Raise it in hermitator's Settings.", spent, u.MonthlyCap)
		}
	}
	return ""
}

// capWarning says so once, when a turn takes spending past 80% of a cap.
func (m *Manager) capWarning(ctx context.Context, chat chatRef, rec *store.Turn) {
	if rec.CostUSD <= 0 || !chat.ok() {
		return
	}
	ag, err := m.store.Agent(ctx, rec.AgentID)
	if err != nil {
		return
	}
	u, err := m.store.User(ctx, ag.UserID)
	if err != nil {
		return
	}
	day, month := capWindow(u, time.Now())
	for _, c := range []struct {
		limit float64
		since time.Time
		name  string
	}{{u.DailyCap, day, "today's"}, {u.MonthlyCap, month, "this month's"}} {
		if c.limit <= 0 {
			continue
		}
		after, _ := m.store.SpentSince(ctx, u.ID, c.since)
		before := after - rec.CostUSD
		if before < 0.8*c.limit && after >= 0.8*c.limit && after < c.limit {
			chat.send(fmt.Sprintf("💸 $%.2f of %s $%.2f spending limit used.", after, c.name, c.limit))
		}
	}
}
