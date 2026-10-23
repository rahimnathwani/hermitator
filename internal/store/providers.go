package store

import (
	"context"
	"strings"
	"time"
)

// SecretProviderPrefix starts the names of the keys of users' own
// providers: "provider:explabs".
const SecretProviderPrefix = "provider:"

// APIs a user's own provider can speak, in pi's names.
const (
	APIResponses   = "openai-responses"
	APICompletions = "openai-completions"
)

// Provider is a model provider a user added: an endpoint pi talks to with
// one of the OpenAI APIs.
type Provider struct {
	UserID  string
	Name    string
	API     string
	BaseURL string
	// Models are the model IDs it serves; the first is the default.
	Models        []string
	Images        bool
	Reasoning     bool
	ContextWindow int
	CreatedAt     time.Time
}

const providerColumns = `user_id, name, api, base_url, models, images, reasoning, context_window, created_at`

func scanProvider(row interface{ Scan(...any) error }) (*Provider, error) {
	var p Provider
	var models, created string
	if err := row.Scan(&p.UserID, &p.Name, &p.API, &p.BaseURL, &models, &p.Images, &p.Reasoning, &p.ContextWindow, &created); err != nil {
		return nil, one(err)
	}
	p.Models = strings.Fields(models)
	p.CreatedAt = parseTS(created)
	return &p, nil
}

// PutProvider adds a provider or replaces the one of the same name.
func (s *Store) PutProvider(ctx context.Context, p *Provider) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO providers (`+providerColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, name) DO UPDATE SET api = excluded.api, base_url = excluded.base_url, models = excluded.models,
			images = excluded.images, reasoning = excluded.reasoning, context_window = excluded.context_window`,
		p.UserID, p.Name, p.API, p.BaseURL, strings.Join(p.Models, " "), p.Images, p.Reasoning, p.ContextWindow, ts(p.CreatedAt))
	return err
}

// Provider returns one of a user's providers.
func (s *Store) Provider(ctx context.Context, userID, name string) (*Provider, error) {
	return scanProvider(s.db.QueryRowContext(ctx, `SELECT `+providerColumns+` FROM providers WHERE user_id = ? AND name = ?`, userID, name))
}

// Providers lists a user's providers, oldest first.
func (s *Store) Providers(ctx context.Context, userID string) ([]*Provider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+providerColumns+` FROM providers WHERE user_id = ? ORDER BY created_at, name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteProvider removes a provider and its key.
func (s *Store) DeleteProvider(ctx context.Context, userID, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM providers WHERE user_id = ? AND name = ?`, userID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_secrets WHERE user_id = ? AND name = ?`, userID, SecretProviderPrefix+name); err != nil {
		return err
	}
	return tx.Commit()
}
