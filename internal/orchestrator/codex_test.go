package orchestrator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rahimnathwani/hermitator/internal/store"
)

func TestChatGPTPlaceholders(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.cfg.EgressKey = []byte("egress test key")
	soon := time.Now().Add(time.Hour).UnixMilli()
	stored := `{"openai-codex":{"type":"oauth","access":"real-access","refresh":"real-refresh","expires":` + jsonInt(soon) + `,"accountId":"acct-123"},"other":{"type":"api_key","key":"k"}}`
	e.m.PutSecret(ctx, e.user.ID, store.SecretPiAuth, stored)

	// What the sandbox gets decodes the way pi expects, never expires, and
	// carries no real token.
	sb := e.m.sandboxPiAuth(e.user.ID, stored)
	if strings.Contains(sb, "real-") {
		t.Fatalf("real tokens in the sandbox's auth.json: %s", sb)
	}
	entry, _, _ := codexEntry(sb)
	parts := strings.Split(entryString(entry, "access"), ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]map[string]string
	json.Unmarshal(payload, &claims)
	if len(parts) != 3 || claims["https://api.openai.com/auth"]["chatgpt_account_id"] != "acct-123" {
		t.Fatalf("placeholder JWT %q does not carry the account", entryString(entry, "access"))
	}
	if entry["expires"].(float64) != codexNeverExpires || !strings.Contains(sb, `"other"`) {
		t.Fatalf("sandbox auth.json = %s", sb)
	}

	// Read back unchanged, or with another provider added, the real
	// ChatGPT entry is what is stored.
	if got := e.m.restorePiAuth(ctx, e.user.ID, sb); got != stored {
		t.Fatalf("untouched file restored as %s", got)
	}
	var withNew map[string]json.RawMessage
	json.Unmarshal([]byte(sb), &withNew)
	withNew["anthropic"] = json.RawMessage(`{"type":"oauth","access":"a"}`)
	b, _ := json.Marshal(withNew)
	got := e.m.restorePiAuth(ctx, e.user.ID, string(b))
	if !strings.Contains(got, "real-access") || !strings.Contains(got, `"anthropic"`) || strings.Contains(got, "hermitator") {
		t.Fatalf("restored = %s", got)
	}

	// Near expiry, the host refreshes, once, and the proxy gets the new
	// token behind the same placeholder.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		r.ParseForm()
		if r.Form.Get("refresh_token") != "real-refresh" || r.Form.Get("grant_type") != "refresh_token" {
			http.Error(w, "bad", 400)
			return
		}
		w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":864000}`))
	}))
	defer srv.Close()
	defer func(u string) { codexTokenURL = u }(codexTokenURL)
	codexTokenURL = srv.URL
	if err := e.m.refreshCodex(ctx, e.user.ID); err != nil {
		t.Fatal(err)
	}
	e.m.refreshCodex(ctx, e.user.ID)
	if calls != 1 {
		t.Fatalf("%d refreshes, want 1", calls)
	}
	secrets := e.m.codexSecrets(ctx, e.user.ID)
	if secrets[0].Value != "new-access" || secrets[0].Placeholder != entryString(entry, "access") || secrets[1].Value != "new-refresh" || len(secrets[1].Hosts) != 0 {
		t.Fatalf("proxy secrets after refresh = %+v", secrets)
	}
	if !strings.Contains(e.m.Secret(ctx, e.user.ID, store.SecretPiAuth), `"other"`) {
		t.Fatal("refresh dropped another provider")
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }
