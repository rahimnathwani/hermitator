package store

import (
	"context"
	"time"
)

// Inbound is a message as it arrived, in the gateway's own encoding.
type Inbound struct {
	MessageID  string
	Raw        []byte
	ReceivedAt time.Time
}

// Receive records a message as it arrives; it reports false if the
// message was recorded before, when WhatsApp delivers one again.
func (s *Store) Receive(ctx context.Context, accountID, messageID string, raw []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO inbound (account_id, message_id, raw, received_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (account_id, message_id) DO NOTHING`, accountID, messageID, raw, ts(time.Now()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// InboundDone marks messages as taken care of: answered, or not for any
// agent.
func (s *Store) InboundDone(ctx context.Context, accountID string, messageIDs ...string) error {
	for _, id := range messageIDs {
		if _, err := s.db.ExecContext(ctx, `UPDATE inbound SET done = 1, raw = x'' WHERE account_id = ? AND message_id = ?`, accountID, id); err != nil {
			return err
		}
	}
	return nil
}

// PendingInbound returns an account's messages that no turn has taken
// yet, oldest first.
func (s *Store) PendingInbound(ctx context.Context, accountID string) ([]*Inbound, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT message_id, raw, received_at FROM inbound WHERE account_id = ? AND done = 0 ORDER BY rowid`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Inbound
	for rows.Next() {
		var in Inbound
		var at string
		if err := rows.Scan(&in.MessageID, &in.Raw, &at); err != nil {
			return nil, err
		}
		in.ReceivedAt = parseTS(at)
		out = append(out, &in)
	}
	return out, rows.Err()
}

// PruneInbound forgets messages received before t.
func (s *Store) PruneInbound(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM inbound WHERE received_at < ?`, ts(before))
	return err
}
