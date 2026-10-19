package store

import (
	"context"
	"errors"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
)

// MaxPreviews is how many ports one agent may have preview links for.
const MaxPreviews = 10

// ErrTooManyPreviews is returned when an agent already has MaxPreviews.
var ErrTooManyPreviews = errors.New("too many preview links")

// Preview is a port in an agent's sandbox reachable at a public URL.
type Preview struct {
	AgentID   string
	Port      int
	Nonce     string
	CreatedAt time.Time
}

// PutPreview returns an agent's preview of port, creating it if needed.
func (s *Store) PutPreview(ctx context.Context, agentID string, port int) (*Preview, error) {
	if p, err := s.Preview(ctx, agentID, port); err == nil {
		return p, nil
	}
	var n int
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM previews WHERE agent_id = ?`, agentID).Scan(&n)
	if n >= MaxPreviews {
		return nil, ErrTooManyPreviews
	}
	p := &Preview{AgentID: agentID, Port: port, Nonce: ids.Token(), CreatedAt: time.Now()}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO previews (agent_id, port, nonce, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (agent_id, port) DO NOTHING`, p.AgentID, p.Port, p.Nonce, ts(p.CreatedAt)); err != nil {
		return nil, err
	}
	return s.Preview(ctx, agentID, port)
}

// Preview returns an agent's preview of port.
func (s *Store) Preview(ctx context.Context, agentID string, port int) (*Preview, error) {
	p := Preview{AgentID: agentID, Port: port}
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT nonce, created_at FROM previews WHERE agent_id = ? AND port = ?`, agentID, port).Scan(&p.Nonce, &created)
	if err != nil {
		return nil, one(err)
	}
	p.CreatedAt = parseTS(created)
	return &p, nil
}

// Previews lists an agent's previews by port.
func (s *Store) Previews(ctx context.Context, agentID string) ([]*Preview, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT port, nonce, created_at FROM previews WHERE agent_id = ? ORDER BY port`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Preview
	for rows.Next() {
		p := &Preview{AgentID: agentID}
		var created string
		if err := rows.Scan(&p.Port, &p.Nonce, &created); err != nil {
			return nil, err
		}
		p.CreatedAt = parseTS(created)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePreviews revokes every preview link of an agent, reporting how
// many ports had one.
func (s *Store) DeletePreviews(ctx context.Context, agentID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM previews WHERE agent_id = ?`, agentID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
