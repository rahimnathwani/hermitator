package web

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/coder/websocket"

	"github.com/rahimnathwani/hermitator/internal/browser"
)

func (s *Server) browserPage(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		s.fail(w, r, v, http.StatusNotFound, "No such agent.")
		return
	}
	s.render(w, r, v, "browser.html", a.Name+" browser", "agents", s.agentData(r, a, "browser"))
}

// browserSocket shows the agent's browser on the page and plays the
// user's taps and typing into it (browser.Relay describes the messages).
func (s *Server) browserSocket(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	fail := func(msg string) {
		b, _ := json.Marshal(map[string]string{"type": "error", "message": msg})
		conn.Write(ctx, websocket.MessageText, b)
		conn.Close(websocket.StatusInternalError, "browser unavailable")
	}
	addr, release, err := s.Agents.Browser(ctx, a.ID)
	if err != nil {
		fail("Could not start the browser: " + err.Error())
		return
	}
	defer release()
	s.audit(r, v, "agent.browser", "agent="+a.ID)
	cdp, err := browser.Dial(ctx, addr)
	if err != nil {
		fail("Could not reach the browser: " + err.Error())
		return
	}
	defer cdp.Close()

	in := make(chan []byte, 16)
	go func() {
		defer close(in)
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				cancel()
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			select {
			case in <- data:
			case <-ctx.Done():
				return
			}
		}
	}()
	relay := &browser.Relay{C: cdp, Send: conn.Write}
	if err := relay.Run(ctx, in); err != nil && ctx.Err() == nil {
		fail(err.Error())
		return
	}
	conn.Close(websocket.StatusNormalClosure, "")
}

func (s *Server) forgetBrowser(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.Agents.ForgetBrowser(r.Context(), a.ID); err != nil {
		s.back(w, r, "/agents/"+a.ID+"/settings", "Could not forget the browser: "+err.Error())
		return
	}
	s.audit(r, v, "agent.browser.forget", "agent="+a.ID)
	s.back(w, r, "/agents/"+a.ID+"/settings", "The browser is closed, and its cookies and saved logins are deleted.")
}
