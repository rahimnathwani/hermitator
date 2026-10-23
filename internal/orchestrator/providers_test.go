package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rahimnathwani/hermitator/internal/store"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// fakePi answers one prompt and keeps what it was started with.
const fakePi = `#!/bin/sh
read -r line
printf '%s\n' "$@" > "$HOME/pi-args"
cat "$HOME/.pi/agent/models.json" > "$HOME/pi-models"
printf '%s' "$HERMITATOR_PROVIDER_EXPLABS_KEY" > "$HOME/pi-key"
echo '{"type":"message_end","message":{"role":"assistant","model":"qwen3.8-27b","content":[{"type":"text","text":"hello from qwen"}]}}'
echo '{"type":"agent_settled"}'
cat >/dev/null
`

func explabs(userID string) *store.Provider {
	return &store.Provider{UserID: userID, Name: "explabs", API: store.APIResponses,
		BaseURL: "https://api.experientiallabs.ai/v1/responses", Models: []string{"qwen3.8-27b\nqwen3.8-7b"}, Reasoning: true}
}

func TestCleanProvider(t *testing.T) {
	p := explabs("u")
	p.Models = []string{"qwen3.8-27b, qwen3.8-7b\n qwen3.8-27b"}
	if err := CleanProvider(p); err != nil {
		t.Fatal(err)
	}
	if p.BaseURL != "https://api.experientiallabs.ai/v1" || strings.Join(p.Models, " ") != "qwen3.8-27b qwen3.8-7b" {
		t.Fatalf("cleaned = %q %q", p.BaseURL, p.Models)
	}
	for _, bad := range []func(*store.Provider){
		func(p *store.Provider) { p.Name = "Has Spaces" },
		func(p *store.Provider) { p.BaseURL = "http://api.experientiallabs.ai/v1" },
		func(p *store.Provider) { p.BaseURL = "https://user:pass@api.experientiallabs.ai/v1" },
		func(p *store.Provider) { p.BaseURL = "https://localhost/v1" },
		func(p *store.Provider) { p.API = "anthropic-messages" },
		func(p *store.Provider) { p.Models = []string{" \n "} },
	} {
		p := explabs("u")
		bad(p)
		if CleanProvider(p) == nil {
			t.Errorf("accepted %+v", p)
		}
	}
}

