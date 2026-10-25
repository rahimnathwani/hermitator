package imageupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rahimnathwani/hermitator/internal/sandbox"
)

const containerfile = `FROM ubuntu
ARG NODE_MAJOR=22
ARG CLAUDE_CODE_VERSION=2.1.302
ARG PI_VERSION=1.3.0
ARG PLAYWRIGHT_VERSION=1.64.1
ARG PLAYWRIGHT_PYTHON_VERSION=1.64.0
ARG RELEASED_BEFORE=2026-10-18T01:00:47.000Z
`

// fakeRegistry answers like npm and PyPI. Releases younger than a week
// must be passed over; days are how long ago each came out.
func fakeRegistry(t *testing.T, npm map[string]map[string]int, pypi map[string]int) *httptest.Server {
	day := func(n int) string {
		return time.Now().Add(-time.Duration(n) * 24 * time.Hour).UTC().Format(time.RFC3339)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/pypi/") {
			releases := map[string][]map[string]string{}
			for v, d := range pypi {
				releases[v] = []map[string]string{{"upload_time_iso_8601": day(d)}}
			}
			json.NewEncoder(w).Encode(map[string]any{"releases": releases})
			return
		}
		pkg := strings.ReplaceAll(strings.TrimPrefix(r.URL.Path, "/"), "%2f", "/")
		versions, ok := npm[pkg]
		if !ok {
			http.NotFound(w, r)
			return
		}
		times := map[string]string{"created": day(1000), "modified": day(0)}
		for v, d := range versions {
			times[v] = day(d)
		}
		json.NewEncoder(w).Encode(map[string]any{"time": times})
	}))
}

type fixture struct {
	u      *Updater
	images string
	builds [][]string
	notes  []string
	inUse  map[string]bool
	fail   bool
	mu     sync.Mutex
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	os.MkdirAll(filepath.Join(src, "image"), 0o755)
	os.MkdirAll(filepath.Join(src, "deploy"), 0o755)
	os.WriteFile(filepath.Join(src, "image", "Containerfile"), []byte(containerfile), 0o644)
	os.WriteFile(filepath.Join(src, "deploy", "build-image.sh"), []byte("#!/bin/sh\n"), 0o755)
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "podman"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	images := filepath.Join(root, "images")
	f := &fixture{images: images, inUse: map[string]bool{}}
	f.addImage("universal-20261001", "")
	reg := fakeRegistry(t, map[string]map[string]int{
		"@anthropic-ai/claude-code":       {"2.1.302": 30, "2.1.309": 9, "2.1.310": 8, "2.1.311": 2, "2.2.0-beta.1": 20},
		"@earendil-works/pi-coding-agent": {"1.3.0": 30},
		"playwright":                      {"1.64.1": 30, "1.65.0": 12, "1.65.2": 10, "1.66.0": 9},
	}, map[string]int{"1.64.0": 30, "1.65.0": 11, "1.66.0": 3})
	t.Cleanup(reg.Close)
	f.u = &Updater{
		Images:  &sandbox.Images{Dir: images},
		Source:  src,
		DataDir: root,
		Keep:    1,
		NPM:     reg.URL,
		PyPI:    reg.URL,
		Notify: func(_ context.Context, text string) {
			f.mu.Lock()
			f.notes = append(f.notes, text)
			f.mu.Unlock()
		},
		InUse: func(context.Context) (map[string]bool, error) { return f.inUse, nil },
		Build: func(_ context.Context, argv, env []string) ([]byte, error) {
			f.builds = append(f.builds, argv)
			if f.fail {
				return []byte("step 9/12\nE: unable to fetch\n"), errors.New("exit status 1")
			}
			// What build-image.sh leaves: a dated directory with the
			// pins, and the family's symlink pointed at it.
			var pins []string
			for i, a := range argv {
				if a == "--build-arg" {
					pins = append(pins, argv[i+1])
				}
			}
			v := fmt.Sprintf("universal-202610%02d", 10+len(f.builds))
			f.addImage(v, strings.Join(pins, "\n"))
			return []byte("exported"), nil
		},
		RemoveTag: func(context.Context, string) {},
	}
	return f
}

func (f *fixture) addImage(version, pins string) {
	os.MkdirAll(filepath.Join(f.images, version, "rootfs"), 0o755)
	os.WriteFile(filepath.Join(f.images, version, "IMAGE"), []byte("localhost/hermitator-universal:"+version), 0o644)
	if pins != "" {
		os.WriteFile(filepath.Join(f.images, version, "VERSIONS"), []byte(pins+"\n"), 0o644)
	}
	os.Remove(filepath.Join(f.images, "universal"))
	os.Symlink(version, filepath.Join(f.images, "universal"))
}

