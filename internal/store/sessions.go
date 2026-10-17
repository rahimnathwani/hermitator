package store

import (
	"context"
	"time"
)

// Session is a logged-in browser.
type Session struct {
	TokenHash   string
	ActorID     string
	EffectiveID string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// Impersonating reports whether an admin is acting as someone else.
func (s *Session) Impersonating() bool { return s.ActorID != s.EffectiveID }

// CreateSession stores a session for actor, acting as themselves.
func (s *Store) CreateSession(ctx context.Context, tokenHash, actorID string, ttl time.Duration) (*Session, error) {
	now := time.Now()
	sess := &Session{TokenHash: tokenHash, ActorID: actorID, EffectiveID: actorID, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, actor_id, effective_id, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		sess.TokenHash, sess.ActorID, sess.EffectiveID, ts(sess.CreatedAt), ts(sess.ExpiresAt))
	return sess, err
}

// Session returns an unexpired session by token hash.
func (s *Store) Session(ctx context.Context, tokenHash string) (*Session, error) {
	var sess Session
	var created, expires string
	err := s.db.QueryRowContext(ctx, `SELECT token_hash, actor_id, effective_id, created_at, expires_at FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, ts(time.Now())).Scan(&sess.TokenHash, &sess.ActorID, &sess.EffectiveID, &created, &expires)
	if err != nil {
		return nil, one(err)
	}
	sess.CreatedAt, sess.ExpiresAt = parseTS(created), parseTS(expires)
	return &sess, nil
}

// SetEffectiveUser switches whose resources a session acts on.
func (s *Store) SetEffectiveUser(ctx context.Context, tokenHash, effectiveID string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE sessions SET effective_id = ? WHERE token_hash = ?`, effectiveID, tokenHash))
}

// DeleteSession logs a browser out.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// PruneSessions deletes expired sessions.
func (s *Store) PruneSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, ts(time.Now()))
	return err
}
