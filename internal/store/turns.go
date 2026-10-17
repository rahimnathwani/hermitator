package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
)

// Turn outcomes.
const (
	TurnRunning     = "running"
	TurnOK          = "ok"
	TurnError       = "error"
	TurnAborted     = "aborted"
	TurnInterrupted = "interrupted"
)

// Turn is one prompt-and-reply cycle of an agent.
type Turn struct {
	ID         string
	AgentID    string
	AccountID  string
	ChatJID    string
	Harness    string
	Status     string
	Input      string
	Output     string
	Tools      int
	Wake       string
	WakeMS     int
	CostUSD    float64
	StartedAt  time.Time
	FinishedAt time.Time
}

// Duration is how long the turn took, or has taken so far.
func (t *Turn) Duration() time.Duration {
	end := t.FinishedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(t.StartedAt)
}

const turnColumns = `id, agent_id, account_id, chat_jid, harness, status, input, output, tools, wake, wake_ms, cost_usd, started_at, finished_at`

func scanTurn(row interface{ Scan(...any) error }) (*Turn, error) {
	var t Turn
	var started string
	var finished sql.NullString
	if err := row.Scan(&t.ID, &t.AgentID, &t.AccountID, &t.ChatJID, &t.Harness, &t.Status, &t.Input, &t.Output,
		&t.Tools, &t.Wake, &t.WakeMS, &t.CostUSD, &started, &finished); err != nil {
		return nil, one(err)
	}
	t.StartedAt, t.FinishedAt = parseTS(started), parseNullTS(finished)
	return &t, nil
}

// CreateTurn inserts a running turn.
func (s *Store) CreateTurn(ctx context.Context, t *Turn) error {
	t.ID = ids.New()
	t.StartedAt = time.Now()
	t.Status = TurnRunning
	_, err := s.db.ExecContext(ctx, `INSERT INTO turns (`+turnColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.AgentID, t.AccountID, t.ChatJID, t.Harness, t.Status, t.Input, t.Output, t.Tools, t.Wake, t.WakeMS, t.CostUSD, ts(t.StartedAt), nil)
	return err
}

// FinishTurn records a turn's outcome.
func (s *Store) FinishTurn(ctx context.Context, t *Turn) error {
	t.FinishedAt = time.Now()
	return affected(s.db.ExecContext(ctx, `UPDATE turns SET status = ?, output = ?, tools = ?, wake = ?, wake_ms = ?, cost_usd = ?, finished_at = ? WHERE id = ?`,
		t.Status, t.Output, t.Tools, t.Wake, t.WakeMS, t.CostUSD, ts(t.FinishedAt), t.ID))
}

// InterruptRunningTurns marks turns left running by a previous process.
func (s *Store) InterruptRunningTurns(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE turns SET status = ?, finished_at = ? WHERE status = ?`, TurnInterrupted, ts(time.Now()), TurnRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Turns lists an agent's most recent turns, newest first.
func (s *Store) Turns(ctx context.Context, agentID string, limit int) ([]*Turn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+turnColumns+` FROM turns WHERE agent_id = ? ORDER BY started_at DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Turn
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Turn returns a single turn.
func (s *Store) Turn(ctx context.Context, id string) (*Turn, error) {
	return scanTurn(s.db.QueryRowContext(ctx, `SELECT `+turnColumns+` FROM turns WHERE id = ?`, id))
}
