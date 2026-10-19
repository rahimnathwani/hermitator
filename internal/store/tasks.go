package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
)

// Who set up a task.
const (
	TaskByOwner = "owner"
	TaskByAgent = "agent"
)

// MaxTasks is how many tasks one agent may have.
const MaxTasks = 25

// ErrTooManyTasks is returned when an agent already has MaxTasks.
var ErrTooManyTasks = errors.New("too many scheduled tasks")

// Task is a prompt an agent runs later: once, or on a cron schedule.
type Task struct {
	ID        string
	AgentID   string
	AccountID string
	ChatJID   string
	Name      string
	Prompt    string
	Cron      string    // empty for a one-off
	NextRun   time.Time // zero once a one-off has run
	Paused    bool
	CreatedBy string
	LastRun   time.Time
	Runs      int
	CreatedAt time.Time
	// Check, if set, is a shell command run first each time: the task's
	// turn starts only if it prints something.
	Check string
	// CheckFailing is set once the owner has been told the check fails.
	CheckFailing bool
}

const taskColumns = `id, agent_id, account_id, chat_jid, name, prompt, cron, next_run, paused, created_by, last_run, runs, created_at, check_command, check_failing`

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	var next, last sql.NullString
	var created string
	if err := row.Scan(&t.ID, &t.AgentID, &t.AccountID, &t.ChatJID, &t.Name, &t.Prompt, &t.Cron, &next, &t.Paused,
		&t.CreatedBy, &last, &t.Runs, &created, &t.Check, &t.CheckFailing); err != nil {
		return nil, one(err)
	}
	t.NextRun, t.LastRun, t.CreatedAt = parseNullTS(next), parseNullTS(last), parseTS(created)
	return &t, nil
}

// CreateTask adds a task, refusing more than MaxTasks per agent.
func (s *Store) CreateTask(ctx context.Context, t *Task) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE agent_id = ? AND next_run IS NOT NULL`, t.AgentID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxTasks {
		return ErrTooManyTasks
	}
	t.ID, t.CreatedAt = ids.New(), time.Now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks (`+taskColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.AgentID, t.AccountID, t.ChatJID, t.Name, t.Prompt, t.Cron, nullTS(t.NextRun), t.Paused, t.CreatedBy,
		nullTS(t.LastRun), t.Runs, ts(t.CreatedAt), t.Check, t.CheckFailing); err != nil {
		return err
	}
	return tx.Commit()
}

// Tasks lists an agent's tasks that will still run, soonest first.
func (s *Store) Tasks(ctx context.Context, agentID string) ([]*Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks WHERE agent_id = ? AND next_run IS NOT NULL ORDER BY next_run`, agentID)
}

// DueTasks lists unpaused tasks whose time has come.
func (s *Store) DueTasks(ctx context.Context, now time.Time) ([]*Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks WHERE paused = 0 AND next_run IS NOT NULL AND next_run <= ? ORDER BY next_run`, ts(now))
}

func (s *Store) tasks(ctx context.Context, query string, args ...any) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Task returns one task.
func (s *Store) Task(ctx context.Context, id string) (*Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
}

// TaskRan records a run and when the task runs next (zero: never again).
func (s *Store) TaskRan(ctx context.Context, id string, at, next time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE tasks SET last_run = ?, next_run = ?, runs = runs + 1 WHERE id = ?`, ts(at), nullTS(next), id))
}

// SetTaskPaused pauses or resumes a task; resuming sets its next run.
func (s *Store) SetTaskPaused(ctx context.Context, id string, paused bool, next time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE tasks SET paused = ?, next_run = COALESCE(?, next_run) WHERE id = ?`, paused, nullTS(next), id))
}

// SetTaskCheckFailing records whether the owner knows the task's check fails.
func (s *Store) SetTaskCheckFailing(ctx context.Context, id string, failing bool) error {
	return affected(s.db.ExecContext(ctx, `UPDATE tasks SET check_failing = ? WHERE id = ?`, failing, id))
}

// DeleteTask removes a task.
func (s *Store) DeleteTask(ctx context.Context, id string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id))
}
