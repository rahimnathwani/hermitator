package orchestrator

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// A stand-in for Chromium that only serves /json/version, which is all
// the host needs to find DevTools.
const fakeChromium = `#!/usr/bin/env python3
import json, os, signal, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
if "--version" in sys.argv:
    print("Chromium 141.0.7390.37"); sys.exit(0)
port = int([a for a in sys.argv if a.startswith("--remote-debugging-port=")][0].split("=")[1])
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        b = json.dumps({"webSocketDebuggerUrl": "ws://127.0.0.1:%d/devtools/browser/x" % port}).encode()
        self.send_response(200); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
signal.signal(signal.SIGTERM, lambda *a: os._exit(0))
HTTPServer(("127.0.0.1", port), H).serve_forever()
`

func TestBrowserStartsInTheSandboxAndForgets(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	// The unsandboxed runtime puts the forwarder on the host's loopback,
	// at the one fixed port.
	fwd := net.JoinHostPort("127.0.0.1", strconv.Itoa(turn.BrowserPort))
	if l, err := net.Listen("tcp", fwd); err != nil {
		t.Skipf("%s is taken: %v", fwd, err)
	} else {
		l.Close()
	}
	chrome := filepath.Join(t.TempDir(), "chrome")
	os.WriteFile(chrome, []byte(fakeChromium), 0o755)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	debug := l.Addr().String()
	l.Close()
	t.Setenv("HERMITATOR_CHROMIUM", chrome)
	t.Setenv("HERMITATOR_BROWSER_DEBUG_ADDR", debug)
	e := setup(t)
	e.m.cfg.PreviewLoopback = true
	ctx := context.Background()
	t.Cleanup(func() { e.m.ForgetBrowser(ctx, e.agent.ID) })

	addr, release, err := e.m.Browser(ctx, e.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if addr != fwd {
		t.Fatalf("address %s, want %s", addr, fwd)
	}
	if e.m.Status(e.agent.ID).State != sandbox.Running {
		t.Fatal("the agent is not awake")
	}
	resp, err := http.Get("http://" + addr + "/json/version")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	release()

	// Opening the tab again finds it running.
	if _, release, err = e.m.Browser(ctx, e.agent.ID); err != nil {
		t.Fatal(err)
	}
	release()

	if err := e.m.ForgetBrowser(ctx, e.agent.ID); err != nil {
		t.Fatal(err)
	}
	if resp, err := http.Get("http://" + addr + "/json/version"); err == nil {
		resp.Body.Close()
		t.Fatal("the browser still answers after it was forgotten")
	}
}

func TestPromptOffersTheBrowser(t *testing.T) {
	e := setup(t)
	spec, err := e.m.Spec(e.agent)
	if err != nil {
		t.Fatal(err)
	}
	p := e.m.agentPrompt(spec, nil, Message{})
	if !strings.Contains(p, "hermitator-guest browser URL") || !strings.Contains(p, "https://hermitator.example.com/agents/"+e.agent.ID+"/browser") {
		t.Fatalf("prompt does not offer the browser:\n%s", p)
	}
}
