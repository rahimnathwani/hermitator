package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckImageRunsTheSelftestOffline(t *testing.T) {
	dir := t.TempDir()
	guest := filepath.Join(dir, "hermitator-guest")
	if err := os.WriteFile(guest, []byte("guest"), 0o755); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(dir, "args")
	// The fake podman records its arguments and checks the bundle is
	// really there to mount.
	fake := filepath.Join(dir, "podman")
	script := `#!/bin/sh
printf '%s\n' "$@" >` + logFile + `
for a in "$@"; do
	case $a in
	*:/opt/hermitator:ro) test -x "${a%%:/opt/hermitator:ro}/bin/hermitator-guest" || exit 3 ;;
	esac
done
exit ${FAKE_EXIT:-0}
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := podmanBin
	podmanBin = fake
	t.Cleanup(func() { podmanBin = old })

	if err := checkImage(context.Background(), "localhost/hermitator-universal:20261119", guest); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(logFile)
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	for _, want := range []string{"--network=none", "--rm", "localhost/hermitator-universal:20261119", "selftest"} {
		found := false
		for _, a := range args {
			found = found || a == want
		}
		if !found {
			t.Errorf("podman run without %q: %q", want, args)
		}
	}

	t.Setenv("FAKE_EXIT", "1")
	if err := checkImage(context.Background(), "localhost/hermitator-universal:20261119", guest); err == nil {
		t.Fatal("a failing selftest passed the check")
	}
}
