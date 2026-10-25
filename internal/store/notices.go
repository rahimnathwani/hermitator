package store

import (
	"context"
	"time"
)

// NoticeSent reports whether a user has been sent a notice of this kind.
func (s *Store) NoticeSent(ctx context.Context, userID, kind string) bool {
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE user_id = ? AND kind = ?`, userID, kind).Scan(&n)
	return n > 0
}

// MarkNotice records that a user has been sent a notice of this kind.
func (s *Store) MarkNotice(ctx context.Context, userID, kind string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO notices (user_id, kind, sent_at) VALUES (?, ?, ?)
		ON CONFLICT (user_id, kind) DO UPDATE SET sent_at = excluded.sent_at`, userID, kind, ts(time.Now()))
	return err
}

// ClearNotice forgets the notices whose kind starts with prefix, so they
// can be sent again: a new token makes old warnings about it moot.
func (s *Store) ClearNotice(ctx context.Context, userID, prefix string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notices WHERE user_id = ? AND substr(kind, 1, ?) = ?`, userID, len(prefix), prefix)
	return err
}
