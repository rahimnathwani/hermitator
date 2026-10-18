package web

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/rahimnathwani/hermitator/internal/orchestrator"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// The panel shows choices as segmented controls rather than dropdowns, so
// every setting and every option is visible without opening anything.

type option struct{ Value, Label, Title string }

// segmented is a radio group drawn as a row of buttons.
type segmented struct {
	Name    string
	Current string
	Options []option
}

func seg(name, current string, opts ...option) segmented {
	return segmented{Name: name, Current: current, Options: opts}
}

func triggerChoice(name, current string) segmented {
	return seg(name, current,
		option{Value: store.TriggerAll, Label: "always"},
		option{Value: store.TriggerMentions, Label: "when mentioned"},
		option{Value: store.TriggerNone, Label: "never"})
}

var memoryLimits = []int{1024, 2048, 4096, 8192, 16384}

func memoryChoice(current int) segmented {
	limits := memoryLimits
	if !slices.Contains(limits, current) {
		// Set some other way; keep it selectable rather than lose it.
		limits = append(slices.Clone(limits), current)
		slices.Sort(limits)
	}
	s := seg("memory_mb", strconv.Itoa(current))
	for _, mb := range limits {
		label := strconv.Itoa(mb) + " MB"
		if mb%1024 == 0 {
			label = strconv.Itoa(mb/1024) + " GB"
		}
		s.Options = append(s.Options, option{Value: strconv.Itoa(mb), Label: label})
	}
	return s
}

// otherModel is the value of the "other…" choice, which takes a model ID
// typed by hand. It cannot be a real model name.
const otherModel = "+other"

// agentFields are the harness, provider, model and effort controls that
// the new-agent and settings forms share.
type agentFields struct {
	Harness      segmented
	Provider     segmented
	Models       []modelSet
	ClaudeEffort segmented
	PiEffort     segmented
}

// modelSet is the model choice for one harness and provider. A form holds
// one for every combination and shows the one selected, so switching
// provider never leaves a model the new provider does not have.
type modelSet struct {
	Harness, Provider string
	Hidden            bool
	Choice            segmented
	Other             string // the free-text input's name
	Custom            string // its value, for a model off the list
	Default           string // what "default" runs, when hermitator decides
}

func modelKey(harness, provider string) string {
	if harness == store.HarnessPi {
		return "pi-" + providerOrCodex(provider)
	}
	return harness
}

// providerName is a pi provider's short display name.
func providerName(id string) string {
	for _, p := range orchestrator.PiProviders {
		if p.ID == providerOrCodex(id) {
			return p.Short
		}
	}
	return id
}

func providerOrCodex(p string) string {
	if p == "" {
		return orchestrator.ProviderCodex
	}
	return p
}

func newAgentFields(a *store.Agent) agentFields {
	f := agentFields{
		Harness: seg("harness", a.Harness,
			option{Value: store.HarnessClaude, Label: "Claude Code"},
			option{Value: store.HarnessPi, Label: "pi"}),
		ClaudeEffort: effortChoice(a, store.HarnessClaude, "effort_claude"),
		PiEffort:     effortChoice(a, store.HarnessPi, "effort_pi"),
		Models:       []modelSet{modelChoice(a, store.HarnessClaude, "", "")},
	}
	f.Provider = seg("provider", providerOrCodex(a.PiProvider))
	for _, p := range orchestrator.PiProviders {
		f.Provider.Options = append(f.Provider.Options, option{Value: p.ID, Label: p.Short, Title: p.Name})
		f.Models = append(f.Models, modelChoice(a, store.HarnessPi, p.ID, p.DefaultModel))
	}
	return f
}

func effortChoice(a *store.Agent, harness, name string) segmented {
	current := ""
	if a.Harness == harness {
		current = a.Effort
	}
	s := seg(name, current, option{Value: "", Label: "default"})
	for _, e := range orchestrator.Efforts(harness) {
		s.Options = append(s.Options, option{Value: e, Label: e})
	}
	return s
}

func modelChoice(a *store.Agent, harness, provider, def string) modelSet {
	key := modelKey(harness, provider)
	active := key == modelKey(a.Harness, a.PiProvider)
	suggested := orchestrator.ModelSuggestions(harness, provider)
	set := modelSet{
		Harness:  harness,
		Provider: provider,
		Hidden:   !active,
		Other:    "model-other:" + key,
		Default:  def,
		Choice:   seg("model:"+key, "", option{Value: "", Label: "default", Title: def}),
	}
	for _, m := range suggested {
		set.Choice.Options = append(set.Choice.Options, option{Value: m, Label: shortModel(m), Title: m})
	}
	set.Choice.Options = append(set.Choice.Options, option{Value: otherModel, Label: "other…"})
	if active && a.Model != "" {
		set.Choice.Current = a.Model
		if !slices.Contains(suggested, a.Model) {
			set.Choice.Current, set.Custom = otherModel, a.Model
		}
	}
	return set
}

// shortModel drops OpenRouter's vendor prefix: "~anthropic/claude-opus-latest"
// reads as "claude-opus-latest". The full ID stays in the tooltip.
func shortModel(m string) string {
	m = strings.TrimPrefix(m, "~")
	if _, after, ok := strings.Cut(m, "/"); ok {
		return after
	}
	return m
}

// formModel reads the model picked for a harness and provider. A plain
// "model" field also works, for scripts posting the form by hand.
func formModel(r *http.Request, harness, provider string) string {
	key := modelKey(harness, provider)
	m := formValue(r, "model:"+key)
	if _, ok := r.Form["model:"+key]; !ok {
		return formValue(r, "model")
	}
	if m == otherModel {
		return formValue(r, "model-other:"+key)
	}
	return m
}

// formEffort reads the effort picked for a harness, dropping anything it
// does not take.
func formEffort(r *http.Request, harness string) string {
	e := formValue(r, "effort_"+harness)
	if !slices.Contains(orchestrator.Efforts(harness), e) {
		return ""
	}
	return e
}
