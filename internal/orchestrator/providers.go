package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/rahimnathwani/hermitator/internal/store"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// Users' own providers reach pi through its models.json. Each is an entry
// there named "custom-NAME", so it cannot collide with a provider pi has
// built in, and its apiKey names an environment variable: the file holds
// nothing secret, and the key travels in the turn's environment like
// DeepSeek's does.

const customPrefix = turn.CustomProviderPrefix

// CustomProvider is the pi provider ID of a user's provider.
func CustomProvider(name string) string { return customPrefix + name }

// CustomName returns the name of a user's provider from its pi provider
// ID, if it is one.
func CustomName(provider string) (string, bool) { return strings.CutPrefix(provider, customPrefix) }

// ProviderKeyEnv is the variable a user's provider's key reaches the
// sandbox as.
func ProviderKeyEnv(name string) string {
	return "HERMITATOR_PROVIDER_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_KEY"
}

// ProviderAPIs are the APIs a user's provider can speak.
var ProviderAPIs = []struct{ ID, Name string }{
	{store.APIResponses, "Responses"},
	{store.APICompletions, "Chat Completions"},
}

// Provider is a pi provider an agent can choose: one of hermitator's or
// one its owner added.
type Provider struct {
	ID, Name, Short, DefaultModel string
	Models                        []string
	// Custom is set for the owner's own providers.
	Custom bool
}

// Providers lists the pi providers a user can choose, theirs last.
func (m *Manager) Providers(ctx context.Context, userID string) []Provider {
	var out []Provider
	for _, p := range PiProviders {
		p.Models = PiModels[p.ID]
		out = append(out, p)
	}
	own, _ := m.store.Providers(ctx, userID)
	for _, p := range own {
		out = append(out, ownProvider(p))
	}
	return out
}

func ownProvider(p *store.Provider) Provider {
	out := Provider{ID: CustomProvider(p.Name), Name: p.Name, Short: p.Name, Models: p.Models, Custom: true}
	if len(p.Models) > 0 {
		out.DefaultModel = p.Models[0]
	}
	return out
}

// modelChoices lists the models an agent is likely to be told to run, and
// what it runs when none is set ("" when the harness decides).
func (m *Manager) modelChoices(ctx context.Context, ag *store.Agent) (suggested []string, def string) {
	if ag.Harness != store.HarnessPi {
		return ClaudeModels, ""
	}
	if name, ok := CustomName(ag.PiProvider); ok {
		p, err := m.store.Provider(ctx, ag.UserID, name)
		if err != nil {
			return nil, ""
		}
		own := ownProvider(p)
		return own.Models, own.DefaultModel
	}
	return ModelSuggestions(ag.Harness, ag.PiProvider), DefaultModelFor(ag.PiProvider)
}

var providerNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,29}$`)

// CleanProvider checks a provider a user typed in and tidies it, or
// explains in a sentence what is wrong with it.
func CleanProvider(p *store.Provider) error {
	p.Name = strings.ToLower(strings.TrimSpace(p.Name))
	if !providerNameRE.MatchString(p.Name) {
		return fmt.Errorf("a provider's name is up to 30 lowercase letters, digits and dashes, starting with a letter, like explabs")
	}
	if !slices.ContainsFunc(ProviderAPIs, func(a struct{ ID, Name string }) bool { return a.ID == p.API }) {
		return fmt.Errorf("choose the API the provider speaks")
	}
	u, err := url.Parse(strings.TrimSpace(p.BaseURL))
	if err != nil || u.Scheme != "https" || !ValidHost(u.Hostname()) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("the base URL is the https address the API lives under, like https://api.example.com/v1")
	}
	// People paste the endpoint from the provider's docs; pi adds that part.
	path := strings.TrimRight(u.Path, "/")
	for _, endpoint := range []string{"/responses", "/chat/completions"} {
		path = strings.TrimSuffix(path, endpoint)
	}
	u.Path, u.RawPath = path, ""
	p.BaseURL = u.String()
	models := strings.FieldsFunc(strings.Join(p.Models, " "), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' })
	p.Models = nil
	for _, id := range models {
		if !slices.Contains(p.Models, id) {
			p.Models = append(p.Models, id)
		}
	}
	if len(p.Models) == 0 || len(p.Models) > 50 || slices.ContainsFunc(p.Models, func(id string) bool { return len(id) > 200 }) {
		return fmt.Errorf("list the IDs of the models it serves, one per line, like qwen3.8-27b")
	}
	if p.ContextWindow < 0 || p.ContextWindow > 100_000_000 {
		return fmt.Errorf("the context window is a number of tokens, like 262144; leave it empty if you do not know it")
	}
	return nil
}

// SaveProvider adds or replaces one of a user's providers. An empty key
// keeps the one saved before; a new provider needs one.
func (m *Manager) SaveProvider(ctx context.Context, p *store.Provider, key string) error {
	if err := CleanProvider(p); err != nil {
		return err
	}
	secret := store.SecretProviderPrefix + p.Name
	if key == "" && m.Secret(ctx, p.UserID, secret) == "" {
		return fmt.Errorf("paste the provider's API key")
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return fmt.Errorf("that does not look like an API key: it contains whitespace")
	}
	if old, err := m.store.Provider(ctx, p.UserID, p.Name); err == nil {
		p.CreatedAt = old.CreatedAt
	}
	if err := m.store.PutProvider(ctx, p); err != nil {
		return err
	}
	if key != "" {
		if err := m.PutSecret(ctx, p.UserID, secret, key); err != nil {
			return err
		}
	}
	m.ForgetSecrets(p.UserID)
	return nil
}

// DeleteProvider removes one of a user's providers and its key.
func (m *Manager) DeleteProvider(ctx context.Context, userID, name string) error {
	if err := m.store.DeleteProvider(ctx, userID, name); err != nil {
		return err
	}
	m.ForgetSecrets(userID)
	return nil
}

// ownProviders adds the user's providers that have a key to a pi turn's
// credentials: the keys to its environment, and their entries to pi's
// models.json.
func (m *Manager) ownProviders(ctx context.Context, userID string, c *credentials) []*store.Provider {
	all, _ := m.store.Providers(ctx, userID)
	var keyed []*store.Provider
	for _, p := range all {
		if key := m.Secret(ctx, userID, store.SecretProviderPrefix+p.Name); key != "" {
			c.env[ProviderKeyEnv(p.Name)] = key
			keyed = append(keyed, p)
		}
	}
	if len(keyed) > 0 {
		c.files = append(c.files, piModelsFile(keyed))
	}
	return keyed
}

// piModelsFile describes providers the way pi's models.json does. The
// guest merges it into the file already there, so providers the agent
// added itself are kept.
func piModelsFile(providers []*store.Provider) turn.File {
	type model struct {
		ID            string   `json:"id"`
		Reasoning     bool     `json:"reasoning,omitempty"`
		Input         []string `json:"input,omitempty"`
		ContextWindow int      `json:"contextWindow,omitempty"`
	}
	type provider struct {
		BaseURL string  `json:"baseUrl"`
		API     string  `json:"api"`
		APIKey  string  `json:"apiKey"`
		Models  []model `json:"models"`
	}
	doc := struct {
		Providers map[string]provider `json:"providers"`
	}{map[string]provider{}}
	for _, p := range providers {
		e := provider{BaseURL: p.BaseURL, API: p.API, APIKey: "$" + ProviderKeyEnv(p.Name)}
		for _, id := range p.Models {
			md := model{ID: id, Reasoning: p.Reasoning, ContextWindow: p.ContextWindow}
			if p.Images {
				md.Input = []string{"text", "image"}
			}
			e.Models = append(e.Models, md)
		}
		doc.Providers[CustomProvider(p.Name)] = e
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return turn.File{Path: turn.PiModelsPath, Content: string(b)}
}

// providerHost is the host a provider's key may be sent to.
func providerHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
