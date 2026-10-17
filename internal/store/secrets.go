package store

import (
	"context"
	"time"
)

// Secret names. Values are always ciphertext produced by secrets.Box.
const (
	SecretClaudeToken   = "claude_oauth_token"
	SecretPiAuth        = "pi_auth_json"
	SecretDeepSeekKey   = "deepseek_api_key"
	SecretOpenRouterKey = "openrouter_api_key"
)

// PutSecret stores (or replaces) an encrypted secret.
func (s *Store) PutSecret(ctx context.Context, userID, name string, ciphertext []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_secrets (user_id, name, ciphertext, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, name) DO UPDATE SET ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		userID, name, ciphertext, ts(time.Now()))
	return err
}

// Secret returns an encrypted secret.
func (s *Store) Secret(ctx context.Context, userID, name string) ([]byte, error) {
	var ct []byte
	err := s.db.QueryRowContext(ctx, `SELECT ciphertext FROM user_secrets WHERE user_id = ? AND name = ?`, userID, name).Scan(&ct)
	return ct, one(err)
}

// SecretTimes returns when each of a user's secrets was last set.
func (s *Store) SecretTimes(ctx context.Context, userID string) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, updated_at FROM user_secrets WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var name, updated string
		if err := rows.Scan(&name, &updated); err != nil {
			return nil, err
		}
		out[name] = parseTS(updated)
	}
	return out, rows.Err()
}

// DeleteSecret removes a secret.
func (s *Store) DeleteSecret(ctx context.Context, userID, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_secrets WHERE user_id = ? AND name = ?`, userID, name)
	return err
}
