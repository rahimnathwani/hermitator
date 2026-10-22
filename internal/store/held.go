package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
)

// MaxHeld is how many held messages a chat keeps; older ones are dropped.
const MaxHeld = 200

// HeldMessage is a message in a linked chat that did not trigger its agent.
// It is handed over, as context, with the next message that does.
type HeldMessage struct {
	ID         string
	AccountID  string
	ChatJID    string
	MessageID  string
	Sender     string
	SenderName string
	FromOwner  bool
	Text       string
	MediaKind  string
	MediaMIME  string
	// MediaFile is the attachment's name in the agent's inbox.
	MediaFile string
	Voice     bool
	SentAt    time.Time
}

// Hold stores a message for later, dropping the oldest beyond MaxHeld.
func (s *Store) Hold(ctx context.Context, m *HeldMessage) error {
	m.ID = ids.New()
	if m.SentAt.IsZero() {
		m.SentAt = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO held_messages (id, account_id, chat_jid, message_id, sender, sender_name, from_owner, text, media_kind, media_mime, media_file, voice, sent_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.AccountID, m.ChatJID, m.MessageID, m.Sender, m.SenderName, m.FromOwner, m.Text,
		m.MediaKind, m.MediaMIME, m.MediaFile, m.Voice, ts(m.SentAt)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM held_messages WHERE account_id = ? AND chat_jid = ? AND id NOT IN (
			SELECT id FROM held_messages WHERE account_id = ? AND chat_jid = ? ORDER BY sent_at DESC LIMIT ?)`,
		m.AccountID, m.ChatJID, m.AccountID, m.ChatJID, MaxHeld); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateHeldText replaces a held message's text after the user edited
// it; it reports whether the message was held.
func (s *Store) UpdateHeldText(ctx context.Context, accountID, messageID, text string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE held_messages SET text = ? WHERE account_id = ? AND message_id = ?`, text, accountID, messageID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Held returns a chat's held messages, oldest first.
func (s *Store) Held(ctx context.Context, accountID, chatJID string) ([]*HeldMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, account_id, chat_jid, message_id, sender, sender_name, from_owner, text, media_kind, media_mime, media_file, voice, sent_at
		FROM held_messages WHERE account_id = ? AND chat_jid = ? ORDER BY sent_at`, accountID, chatJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*HeldMessage
	for rows.Next() {
		var m HeldMessage
		var sent string
		if err := rows.Scan(&m.ID, &m.AccountID, &m.ChatJID, &m.MessageID, &m.Sender, &m.SenderName, &m.FromOwner, &m.Text,
			&m.MediaKind, &m.MediaMIME, &m.MediaFile, &m.Voice, &sent); err != nil {
			return nil, err
		}
		m.SentAt = parseTS(sent)
		out = append(out, &m)
	}
	return out, rows.Err()
}

// HeldCount returns how many messages a chat is holding.
func (s *Store) HeldCount(ctx context.Context, accountID, chatJID string) int {
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM held_messages WHERE account_id = ? AND chat_jid = ?`, accountID, chatJID).Scan(&n)
	return n
}

// Release deletes held messages once they have been delivered.
func (s *Store) Release(ctx context.Context, ids []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM held_messages WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id); err != nil && err != sql.ErrNoRows {
			return err
		}
	}
	return tx.Commit()
}
