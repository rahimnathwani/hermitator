package web

import (
	"net/http"
	"time"

	"github.com/rahimnathwani/hermitator/internal/store"
)

type accountView struct {
	*store.Account
	Online   bool
	Fallback string
}

func (s *Server) accountFor(r *http.Request, v *viewer) (*store.Account, error) {
	a, err := s.Store.Account(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if a.UserID != v.User.ID {
		return nil, store.ErrNotFound
	}
	return a, nil
}

func (s *Server) accountsPage(w http.ResponseWriter, r *http.Request, v *viewer) {
	ctx := r.Context()
	accounts, _ := s.Store.Accounts(ctx, v.User.ID)
	agents, _ := s.Store.Agents(ctx, v.User.ID)
	names := map[string]string{}
	for _, a := range agents {
		names[a.ID] = a.Name
	}
	var views []accountView
	for _, a := range accounts {
		views = append(views, accountView{Account: a, Online: s.Gateway.Connected(a.ID), Fallback: names[a.FallbackAgentID]})
	}
	numbers, _ := s.Store.OwnerNumbers(ctx, v.User.ID)
	s.render(w, r, v, "accounts.html", "WhatsApp", "accounts", map[string]any{"Accounts": views, "Agents": agents, "Numbers": numbers})
}

func (s *Server) startPairing(w http.ResponseWriter, r *http.Request, v *viewer) {
	p, err := s.Gateway.StartPairing(r.Context(), v.User.ID)
	if err != nil {
		s.back(w, r, "/accounts", "Could not reach WhatsApp: "+err.Error())
		return
	}
	http.Redirect(w, r, "/accounts/pair/"+p.ID, http.StatusSeeOther)
}

func (s *Server) pairPage(w http.ResponseWriter, r *http.Request, v *viewer) {
	if _, err := s.Gateway.Pairing(r.PathValue("pid"), v.User.ID); err != nil {
		s.setFlash(w, "That pairing has ended. Start a new one.")
		http.Redirect(w, r, "/accounts", http.StatusSeeOther)
		return
	}
	s.render(w, r, v, "pair.html", "Link WhatsApp", "accounts", map[string]any{"ID": r.PathValue("pid")})
}

func (s *Server) pairEvents(w http.ResponseWriter, r *http.Request, v *viewer) {
	p, err := s.Gateway.Pairing(r.PathValue("pid"), v.User.ID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	events, stop := p.Subscribe()
	defer stop()
	st, err := newStream(w)
	if err != nil {
		return
	}
	tick := time.NewTicker(pingInterval)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-events:
			st.send(ev.Type, ev)
			if ev.Type == "success" || ev.Type == "error" {
				return
			}
		case <-tick.C:
			st.ping()
		}
	}
}

func (s *Server) pairPhone(w http.ResponseWriter, r *http.Request, v *viewer) {
	p, err := s.Gateway.Pairing(r.PathValue("pid"), v.User.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	code, err := s.Gateway.PairPhone(r.Context(), p, formValue(r, "phone"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(code))
}

func (s *Server) setFallback(w http.ResponseWriter, r *http.Request, v *viewer) {
	acct, err := s.accountFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	agentID := formValue(r, "agent")
	if agentID != "" {
		a, err := s.Store.Agent(r.Context(), agentID)
		if err != nil || a.UserID != v.User.ID {
			http.NotFound(w, r)
			return
		}
	}
	s.Store.SetAccountFallback(r.Context(), acct.ID, agentID)
	s.audit(r, v, "account.fallback", "account="+acct.ID+" agent="+agentID)
	s.back(w, r, "/accounts", "Saved.")
}

func (s *Server) setKind(w http.ResponseWriter, r *http.Request, v *viewer) {
	acct, err := s.accountFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	kind := formValue(r, "kind")
	if kind != store.KindAgent && kind != store.KindPersonal {
		http.Error(w, "unknown kind", http.StatusBadRequest)
		return
	}
	s.Store.SetAccountKind(r.Context(), acct.ID, kind)
	s.Gateway.ApplyKind(r.Context(), acct.ID)
	s.audit(r, v, "account.kind", "account="+acct.ID+" kind="+kind)
	s.back(w, r, "/accounts", "Saved.")
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request, v *viewer) {
	acct, err := s.accountFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.Gateway.Remove(r.Context(), acct.ID); err != nil {
		s.back(w, r, "/accounts", "Could not remove the account: "+err.Error())
		return
	}
	s.audit(r, v, "account.remove", "account="+acct.ID+" phone="+acct.Phone)
	s.back(w, r, "/accounts", "Unlinked +"+acct.Phone+". It no longer appears in your phone's linked devices.")
}
