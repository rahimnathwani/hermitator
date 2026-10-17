package web

import (
	"errors"
	"net/http"
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
	s.render(w, r, v, "settings.html", "Settings", "settings", map[string]any{
		"Claude":     times[store.SecretClaudeToken],
		"DeepSeek":   times[store.SecretDeepSeekKey],
		"OpenRouter": times[store.SecretOpenRouterKey],
		"Codex":      s.Agents.HasPiProvider(ctx, v.User.ID, orchestrator.ProviderCodex),
		"CodexSince": times[store.SecretPiAuth],
		"HasAgents":  len(agents) > 0,
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
