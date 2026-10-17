package sandbox

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestParsePS(t *testing.T) {
	out := []byte(`UID       PID       PPID      PGID      C         TTY       STIME     TIME      CMD
0         1         0         1         0         ?         23:26     0s        hermitator-gues
0         9         1         9         0         ?         23:26     0s        node server.js
`)
	ps := parsePS(out)
	if len(ps) != 2 || ps[1].PID != 9 || ps[1].PPID != 1 || ps[1].Cmd != "node server.js" {
		t.Fatalf("parsePS = %+v", ps)
	}
	if IdleProcesses(ps) {
		t.Fatal("a sandbox running node is not idle")
	}
	if !IdleProcesses(ps[:1]) {
		t.Fatal("a sandbox with only init is idle")
	}
}

func TestOCISpec(t *testing.T) {
	s := &Spec{ID: "abc", Dir: "/var/lib/hermitator/agents/abc", Tools: "/var/lib/hermitator/tools/1234",
		Image:    Image{Rootfs: "/img", Env: []string{"PATH=/bad", "PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright", "FOO=bar"}},
		IP:       netip.MustParseAddr("10.88.0.2"),
		MemoryMB: 2048}
	spec := ociSpec(s, "/var/run/netns/hm-abc")
	if spec.Root.Path != "/var/lib/hermitator/agents/abc/merged" {
		t.Fatalf("root = %s", spec.Root.Path)
	}
	if *spec.Linux.Resources.Memory.Limit != 2048<<20 {
		t.Fatalf("memory = %d", *spec.Linux.Resources.Memory.Limit)
	}
	env := strings.Join(spec.Process.Env, "\n")
	if !strings.Contains(env, "PATH=/opt/hermitator/bin:") || strings.Contains(env, "PATH=/bad") || !strings.Contains(env, "FOO=bar") {
		t.Fatalf("env:\n%s", env)
	}
	var netns string
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == "network" {
			netns = ns.Path
		}
	}
	if netns != "/var/run/netns/hm-abc" {
		t.Fatalf("netns = %q", netns)
	}
	if got := len(vethName("abcdefghij")); got > 15 {
		t.Fatalf("veth name is %d bytes; Linux allows 15", got)
	}
}

func TestInstallTools(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(t.TempDir(), "guest")
	os.WriteFile(bin, []byte("binary"), 0o755)
	files := fstest.MapFS{"pi-login.mjs": {Data: []byte("x")}}
	a, err := InstallTools(root, bin, files)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := InstallTools(root, bin, files)
	if a != b {
		t.Fatal("same content produced a different bundle")
	}
	if target, _ := os.Readlink(filepath.Join(a, "bin", "whatsapp-send")); target != "hermitator-guest" {
		t.Fatalf("whatsapp-send -> %q", target)
	}
	os.WriteFile(bin, []byte("binary v2"), 0o755)
	c, _ := InstallTools(root, bin, files)
	if c == a {
		t.Fatal("new binary reused the old bundle")
	}
	PruneTools(root, c, nil)
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Fatal("unreferenced bundle survived pruning")
	}
}