func TestOwnProviderTurn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	chat := "self@s.whatsapp.net"
	bin := filepath.Join(t.TempDir(), "pi")
	os.WriteFile(bin, []byte(fakePi), 0o755)
	t.Setenv("HERMITATOR_PI_BIN", bin)

	if err := e.m.SaveProvider(ctx, explabs(e.user.ID), ""); err == nil {
		t.Fatal("a new provider was saved without a key")
	}
	if err := e.m.SaveProvider(ctx, explabs(e.user.ID), "sk-explabs-secret"); err != nil {
		t.Fatal(err)
	}
	ag, _ := e.st.Agent(ctx, e.agent.ID)
	ag.Harness, ag.PiProvider = store.HarnessPi, CustomProvider("explabs")
	if err := e.m.UpdateAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	// The agent kept a provider of its own in models.json, and an older
	// hermitator entry is still there.
	spec, _ := e.m.Spec(ag)
	models := filepath.Join(spec.Dir, "home", ".pi", "agent", "models.json")
	os.MkdirAll(filepath.Dir(models), 0o700)
	os.WriteFile(models, []byte(`{"providers":{"ollama":{"baseUrl":"http://localhost:11434/v1"},"custom-gone":{"baseUrl":"https://gone.example.com"}}}`), 0o600)

	e.link(t, chat)
	e.m.HandleMessage(ctx, e.msg(chat, "hi", true))
	if got := e.rec.wait(t, "text"); got.body != "hello from qwen" {
		t.Fatalf("reply = %q", got.body)
	}
	home := filepath.Join(spec.Dir, "home")
	args, _ := os.ReadFile(filepath.Join(home, "pi-args"))
	if !strings.Contains(string(args), "--provider\ncustom-explabs\n--model\nqwen3.8-27b\n") {
		t.Fatalf("pi args:\n%s", args)
	}
	if key, _ := os.ReadFile(filepath.Join(home, "pi-key")); string(key) != "sk-explabs-secret" {
		t.Fatalf("key in the environment = %q", key)
	}
	var doc struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			API     string `json:"api"`
			APIKey  string `json:"apiKey"`
			Models  []struct {
				ID        string `json:"id"`
				Reasoning bool   `json:"reasoning"`
			} `json:"models"`
		} `json:"providers"`
	}
	raw, _ := os.ReadFile(filepath.Join(home, "pi-models"))
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("models.json: %v\n%s", err, raw)
	}
	p := doc.Providers["custom-explabs"]
	if p.BaseURL != "https://api.experientiallabs.ai/v1" || p.API != "openai-responses" || p.APIKey != "$HERMITATOR_PROVIDER_EXPLABS_KEY" ||
		len(p.Models) != 2 || !p.Models[0].Reasoning {
		t.Fatalf("custom-explabs = %+v", p)
	}
	if _, ok := doc.Providers["ollama"]; !ok {
		t.Fatal("the agent's own provider was dropped")
	}
	if _, ok := doc.Providers["custom-gone"]; ok {
		t.Fatal("a provider hermitator no longer sends was kept")
	}
	if strings.Contains(string(raw), "sk-explabs-secret") {
		t.Fatal("the key was written to models.json")
	}

	// /model knows the provider's models, and /status names the default.
	e.m.HandleMessage(ctx, e.msg(chat, "/model", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "qwen3.8-27b, qwen3.8-7b") || !strings.Contains(got.body, "default model (qwen3.8-27b)") {
		t.Fatalf("/model = %q", got.body)
	}

	// Removed, the agent says so instead of failing inside pi.
	if err := e.m.DeleteProvider(ctx, e.user.ID, "explabs"); err != nil {
		t.Fatal(err)
	}
	if e.m.Secret(ctx, e.user.ID, store.SecretProviderPrefix+"explabs") != "" {
		t.Fatal("the key outlived its provider")
	}
	e.m.HandleMessage(ctx, e.msg(chat, "still there?", true))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "your provider explabs") || !strings.Contains(got.body, "/agents/"+ag.ID+"/settings") {
		t.Fatalf("reply after removal = %q", got.body)
	}
}

func TestOwnProviderKeyStaysOut(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.cfg.EgressKey = []byte("egress test key")
	e.m.SaveProvider(ctx, explabs(e.user.ID), "sk-explabs-secret")
	u, _ := e.st.User(ctx, e.user.ID)

	var found bool
	for _, s := range e.m.egressSecrets(ctx, u) {
		if s.Value == "sk-explabs-secret" {
			found = true
			if len(s.Hosts) != 1 || s.Hosts[0] != "api.experientiallabs.ai" || s.Placeholder == "" || s.Placeholder == s.Value {
				t.Fatalf("egress secret = %+v", s)
			}
		}
	}
	if !found {
		t.Fatal("the proxy does not know the provider's key")
	}

	// URLs and model IDs in models.json are not mistaken for secrets.
	ag := &store.Agent{UserID: e.user.ID, Harness: store.HarnessPi, PiProvider: CustomProvider("explabs")}
	c, problem := e.m.credentials(ctx, ag)
	if problem != "" || c.model != "qwen3.8-27b" {
		t.Fatalf("credentials: %q, model %q", problem, c.model)
	}
	r := newRedactor(c.redact)
	if got := r.apply("see https://api.experientiallabs.ai/v1 and sk-explabs-secret"); got != "see https://api.experientiallabs.ai/v1 and [secret removed]" {
		t.Fatalf("redacted = %q", got)
	}
	if !strings.Contains(c.files[0].Content, "custom-explabs") || c.files[0].Path != turn.PiModelsPath {
		t.Fatalf("files = %+v", c.files)
	}
}
