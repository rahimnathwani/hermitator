package store

import (
	"context"
	"strings"
	"time"
	"unicode"
)

// NormalizePhone reduces a phone number to its digits, international
// format without the plus.
func NormalizePhone(phone string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, phone)
}

// AddOwnerNumber records that a phone number belongs to a user. Adding a
// number twice is not an error.
func (s *Store) AddOwnerNumber(ctx context.Context, userID, phone string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO owner_numbers (user_id, phone, created_at) VALUES (?, ?, ?) ON CONFLICT (user_id, phone) DO NOTHING`,
		userID, NormalizePhone(phone), ts(time.Now()))
	return err
}

// OwnerNumbers lists a user's numbers.
func (s *Store) OwnerNumbers(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT phone FROM owner_numbers WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// IsOwnerNumber reports whether phone belongs to the user.
func (s *Store) IsOwnerNumber(ctx context.Context, userID, phone string) bool {
	phone = NormalizePhone(phone)
	if phone == "" {
		return false
	}
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM owner_numbers WHERE user_id = ? AND phone = ?`, userID, phone).Scan(&n)
	return n > 0
}

// DeleteOwnerNumber forgets one of a user's numbers.
func (s *Store) DeleteOwnerNumber(ctx context.Context, userID, phone string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM owner_numbers WHERE user_id = ? AND phone = ?`, userID, NormalizePhone(phone)))
}
