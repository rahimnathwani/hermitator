package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/schedule"
	"github.com/rahimnathwani/hermitator/internal/store"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// Scheduled tasks. The host keeps the timetable, so an agent with tasks
// still sleeps, and uses no memory, until one is due. A task runs as a
// message to the agent in the chat it was set up from, in the agent's
// usual conversation, and the reply is headed with the task's name.

const (
	// minTaskGap is the shortest time between runs of one task: anything
	// more frequent would keep the agent awake for good.
	minTaskGap    = 5 * time.Minute
	maxTaskAhead  = 366 * 24 * time.Hour
	schedulerTick = 20 * time.Second
)

func (m *Manager) scheduler() {
	tick := time.NewTicker(schedulerTick)
	defer tick.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-tick.C:
			m.runDueTasks(now)
		}
	}
}

// runDueTasks starts every task whose time has come. A task missed while
// hermitator was down runs once when it is back.
func (m *Manager) runDueTasks(now time.Time) {
	due, err := m.store.DueTasks(m.ctx, now)
	if err != nil {
		m.log.Error("due tasks", "err", err)
		return
	}
	for _, t := range due {
		ag, err := m.store.Agent(m.ctx, t.AgentID)
		if err != nil {
			continue
		}
		loc := m.location(m.ctx, ag)
		var next time.Time
		if t.Cron != "" {
			if c, err := schedule.Parse(t.Cron); err == nil {
				next = c.Next(now, loc)
			}
		}
		m.store.TaskRan(m.ctx, t.ID, now, next)
		if !m.stillLinked(m.ctx, t) {
			m.store.SetTaskPaused(m.ctx, t.ID, true, time.Time{})
			m.log.Warn("paused a task whose chat is no longer linked", "agent", t.AgentID, "task", t.ID)
			continue
		}
		by := "the owner"
		if t.CreatedBy == store.TaskByAgent {
			by = "you"
		}
		when := "once"
		if t.Cron != "" {
			when = "on the schedule " + t.Cron
		}
		msg := Message{
			AccountID: t.AccountID, Chat: t.ChatJID, Owner: true, Time: now, TaskName: t.Name,
			Prompt: fmt.Sprintf("[Scheduled task %q, set up by %s on %s, running %s. Do what it asks; your reply goes to this chat.]\n\n%s",
				t.Name, by, t.CreatedAt.In(loc).Format("2 Jan 2006"), when, t.Prompt),
		}
		m.log.Info("running scheduled task", "agent", t.AgentID, "task", t.ID)
		m.actor(t.AgentID).do(func(a *actor) { a.enqueue(msg) })
		if spec, err := m.Spec(ag); err == nil {
			m.writeTasks(m.ctx, spec, ag)
		}
	}
}

// stillLinked reports whether the task's chat still reaches its agent.
func (m *Manager) stillLinked(ctx context.Context, t *store.Task) bool {
	if route, err := m.store.Route(ctx, t.AccountID, t.ChatJID); err == nil {
		return route.AgentID == t.AgentID
	}
	acct, err := m.store.Account(ctx, t.AccountID)
	return err == nil && acct.FallbackAgentID == t.AgentID
}

func (m *Manager) location(ctx context.Context, ag *store.Agent) *time.Location {
	if u, err := m.store.User(ctx, ag.UserID); err == nil {
		return u.Location()
	}
	return time.UTC
}

// newTask validates a schedule request and works out the first run.
func (m *Manager) newTask(ctx context.Context, ag *store.Agent, chat chatRef, req turn.HostRequest, by string) (*store.Task, error) {
	if !chat.ok() {
		return nil, errors.New("tasks need a WhatsApp chat to run in, and this message did not come from one")
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, errors.New("say what the task should do")
	}
	if len(prompt) > 4000 {
		return nil, errors.New("the prompt is over 4000 characters; keep the detail in a file and point to it")
	}
	loc, now := m.location(ctx, ag), time.Now()
	t := &store.Task{AgentID: ag.ID, AccountID: chat.msg.AccountID, ChatJID: chat.msg.Chat, Prompt: prompt, CreatedBy: by}
	switch {
	case req.Cron != "":
		c, err := schedule.Parse(req.Cron)
		if err != nil {
			return nil, err
		}
		if gap := c.MinGap(now, loc); gap < minTaskGap {
			return nil, fmt.Errorf("%q runs every %s; the most often a task may run is every %s", req.Cron, gap, minTaskGap)
		}
		t.Cron, t.NextRun = c.String(), c.Next(now, loc)
		if t.NextRun.IsZero() {
			return nil, fmt.Errorf("%q never runs", req.Cron)
		}
	case req.At != "":
		at, err := turn.ParseAt(req.At, now, loc)
		if err != nil {
			return nil, err
		}
		t.NextRun = at
	case req.In != "":
		d, err := time.ParseDuration(req.In)
		if err != nil {
			return nil, fmt.Errorf("--in %q: use a duration such as 45m or 2h30m", req.In)
		}
		t.NextRun = now.Add(d)
	default:
		return nil, errors.New("say when: --cron, --at or --in")
	}
	if !t.NextRun.After(now) {
		return nil, fmt.Errorf("%s is in the past", t.NextRun.In(loc).Format("Mon 2 Jan 15:04"))
	}
	if t.NextRun.Sub(now) > maxTaskAhead {
		return nil, errors.New("that is more than a year away")
	}
	t.Name = strings.TrimSpace(req.Name)
	if t.Name == "" {
		t.Name = clip(strings.Join(strings.Fields(prompt), " "), 60)
	}
	t.Name = clip(t.Name, 80)
	if err := m.store.CreateTask(ctx, t); errors.Is(err, store.ErrTooManyTasks) {
		return nil, fmt.Errorf("this agent already has %d tasks; cancel some first", store.MaxTasks)
	} else if err != nil {
		return nil, err
	}
	return t, nil
}

