package orchestrator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rahimnathwani/hermitator/internal/egress"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// ChatGPT for pi through the egress proxy. pi decodes its ChatGPT access
// token, a JWT, for the account ID, and refreshes it in place when it
// nears expiry, which would put real tokens in the sandbox. With
// placeholders on, the sandbox's auth.json holds an unsigned JWT that
// decodes the same way and never expires, and the host refreshes the real
// token instead.

var codexTokenURL = "https://auth.openai.com/oauth/token"

// codexClientID is the OAuth client pi logs in as.
const codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

var codexHosts = []string{"chatgpt.com"}

const (
	// refreshCodexWithin is how close to expiry the host refreshes the
	// real token, comfortably longer than a turn.
	refreshCodexWithin = 6 * time.Hour
	// codexNeverExpires is the expiry the sandbox's copy claims (2100), so
	// pi never tries to refresh it.
	codexNeverExpires = 4102444800000
	// retryCodexAfter keeps a failing refresh from being retried on every
	// connection.
	retryCodexAfter = 5 * time.Minute
)

type codexState struct {
	mu     sync.Mutex
	failed map[string]time.Time
}

// codexEntry is pi's ChatGPT credential in an auth.json, as a map so
// fields hermitator does not know survive.
func codexEntry(authJSON string) (map[string]any, map[string]json.RawMessage, bool) {
	var auth map[string]json.RawMessage
	if json.Unmarshal([]byte(authJSON), &auth) != nil {
		return nil, nil, false
	}
	var entry map[string]any
	if json.Unmarshal(auth[ProviderCodex], &entry) != nil || entry == nil {
		return nil, auth, false
	}
	return entry, auth, true
}

func entryString(e map[string]any, k string) string { s, _ := e[k].(string); return s }

// codexPlaceholders are a user's stand-ins for the ChatGPT access and
// refresh tokens. The access one is a JWT carrying the real account ID,
// since pi reads it from there.
func (m *Manager) codexPlaceholders(userID, accountID string) (access, refresh string) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": accountID},
		"exp":                         codexNeverExpires / 1000,
		"hermitator":                  egress.Placeholder(m.cfg.EgressKey, userID, ProviderCodex, ""),
	})
	access = header + "." + enc.EncodeToString(payload) + ".hermitator"
	refresh = egress.Placeholder(m.cfg.EgressKey, userID, ProviderCodex+" refresh", "")
	return access, refresh
}

// codexSecrets are the proxy's entries for a user's ChatGPT login. The
// refresh token has no hosts: its placeholder is refused everywhere.
func (m *Manager) codexSecrets(ctx context.Context, userID string) []egress.Secret {
	entry, _, ok := codexEntry(m.Secret(ctx, userID, store.SecretPiAuth))
	if !ok {
		return nil
	}
	access, refresh := m.codexPlaceholders(userID, entryString(entry, "accountId"))
	return []egress.Secret{
		{Name: ProviderCodex, Value: entryString(entry, "access"), Hosts: codexHosts, Placeholder: access},
		{Name: ProviderCodex + " refresh", Value: entryString(entry, "refresh"), Placeholder: refresh},
	}
}

// sandboxPiAuth is the auth.json a sandbox holding placeholders gets.
func (m *Manager) sandboxPiAuth(userID, authJSON string) string {
	entry, auth, ok := codexEntry(authJSON)
	if !ok {
		return authJSON
	}
	entry["access"], entry["refresh"] = m.codexPlaceholders(userID, entryString(entry, "accountId"))
	entry["expires"] = codexNeverExpires
	auth[ProviderCodex], _ = json.Marshal(entry)
	b, _ := json.MarshalIndent(auth, "", "  ")
	return string(b)
}

// restorePiAuth undoes sandboxPiAuth on an auth.json read back from a
// sandbox: a ChatGPT entry still holding the placeholder becomes the
// stored real one. Anything else, such as a fresh /login from the
// terminal, is kept as the sandbox has it.
func (m *Manager) restorePiAuth(ctx context.Context, userID, fromSandbox string) string {
	entry, auth, ok := codexEntry(fromSandbox)
	if !ok {
		return fromSandbox
	}
	stored := m.Secret(ctx, userID, store.SecretPiAuth)
	if strings.TrimSpace(fromSandbox) == strings.TrimSpace(m.sandboxPiAuth(userID, stored)) {
		return stored // untouched, so nothing to store
	}
	real, _, ok := codexEntry(stored)
	if !ok {
		return fromSandbox
	}
	if access, _ := m.codexPlaceholders(userID, entryString(entry, "accountId")); entryString(entry, "access") != access {
		return fromSandbox
	}
	auth[ProviderCodex], _ = json.Marshal(real)
	b, _ := json.MarshalIndent(auth, "", "  ")
	return string(b)
}

// refreshCodex renews a user's ChatGPT token on the host when it nears
// expiry. OpenAI rotates refresh tokens, so refreshes are serialised.
func (m *Manager) refreshCodex(ctx context.Context, userID string) error {
	m.codex.mu.Lock()
	defer m.codex.mu.Unlock()
	if t, ok := m.codex.failed[userID]; ok && time.Since(t) < retryCodexAfter {
		return nil
	}
	entry, _, ok := codexEntry(m.Secret(ctx, userID, store.SecretPiAuth))
	if !ok {
		return nil
	}
	expires, _ := entry["expires"].(float64)
	if time.Until(time.UnixMilli(int64(expires))) > refreshCodexWithin {
		return nil
	}
	err := m.renewCodex(ctx, userID, entry)
	if err != nil {
		if m.codex.failed == nil {
			m.codex.failed = map[string]time.Time{}
		}
		m.codex.failed[userID] = time.Now()
		return err
	}
	delete(m.codex.failed, userID)
	m.ForgetSecrets(userID)
	return nil
}

func (m *Manager) renewCodex(ctx context.Context, userID string, entry map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {entryString(entry, "refresh")}, "client_id": {codexClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var tok struct {
		Access    string  `json:"access_token"`
		Refresh   string  `json:"refresh_token"`
		ExpiresIn float64 `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ChatGPT token refresh: %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.Access == "" || tok.Refresh == "" || tok.ExpiresIn <= 0 {
		return fmt.Errorf("ChatGPT token refresh: unexpected response")
	}
	entry["access"], entry["refresh"] = tok.Access, tok.Refresh
	entry["expires"] = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UnixMilli()
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return m.mergePiCredential(ctx, userID, ProviderCodex, b)
}
