package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

func (s *Server) terminalPage(w http.ResponseWriter, r *http.Request, v *viewer) {
	a, err := s.agentFor(r, v)
	if err != nil {
		s.fail(w, r, v, http.StatusNotFound, "No such agent.")
		return
	}
	s.render(w, r, v, "terminal.html", a.Name+" terminal", "agents", map[string]any{"Agent": a})
}

// terminalSocket bridges xterm.js to a login shell inside the sandbox.
// Binary frames carry terminal bytes both ways; text frames from the
// browser are JSON control messages ({"type":"resize","cols":..,"rows":..}).
func (s *Server) terminalSocket(w http.ResponseWriter, r *http.Request, v *viewer) {
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
	conn.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	cmd, done, err := s.Agents.Terminal(ctx, a.ID)
	if err != nil {
		conn.Write(ctx, websocket.MessageBinary, []byte("\r\nCould not open a terminal: "+err.Error()+"\r\n"))
		conn.Close(websocket.StatusInternalError, "terminal unavailable")
		return
	}
	defer done()
	s.audit(r, v, "agent.terminal", "agent="+a.ID)

	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 100, Rows: 30})
	if err != nil {
		conn.Write(ctx, websocket.MessageBinary, []byte("\r\nCould not start a shell: "+err.Error()+"\r\n"))
		conn.Close(websocket.StatusInternalError, "shell failed")
		return
	}
	defer tty.Close()
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
		cancel()
	}()
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
	}()

	// sandbox -> browser
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				if err != io.EOF && !os.IsNotExist(err) {
					s.Log.Debug("terminal read", "err", err)
				}
				cancel()
				return
			}
		}
	}()

	// browser -> sandbox
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		if typ == websocket.MessageText {
			var ctl struct {
				Type string `json:"type"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if json.Unmarshal(data, &ctl) == nil && ctl.Type == "resize" && ctl.Cols > 0 && ctl.Rows > 0 {
				pty.Setsize(tty, &pty.Winsize{Cols: ctl.Cols, Rows: ctl.Rows})
			}
			continue
		}
		if _, err := tty.Write(data); err != nil {
			break
		}
	}
	conn.Close(websocket.StatusNormalClosure, "")
}