// describeTask is one line for the chat and the agent's task list.
func describeTask(t *store.Task, loc *time.Location) string {
	when := "once"
	if t.Cron != "" {
		when = "cron " + t.Cron
	}
	next := "paused"
	if !t.Paused {
		next = "next " + t.NextRun.In(loc).Format("Mon 2 Jan 15:04")
	}
	return fmt.Sprintf("%s (%s, %s)", t.Name, when, next)
}

// writeTasks keeps the agent's copy of its task list current, for
// `hermitator-guest tasks`.
func (m *Manager) writeTasks(ctx context.Context, spec *sandbox.Spec, ag *store.Agent) {
	list, err := m.store.Tasks(ctx, ag.ID)
	if err != nil {
		return
	}
	loc := m.location(ctx, ag)
	out := []turn.TaskInfo{}
	for _, t := range list {
		sched := "once"
		if t.Cron != "" {
			sched = t.Cron
		}
		out = append(out, turn.TaskInfo{ID: shortID(t.ID), Name: t.Name, Schedule: sched, Paused: t.Paused,
			NextRun: t.NextRun.In(loc).Format("2006-01-02 15:04 MST"), By: t.CreatedBy, Prompt: clip(t.Prompt, 300)})
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	writeShared(spec, turn.TasksFile, data)
}

func shortID(id string) string { return id[:min(6, len(id))] }

// findTask finds an agent's task by its short ID or its number in /tasks.
func (m *Manager) findTask(ctx context.Context, agentID, ref string) (*store.Task, error) {
	list, err := m.store.Tasks(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if n, err := strconv.Atoi(ref); err == nil && n >= 1 && n <= len(list) {
		return list[n-1], nil
	}
	for _, t := range list {
		if ref != "" && strings.HasPrefix(t.ID, ref) {
			return t, nil
		}
	}
	return nil, store.ErrNotFound
}

// handleRequests answers what guest commands asked of the host.
func (m *Manager) handleRequests(ctx context.Context, spec *sandbox.Spec, ag *store.Agent, chat chatRef) {
	root, err := spec.Shared()
	if err != nil {
		return
	}
	defer root.Close()
	dir, err := root.Open(turn.RequestsDir)
	if err != nil {
		return
	}
	entries, _ := dir.ReadDir(-1)
	dir.Close()
	for _, e := range entries {
		name := e.Name()
		path := turn.RequestsDir + "/" + name
		if strings.HasSuffix(name, ".answer.json") {
			// The guest gave up waiting; don't let answers pile up.
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 10*time.Minute {
				root.Remove(path)
			}
			continue
		}
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") || !e.Type().IsRegular() {
			continue
		}
		raw, err := readRegular(root, path)
		root.Remove(path)
		var req turn.HostRequest
		if err == nil {
			err = json.Unmarshal(raw, &req)
		}
		answer := turn.HostAnswer{}
		if err != nil {
			answer.Message = "unreadable request: " + err.Error()
		} else {
			answer = m.answer(ctx, spec, ag, chat, req)
		}
		data, _ := json.Marshal(answer)
		root.WriteFile(strings.TrimSuffix(path, ".json")+".answer.json", data, 0o644)
	}
}

func (m *Manager) answer(ctx context.Context, spec *sandbox.Spec, ag *store.Agent, chat chatRef, req turn.HostRequest) turn.HostAnswer {
	loc := m.location(ctx, ag)
	switch req.Op {
	case "schedule":
		t, err := m.newTask(ctx, ag, chat, req, store.TaskByAgent)
		if err != nil {
			return turn.HostAnswer{Message: "Not scheduled: " + err.Error()}
		}
		m.writeTasks(ctx, spec, ag)
		// The owner hears about it from hermitator, not only the agent.
		chat.send("📅 Scheduled by " + ag.Name + ": " + describeTask(t, loc) + ". /tasks to see or cancel it.")
		return turn.HostAnswer{OK: true, Message: fmt.Sprintf("Scheduled as %s: %s. The owner has been told.", shortID(t.ID), describeTask(t, loc))}
	case "unschedule":
		t, err := m.findTask(ctx, ag.ID, req.Task)
		if err != nil {
			return turn.HostAnswer{Message: "No task " + req.Task + "; see hermitator-guest tasks."}
		}
		m.store.DeleteTask(ctx, t.ID)
		m.writeTasks(ctx, spec, ag)
		chat.send("📅 " + ag.Name + " cancelled a task: " + t.Name + ".")
		return turn.HostAnswer{OK: true, Message: "Cancelled " + t.Name + "."}
	}
	return turn.HostAnswer{Message: "unknown request " + req.Op}
}

// tasksCommand handles /tasks.
func (m *Manager) tasksCommand(ctx context.Context, ag *store.Agent, arg string, reply func(string)) {
	loc := m.location(ctx, ag)
	verb, ref, _ := strings.Cut(strings.TrimSpace(arg), " ")
	verb = strings.ToLower(verb)
	if verb != "" {
		t, err := m.findTask(ctx, ag.ID, strings.TrimSpace(ref))
		if err != nil {
			reply("No such task. /tasks lists them with their numbers.")
			return
		}
		switch verb {
		case "pause":
			m.PauseTask(ctx, ag, t, true)
			reply("⏸ Paused: " + t.Name + ".")
		case "resume":
			next, _ := m.PauseTask(ctx, ag, t, false)
			reply("▶️ Resumed: " + t.Name + ", next " + next.In(loc).Format("Mon 2 Jan 15:04") + ".")
		case "delete", "cancel", "rm":
			m.DeleteTask(ctx, ag, t)
			reply("🗑 Deleted: " + t.Name + ".")
		default:
			reply("Use /tasks, /tasks pause N, /tasks resume N or /tasks delete N.")
		}
		return
	}
	list, _ := m.store.Tasks(ctx, ag.ID)
	if len(list) == 0 {
		reply("*" + ag.Name + "* has no scheduled tasks. Ask it to do something later, for example \"every weekday at 9, check CI and tell me\".")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*%s's scheduled tasks* (%s)\n", ag.Name, loc)
	for i, t := range list {
		fmt.Fprintf(&b, "\n%d. %s", i+1, describeTask(t, loc))
	}
	b.WriteString("\n\n/tasks pause N, /tasks resume N, /tasks delete N")
	reply(b.String())
}

// timezoneCommand handles /timezone.
func (m *Manager) timezoneCommand(ctx context.Context, ag *store.Agent, arg string, reply func(string)) {
	u, err := m.store.User(ctx, ag.UserID)
	if err != nil {
		return
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		reply(fmt.Sprintf("Schedules use %s; it is %s there. Change it with /timezone Europe/London (any IANA name).", u.Location(), time.Now().In(u.Location()).Format("Mon 15:04")))
		return
	}
	loc, err := time.LoadLocation(arg)
	if err != nil || arg == "Local" {
		reply(fmt.Sprintf("%q is not a time zone name; use one like Europe/London or America/Los_Angeles.", arg))
		return
	}
	m.store.SetTimezone(ctx, u.ID, loc.String())
	reply(fmt.Sprintf("🕐 Schedules now use %s, where it is %s. Existing tasks keep their next run until it comes round.", loc, time.Now().In(loc).Format("Mon 15:04")))
}

// PauseTask pauses or resumes a task. A resumed task runs next at its
// schedule's next time, or, for a one-off whose time has passed, in a
// minute.
func (m *Manager) PauseTask(ctx context.Context, ag *store.Agent, t *store.Task, paused bool) (time.Time, error) {
	var next time.Time
	if !paused {
		next = time.Now().Add(time.Minute)
		if t.Cron != "" {
			if c, err := schedule.Parse(t.Cron); err == nil {
				next = c.Next(time.Now(), m.location(ctx, ag))
			}
		} else if t.NextRun.After(time.Now()) {
			next = t.NextRun
		}
	}
	err := m.store.SetTaskPaused(ctx, t.ID, paused, next)
	m.refreshTasks(ctx, ag)
	return next, err
}

// DeleteTask removes a task.
func (m *Manager) DeleteTask(ctx context.Context, ag *store.Agent, t *store.Task) error {
	err := m.store.DeleteTask(ctx, t.ID)
	m.refreshTasks(ctx, ag)
	return err
}

// Location is the time zone an agent's schedules use: its owner's.
func (m *Manager) Location(ctx context.Context, ag *store.Agent) *time.Location {
	return m.location(ctx, ag)
}

func (m *Manager) refreshTasks(ctx context.Context, ag *store.Agent) {
	if spec, err := m.Spec(ag); err == nil {
		m.writeTasks(ctx, spec, ag)
	}
}
