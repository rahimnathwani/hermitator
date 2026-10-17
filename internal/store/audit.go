package store

import (
	"context"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
)

// AuditEntry records a state-changing action and who really performed it.
type AuditEntry struct {
	ID          string
	ActorID     string
	EffectiveID string
	Action      string
	Detail      string
	CreatedAt   time.Time
}

// Audit appends to the audit log.
func (s *Store) Audit(ctx context.Context, actorID, effectiveID, action, detail string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_log (id, actor_id, effective_id, action, detail, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		ids.New(), actorID, effectiveID, action, detail, ts(time.Now()))
	return err
}

// AuditLog returns the newest entries first.
func (s *Store) AuditLog(ctx context.Context, limit int) ([]*AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, actor_id, effective_id, action, detail, created_at FROM audit_log ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		var e AuditEntry
		var created string
		if err := rows.Scan(&e.ID, &e.ActorID, &e.EffectiveID, &e.Action, &e.Detail, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTS(created)
		out = append(out, &e)
	}
	return out, rows.Err()
}
