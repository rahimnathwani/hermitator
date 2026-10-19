package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/auth"
	"github.com/rahimnathwani/hermitator/internal/ids"
)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := s.loadViewer(r); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, nil, "login.html", "Log in", "", map[string]string{"Next": safeNext(r.URL.Query().Get("next"))})
}

// safeNext only allows redirects to paths on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	next := safeNext(r.FormValue("next"))
	again := func(msg string) {
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, nil, "login.html", "Log in", "", map[string]string{"Next": next, "Error": msg, "Email": r.FormValue("email")})
	}
	if !s.limiter.allow(clientIP(r), 10, 10*time.Minute) {
		again("Too many attempts. Wait a few minutes and try again.")
		return
	}
	email, password := r.FormValue("email"), r.FormValue("password")
	user, err := s.Store.UserByEmail(ctx, email)
	if err != nil {
		auth.CheckDummy(password)
		again("Wrong email or password.")
		return
	}
	if !auth.CheckPassword(user.PasswordHash, password) {
		again("Wrong email or password.")
		return
	}
	if user.Suspended {
		again("This account is suspended.")
		return
	}
	token := ids.Token()
	if _, err := s.Store.CreateSession(ctx, ids.Hash(token), user.ID, sessionTTL); err != nil {
		s.Log.Error("create session", "err", err)
		again("Something went wrong. Try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookie(sessionCookie), Value: token, Path: "/", MaxAge: int(sessionTTL / time.Second),
		HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
	s.Store.Audit(ctx, user.ID, user.ID, "login", clientIP(r))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.cookie(sessionCookie)); err == nil {
		s.Store.DeleteSession(r.Context(), ids.Hash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookie(sessionCookie), Path: "/", MaxAge: -1, Secure: s.secureCookies()})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, v *viewer) {
	// Always the real user's password, even while impersonating.
	if !auth.CheckPassword(v.Actor.PasswordHash, r.FormValue("current")) {
		s.back(w, r, "/settings", "Your current password was wrong.")
		return
	}
	if r.FormValue("new") != r.FormValue("confirm") {
		s.back(w, r, "/settings", "The new passwords did not match.")
		return
	}
	hash, err := auth.HashPassword(r.FormValue("new"))
	if err != nil {
		s.back(w, r, "/settings", err.Error())
		return
	}
	// SetPassword ends every session, including this one; start a new one.
	if err := s.Store.SetPassword(r.Context(), v.Actor.ID, hash); err != nil {
		s.back(w, r, "/settings", "Could not change the password: "+err.Error())
		return
	}
	s.audit(r, v, "user.password", "")
	token := ids.Token()
	s.Store.CreateSession(r.Context(), ids.Hash(token), v.Actor.ID, sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name: s.cookie(sessionCookie), Value: token, Path: "/", MaxAge: int(sessionTTL / time.Second),
		HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
	s.setFlash(w, "Password changed. Other browsers have been logged out.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
