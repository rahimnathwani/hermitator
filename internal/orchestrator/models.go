package orchestrator

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rahimnathwani/hermitator/internal/store"
)

// What the harnesses accept. Model lists are suggestions: anything else is
// passed through, so new models work without a hermitator release.
var (
	// ClaudeModels are the aliases Claude Code resolves to its latest models.
	ClaudeModels = []string{"fable", "opus", "sonnet", "haiku"}
	// ClaudeEfforts are Claude Code's --effort levels.
	ClaudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}
	// PiEfforts are pi's --thinking levels.
	PiEfforts = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}
	// PiModels are suggestions per pi provider.
	PiModels = map[string][]string{
		ProviderCodex:      {"gpt-6.1-sol", "gpt-6-sol", "gpt-6-luna", "gpt-6-astra", "gpt-5.6-terra", "gpt-5.6-sol", "gpt-5.6-luna", "gpt-5.5"},
		ProviderDeepSeek:   {"deepseek-v4-pro", "deepseek-flash"},
		ProviderOpenRouter: {"~anthropic/claude-sonnet-latest", "~anthropic/claude-opus-latest", "~openai/gpt-sol-latest", "~google/gemini-pro-latest", "~deepseek/deepseek-pro-latest", "~moonshotai/kimi-latest", "~z-ai/glm-latest"},
	}
)

// Efforts lists the effort levels a harness accepts.
func Efforts(harness string) []string {
	if harness == store.HarnessPi {
		return PiEfforts
	}
	return ClaudeEfforts
}

// ModelSuggestions lists likely models for an agent on Claude Code or one
// of hermitator's own pi providers.
func ModelSuggestions(harness, provider string) []string {
	if harness == store.HarnessPi {
		return PiModels[providerOrDefault(provider)]
	}
	return ClaudeModels
}

func validEffort(harness, effort string) bool {
	return effort == "" || slices.Contains(Efforts(harness), effort)
}

func knownModel(harness string, suggested []string, model string) bool {
	if harness == store.HarnessClaude && strings.HasPrefix(model, "claude-") {
		return true
	}
	return slices.Contains(suggested, model)
}

// fitHarness drops a model or effort that only made sense for the harness
// an agent is leaving: "opus" means nothing to pi, nor "minimal" to Claude.
func fitHarness(prev, next *store.Agent) {
	if prev.Harness == next.Harness {
		return
	}
	if next.Model == prev.Model {
		next.Model = ""
	}
	if !validEffort(next.Harness, next.Effort) {
		next.Effort = ""
	}
}

// describeModel says what an agent runs; def is its default model, if
// hermitator knows it.
func describeModel(ag *store.Agent, def string) string {
	model := ag.Model
	if model == "" {
		model = "the default model"
		if def != "" {
			model = "the default model (" + def + ")"
		}
	}
	effort := ag.Effort
	if effort == "" {
		effort = "default"
	}
	return fmt.Sprintf("%s at %s effort", model, effort)
}

// DefaultModelFor returns the model pi uses for a provider when none is set.
func DefaultModelFor(provider string) string {
	for _, p := range PiProviders {
		if p.ID == providerOrDefault(provider) {
			return p.DefaultModel
		}
	}
	return ""
}

// modelCommand handles /model.
func (m *Manager) modelCommand(ctx context.Context, ag *store.Agent, arg string, reply func(string)) {
	suggested, def := m.modelChoices(ctx, ag)
	choices := strings.Join(suggested, ", ")
	if ag.Harness == store.HarnessClaude {
		choices += ", or a full model ID"
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		reply(fmt.Sprintf("*%s* uses %s.\nChange it with /model NAME: %s. /model default goes back to the default.", ag.Name, describeModel(ag, def), choices))
		return
	}
	model := arg
	if strings.EqualFold(arg, "default") {
		model = ""
	} else if ag.Harness == store.HarnessClaude {
		model = strings.ToLower(model)
	}
	if err := m.store.SetAgentModel(ctx, ag.ID, model, ag.Effort); err != nil {
		reply("⚠️ Could not save that: " + err.Error())
		return
	}
	ag.Model = model
	text := fmt.Sprintf("🧠 From the next message, *%s* uses %s. The conversation carries on.", ag.Name, describeModel(ag, def))
	if model != "" && !knownModel(ag.Harness, suggested, model) {
		text += fmt.Sprintf("\n\nI do not recognise %q; if it is wrong, the next turn will fail with the provider's own error. Usual choices: %s.", model, choices)
	}
	reply(text)
}

// effortCommand handles /effort.
func (m *Manager) effortCommand(ctx context.Context, ag *store.Agent, arg string, reply func(string)) {
	levels := strings.Join(Efforts(ag.Harness), ", ")
	_, def := m.modelChoices(ctx, ag)
	arg = strings.ToLower(strings.TrimSpace(arg))
	if arg == "" {
		reply(fmt.Sprintf("*%s* uses %s.\nChange it with /effort LEVEL: %s. /effort default goes back to the default.", ag.Name, describeModel(ag, def), levels))
		return
	}
	effort := arg
	if effort == "default" {
		effort = ""
	}
	if !validEffort(ag.Harness, effort) {
		reply(fmt.Sprintf("%s does not take %q. Levels: %s.", harnessName(ag.Harness), arg, levels))
		return
	}
	if err := m.store.SetAgentModel(ctx, ag.ID, ag.Model, effort); err != nil {
		reply("⚠️ Could not save that: " + err.Error())
		return
	}
	ag.Effort = effort
	reply(fmt.Sprintf("🧠 From the next message, *%s* uses %s.", ag.Name, describeModel(ag, def)))
}

// parseOnce splits "/once opus max: review this" into overrides and the
// message. The colon is optional: leading words that name a model the
// agent is likely to run or an effort level are taken as overrides.
func parseOnce(ag *store.Agent, suggested []string, arg string) (model, effort, text string) {
	take := func(word string) bool {
		w := strings.ToLower(word)
		switch {
		case effort == "" && validEffort(ag.Harness, w):
			effort = w
		case model == "" && (knownModel(ag.Harness, suggested, w) || knownModel(ag.Harness, suggested, word)):
			model = word
			if ag.Harness == store.HarnessClaude {
				model = w
			}
		default:
			return false
		}
		return true
	}
	if head, rest, ok := strings.Cut(arg, ":"); ok && len(strings.Fields(head)) <= 2 {
		words := strings.Fields(head)
		all := true
		for _, w := range words {
			if !take(w) {
				all = false
			}
		}
		if all {
			return model, effort, strings.TrimSpace(rest)
		}
		model, effort = "", ""
	}
	words := strings.Fields(arg)
	i := 0
	for i < len(words) && i < 2 && take(words[i]) {
		i++
	}
	rest := arg
	for _, w := range words[:i] {
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), w))
	}
	return model, effort, rest
}
