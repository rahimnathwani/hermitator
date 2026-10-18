// Package web is hermitator's control panel: server-rendered pages with
// htmx for navigation, server-sent events for live updates and xterm.js
// for the terminal.
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rahimnathwani/hermitator/internal/gateway"
	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/orchestrator"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/store"
)

//go:embed templates static
var assets embed.FS

const (
	sessionCookie = "hermitator_session"
	flashCookie   = "hermitator_flash"
	sessionTTL    = 30 * 24 * time.Hour
)

// Gateway is what the panel needs from the WhatsApp side.
type Gateway interface {
	Connected(accountID string) bool
	StartPairing(ctx context.Context, userID string) (*gateway.Pairing, error)
	Pairing(id, userID string) (*gateway.Pairing, error)
	PairPhone(ctx context.Context, p *gateway.Pairing, phone string) (string, error)
	Remove(ctx context.Context, accountID string) error
	ApplyKind(ctx context.Context, accountID string) error
}

// Server serves the control panel.
type Server struct {
	Store   *store.Store
	Agents  *orchestrator.Manager
	Gateway Gateway
	Images  *sandbox.Images
	Log     *slog.Logger
	// PublicURL is where users reach the panel; it decides whether cookies
	// are marked Secure.
	PublicURL string
	Version   string

	pages   map[string]*template.Template
	assetV  string
	limiter limiter
}

// assetVersion hashes the embedded static files, so asset URLs change
// exactly when their contents do and can be cached forever.
func assetVersion() string {
	h := sha256.New()
	fs.WalkDir(assets, "static", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(assets, path)
			h.Write([]byte(path))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Handler builds the HTTP handler.
func (s *Server) Handler() (http.Handler, error) {
	s.assetV = assetVersion()
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	static, _ := fs.Sub(assets, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheForever(http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)

	// Pages for users who own agents. Admins reach them by impersonating.
	tenant := func(pattern string, h handler) { mux.Handle(pattern, s.authed(s.tenantOnly(h))) }
	tenant("GET /{$}", s.dashboard)
	tenant("POST /agents", s.createAgent)
	tenant("GET /agents/{id}", s.agentPage)
	tenant("GET /agents/{id}/card", s.agentCard)
	tenant("POST /agents/{id}/settings", s.updateAgent)
	tenant("POST /agents/{id}/wake", s.wakeAgent)
	tenant("POST /agents/{id}/upgrade-image", s.upgradeImage)
	tenant("POST /agents/{id}/sleep", s.sleepAgent)
	tenant("POST /agents/{id}/stop", s.stopTurn)
	tenant("POST /agents/{id}/new-session", s.newSession)
	tenant("POST /agents/{id}/link-code", s.newLinkCode)
	tenant("POST /agents/{id}/prompt", s.prompt)
	tenant("POST /agents/{id}/delete", s.deleteAgent)
	tenant("POST /agents/{id}/routes/delete", s.deleteRoute)
	tenant("POST /agents/{id}/routes/triggers", s.routeTriggers)
	tenant("GET /agents/{id}/events", s.agentEvents)
	tenant("GET /agents/{id}/terminal", s.terminalPage)
	tenant("GET /agents/{id}/terminal/ws", s.terminalSocket)
	tenant("GET /accounts", s.accountsPage)
	tenant("POST /accounts/pair", s.startPairing)
	tenant("GET /accounts/pair/{pid}", s.pairPage)
	tenant("GET /accounts/pair/{pid}/events", s.pairEvents)
	tenant("POST /accounts/pair/{pid}/phone", s.pairPhone)
	tenant("POST /accounts/{id}/fallback", s.setFallback)
	tenant("POST /accounts/{id}/kind", s.setKind)
	tenant("POST /accounts/{id}/delete", s.deleteAccount)
	tenant("GET /settings", s.settingsPage)
	tenant("POST /settings/secret", s.saveSecret)
	tenant("POST /settings/numbers", s.addNumber)
	tenant("POST /settings/numbers/delete", s.deleteNumber)
	tenant("GET /settings/codex/login", s.codexLogin)
	tenant("POST /settings/codex/disconnect", s.codexDisconnect)

	// Everyone can change their own password.
	mux.Handle("POST /account/password", s.authed(s.changePassword))

	admin := func(pattern string, h handler) { mux.Handle(pattern, s.authed(s.adminOnly(h))) }
	admin("GET /admin/users", s.usersPage)
	admin("POST /admin/users", s.createUser)
	admin("POST /admin/users/{id}/suspend", s.suspendUser)
	admin("POST /admin/users/{id}/quota", s.setQuota)
	admin("POST /admin/users/{id}/password", s.resetPassword)
	admin("POST /admin/users/{id}/delete", s.deleteUser)
	admin("POST /admin/impersonate/{id}", s.impersonate)
	admin("GET /admin/audit", s.auditPage)
	// Ending impersonation is checked against the real (acting) user.
	mux.Handle("POST /admin/stop-impersonating", s.authed(s.stopImpersonating))

	csrf := http.NewCrossOriginProtection()
	return s.headers(csrf.Handler(mux)), nil
}

// viewer is who is looking at a page.
type viewer struct {
	Session *store.Session
	Actor   *store.User // the human who logged in
	User    *store.User // whose resources they act on
}

// Impersonating reports whether an admin is acting as another user.
func (v *viewer) Impersonating() bool { return v.Actor.ID != v.User.ID }

type handler func(w http.ResponseWriter, r *http.Request, v *viewer)

func (s *Server) authed(h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.loadViewer(r)
		if err != nil {
			if r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, "/events") && !strings.HasSuffix(r.URL.Path, "/ws") {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Error(w, "not logged in", http.StatusUnauthorized)
			return
		}
		h(w, r, v)
	})
}

func (s *Server) loadViewer(r *http.Request) (*viewer, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, err
	}
	sess, err := s.Store.Session(r.Context(), ids.Hash(c.Value))
	if err != nil {
		return nil, err
	}
	actor, err := s.Store.User(r.Context(), sess.ActorID)
	if err != nil || actor.Suspended {
		return nil, errors.New("user unavailable")
	}
	user := actor
	if sess.Impersonating() {
		if !actor.IsAdmin() {
			return nil, errors.New("impersonation by a non-admin")
		}
		if user, err = s.Store.User(r.Context(), sess.EffectiveID); err != nil {
			return nil, err
		}
	}
	return &viewer{Session: sess, Actor: actor, User: user}, nil
}

