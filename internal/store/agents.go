package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
)

// Harnesses.
const (
	HarnessClaude = "claude"
	HarnessPi     = "pi"
)

// Agent states, mirrored from the sandbox so the UI can show them without
// asking the runtime.
const (
	StateStopped      = "stopped"
	StateRunning      = "running"
	StateCheckpointed = "checkpointed"
)

// Agent is one sandbox, its workspace and its conversation.
type Agent struct {
	ID      string
	UserID  string
	Name    string
	Image   string
	Harness string
	Model   string
	// Effort is the harness's thinking effort; empty for its default.
	Effort     string
	PiProvider string
	State      string
	SessionID  string
	// SessionCost is the running total Claude Code last reported for the
	// session, so the next turn can record only its own share.
	SessionCost float64
	IdleTimeout time.Duration
	MemoryMB    int
	IP          string
	LastActive  time.Time
	CreatedAt   time.Time
}

const agentColumns = `id, user_id, name, image, harness, model, effort, pi_provider, state, session_id, session_cost, idle_timeout_sec, memory_mb, ip, last_active_at, created_at`

func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var idle int
	var last sql.NullString
	var created string
	if err := row.Scan(&a.ID, &a.UserID, &a.Name, &a.Image, &a.Harness, &a.Model, &a.Effort, &a.PiProvider, &a.State,
		&a.SessionID, &a.SessionCost, &idle, &a.MemoryMB, &a.IP, &last, &created); err != nil {
		return nil, one(err)
	}
	a.IdleTimeout = time.Duration(idle) * time.Second
	a.LastActive = parseNullTS(last)
	a.CreatedAt = parseTS(created)
	return &a, nil
}

// CreateAgent inserts a, assigning an ID and the lowest free address in
// subnet. The first address of the subnet is the host's bridge.
func (s *Store) CreateAgent(ctx context.Context, a *Agent, subnet netip.Prefix) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	ip, err := freeAddress(ctx, tx, subnet)
	if err != nil {
		return err
	}

	a.ID = ids.New()
	a.IP = ip
	a.CreatedAt = time.Now()
	if a.State == "" {
		a.State = StateStopped
	}
	if a.IdleTimeout == 0 {
		a.IdleTimeout = 5 * time.Minute
	}
	if a.MemoryMB == 0 {
		a.MemoryMB = 4096
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agents (`+agentColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.UserID, a.Name, a.Image, a.Harness, a.Model, a.Effort, a.PiProvider, a.State, a.SessionID, a.SessionCost,
		int(a.IdleTimeout/time.Second), a.MemoryMB, a.IP, nullTS(a.LastActive), ts(a.CreatedAt))
	if isUnique(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ReassignAgentIP gives an agent the lowest free address in subnet, for
// when the agents' network has been changed.
func (s *Store) ReassignAgentIP(ctx context.Context, id string, subnet netip.Prefix) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	ip, err := freeAddress(ctx, tx, subnet)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET ip = ? WHERE id = ?`, ip, id); err != nil {
		return "", err
	}
	return ip, tx.Commit()
}

// freeAddress returns the lowest address in subnet no agent uses. The first
// address is the host's end of the bridge.
func freeAddress(ctx context.Context, tx *sql.Tx, subnet netip.Prefix) (string, error) {
	used := map[netip.Addr]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT ip FROM agents`)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			rows.Close()
			return "", err
		}
		if addr, err := netip.ParseAddr(ip); err == nil {
			used[addr] = true
		}
	}
	rows.Close()
	gateway := subnet.Masked().Addr().Next()
	ip := gateway.Next()
	for used[ip] {
		ip = ip.Next()
	}
	if !subnet.Contains(ip) || !subnet.Contains(ip.Next()) {
		return "", fmt.Errorf("no free addresses left in %s", subnet)
	}
	return ip.String(), nil
}

// Agent returns an agent by ID.
func (s *Store) Agent(ctx context.Context, id string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentColumns+` FROM agents WHERE id = ?`, id))
}

// Agents lists a user's agents, or every agent when userID is empty.
func (s *Store) Agents(ctx context.Context, userID string) ([]*Agent, error) {
	query := `SELECT ` + agentColumns + ` FROM agents`
	var args []any
	if userID != "" {
		query += ` WHERE user_id = ?`
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountAgents returns how many agents a user owns.
func (s *Store) CountAgents(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// UpdateAgentSettings saves the user-editable fields of a. The session is
// left alone; see SetAgentSession.
func (s *Store) UpdateAgentSettings(ctx context.Context, a *Agent) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET name = ?, harness = ?, model = ?, effort = ?, pi_provider = ?, idle_timeout_sec = ?, memory_mb = ? WHERE id = ?`,
		a.Name, a.Harness, a.Model, a.Effort, a.PiProvider, int(a.IdleTimeout/time.Second), a.MemoryMB, a.ID)
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// SetAgentState records the sandbox state.
func (s *Store) SetAgentState(ctx context.Context, id, state string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE agents SET state = ? WHERE id = ?`, state, id))
}

// SetAgentSession records the harness session the agent continues. A new
// session starts with no running cost.
func (s *Store) SetAgentSession(ctx context.Context, id, sessionID string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE agents SET session_id = ?, session_cost = 0 WHERE id = ?`, sessionID, id))
}

// SetAgentSessionCost records the session and its running cost together.
func (s *Store) SetAgentSessionCost(ctx context.Context, id, sessionID string, cost float64) error {
	return affected(s.db.ExecContext(ctx, `UPDATE agents SET session_id = ?, session_cost = ? WHERE id = ?`, sessionID, cost, id))
}

// SetAgentModel sets the model and effort the agent's turns use.
func (s *Store) SetAgentModel(ctx context.Context, id, model, effort string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE agents SET model = ?, effort = ? WHERE id = ?`, model, effort, id))
}

// SetAgentImage repins an agent to another image version.
func (s *Store) SetAgentImage(ctx context.Context, id, image string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE agents SET image = ? WHERE id = ?`, image, id))
}

// TouchAgent records activity now.
func (s *Store) TouchAgent(ctx context.Context, id string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE agents SET last_active_at = ? WHERE id = ?`, ts(time.Now()), id))
}

// DeleteAgent removes an agent row. The caller tears down its sandbox.
func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id))
}

// AgentByIP returns the agent at a bridge address.
func (s *Store) AgentByIP(ctx context.Context, ip string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentColumns+` FROM agents WHERE ip = ?`, ip))
}
