package sandbox

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
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

func TestOverlayCanChangeImage(t *testing.T) {
	s := &Spec{Dir: "/a", Image: Image{Rootfs: "/img/universal-2/rootfs"}}
	if opts := overlayOptions(s); !strings.Contains(opts, "index=off") {
		t.Fatalf("overlay options %q would pin the upper layer to its first image", opts)
	}
}

func TestOwnHarnessCopies(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "image")
	s := &Spec{Dir: filepath.Join(dir, "agent"), Image: Image{Rootfs: image}}
	write := func(root, name, content string) {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	write(image, "usr/lib/node_modules/@anthropic-ai/claude-code/package.json", `{"version":"2.1.302"}`)
	if got := s.OwnCopies(); len(got) != 0 {
		t.Fatalf("own copies before installing any: %+v", got)
	}
	write(s.upper(), "usr/lib/node_modules/@anthropic-ai/claude-code/package.json", `{"version":"2.1.290"}`)
	write(s.upper(), "usr/lib/node_modules/@anthropic-ai/other/package.json", `{}`)
	got := s.OwnCopies()
	if len(got) != 1 || got[0].Name != "Claude Code" || got[0].Own != "2.1.290" || got[0].Image != "2.1.302" {
		t.Fatalf("own copies = %+v", got)
	}

	// A symlink the agent planted must not lead the host anywhere.
	outside := filepath.Join(dir, "host-file")
	os.WriteFile(outside, []byte("keep"), 0o644)
	os.MkdirAll(filepath.Join(s.upper(), "usr/bin"), 0o755)
	os.Symlink(outside, filepath.Join(s.upper(), "usr/bin/pi"))

	if err := s.DropOwnCopies(); err != nil {
		t.Fatal(err)
	}
	if got := s.OwnCopies(); len(got) != 0 {
		t.Fatalf("own copies after dropping them: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(s.upper(), "usr/lib/node_modules/@anthropic-ai/other")); err != nil {
		t.Fatal("dropped a package that is not a harness")
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Fatal("followed a planted symlink out of the upper layer")
	}
}

func TestLimitAll(t *testing.T) {
	for _, c := range []struct{ total, want uint64 }{
		{64 << 30, 60 << 30},       // the reserve is at most 4 GiB
		{8 << 30, 8<<30 - 8<<30/5}, // a fifth
		{2 << 30, 1 << 30},         // at least 1 GiB
	} {
		root := t.TempDir()
		got, err := LimitAll(root, c.total)
		if err != nil || got != c.want {
			t.Fatalf("LimitAll(%d) = %d, %v; want %d", c.total, got, err, c.want)
		}
		b, _ := os.ReadFile(filepath.Join(root, AgentsCgroup, "memory.max"))
		w, _ := os.ReadFile(filepath.Join(root, AgentsCgroup, "cpu.weight"))
		if string(b) != fmt.Sprint(c.want) || string(w) != "50" {
			t.Fatalf("wrote memory.max=%s cpu.weight=%s", b, w)
		}
	}
	if _, err := LimitAll(t.TempDir(), 512<<20); err == nil {
		t.Fatal("limited a machine with less memory than the reserve")
	}
}

func TestHolding(t *testing.T) {
	s := &Spec{Dir: t.TempDir()}
	now := time.Now()
	if s.Holding(now) {
		t.Fatal("holding with no holds")
	}
	holds := filepath.Join(s.SharedDir(), "holds")
	os.MkdirAll(holds, 0o755)
	fresh := filepath.Join(holds, fmt.Sprintf("%d-1", now.Unix()))
	os.WriteFile(fresh, nil, 0o644)
	if !s.Holding(now) {
		t.Fatal("a fresh hold does not hold")
	}
	// A hold whose job stopped refreshing it lapses, and so does one that
	// has gone on for too long.
	os.Chtimes(fresh, now.Add(-10*time.Minute), now.Add(-10*time.Minute))
	ancient := filepath.Join(holds, fmt.Sprintf("%d-2", now.Add(-13*time.Hour).Unix()))
	os.WriteFile(ancient, nil, 0o644)
	if s.Holding(now) {
		t.Fatal("stale holds still hold")
	}
	if entries, _ := os.ReadDir(holds); len(entries) != 0 {
		t.Fatalf("stale holds were not cleared: %v", entries)
	}
}

func TestRuleset(t *testing.T) {
	n := &Network{Bridge: "hermitator0", Subnet: netip.MustParsePrefix("10.231.0.0/16"), EgressPort: 3129}
	rs := n.ruleset([]Peer{{ID: "a1", IP: netip.MustParseAddr("10.231.0.2")}, {ID: "b2", IP: netip.MustParseAddr("10.231.0.3"), Egress: true}})
	for _, want := range []string{
		`elements = { "hva1" . 10.231.0.2, "hvb2" . 10.231.0.3 }`,
		`set egress { type ipv4_addr; elements = { 10.231.0.3 }; }`,
		`ip saddr @egress ip daddr != { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/3 } tcp dport 443 dnat ip to 10.231.0.1:3129`,
		`ip saddr @egress ip daddr 10.231.0.1 tcp dport 3129 accept`,
		`iifname "hv*" iifname . ip saddr != @ports drop`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("ruleset lacks %s\n%s", want, rs)
		}
	}
	// With the proxy off there is no redirect at all.
	if off := (&Network{Bridge: "hermitator0", Subnet: n.Subnet}).ruleset(nil); strings.Contains(off, "dnat") {
		t.Error("redirects with the proxy off")
	}
}
