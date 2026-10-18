package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// page is what every full-page template receives.
type page struct {
	Title   string
	Nav     string
	V       *viewer
	Flash   string
	Version string // the asset version, for cache-busting URLs
	Data    any
}

var funcs = template.FuncMap{
	"ago": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return humanDuration(time.Since(t)) + " ago"
	},
	"until":    func(t time.Time) string { return humanDuration(time.Until(t)) },
	"dur":      humanDuration,
	"triggers": triggerChoice,
	"icon":     icon,
	"capital": func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	},
	"turnStatus": func(s string) string {
		switch s {
		case "ok":
			return "Done"
		case "error":
			return "Failed"
		case "aborted":
			return "Stopped"
		}
		return strings.ToUpper(s[:1]) + s[1:]
	},
	"triggerLabel": func(t string) string {
		for _, o := range triggerChoice("", "").Options {
			if o.Value == t {
				return o.Label
			}
		}
		return t
	},
	"memory": func(mb int) string {
		if mb%1024 == 0 {
			return strconv.Itoa(mb/1024) + " GB"
		}
		return strconv.Itoa(mb) + " MB"
	},
	"provider": providerName,
	"initial": func(email string) string {
		if email == "" {
			return "?"
		}
		return strings.ToUpper(email[:1])
	},
	"ms": func(n int) string { return humanDuration(time.Duration(n) * time.Millisecond) },
	"stateLabel": func(state string, busy bool) string {
		switch {
		case busy:
			return "working"
		case state == sandbox.Running:
			return "awake"
		case state == sandbox.Checkpointed:
			return "checkpointed"
		default:
			return "stopped"
		}
	},
	"harness": func(h string) string {
		if h == store.HarnessPi {
			return "pi"
		}
		return "Claude Code"
	},
	"clip": func(n int, s string) string {
		if r := []rune(s); len(r) > n {
			return string(r[:n-1]) + "…"
		}
		return s
	},
	"money": func(f float64) string {
		if f == 0 {
			return ""
		}
		if f < 0.01 {
			return "<$0.01"
		}
		return fmt.Sprintf("$%.2f", f)
	},
	"seconds": func(d time.Duration) int { return int(d / time.Second) },
	"minutes": func(d time.Duration) int { return int(d / time.Minute) },
}

func humanDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func (s *Server) parseTemplates() error {
	s.pages = map[string]*template.Template{}
	entries, err := fs.ReadDir(assets, "templates")
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "layout.html" || name == "partials.html" {
			continue
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html", "templates/"+name)
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	partials, err := template.New("partials").Funcs(funcs).ParseFS(assets, "templates/partials.html")
	if err != nil {
		return err
	}
	s.pages["partials"] = partials
	return nil
}

// render writes a full page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, v *viewer, name, title, nav string, data any) {
	p := page{Title: title, Nav: nav, V: v, Flash: s.takeFlash(w, r), Version: s.assetV, Data: data}
	s.write(w, s.pages[name], "layout", p)
}

// partial writes one named template from partials.html.
func (s *Server) partial(w http.ResponseWriter, name string, data any) {
	s.write(w, s.pages["partials"], name, data)
}

func (s *Server) write(w http.ResponseWriter, t *template.Template, name string, data any) {
	if t == nil {
		http.Error(w, "missing template", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.Log.Error("render", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// fail shows an error page.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, v *viewer, code int, msg string) {
	w.WriteHeader(code)
	s.render(w, r, v, "error.html", http.StatusText(code), "", msg)
}

func formValue(r *http.Request, key string) string { return strings.TrimSpace(r.FormValue(key)) }
