package store

import (
	"context"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
)

// Unsent is a text on its way to a chat.
type Unsent struct {
	ID        string
	AccountID string
	ChatJID   string
	Text      string
	Attempts  int
	CreatedAt time.Time
}

// QueueSends writes down texts about to be sent to a chat, in order,
// due for a retry at retryAt unless DeleteSend crosses them off first.
func (s *Store) QueueSends(ctx context.Context, accountID, chatJID string, texts []string, retryAt time.Time) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := ts(time.Now())
	out := make([]string, len(texts))
	for i, text := range texts {
		out[i] = ids.New()
		if _, err := tx.ExecContext(ctx, `INSERT INTO unsent (id, account_id, chat_jid, text, next_try, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			out[i], accountID, chatJID, text, ts(retryAt), now); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

// DeleteSend crosses off a text that has gone.
func (s *Store) DeleteSend(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM unsent WHERE id = ?`, id)
	return err
}

// SendFailed records a failed attempt and when to try again.
func (s *Store) SendFailed(ctx context.Context, id string, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE unsent SET attempts = attempts + 1, next_try = ? WHERE id = ?`, ts(next), id)
	return err
}

// DueSends returns texts due for another try, oldest first.
func (s *Store) DueSends(ctx context.Context, now time.Time) ([]*Unsent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, account_id, chat_jid, text, attempts, created_at FROM unsent
		WHERE next_try <= ? ORDER BY rowid`, ts(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Unsent
	for rows.Next() {
		var u Unsent
		var created string
		if err := rows.Scan(&u.ID, &u.AccountID, &u.ChatJID, &u.Text, &u.Attempts, &created); err != nil {
			return nil, err
		}
		u.CreatedAt = parseTS(created)
		out = append(out, &u)
	}
	return out, rows.Err()
}
