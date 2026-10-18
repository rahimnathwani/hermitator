//go:build manual

package egress

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestRealClaude runs the real Claude Code through the proxy, with
// api.anthropic.com decrypted, to check that it trusts the egress CA and
// that streaming survives. It spends a fraction of a cent:
//
//	go test -tags manual -run TestRealClaude -v ./internal/egress
func TestRealClaude(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	var intercepted atomic.Int32
	p := &Proxy{
		CA:      ca,
		DialAny: true,
		Log:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Resolve: func(context.Context, netip.Addr) ([]Secret, bool) {
			intercepted.Add(1)
			// A secret for the host, so the proxy decrypts it; its
			// placeholder is not in play here.
			return []Secret{{Name: "probe", Placeholder: "never-sent-placeholder", Value: "x", Hosts: []string{"api.anthropic.com"}}}, true
		},
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go p.ServeConnect(context.Background(), l)
	caFile := filepath.Join(dir, "egress-ca.crt")
	cmd := exec.Command("claude", "-p", "Reply with exactly: ok", "--model", "haiku", "--max-turns", "1", "--output-format", "stream-json", "--verbose")
	cmd.Env = append(os.Environ(), "HTTPS_PROXY=http://"+l.Addr().String(), "NODE_EXTRA_CA_CERTS="+caFile)
	out, err := cmd.CombinedOutput()
	t.Logf("connections through the proxy: %d", intercepted.Load())
	if err != nil || !strings.Contains(string(out), `"result":"ok"`) {
		t.Fatalf("claude through the proxy: %v\n%s", err, out[max(0, len(out)-2000):])
	}
}
