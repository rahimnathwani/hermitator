package store

import (
	"context"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
)

// Roles.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// User is someone who can log into the web UI.
type User struct {
	ID           string
	Email        string
	PasswordHash string
	Role         string
	MaxAgents    int
	Suspended    bool
	CreatedAt    time.Time
}

// IsAdmin reports whether the user administers the instance.
func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }

const userColumns = `id, email, password_hash, role, max_agents, suspended, created_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created string
	if err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.MaxAgents, &u.Suspended, &created); err != nil {
		return nil, one(err)
	}
	u.CreatedAt = parseTS(created)
	return &u, nil
}

// NormalizeEmail lowercases and trims an address for storage and lookup.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// CreateUser inserts u, assigning an ID and creation time.
func (s *Store) CreateUser(ctx context.Context, u *User) error {
	u.ID = ids.New()
	u.Email = NormalizeEmail(u.Email)
	u.CreatedAt = time.Now()
	if u.MaxAgents == 0 {
		u.MaxAgents = 5
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO users (`+userColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.PasswordHash, u.Role, u.MaxAgents, u.Suspended, ts(u.CreatedAt))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// User returns the user with the given ID.
func (s *Store) User(ctx context.Context, id string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// UserByEmail returns the user with the given address.
func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ?`, NormalizeEmail(email)))
}

// Users lists every user, admins first.
func (s *Store) Users(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY role = 'user', email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetPassword replaces a user's password hash and ends their sessions.
func (s *Store) SetPassword(ctx context.Context, id, hash string) error {
	if err := affected(s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE actor_id = ?`, id)
	return err
}

// SetSuspended suspends or reinstates a user. Suspension ends their sessions.
func (s *Store) SetSuspended(ctx context.Context, id string, suspended bool) error {
	if err := affected(s.db.ExecContext(ctx, `UPDATE users SET suspended = ? WHERE id = ?`, suspended, id)); err != nil {
		return err
	}
	if suspended {
		_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE actor_id = ? OR effective_id = ?`, id, id)
		return err
	}
	return nil
}

// SetMaxAgents changes a user's agent quota.
func (s *Store) SetMaxAgents(ctx context.Context, id string, n int) error {
	return affected(s.db.ExecContext(ctx, `UPDATE users SET max_agents = ? WHERE id = ?`, n, id))
}

// DeleteUser removes a user and, by cascade, everything they own.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id))
}