// tenantOnly keeps admins (unless impersonating) out of pages that act on
// agents and WhatsApp accounts. An admin's own account owns nothing.
func (s *Server) tenantOnly(h handler) handler {
	return func(w http.ResponseWriter, r *http.Request, v *viewer) {
		if v.User.IsAdmin() {
			if r.Method == http.MethodGet && r.URL.Path == "/" {
				http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
				return
			}
			http.Error(w, "Admins do not own agents or WhatsApp accounts. Impersonate a user first.", http.StatusForbidden)
			return
		}
		h(w, r, v)
	}
}

func (s *Server) adminOnly(h handler) handler {
	return func(w http.ResponseWriter, r *http.Request, v *viewer) {
		if !v.Actor.IsAdmin() || v.Impersonating() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r, v)
	}
}

func (s *Server) audit(r *http.Request, v *viewer, action, detail string) {
	if err := s.Store.Audit(r.Context(), v.Actor.ID, v.User.ID, action, detail); err != nil {
		s.Log.Error("audit", "err", err)
	}
}

func (s *Server) secureCookies() bool { return strings.HasPrefix(s.PublicURL, "https://") }

func (s *Server) setFlash(w http.ResponseWriter, msg string) {
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: url.QueryEscape(msg), Path: "/", MaxAge: 60,
		HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode})
}

func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Path: "/", MaxAge: -1})
	msg, _ := url.QueryUnescape(c.Value)
	return msg
}

// back redirects to the referring page (or fallback) with a flash message.
func (s *Server) back(w http.ResponseWriter, r *http.Request, fallback, flash string) {
	if flash != "" {
		s.setFlash(w, flash)
	}
	target := fallback
	if ref, err := url.Parse(r.Referer()); err == nil && ref.Path != "" && sameOrigin(r, ref) {
		target = ref.RequestURI()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func sameOrigin(r *http.Request, u *url.URL) bool {
	return u.Host == "" || u.Host == r.Host
}

func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func cacheForever(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Asset URLs carry the build version, so they can be cached hard.
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP is the browser's address, trusting X-Forwarded-For only from a
// reverse proxy on the same machine.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			parts := strings.Split(fwd, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	return host
}

// limiter throttles login attempts per client address.
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (l *limiter) allow(key string, n int, per time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	now := time.Now()
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < per {
			recent = append(recent, t)
		}
	}
	if len(recent) >= n {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	return true
}

// stream is a server-sent events response. Handlers write to it from
// their own goroutine only.
type stream struct {
	w http.ResponseWriter
	f http.Flusher
}

func newStream(w http.ResponseWriter) (*stream, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming unsupported")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &stream{w: w, f: f}, nil
}

// send writes one event with a JSON payload.
func (s *stream) send(event string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data)
	s.f.Flush()
}

// ping keeps proxies from closing an idle stream.
func (s *stream) ping() {
	fmt.Fprint(s.w, ": ping\n\n")
	s.f.Flush()
}

const pingInterval = 25 * time.Second
