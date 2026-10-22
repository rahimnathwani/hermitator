package store

import (
	"context"
	"database/sql"
	"time"
)

// ThreadKey names one chat's conversation with an agent. The web console's
// own conversation has no account or chat.
type ThreadKey struct {
	AgentID   string
	AccountID string
	ChatJID   string
}

// Thread is where one chat's conversation with an agent stands.
type Thread struct {
	ThreadKey
	// SessionID is the harness session the chat continues; empty until
	// its first turn, and after /new.
	SessionID string
	// SessionCost is the running total Claude Code last reported for the
	// session, so the next turn can record only its own share.
	SessionCost float64
	// ContextTokens is the conversation's size as of its last turn; 0
	// once compacted, or after /new.
	ContextTokens int
	// ReviewTurns counts turns with a user message since the conversation
	// was last reviewed for things to remember.
	ReviewTurns int
	// LastAt is when a turn last finished in it.
	LastAt time.Time
}

const threadColumns = `agent_id, account_id, chat_jid, session_id, session_cost, context_tokens, review_turns, last_at`

func scanThread(row interface{ Scan(...any) error }) (*Thread, error) {
	var t Thread
	var last sql.NullString
	if err := row.Scan(&t.AgentID, &t.AccountID, &t.ChatJID, &t.SessionID, &t.SessionCost, &t.ContextTokens, &t.ReviewTurns, &last); err != nil {
		return nil, err
	}
	t.LastAt = parseNullTS(last)
	return &t, nil
}

// Thread returns a chat's conversation with an agent; one that has not
// started yet comes back empty, not as an error.
func (s *Store) Thread(ctx context.Context, k ThreadKey) (*Thread, error) {
	t, err := scanThread(s.db.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM chat_threads WHERE agent_id = ? AND account_id = ? AND chat_jid = ?`,
		k.AgentID, k.AccountID, k.ChatJID))
	if err == sql.ErrNoRows {
		return &Thread{ThreadKey: k}, nil
	}
	return t, err
}

// Threads lists an agent's conversations, most recently active first.
func (s *Store) Threads(ctx context.Context, agentID string) ([]*Thread, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+threadColumns+` FROM chat_threads WHERE agent_id = ? ORDER BY last_at DESC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// upsertThread applies set, an assignment list, to a conversation, adding
// it if new.
func (s *Store) upsertThread(ctx context.Context, k ThreadKey, set string, args ...any) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO chat_threads (agent_id, account_id, chat_jid) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		k.AgentID, k.AccountID, k.ChatJID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE chat_threads SET `+set+` WHERE agent_id = ? AND account_id = ? AND chat_jid = ?`,
		append(args, k.AgentID, k.AccountID, k.ChatJID)...)
	return err
}

// SetThreadSession records the harness session a chat continues. A new
// session starts with no running cost and no size.
func (s *Store) SetThreadSession(ctx context.Context, k ThreadKey, sessionID string) error {
	return s.upsertThread(ctx, k, `session_id = ?, session_cost = 0, context_tokens = 0`, sessionID)
}

// SetThreadSessionCost records the session and its running cost together.
func (s *Store) SetThreadSessionCost(ctx context.Context, k ThreadKey, sessionID string, cost float64) error {
	return s.upsertThread(ctx, k, `session_id = ?, session_cost = ?`, sessionID, cost)
}

// SetThreadContext records the size of a conversation.
func (s *Store) SetThreadContext(ctx context.Context, k ThreadKey, tokens int) error {
	return s.upsertThread(ctx, k, `context_tokens = ?`, tokens)
}

// TouchThread records that a turn in the conversation has just finished.
// Compactions and reviews do not count: they come after the last turn.
func (s *Store) TouchThread(ctx context.Context, k ThreadKey) error {
	return s.upsertThread(ctx, k, `last_at = ?`, ts(time.Now()))
}

// CountReviewTurn counts a turn with a user message towards the next review.
func (s *Store) CountReviewTurn(ctx context.Context, k ThreadKey) error {
	return s.upsertThread(ctx, k, `review_turns = review_turns + 1`)
}

// ResetReviewTurns starts the count again, once a review has run.
func (s *Store) ResetReviewTurns(ctx context.Context, k ThreadKey) error {
	return s.upsertThread(ctx, k, `review_turns = 0`)
}

// ClearThreads makes every chat start afresh, as switching harness does.
func (s *Store) ClearThreads(ctx context.Context, agentID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM chat_threads WHERE agent_id = ?`, agentID)
	return err
}
