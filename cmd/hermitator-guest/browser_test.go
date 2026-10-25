package main

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

// fakeChromium answers --version and otherwise serves the DevTools HTTP
// endpoints the guest uses, recording how it was started and which URLs
// it was asked to open.
const fakeChromium = `#!/usr/bin/env python3
import json, os, signal, sys, urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer
if "--version" in sys.argv:
    print("Chromium 141.0.7390.37"); sys.exit(0)
rec = os.environ["FAKE_CHROME_RECORD"]
with open(rec, "a") as f: f.write("argv " + json.dumps(sys.argv[1:]) + "\n")
port = int([a for a in sys.argv if a.startswith("--remote-debugging-port=")][0].split("=")[1])
pages = [{"id": "blank", "type": "page", "title": "", "url": "about:blank"}]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def reply(self, obj):
        b = json.dumps(obj).encode(); self.send_response(200)
        self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(b)))
        self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        p = self.path
        if p == "/json/version": return self.reply({"Browser": "Chrome/141", "webSocketDebuggerUrl": "ws://127.0.0.1:%d/devtools/browser/x" % port})
        if p == "/json/list": return self.reply(pages)
        if p.startswith("/json/close/"):
            pages[:] = [x for x in pages if x["id"] != p.split("/")[-1]]; return self.reply("closed")
        if p.startswith("/json/activate/"): return self.reply("activated")
        self.send_response(404); self.end_headers()
    def do_PUT(self):
        u = urllib.parse.unquote(self.path.split("?", 1)[1])
        with open(rec, "a") as f: f.write("open " + u + "\n")
        pages.insert(0, {"id": "p%d" % len(pages), "type": "page", "title": "t", "url": u}); self.reply(pages[0])
def bye(*a):
    with open(rec, "a") as f: f.write("terminated\n")
    os._exit(0)
signal.signal(signal.SIGTERM, bye)
HTTPServer(("127.0.0.1", port), H).serve_forever()
`

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestBrowserStartsOpensAndForgets(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	guest := filepath.Join(dir, "hermitator-guest")
	if out, err := exec.Command("go", "build", "-o", guest, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	chrome := filepath.Join(dir, "chrome")
	os.WriteFile(chrome, []byte(fakeChromium), 0o755)
	home, record := filepath.Join(dir, "home"), filepath.Join(dir, "record")
	debug, listen := freeAddr(t), freeAddr(t)
	env := append(os.Environ(), "HOME="+home, "HERMITATOR_CHROMIUM="+chrome, "FAKE_CHROME_RECORD="+record,
		"HERMITATOR_BROWSER_DEBUG_ADDR="+debug, turn.BrowserListenEnv+"="+listen)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(guest, append([]string{"browser"}, args...)...)
		cmd.Env = env
		// The daemon must not hold the caller's output open, or a spool
		// job running this would never end; Output waits for exactly that.
		done := make(chan struct{})
		var out []byte
		var err error
		go func() { out, err = cmd.Output(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("browser %v did not return (output held open?)", args)
		}
		if err != nil {
			t.Fatalf("browser %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	t.Cleanup(func() { run("--close") })

	if out := run("--status"); !strings.Contains(out, "not running") {
		t.Fatalf("status before start = %q", out)
	}
	if out := run("example.com/login?next=/a&b=c"); !strings.Contains(out, "Opened https://example.com/login?next=/a&b=c") {
		t.Fatalf("start = %q", out)
	}
	rec, _ := os.ReadFile(record)
	for _, want := range []string{"--headless=new", "--user-data-dir=" + filepath.Join(home, ".hermitator", "browser"),
		"Chrome/141.0.0.0", `"https://example.com/login?next=/a&b=c"`} {
		if !strings.Contains(string(rec), want) {
			t.Fatalf("chromium started without %s:\n%s", want, rec)
		}
	}

	// Once running, a URL opens in a tab; the blank one goes.
	run("https://second.example/")
	rec, _ = os.ReadFile(record)
	if strings.Count(string(rec), "argv ") != 1 || !strings.Contains(string(rec), "open https://second.example/") {
		t.Fatalf("second open:\n%s", rec)
	}
	if out := run("--status"); strings.Contains(out, "about:blank") || !strings.Contains(out, "second.example") {
		t.Fatalf("status = %q", out)
	}

	// The host reaches DevTools through the forwarder.
	resp, err := http.Get("http://" + listen + "/json/version")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("through the forwarder: %v %v", resp, err)
	}
	resp.Body.Close()

	os.WriteFile(filepath.Join(home, ".hermitator", "browser", "Cookies"), []byte("x"), 0o600)
	run("--forget")
	if _, err := os.Stat(filepath.Join(home, ".hermitator", "browser")); !os.IsNotExist(err) {
		t.Fatalf("profile still there: %v", err)
	}
	rec, _ = os.ReadFile(record)
	if !strings.Contains(string(rec), "terminated") {
		t.Fatalf("chromium was not told to quit:\n%s", rec)
	}
	if out := run("--status"); !strings.Contains(out, "not running") {
		t.Fatalf("status after forget = %q", out)
	}
}

func TestBrowserURL(t *testing.T) {
	for in, want := range map[string]string{"example.com": "https://example.com", "http://x.test/a": "http://x.test/a", "about:blank": "about:blank"} {
		if got, err := browserURL(in); err != nil || got != want {
			t.Errorf("browserURL(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := browserURL("https://"); err == nil {
		t.Error("an empty host was accepted")
	}
}
