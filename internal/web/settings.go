package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/orchestrator"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// secretFields are the credentials users paste in.
var secretFields = map[string]string{
	"claude":     store.SecretClaudeToken,
	"deepseek":   store.SecretDeepSeekKey,
	"openrouter": store.SecretOpenRouterKey,
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, v *viewer) {
	ctx := r.Context()
	times, _ := s.Store.SecretTimes(ctx, v.User.ID)
	agents, _ := s.Store.Agents(ctx, v.User.ID)
	numbers, _ := s.Store.OwnerNumbers(ctx, v.User.ID)
	// Which agents each credential serves, so removing one can name them.
	uses := map[string][]string{}
	for _, a := range agents {
		key := a.Harness
		if a.Harness == store.HarnessPi {
			key = providerOrCodex(a.PiProvider)
		}
		uses[key] = append(uses[key], a.Name)
	}
	secretHosts, _ := s.Store.SecretHosts(ctx, v.User.ID)
	// ?provider=NAME fills the provider form in to edit that one.
	editProvider := &store.Provider{API: store.APIResponses}
	if p, err := s.Store.Provider(ctx, v.User.ID, r.URL.Query().Get("provider")); err == nil {
		editProvider = p
	}
	loc := v.User.Location()
	now := time.Now().In(loc)
	spentDay, _ := s.Store.SpentSince(ctx, v.User.ID, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc))
	spentMonth, _ := s.Store.SpentSince(ctx, v.User.ID, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc))
	s.render(w, r, v, "settings.html", "Settings", "settings", map[string]any{
		"Uses":        uses,
		"GitHub":      times[store.SecretGitHubToken],
		"GitHubLogin": v.User.GitHubLogin,
		"GitName":     v.User.GitName,
		"GitEmail":    v.User.GitEmail,
		"Secrets":     envSecrets(times, secretHosts),
		"Egress":      v.User.Egress,
		"EgressHere":  s.Agents.Egress(),
		"Timezone":    v.User.Timezone,
		"Now":         now.Format("Mon 15:04"),
		"DailyCap":    v.User.DailyCap,
		"MonthlyCap":  v.User.MonthlyCap,
		"SpentDay":    spentDay,
		"SpentMonth":  spentMonth,
		"Numbers":     numbers,
		"Claude":      times[store.SecretClaudeToken],
		"DeepSeek":    times[store.SecretDeepSeekKey],
		"OpenRouter":  times[store.SecretOpenRouterKey],
		"Codex":       s.Agents.HasPiProvider(ctx, v.User.ID, orchestrator.ProviderCodex),
		"CodexSince":  times[store.SecretPiAuth],
		"HasAgents":   len(agents) > 0,
		"Providers":   s.providerViews(ctx, v.User.ID, times),
		"Edit":        editProvider,
		"APIs":        orchestrator.ProviderAPIs,
	})
}

func (s *Server) saveSecret(w http.ResponseWriter, r *http.Request, v *viewer) {
	field := formValue(r, "field")
	name, ok := secretFields[field]
	if !ok {
		http.Error(w, "unknown secret", http.StatusBadRequest)
		return
	}
	value := strings.TrimSpace(r.FormValue("value"))
	if r.FormValue("remove") != "" {
		value = ""
	}
	if value != "" && strings.ContainsAny(value, " \t\r\n") {
		s.back(w, r, "/settings", "That does not look like a token: it contains whitespace.")
		return
	}
	if field == "claude" && value != "" && !strings.HasPrefix(value, "sk-ant-") {
		s.back(w, r, "/settings", "Claude tokens from `claude setup-token` start with sk-ant-.")
		return
	}
	if err := s.Agents.PutSecret(r.Context(), v.User.ID, name, value); err != nil {
		s.back(w, r, "/settings", "Could not save: "+err.Error())
		return
	}
	s.Agents.ForgetSecrets(v.User.ID)
	if value == "" {
		s.audit(r, v, "secret.delete", name)
		s.back(w, r, "/settings", "Removed.")
		return
	}
	s.audit(r, v, "secret.set", name)
	s.back(w, r, "/settings", "Saved. Agents use it from their next turn.")
}

