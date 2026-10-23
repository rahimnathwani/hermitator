package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/orchestrator"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// providerView is one of a user's own providers on the Settings page.
type providerView struct {
	*store.Provider
	ID       string // pi's name for it, which agents store
	APIName  string
	KeySaved time.Time
}

func (s *Server) providerViews(ctx context.Context, userID string, times map[string]time.Time) []providerView {
	own, _ := s.Store.Providers(ctx, userID)
	var out []providerView
	for _, p := range own {
		out = append(out, providerView{Provider: p, ID: orchestrator.CustomProvider(p.Name), APIName: apiName(p.API),
			KeySaved: times[store.SecretProviderPrefix+p.Name]})
	}
	return out
}

func apiName(id string) string {
	for _, a := range orchestrator.ProviderAPIs {
		if a.ID == id {
			return a.Name
		}
	}
	return id
}

// saveProvider adds one of the user's own providers, or replaces the one
// of the same name.
func (s *Server) saveProvider(w http.ResponseWriter, r *http.Request, v *viewer) {
	p := &store.Provider{
		UserID:    v.User.ID,
		Name:      formValue(r, "name"),
		API:       formValue(r, "api"),
		BaseURL:   formValue(r, "base_url"),
		Models:    []string{r.FormValue("models")},
		Images:    r.FormValue("images") != "",
		Reasoning: r.FormValue("reasoning") != "",
	}
	if raw := strings.ReplaceAll(formValue(r, "context_window"), ",", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			s.back(w, r, "/settings", "The context window is a number of tokens, like 262144; leave it empty if you do not know it.")
			return
		}
		p.ContextWindow = n
	}
	_, err := s.Store.Provider(r.Context(), v.User.ID, strings.ToLower(p.Name))
	isNew := err != nil
	if err := s.Agents.SaveProvider(r.Context(), p, formValue(r, "key")); err != nil {
		s.back(w, r, "/settings", "Could not save the provider: "+err.Error()+".")
		return
	}
	s.audit(r, v, "provider.set", fmt.Sprintf("%s %s %s", p.Name, p.API, p.BaseURL))
	msg := "Saved " + p.Name + ". Agents on it use the change from their next turn."
	if isNew {
		msg = "Added " + p.Name + ". Choose it as a pi agent's provider in that agent's settings."
	}
	s.setFlash(w, msg)
	// Not back to the referrer, which may be the form prefilled for an edit.
	http.Redirect(w, r, "/settings#providers", http.StatusSeeOther)
}

func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request, v *viewer) {
	name := formValue(r, "name")
	if err := s.Agents.DeleteProvider(r.Context(), v.User.ID, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.back(w, r, "/settings", "No such provider.")
			return
		}
		s.back(w, r, "/settings", "Could not remove it: "+err.Error())
		return
	}
	s.audit(r, v, "provider.delete", name)
	s.back(w, r, "/settings", "Removed "+name+" and its key.")
}
