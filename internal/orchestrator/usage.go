package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/store"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// Subscription usage, as Claude Code reports it with each turn: how much
// of the five-hour and weekly allowance is used, and when each resets.

var windowNames = map[string]struct {
	label  string
	length time.Duration
}{
	"five_hour":        {"5-hour", 5 * time.Hour},
	"seven_day":        {"weekly", 7 * 24 * time.Hour},
	"seven_day_opus":   {"weekly Opus", 7 * 24 * time.Hour},
	"seven_day_sonnet": {"weekly Sonnet", 7 * 24 * time.Hour},
}

// recordUsage stores a usage report, and warns the chat once when a
// window crosses 90%.
func (m *Manager) recordUsage(ctx context.Context, ag *store.Agent, windows []turn.UsageWindow, chat chatRef) {
	before := map[string]float64{}
	if old, err := m.store.Usage(ctx, ag.UserID); err == nil {
		for _, w := range old {
			if w.Source == store.HarnessClaude && time.Now().Before(w.ResetsAt) {
				before[w.Name] = w.Utilization
			}
		}
	}
	for _, w := range windows {
		m.store.PutUsage(ctx, ag.UserID, store.UsageWindow{Source: store.HarnessClaude, Name: w.Name, Utilization: w.Utilization, ResetsAt: time.Unix(w.ResetsAt, 0)})
		if before[w.Name] < 0.9 && w.Utilization >= 0.9 && chat.ok() {
			chat.send(fmt.Sprintf("📊 Your Claude %s allowance is %.0f%% used; it resets %s.", windowLabel(w.Name), 100*w.Utilization,
				resetsIn(time.Unix(w.ResetsAt, 0), m.location(ctx, ag))))
		}
	}
}

func windowLabel(name string) string {
	if w, ok := windowNames[name]; ok {
		return w.label
	}
	return strings.ReplaceAll(name, "_", " ")
}

func resetsIn(at time.Time, loc *time.Location) string {
	d := time.Until(at)
	switch {
	case d <= 0:
		return "now"
	case d < 24*time.Hour:
		return fmt.Sprintf("in %s (%s)", humanize(d), at.In(loc).Format("15:04"))
	}
	return "on " + at.In(loc).Format("Mon 2 Jan 15:04")
}

func humanize(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	}
	d = d.Round(time.Minute)
	if h := int(d.Hours()); h > 0 {
		return fmt.Sprintf("%dh%02dm", h, int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// pace compares use with time: positive when ahead of an even spread.
func pace(w store.UsageWindow, length time.Duration, now time.Time) (ahead float64, hitsAt time.Time) {
	elapsed := length - w.ResetsAt.Sub(now)
	if elapsed <= 0 || elapsed > length {
		return 0, time.Time{}
	}
	ahead = w.Utilization - float64(elapsed)/float64(length)
	if w.Utilization > 0 {
		full := now.Add(time.Duration(float64(elapsed) * (1 - w.Utilization) / w.Utilization))
		if full.Before(w.ResetsAt) {
			hitsAt = full
		}
	}
	return ahead, hitsAt
}

// usageText answers /usage.
func (m *Manager) usageText(ctx context.Context, ag *store.Agent) string {
	u, err := m.store.User(ctx, ag.UserID)
	if err != nil {
		return ""
	}
	loc, now := u.Location(), time.Now()
	var b strings.Builder
	windows, _ := m.store.Usage(ctx, u.ID)
	if len(windows) == 0 {
		b.WriteString("No Claude subscription usage reported yet; Claude Code reports it with each turn.")
	} else {
		b.WriteString("*Claude subscription*")
		for _, w := range windows {
			if now.After(w.ResetsAt) {
				fmt.Fprintf(&b, "\n%s: reset since the last reading", windowLabel(w.Name))
				continue
			}
			fmt.Fprintf(&b, "\n%s: %.0f%% used, resets %s", windowLabel(w.Name), 100*w.Utilization, resetsIn(w.ResetsAt, loc))
			if info, ok := windowNames[w.Name]; ok {
				ahead, hits := pace(w, info.length, now)
				switch {
				case !hits.IsZero():
					fmt.Fprintf(&b, "; at this pace, used up %s", resetsIn(hits, loc))
				case ahead > 0.05:
					b.WriteString("; ahead of an even pace")
				}
			}
		}
		fmt.Fprintf(&b, "\n_As of %s ago._", humanize(now.Sub(windows[0].UpdatedAt)))
	}
	day, month := capWindow(u, now)
	spentDay, _ := m.store.SpentSince(ctx, u.ID, day)
	spentMonth, _ := m.store.SpentSince(ctx, u.ID, month)
	fmt.Fprintf(&b, "\n\n*At API prices*\nToday: $%.2f%s\nThis month: $%.2f%s", spentDay, capNote(u.DailyCap), spentMonth, capNote(u.MonthlyCap))
	return b.String()
}

func capNote(limit float64) string {
	if limit <= 0 {
		return ""
	}
	return fmt.Sprintf(" of a $%.2f limit", limit)
}

// planText answers /plan from the agent's last task list, without waking
// it; only the chat whose turn made the list sees it.
func (m *Manager) planText(ag *store.Agent, k store.ThreadKey) string {
	var todos []turn.Todo
	var at time.Time
	m.actor(ag.ID).call(func(a *actor) error {
		if a.todosThread == k {
			todos, at = a.todos, a.todosAt
		}
		return nil
	})
	if len(todos) == 0 {
		return "*" + ag.Name + "* has not made a task list in this conversation. Agents make one for bigger jobs; ask for progress instead."
	}
	marks := map[string]string{"completed": "✅", "in_progress": "▶️"}
	var b strings.Builder
	fmt.Fprintf(&b, "*%s's plan* (updated %s ago)", ag.Name, humanize(time.Since(at)))
	for _, t := range todos {
		mark := marks[t.Status]
		if mark == "" {
			mark = "◻️"
		}
		fmt.Fprintf(&b, "\n%s %s", mark, t.Content)
	}
	return b.String()
}
