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
	// Timezone is an IANA name for the owner's schedules; empty is UTC.
	Timezone string
	// DailyCap and MonthlyCap limit API-equivalent spending; 0 is none.
	DailyCap   float64
	MonthlyCap float64
	// GitName and GitEmail are who the user's agents commit as.
	GitName  string
	GitEmail string
	// GitHubLogin is the account the user's GitHub token signs in as.
	GitHubLogin string
	// Egress keeps the user's credentials out of their sandboxes.
	Egress    bool
	CreatedAt time.Time
}

// Location is the user's time zone, falling back to UTC.
func (u *User) Location() *time.Location {
	if loc, err := time.LoadLocation(u.Timezone); err == nil && u.Timezone != "" {
		return loc
	}
	return time.UTC
}

// IsAdmin reports whether the user administers the instance.
func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }

const userColumns = `id, email, password_hash, role, max_agents, suspended, timezone, daily_cap, monthly_cap, git_name, git_email, github_login, egress, created_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created string
	if err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.MaxAgents, &u.Suspended, &u.Timezone, &u.DailyCap, &u.MonthlyCap, &u.GitName, &u.GitEmail, &u.GitHubLogin, &u.Egress, &created); err != nil {
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO users (`+userColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.PasswordHash, u.Role, u.MaxAgents, u.Suspended, u.Timezone, u.DailyCap, u.MonthlyCap, u.GitName, u.GitEmail, u.GitHubLogin, u.Egress, ts(u.CreatedAt))
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

// SetTimezone sets the user's time zone, an IANA name such as Europe/London.
func (s *Store) SetTimezone(ctx context.Context, id, tz string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE users SET timezone = ? WHERE id = ?`, tz, id))
}

// SetCaps sets the user's daily and monthly spending caps; 0 removes one.
func (s *Store) SetCaps(ctx context.Context, id string, daily, monthly float64) error {
	return affected(s.db.ExecContext(ctx, `UPDATE users SET daily_cap = ?, monthly_cap = ? WHERE id = ?`, daily, monthly, id))
}

// SpentSince totals the cost of a user's turns that started since t.
func (s *Store) SpentSince(ctx context.Context, userID string, t time.Time) (float64, error) {
	var total float64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(t.cost_usd), 0) FROM turns t JOIN agents a ON a.id = t.agent_id
		WHERE a.user_id = ? AND t.started_at >= ?`, userID, ts(t)).Scan(&total)
	return total, err
}

// SetGitIdentity sets who the user's agents commit as.
func (s *Store) SetGitIdentity(ctx context.Context, id, name, email string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE users SET git_name = ?, git_email = ? WHERE id = ?`, name, email, id))
}

// SetGitHubLogin records which account the user's GitHub token is for.
func (s *Store) SetGitHubLogin(ctx context.Context, id, login string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE users SET github_login = ? WHERE id = ?`, login, id))
}

// SetEgress turns the egress proxy on or off for a user.
func (s *Store) SetEgress(ctx context.Context, id string, on bool) error {
	return affected(s.db.ExecContext(ctx, `UPDATE users SET egress = ? WHERE id = ?`, on, id))
}

// SetSecretHosts sets where one of a user's own secrets may go; none
// removes the restriction.
func (s *Store) SetSecretHosts(ctx context.Context, userID, name string, hosts []string) error {
	if len(hosts) == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM secret_hosts WHERE user_id = ? AND name = ?`, userID, name)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO secret_hosts (user_id, name, hosts) VALUES (?, ?, ?)
		ON CONFLICT (user_id, name) DO UPDATE SET hosts = excluded.hosts`, userID, name, strings.Join(hosts, " "))
	return err
}

// SecretHosts lists where each of a user's own secrets may go.
func (s *Store) SecretHosts(ctx context.Context, userID string) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, hosts FROM secret_hosts WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var name, hosts string
		if err := rows.Scan(&name, &hosts); err != nil {
			return nil, err
		}
		out[name] = strings.Fields(hosts)
	}
	return out, rows.Err()
}
