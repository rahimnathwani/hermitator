//go:build manual

package orchestrator

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

	"github.com/rahimnathwani/hermitator/internal/egress"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// TestRealPiChatGPT runs the real pi with the auth.json a sandbox holding
// placeholders gets, through the proxy, using this machine's own ChatGPT
// login (copied into a temporary home, never refreshed there):
//
//	go test -tags manual -run TestRealPiChatGPT -v ./internal/orchestrator
func TestRealPiChatGPT(t *testing.T) {
	real, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".pi", "agent", "auth.json"))
	if err != nil {
		t.Skip("no local pi login")
	}
	e := setup(t)
	ctx := context.Background()
	e.m.cfg.EgressKey = []byte("manual test key")
	e.m.PutSecret(ctx, e.user.ID, store.SecretPiAuth, string(real))
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".pi", "agent"), 0o700)
	sandbox := e.m.sandboxPiAuth(e.user.ID, string(real))
	os.WriteFile(filepath.Join(home, ".pi", "agent", "auth.json"), []byte(sandbox), 0o600)

	dir := t.TempDir()
	ca, err := egress.LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	var swapped atomic.Int32
	p := &egress.Proxy{
		CA: ca, DialAny: true,
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
		Resolve: func(context.Context, netip.Addr) ([]egress.Secret, bool) {
			swapped.Add(1)
			return e.m.codexSecrets(ctx, e.user.ID), true
		},
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go p.ServeConnect(context.Background(), l)

	cmd := exec.Command("pi", "-p", "--no-session", "--provider", "openai-codex", "--model", "gpt-5.6-terra", "Reply with exactly: ok")
	cmd.Env = append(os.Environ(), "HOME="+home, "HTTPS_PROXY=http://"+l.Addr().String(), "NODE_USE_ENV_PROXY=1",
		"NODE_EXTRA_CA_CERTS="+filepath.Join(dir, "egress-ca.crt"), "PATH=/opt/homebrew/bin:"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	t.Logf("connections through the proxy: %d; output: %s", swapped.Load(), out[max(0, len(out)-500):])
	if err != nil || !strings.Contains(strings.ToLower(string(out)), "ok") {
		t.Fatalf("pi through the proxy: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(home, ".pi", "agent", "auth.json"))
	if strings.Contains(string(after), entryString(must(codexEntry(string(real))), "refresh")) {
		t.Fatal("a real refresh token reached the sandbox's auth.json")
	}
}

func must[T any](e map[string]any, _ T, _ bool) map[string]any { return e }
