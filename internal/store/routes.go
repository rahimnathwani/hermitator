package store

import (
	"context"
	"time"
)

// Route sends one WhatsApp chat to one agent.
type Route struct {
	AccountID      string
	ChatJID        string
	ChatName       string
	AgentID        string
	RequireMention bool
	CreatedAt      time.Time
}

const routeColumns = `account_id, chat_jid, chat_name, agent_id, require_mention, created_at`

func scanRoute(row interface{ Scan(...any) error }) (*Route, error) {
	var r Route
	var created string
	if err := row.Scan(&r.AccountID, &r.ChatJID, &r.ChatName, &r.AgentID, &r.RequireMention, &created); err != nil {
		return nil, one(err)
	}
	r.CreatedAt = parseTS(created)
	return &r, nil
}

// PutRoute binds a chat to an agent, replacing any previous binding.
func (s *Store) PutRoute(ctx context.Context, r *Route) error {
	r.CreatedAt = time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO routes (`+routeColumns+`) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id, chat_jid) DO UPDATE SET
			chat_name = excluded.chat_name, agent_id = excluded.agent_id,
			require_mention = excluded.require_mention, created_at = excluded.created_at`,
		r.AccountID, r.ChatJID, r.ChatName, r.AgentID, r.RequireMention, ts(r.CreatedAt))
	return err
}

// Route returns the binding for a chat.
func (s *Store) Route(ctx context.Context, accountID, chatJID string) (*Route, error) {
	return scanRoute(s.db.QueryRowContext(ctx, `SELECT `+routeColumns+` FROM routes WHERE account_id = ? AND chat_jid = ?`, accountID, chatJID))
}

// RoutesForAgent lists the chats bound to an agent.
func (s *Store) RoutesForAgent(ctx context.Context, agentID string) ([]*Route, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+routeColumns+` FROM routes WHERE agent_id = ? ORDER BY created_at`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Route
	for rows.Next() {
		r, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRoute unbinds a chat.
func (s *Store) DeleteRoute(ctx context.Context, accountID, chatJID string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM routes WHERE account_id = ? AND chat_jid = ?`, accountID, chatJID))
}

// PutLinkCode replaces an agent's /link code.
func (s *Store) PutLinkCode(ctx context.Context, agentID, code string, ttl time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM link_codes WHERE agent_id = ? OR expires_at <= ?`, agentID, ts(time.Now())); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO link_codes (code, agent_id, expires_at) VALUES (?, ?, ?)`, code, agentID, ts(time.Now().Add(ttl))); err != nil {
		return err
	}
	return tx.Commit()
}

// LinkCode returns an agent's current unexpired code and its expiry.
func (s *Store) LinkCode(ctx context.Context, agentID string) (string, time.Time, error) {
	var code, expires string
	err := s.db.QueryRowContext(ctx, `SELECT code, expires_at FROM link_codes WHERE agent_id = ? AND expires_at > ?`, agentID, ts(time.Now())).Scan(&code, &expires)
	if err != nil {
		return "", time.Time{}, one(err)
	}
	return code, parseTS(expires), nil
}

// ConsumeLinkCode deletes a valid code and returns the agent it was for.
func (s *Store) ConsumeLinkCode(ctx context.Context, code string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var agentID string
	if err := tx.QueryRowContext(ctx, `SELECT agent_id FROM link_codes WHERE code = ? AND expires_at > ?`, code, ts(time.Now())).Scan(&agentID); err != nil {
		return "", one(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM link_codes WHERE code = ?`, code); err != nil {
		return "", err
	}
	return agentID, tx.Commit()
}