func TestUpdaterBuildsWhatHasSettled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	r := f.u.Run(ctx, false)
	if r.Err != nil || r.Built != "universal-20261011" {
		t.Fatalf("first run = %+v", r)
	}
	args := strings.Join(f.builds[0], " ")
	// The week-old Claude Code, not the newer or the beta; Playwright for
	// Python's newest settled minor, and Node's newest patch within it.
	for _, want := range []string{"CLAUDE_CODE_VERSION=2.1.310", "PI_VERSION=1.3.0", "PLAYWRIGHT_PYTHON_VERSION=1.65.0", "PLAYWRIGHT_VERSION=1.65.2", "RELEASED_BEFORE=20"} {
		if !strings.Contains(args, want) {
			t.Fatalf("build args %q lack %s", args, want)
		}
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "Claude Code 2.1.302 → 2.1.310") || !strings.Contains(f.notes[0], "/upgrade") || strings.Contains(f.notes[0], "pi ") {
		t.Fatalf("notice = %q", f.notes)
	}

	// Nothing new: no build, and no message unless asked.
	if r := f.u.Run(ctx, false); r.Built != "" || len(f.builds) != 1 || len(f.notes) != 1 {
		t.Fatalf("second run built again: %+v", r)
	}
	if r := f.u.Run(ctx, true); r.Built != "" || len(f.notes) != 2 || !strings.Contains(f.notes[1], "already has the newest") {
		t.Fatalf("forced run with nothing new = %+v, %q", r, f.notes)
	}
	if s := f.u.Status(); !strings.Contains(s, "universal-20261011 (Claude Code 2.1.310") {
		t.Fatalf("status = %q", s)
	}
}

func TestUpdaterNeverGoesBelowTheRepositoryPins(t *testing.T) {
	f := newFixture(t)
	cf := strings.Replace(containerfile, "CLAUDE_CODE_VERSION=2.1.302", "CLAUDE_CODE_VERSION=2.1.400", 1)
	os.WriteFile(filepath.Join(f.u.Source, "image", "Containerfile"), []byte(cf), 0o644)
	r := f.u.Run(context.Background(), false)
	if r.Err != nil || !strings.Contains(strings.Join(f.builds[0], " "), "CLAUDE_CODE_VERSION=2.1.400") {
		t.Fatalf("run = %+v, args %v", r, f.builds)
	}
}

func TestUpdaterFailureIsReportedOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.fail = true
	r := f.u.Run(ctx, false)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "unable to fetch") {
		t.Fatalf("failed build = %+v", r)
	}
	if cur, _ := f.u.Images.Current("universal"); cur != "universal-20261001" {
		t.Fatalf("a failed build moved the image to %s", cur)
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "failed") {
		t.Fatalf("notes = %q", f.notes)
	}
	// The same versions are not tried every day...
	if r := f.u.Run(ctx, false); len(f.builds) != 1 || !strings.Contains(r.Skipped, "failed to build before") {
		t.Fatalf("retried on its own: %+v", r)
	}
	// ...unless asked.
	f.fail = false
	if r := f.u.Run(ctx, true); r.Built == "" || len(f.builds) != 2 {
		t.Fatalf("forced retry = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(f.images, failedFile)); err == nil {
		t.Fatal("failure marker left after a good build")
	}
}

func TestPruneKeepsWhatAgentsUse(t *testing.T) {
	f := newFixture(t)
	for _, v := range []string{"universal-20260901", "universal-20260915", "universal-20260920", "universal-20260920.1"} {
		f.addImage(v, "")
	}
	f.addImage("universal-20261001", "")
	f.inUse["universal-20260915"] = true
	pruned := f.u.prune(context.Background(), "universal-20261001")
	// Current, one spare (the newest unused), and the one in use stay.
	if strings.Join(pruned, " ") != "universal-20260901 universal-20260920" {
		t.Fatalf("pruned %v", pruned)
	}
	for _, v := range []string{"universal-20261001", "universal-20260920.1", "universal-20260915"} {
		if _, err := os.Stat(filepath.Join(f.images, v, "rootfs")); err != nil {
			t.Fatalf("%s was removed", v)
		}
	}

	// Not knowing what is in use means removing nothing.
	f.u.InUse = func(context.Context) (map[string]bool, error) { return nil, errors.New("database locked") }
	if pruned := f.u.prune(context.Background(), "universal-20261001"); len(pruned) != 0 {
		t.Fatalf("pruned %v without knowing what agents use", pruned)
	}
}

func TestUnavailableWithoutBuildTools(t *testing.T) {
	f := newFixture(t)
	t.Setenv("PATH", t.TempDir())
	r := f.u.Run(context.Background(), false)
	if r.Built != "" || !strings.Contains(r.Skipped, "podman") || len(f.notes) != 0 {
		t.Fatalf("run without podman = %+v, %q", r, f.notes)
	}
	if !strings.Contains(f.u.Status(), "off on this host") {
		t.Fatalf("status = %q", f.u.Status())
	}
}

func TestStoppingMidBuildIsNotAFailure(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.u.Build = func(context.Context, []string, []string) ([]byte, error) {
		cancel()
		return nil, errors.New("signal: terminated")
	}
	r := f.u.Run(ctx, false)
	if r.Err != nil || len(f.notes) != 0 {
		t.Fatalf("stopped build = %+v, %q", r, f.notes)
	}
	if _, err := os.Stat(filepath.Join(f.images, failedFile)); err == nil {
		t.Fatal("versions marked failed because hermitator stopped")
	}
}
