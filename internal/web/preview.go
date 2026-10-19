package web

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/orchestrator"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// Preview links: requests for PORT-AGENT.<preview domain> are proxied to
// that port in the agent's sandbox. The link's secret arrives once in the
// query and is swapped for a cookie scoped to that one host; the panel's
// session never reaches these hosts, and the agent never sees the cookie.

const previewCookie = "hermitator_preview"

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func (s *Server) servePreview(w http.ResponseWriter, r *http.Request, agentID string, port int) {
	ctx := r.Context()
	p, err := s.Store.Preview(ctx, agentID, port)
	if err != nil {
		previewPage(w, http.StatusNotFound, "No preview here", "This link was turned off, or never existed. Ask the agent for a new one.")
		return
	}
	secret := s.Agents.PreviewSecret(p)
	name := s.cookie(previewCookie)
	if q := r.URL.Query(); q.Has(orchestrator.PreviewParam) {
		if subtle.ConstantTimeCompare([]byte(q.Get(orchestrator.PreviewParam)), []byte(secret)) != 1 {
			previewPage(w, http.StatusForbidden, "Link not valid", "This preview link was turned off and replaced. Ask the agent for a new one.")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: name, Value: secret, Path: "/", MaxAge: int(30 * 24 * time.Hour / time.Second),
			HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode})
		q.Del(orchestrator.PreviewParam)
		u := *r.URL
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
		return
	}
	if c, err := r.Cookie(name); err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(secret)) != 1 {
		previewPage(w, http.StatusForbidden, "Open the full link", "This page needs the whole preview link, as the agent sent it.")
		return
	}

	ip, release, err := s.Agents.PreviewTarget(ctx, agentID)
	if err != nil {
		s.Log.Warn("preview wake", "agent", agentID, "err", err)
		previewPage(w, http.StatusBadGateway, "The agent did not wake", "Try again in a moment.")
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		// A live-reload socket can stay open for days; it counts as one
		// visit, so a forgotten tab does not keep the machine awake.
		release()
	} else {
		defer release()
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(ip.String(), strconv.Itoa(port))}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			// Dev servers such as Vite only answer the hosts they expect.
			pr.Out.Host = "localhost:" + strconv.Itoa(port)
			dropCookie(pr.Out, name)
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			previewPage(w, http.StatusBadGateway, "Nothing is answering",
				fmt.Sprintf("Nothing answered on port %d in the agent's machine. Ask the agent to start the server, listening on 0.0.0.0 rather than localhost.", port))
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set("X-Robots-Tag", "noindex")
			return nil
		},
	}
	rp.ServeHTTP(w, r)
}

// dropCookie removes one cookie from a request's Cookie headers.
func dropCookie(r *http.Request, name string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != name {
			r.AddCookie(c)
		}
	}
}

func previewPage(w http.ResponseWriter, status int, title, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>
<body style="font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 16px;color:#222"><h1 style="font-size:1.3rem">%s</h1><p>%s</p><p style="color:#777;font-size:.9rem">hermitator preview</p>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(text))
}

// previewAllowed tells Caddy whether to get a certificate for a hostname:
// only for ports that have a preview link.
func (s *Server) previewAllowed(w http.ResponseWriter, r *http.Request) {
	agentID, port, ok := s.Agents.ParsePreviewHost(r.URL.Query().Get("domain"))
	// Caddy asks directly; a request it forwarded from outside is not it.
	ok = ok && r.Header.Get("X-Forwarded-For") == ""
	if ok {
		_, err := s.Store.Preview(r.Context(), agentID, port)
		ok = err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.Log.Warn("preview lookup", "err", err)
		}
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}
