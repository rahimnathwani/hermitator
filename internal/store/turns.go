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
	ID        string
	AgentID   string
	AccountID string
	ChatJID   string
	Harness   string
	Model     string
	SessionID string
	Status    string
	Input     string
	// Prompt is everything the turn sent the agent, for /retry.
	Prompt     string
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

const turnColumns = `id, agent_id, account_id, chat_jid, harness, model, session_id, status, input, prompt, output, tools, wake, wake_ms, cost_usd, started_at, finished_at`

func scanTurn(row interface{ Scan(...any) error }) (*Turn, error) {
	var t Turn
	var started string
	var finished sql.NullString
	if err := row.Scan(&t.ID, &t.AgentID, &t.AccountID, &t.ChatJID, &t.Harness, &t.Model, &t.SessionID, &t.Status, &t.Input, &t.Prompt, &t.Output,
		&t.Tools, &t.Wake, &t.WakeMS, &t.CostUSD, &started, &finished); err != nil {
		return nil, one(err)
	}
	t.StartedAt, t.FinishedAt = parseTS(started), parseNullTS(finished)
	return &t, nil
}

// CreateTurn inserts a running turn, assigning an ID unless one is set.
func (s *Store) CreateTurn(ctx context.Context, t *Turn) error {
	if t.ID == "" {
		t.ID = ids.New()
	}
	t.StartedAt = time.Now()
	t.Status = TurnRunning
	_, err := s.db.ExecContext(ctx, `INSERT INTO turns (`+turnColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.AgentID, t.AccountID, t.ChatJID, t.Harness, t.Model, t.SessionID, t.Status, t.Input, t.Prompt, t.Output, t.Tools, t.Wake, t.WakeMS, t.CostUSD, ts(t.StartedAt), nil)
	return err
}

// FinishTurn records a turn's outcome.
func (s *Store) FinishTurn(ctx context.Context, t *Turn) error {
	t.FinishedAt = time.Now()
	return affected(s.db.ExecContext(ctx, `UPDATE turns SET status = ?, model = ?, output = ?, tools = ?, wake = ?, wake_ms = ?, cost_usd = ?, finished_at = ? WHERE id = ?`,
		t.Status, t.Model, t.Output, t.Tools, t.Wake, t.WakeMS, t.CostUSD, ts(t.FinishedAt), t.ID))
}

// InterruptRunningTurns marks turns left running by a previous process
// and returns them, so their chats can be told.
func (s *Store) InterruptRunningTurns(ctx context.Context) ([]*Turn, error) {
	rows, err := s.db.QueryContext(ctx, `UPDATE turns SET status = ?, finished_at = ? WHERE status = ? RETURNING `+turnColumns,
		TurnInterrupted, ts(time.Now()), TurnRunning)
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

// LastChatTurn is an agent's most recent turn in one chat.
func (s *Store) LastChatTurn(ctx context.Context, agentID, accountID, chat string) (*Turn, error) {
	return scanTurn(s.db.QueryRowContext(ctx, `SELECT `+turnColumns+` FROM turns WHERE agent_id = ? AND account_id = ? AND chat_jid = ?
		ORDER BY started_at DESC LIMIT 1`, agentID, accountID, chat))
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
