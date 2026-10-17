package web

import (
	"net/http"
	"net/mail"
	"strconv"

	"github.com/rahimnathwani/hermitator/internal/auth"
	"github.com/rahimnathwani/hermitator/internal/store"
)

type userView struct {
	*store.User
	Agents   int
	Accounts int
}

func (s *Server) userFor(r *http.Request) (*store.User, error) {
	return s.Store.User(r.Context(), r.PathValue("id"))
}

func (s *Server) usersPage(w http.ResponseWriter, r *http.Request, v *viewer) {
	ctx := r.Context()
	users, _ := s.Store.Users(ctx)
	var views []userView
	for _, u := range users {
		n, _ := s.Store.CountAgents(ctx, u.ID)
		accts, _ := s.Store.Accounts(ctx, u.ID)
		views = append(views, userView{User: u, Agents: n, Accounts: len(accts)})
	}
	s.render(w, r, v, "users.html", "Users", "users", map[string]any{"Users": views})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, v *viewer) {
	email := formValue(r, "email")
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		s.back(w, r, "/admin/users", "That is not an email address.")
		return
	}
	role := formValue(r, "role")
	if role != store.RoleAdmin {
		role = store.RoleUser
	}
	hash, err := auth.HashPassword(r.FormValue("password"))
	if err != nil {
		s.back(w, r, "/admin/users", err.Error())
		return
	}
	u := &store.User{Email: email, PasswordHash: hash, Role: role}
	if err := s.Store.CreateUser(r.Context(), u); err != nil {
		s.back(w, r, "/admin/users", "Could not create the user: "+err.Error())
		return
	}
	s.audit(r, v, "user.create", u.Email+" role="+role)
	s.back(w, r, "/admin/users", "Created "+u.Email+".")
}

func (s *Server) suspendUser(w http.ResponseWriter, r *http.Request, v *viewer) {
	u, err := s.userFor(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if u.ID == v.Actor.ID {
		s.back(w, r, "/admin/users", "You cannot suspend yourself.")
		return
	}
	suspend := !u.Suspended
	s.Store.SetSuspended(r.Context(), u.ID, suspend)
	if suspend {
		// A suspended user's agents should not keep answering.
		agents, _ := s.Store.Agents(r.Context(), u.ID)
		for _, a := range agents {
			s.Agents.Abort(a.ID)
			s.Agents.Sleep(r.Context(), a.ID)
		}
		s.audit(r, v, "user.suspend", u.Email)
		s.back(w, r, "/admin/users", "Suspended "+u.Email+" and put their agents to sleep.")
		return
	}
	s.audit(r, v, "user.unsuspend", u.Email)
	s.back(w, r, "/admin/users", "Reinstated "+u.Email+".")
}

func (s *Server) setQuota(w http.ResponseWriter, r *http.Request, v *viewer) {
	u, err := s.userFor(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	n, err := strconv.Atoi(formValue(r, "max_agents"))
	if err != nil || n < 0 || n > 1000 {
		s.back(w, r, "/admin/users", "The agent limit must be a number from 0 to 1000.")
		return
	}
	s.Store.SetMaxAgents(r.Context(), u.ID, n)
	s.audit(r, v, "user.quota", u.Email+" max_agents="+strconv.Itoa(n))
	s.back(w, r, "/admin/users", "Saved.")
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request, v *viewer) {
	u, err := s.userFor(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	hash, err := auth.HashPassword(r.FormValue("password"))
	if err != nil {
		s.back(w, r, "/admin/users", err.Error())
		return
	}
	s.Store.SetPassword(r.Context(), u.ID, hash)
	s.audit(r, v, "user.password_reset", u.Email)
	s.back(w, r, "/admin/users", "Password reset for "+u.Email+"; their sessions were ended.")
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, v *viewer) {
	u, err := s.userFor(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if u.ID == v.Actor.ID {
		s.back(w, r, "/admin/users", "You cannot delete yourself.")
		return
	}
	if formValue(r, "confirm") != u.Email {
		s.back(w, r, "/admin/users", "Type the user's email address to confirm.")
		return
	}
	ctx := r.Context()
	agents, _ := s.Store.Agents(ctx, u.ID)
	for _, a := range agents {
		if err := s.Agents.DeleteAgent(ctx, a.ID); err != nil {
			s.back(w, r, "/admin/users", "Could not delete agent "+a.Name+": "+err.Error())
			return
		}
	}
	accounts, _ := s.Store.Accounts(ctx, u.ID)
	for _, a := range accounts {
		s.Gateway.Remove(ctx, a.ID)
	}
	if err := s.Store.DeleteUser(ctx, u.ID); err != nil {
		s.back(w, r, "/admin/users", "Could not delete: "+err.Error())
		return
	}
	s.audit(r, v, "user.delete", u.Email)
	s.back(w, r, "/admin/users", "Deleted "+u.Email+", their agents and their WhatsApp links.")
}

func (s *Server) impersonate(w http.ResponseWriter, r *http.Request, v *viewer) {
	u, err := s.userFor(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if u.IsAdmin() || u.Suspended {
		s.back(w, r, "/admin/users", "You can only act as an active, non-admin user.")
		return
	}
	if err := s.Store.SetEffectiveUser(r.Context(), v.Session.TokenHash, u.ID); err != nil {
		s.back(w, r, "/admin/users", err.Error())
		return
	}
	s.Store.Audit(r.Context(), v.Actor.ID, u.ID, "impersonate.start", u.Email)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) stopImpersonating(w http.ResponseWriter, r *http.Request, v *viewer) {
	if !v.Impersonating() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.Store.SetEffectiveUser(r.Context(), v.Session.TokenHash, v.Actor.ID)
	s.audit(r, v, "impersonate.stop", v.User.Email)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

type auditView struct {
	*store.AuditEntry
	Actor, Effective string
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request, v *viewer) {
	ctx := r.Context()
	entries, _ := s.Store.AuditLog(ctx, 300)
	emails := map[string]string{}
	email := func(id string) string {
		if e, ok := emails[id]; ok {
			return e
		}
		e := id
		if u, err := s.Store.User(ctx, id); err == nil {
			e = u.Email
		}
		emails[id] = e
		return e
	}
	var views []auditView
	for _, e := range entries {
		views = append(views, auditView{AuditEntry: e, Actor: email(e.ActorID), Effective: email(e.EffectiveID)})
	}
	s.render(w, r, v, "audit.html", "Audit log", "audit", map[string]any{"Entries": views})
}