// codexLogin runs the ChatGPT device-code login and streams its progress.
// Leaving the page cancels it.
func (s *Server) codexLogin(w http.ResponseWriter, r *http.Request, v *viewer) {
	events, err := s.Agents.CodexLogin(r.Context(), v.User.ID)
	st, serr := newStream(w)
	if serr != nil {
		return
	}
	if err != nil {
		msg := err.Error()
		if errors.Is(err, orchestrator.ErrNoAgent) {
			msg = "The login runs inside one of your agents' sandboxes. Create an agent first."
		}
		st.send("error", orchestrator.LoginEvent{Type: "error", Message: msg})
		return
	}
	tick := time.NewTicker(pingInterval)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			st.send(ev.Type, ev)
			if ev.Type == "done" {
				s.audit(r, v, "secret.set", store.SecretPiAuth+" openai-codex")
			}
		case <-tick.C:
			st.ping()
		}
	}
}

func (s *Server) codexDisconnect(w http.ResponseWriter, r *http.Request, v *viewer) {
	if err := s.Agents.DisconnectPiProvider(r.Context(), v.User.ID, orchestrator.ProviderCodex); err != nil {
		s.back(w, r, "/settings", "Could not disconnect: "+err.Error())
		return
	}
	s.audit(r, v, "secret.delete", store.SecretPiAuth+" openai-codex")
	s.back(w, r, "/settings", "ChatGPT disconnected.")
}

func (s *Server) addNumber(w http.ResponseWriter, r *http.Request, v *viewer) {
	phone := store.NormalizePhone(formValue(r, "phone"))
	if len(phone) < 8 || len(phone) > 15 {
		s.back(w, r, "/settings", "Enter the full number with its country code, like +44 7700 900123.")
		return
	}
	if err := s.Store.AddOwnerNumber(r.Context(), v.User.ID, phone); err != nil {
		s.back(w, r, "/settings", "Could not save: "+err.Error())
		return
	}
	s.audit(r, v, "number.add", "+"+phone)
	s.back(w, r, "/settings", "Added +"+phone+".")
}

func (s *Server) deleteNumber(w http.ResponseWriter, r *http.Request, v *viewer) {
	phone := store.NormalizePhone(formValue(r, "phone"))
	if err := s.Store.DeleteOwnerNumber(r.Context(), v.User.ID, phone); err != nil {
		s.back(w, r, "/settings", "No such number.")
		return
	}
	s.audit(r, v, "number.delete", "+"+phone)
	s.back(w, r, "/settings", "Removed +"+phone+".")
}

// saveLimits sets the time zone schedules use and the spending caps.
func (s *Server) saveLimits(w http.ResponseWriter, r *http.Request, v *viewer) {
	tz := formValue(r, "timezone")
	if tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil || tz == "Local" {
			s.back(w, r, "/settings", tz+" is not a time zone name; use one like Europe/London.")
			return
		}
		tz = loc.String()
	}
	caps := [2]float64{}
	for i, field := range []string{"daily_cap", "monthly_cap"} {
		if raw := formValue(r, field); raw != "" {
			f, err := strconv.ParseFloat(strings.TrimPrefix(raw, "$"), 64)
			if err != nil || f < 0 || f > 1e6 {
				s.back(w, r, "/settings", "Spending limits are amounts in dollars, such as 5 or 2.50; leave one empty for no limit.")
				return
			}
			caps[i] = f
		}
	}
	ctx := r.Context()
	s.Store.SetTimezone(ctx, v.User.ID, tz)
	s.Store.SetCaps(ctx, v.User.ID, caps[0], caps[1])
	s.audit(r, v, "settings.limits", fmt.Sprintf("tz=%s daily=%.2f monthly=%.2f", tz, caps[0], caps[1]))
	s.back(w, r, "/settings", "Saved.")
}
