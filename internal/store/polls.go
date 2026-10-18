package store

import (
	"context"
	"encoding/json"
	"time"
)

// Poll is a WhatsApp poll an agent sent.
type Poll struct {
	MessageID string
	AccountID string
	ChatJID   string
	Question  string
	Options   []string
	CreatedAt time.Time
}

// PutPoll records a sent poll.
func (s *Store) PutPoll(ctx context.Context, p *Poll) error {
	opts, _ := json.Marshal(p.Options)
	p.CreatedAt = time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO polls (message_id, account_id, chat_jid, question, options, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		p.MessageID, p.AccountID, p.ChatJID, p.Question, string(opts), ts(p.CreatedAt))
	return err
}

// Poll returns a sent poll by its message ID.
func (s *Store) Poll(ctx context.Context, messageID string) (*Poll, error) {
	var p Poll
	var opts, created string
	err := s.db.QueryRowContext(ctx, `SELECT message_id, account_id, chat_jid, question, options, created_at FROM polls WHERE message_id = ?`, messageID).
		Scan(&p.MessageID, &p.AccountID, &p.ChatJID, &p.Question, &opts, &created)
	if err != nil {
		return nil, one(err)
	}
	json.Unmarshal([]byte(opts), &p.Options)
	p.CreatedAt = parseTS(created)
	return &p, nil
}
