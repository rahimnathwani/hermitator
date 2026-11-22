package turn

import (
	"regexp"
	"time"
)

// Antigravity reports tokens but no cost, so its turns are priced here at
// the API list price of the model they ran on: what spending limits and
// /usage count, as for the other harnesses. Prices are US dollars per
// million tokens, from Google's Gemini API price list
// (https://aistudio.google.com/docs/pricing), Vertex AI's partner models
// (https://cloud.google.com/model-garden/partner-models/pricing) and
// Anthropic's, as of November 2026. A model missing here costs nothing,
// and the turn says it could not be priced.

// Rate is a model's price per million tokens from a date on.
type Rate struct {
	From                                 time.Time
	Input, CacheRead, CacheWrite, Output float64
}

// Tokens are what a model call used. Input excludes the cached part;
// Output includes thinking, which is billed like output.
type Tokens struct {
	Input, CacheRead, CacheWrite, Output int
}

func day(s string) time.Time {
	t, _ := time.Parse(time.DateOnly, s)
	return t
}

// Prices are by model, as Antigravity names them without a thinking
// level, each list oldest first.
var Prices = map[string][]Rate{
	"gemini-4-neon":     {{Input: 3, CacheRead: 0.15, CacheWrite: 3, Output: 15}},
	"gemini-4-hydrogen": {{Input: 1.5, CacheRead: 0.075, CacheWrite: 1.5, Output: 7.5}},
	"gemini-4-argon": {
		{Input: 2, CacheRead: 0.10, CacheWrite: 2, Output: 10},
		{From: day("2026-12-01"), Input: 4, CacheRead: 0.20, CacheWrite: 4, Output: 20},
	},
	"gemini-3.8-flash": {
		{Input: 0.75, CacheRead: 0.075, CacheWrite: 0.75, Output: 3.75},
		{From: day("2027-01-01"), Input: 1.5, CacheRead: 0.15, CacheWrite: 1.5, Output: 7.5},
	},
	"gpt-oss-120b":      {{Input: 0.09, CacheRead: 0.09, CacheWrite: 0.09, Output: 0.36}},
	"claude-opus-5-5":   {{Input: 4, CacheRead: 0.20, CacheWrite: 5, Output: 20}},
	"claude-sonnet-5-5": {{Input: 2, CacheRead: 0.10, CacheWrite: 2.5, Output: 10}},
}

// thinkingLevel is the suffix Antigravity's model IDs carry
// (gemini-4-neon-high); it changes how much thinking is billed, not the
// rate.
var thinkingLevel = regexp.MustCompile(`-(low|medium|high|xhigh|max)$`)

// Price is what tokens on model cost now, and whether the model's price
// is known.
func Price(model string, t Tokens) (float64, bool) {
	return PriceAt(model, t, time.Now())
}

// PriceAt is Price on a given day.
func PriceAt(model string, t Tokens, at time.Time) (float64, bool) {
	rates, ok := Prices[model]
	if !ok {
		rates, ok = Prices[thinkingLevel.ReplaceAllString(model, "")]
	}
	if !ok {
		return 0, false
	}
	r := rates[0]
	for _, x := range rates[1:] {
		if !at.Before(x.From) {
			r = x
		}
	}
	return (float64(t.Input)*r.Input + float64(t.CacheRead)*r.CacheRead +
		float64(t.CacheWrite)*r.CacheWrite + float64(t.Output)*r.Output) / 1e6, true
}
