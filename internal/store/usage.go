package store

import (
	"context"
	"time"
)

// UsageWindow is one subscription window as last reported.
type UsageWindow struct {
	Source      string
	Name        string
	Utilization float64
	ResetsAt    time.Time
	UpdatedAt   time.Time
}

// PutUsage records the latest reading of a subscription window.
func (s *Store) PutUsage(ctx context.Context, userID string, w UsageWindow) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage_windows (user_id, source, name, utilization, resets_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, source, name) DO UPDATE SET utilization = excluded.utilization, resets_at = excluded.resets_at, updated_at = excluded.updated_at`,
		userID, w.Source, w.Name, w.Utilization, ts(w.ResetsAt), ts(time.Now()))
	return err
}

// Usage lists a user's windows, by source and name.
func (s *Store) Usage(ctx context.Context, userID string) ([]UsageWindow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source, name, utilization, resets_at, updated_at FROM usage_windows WHERE user_id = ? ORDER BY source, name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageWindow
	for rows.Next() {
		var w UsageWindow
		var resets, updated string
		if err := rows.Scan(&w.Source, &w.Name, &w.Utilization, &resets, &updated); err != nil {
			return nil, err
		}
		w.ResetsAt, w.UpdatedAt = parseTS(resets), parseTS(updated)
		out = append(out, w)
	}
	return out, rows.Err()
}

// Conversation is one session an agent has had with its harness, in one
// chat.
type Conversation struct {
	AgentID     string
	AccountID   string
	ChatJID     string
	SessionID   string
	Harness     string
	FirstPrompt string
	Turns       int
	CostTotal   float64
	StartedAt   time.Time
	LastAt      time.Time
}

// SessionTurn records a turn in a chat's session, adding the session if
// new.
func (s *Store) SessionTurn(ctx context.Context, k ThreadKey, sessionID, harness, prompt string) error {
	now := ts(time.Now())
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_sessions (agent_id, account_id, chat_jid, session_id, harness, first_prompt, turns, started_at, last_at) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (agent_id, session_id) DO UPDATE SET turns = turns + 1, last_at = excluded.last_at`,
		k.AgentID, k.AccountID, k.ChatJID, sessionID, harness, prompt, now, now)
	return err
}

// SetSessionCost records Claude Code's running total for a session.
func (s *Store) SetSessionCost(ctx context.Context, agentID, sessionID string, total float64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agent_sessions SET cost_total = ? WHERE agent_id = ? AND session_id = ?`, total, agentID, sessionID)
	return err
}

// Sessions lists the conversations an agent has had in one chat, most
// recent first.
func (s *Store) Sessions(ctx context.Context, k ThreadKey, limit int) ([]*Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id, account_id, chat_jid, session_id, harness, first_prompt, turns, cost_total, started_at, last_at
		FROM agent_sessions WHERE agent_id = ? AND account_id = ? AND chat_jid = ? ORDER BY last_at DESC LIMIT ?`, k.AgentID, k.AccountID, k.ChatJID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Conversation
	for rows.Next() {
		var x Conversation
		var started, last string
		if err := rows.Scan(&x.AgentID, &x.AccountID, &x.ChatJID, &x.SessionID, &x.Harness, &x.FirstPrompt, &x.Turns, &x.CostTotal, &started, &last); err != nil {
			return nil, err
		}
		x.StartedAt, x.LastAt = parseTS(started), parseTS(last)
		out = append(out, &x)
	}
	return out, rows.Err()
}
