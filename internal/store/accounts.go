package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
)

// Account states.
const (
	AccountPairing      = "pairing"
	AccountConnected    = "connected"
	AccountDisconnected = "disconnected"
	AccountLoggedOut    = "logged_out"
)

// Account is a WhatsApp account linked as a companion device.
type Account struct {
	ID              string
	UserID          string
	JID             string
	Phone           string
	PushName        string
	Status          string
	FallbackAgentID string
	CreatedAt       time.Time
}

const accountColumns = `id, user_id, jid, phone, push_name, status, fallback_agent_id, created_at`

func scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	var a Account
	var jid, fallback sql.NullString
	var created string
	if err := row.Scan(&a.ID, &a.UserID, &jid, &a.Phone, &a.PushName, &a.Status, &fallback, &created); err != nil {
		return nil, one(err)
	}
	a.JID, a.FallbackAgentID = jid.String, fallback.String
	a.CreatedAt = parseTS(created)
	return &a, nil
}

// CreateAccount inserts a newly paired account.
func (s *Store) CreateAccount(ctx context.Context, a *Account) error {
	a.ID = ids.New()
	a.CreatedAt = time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO whatsapp_accounts (`+accountColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.UserID, nullString(a.JID), a.Phone, a.PushName, a.Status, nullString(a.FallbackAgentID), ts(a.CreatedAt))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// Account returns an account by ID.
func (s *Store) Account(ctx context.Context, id string) (*Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM whatsapp_accounts WHERE id = ?`, id))
}

// AccountByJID returns the account for a whatsmeow device JID.
func (s *Store) AccountByJID(ctx context.Context, jid string) (*Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM whatsapp_accounts WHERE jid = ?`, jid))
}

// Accounts lists a user's accounts, or every account when userID is empty.
func (s *Store) Accounts(ctx context.Context, userID string) ([]*Account, error) {
	query := `SELECT ` + accountColumns + ` FROM whatsapp_accounts`
	var args []any
	if userID != "" {
		query += ` WHERE user_id = ?`
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAccountStatus records connection state and, when known, the profile name.
func (s *Store) SetAccountStatus(ctx context.Context, id, status, pushName string) error {
	if pushName != "" {
		return affected(s.db.ExecContext(ctx, `UPDATE whatsapp_accounts SET status = ?, push_name = ? WHERE id = ?`, status, pushName, id))
	}
	return affected(s.db.ExecContext(ctx, `UPDATE whatsapp_accounts SET status = ? WHERE id = ?`, status, id))
}

// SetAccountFallback sets the agent that answers unrouted chats ("" for none).
func (s *Store) SetAccountFallback(ctx context.Context, id, agentID string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE whatsapp_accounts SET fallback_agent_id = ? WHERE id = ?`, nullString(agentID), id))
}

// DeleteAccount removes an account and its routes.
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM whatsapp_accounts WHERE id = ?`, id))
}
