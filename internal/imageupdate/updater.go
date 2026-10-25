// Package imageupdate keeps the agent image current without a laptop: it
// looks for releases of the image's pinned packages that have been out for
// a week, builds a new image with them on the host, prunes old ones no
// agent uses, and tells the host operators. Agents keep the image they
// have until their owner upgrades them.
package imageupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rahimnathwani/hermitator/internal/sandbox"
)

// Updater builds new agent images as their packages move on.
type Updater struct {
	Images *sandbox.Images
	// Family is the image family kept current, "universal".
	Family string
	// Source holds image/ (the Containerfile) and deploy/build-image.sh,
	// as deploy/install.sh leaves them in /usr/local/share/hermitator.
	Source  string
	DataDir string
	// MinAge is how long a release must have been out; a compromised one
	// is usually caught and pulled within days.
	MinAge time.Duration
	// Every is how often to look.
	Every time.Duration
	// Keep is how many unused older versions to keep besides the current
	// one, to go back to.
	Keep int
	Log  *slog.Logger
	// Notify tells the host operators.
	Notify func(ctx context.Context, text string)
	// InUse lists the image versions agents are pinned to; nothing it
	// names is pruned, and an error stops pruning altogether.
	InUse func(ctx context.Context) (map[string]bool, error)

	// For tests: registries, the build, and podman's copy of an image.
	NPM, PyPI string
	Build     func(ctx context.Context, argv, env []string) ([]byte, error)
	RemoveTag func(ctx context.Context, tag string)

	once    sync.Once
	mu      sync.Mutex
	running bool
	since   time.Time
	last    Result
}

// Result is what one look found or did.
type Result struct {
	At      time.Time
	Built   string // the new version, if one was built
	Changes []Change
	Pruned  []string
	// Skipped says why nothing was built, when nothing was.
	Skipped string
	Err     error
}

const (
	failedFile   = ".update-failed"
	buildTimeout = 2 * time.Hour
)

// defaults fills in what was left unset, once, before anything reads it.
func (u *Updater) defaults() { u.once.Do(u.fill) }

func (u *Updater) fill() {
	if u.Family == "" {
		u.Family = "universal"
	}
	if u.MinAge == 0 {
		u.MinAge = 7 * 24 * time.Hour
	}
	if u.Every == 0 {
		u.Every = 24 * time.Hour
	}
	if u.NPM == "" {
		u.NPM = "https://registry.npmjs.org"
	}
	if u.PyPI == "" {
		u.PyPI = "https://pypi.org"
	}
	if u.Log == nil {
		u.Log = slog.Default()
	}
	if u.Build == nil {
		u.Build = func(ctx context.Context, argv, env []string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Env = append(os.Environ(), env...)
			// The script's podman must stop with it: the service's own
			// KillMode=process would leave it building.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
			cmd.WaitDelay = time.Minute
			return cmd.CombinedOutput()
		}
	}
	if u.RemoveTag == nil {
		u.RemoveTag = func(ctx context.Context, tag string) {
			exec.CommandContext(ctx, "podman", "rmi", "--force", tag).Run()
		}
	}
}

// Available reports whether this host can build images at all.
func (u *Updater) Available() error {
	u.defaults()
	if _, err := os.Stat(filepath.Join(u.Source, "deploy", "build-image.sh")); err != nil {
		return fmt.Errorf("no build script in %s", u.Source)
	}
	if _, err := os.Stat(filepath.Join(u.Source, "image", "Containerfile")); err != nil {
		return fmt.Errorf("no Containerfile in %s", u.Source)
	}
	if _, err := exec.LookPath("podman"); err != nil {
		return errors.New("podman is not installed")
	}
	return nil
}

// Loop looks for new releases every u.Every until ctx ends.
func (u *Updater) Loop(ctx context.Context) {
	u.defaults()
	// Not at once: a restart should not start a build.
	first := time.After(10 * time.Minute)
	t := time.NewTicker(u.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first:
		case <-t.C:
		}
		u.Run(ctx, false)
	}
}

// Start looks now, in the background, building even what failed before
// when force is set. It reports false if a look is already running.
func (u *Updater) Start(ctx context.Context, force bool) bool {
	u.mu.Lock()
	busy := u.running
	u.mu.Unlock()
	if busy {
		return false
	}
	go u.Run(ctx, force)
	return true
}

// Run looks for new releases and builds an image with them, telling the
// operators what came of it. One run at a time; a second returns at once.
func (u *Updater) Run(ctx context.Context, force bool) Result {
	u.defaults()
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		return Result{At: time.Now(), Skipped: "a look is already running"}
	}
	u.running, u.since = true, time.Now()
	u.mu.Unlock()

	r := u.run(ctx, force)
	r.At = time.Now()
	u.mu.Lock()
	u.running, u.last = false, r
	u.mu.Unlock()

	switch {
	case r.Err != nil:
		u.Log.Warn("agent image update", "err", r.Err)
		u.notify(ctx, "⚠️ Building a new agent image failed. Agents keep the one they have, and it is not tried again until there is something newer (/host image update tries now).\n\n"+r.Err.Error())
	case r.Built != "":
		u.Log.Info("built agent image", "version", r.Built, "changes", describe(r.Changes), "pruned", r.Pruned)
		text := fmt.Sprintf("🆕 New agent image *%s*: %s.\n\nNew agents start on it. Existing agents keep theirs until upgraded: /upgrade in an agent's chat, /upgrade all for all of yours, or Upgrade on its page.", r.Built, describe(r.Changes))
		if len(r.Pruned) > 0 {
			text += "\n\nRemoved old images no agent uses: " + strings.Join(r.Pruned, ", ") + "."
		}
		u.notify(ctx, text)
	case ctx.Err() != nil:
	case force:
		u.notify(ctx, "Nothing to build: "+r.Skipped+".")
	}
	return r
}

func (u *Updater) notify(ctx context.Context, text string) {
	if u.Notify != nil {
		u.Notify(ctx, text)
	}
}

func (u *Updater) run(ctx context.Context, force bool) Result {
	if err := u.Available(); err != nil {
		return Result{Skipped: err.Error()}
	}
	floor, err := ReadPins(filepath.Join(u.Source, "image", "Containerfile"))
	if err != nil {
		return Result{Err: err}
	}
	current, err := u.Images.Current(u.Family)
	if err != nil {
		return Result{Err: err}
	}
	// An image built before VERSIONS existed was built from the shipped
	// Containerfile, so its pins are the floor's.
	have, err := ReadPins(filepath.Join(u.Images.Dir, current, "VERSIONS"))
	if err != nil {
		have = floor
	}
	reg := registry{npm: u.NPM, pypi: u.PyPI, client: http.DefaultClient}
	want, err := reg.settled(ctx, time.Now().Add(-u.MinAge), floor)
	if err != nil {
		return Result{Err: fmt.Errorf("looking up releases: %w", err)}
	}
	if samePackages(have, want) {
		return Result{Skipped: current + " already has the newest releases a week old"}
	}
	if failed, err := ReadPins(filepath.Join(u.Images.Dir, failedFile)); err == nil && samePackages(failed, want) && !force {
		return Result{Skipped: "the same versions failed to build before"}
	}

	argv := []string{"sh", filepath.Join(u.Source, "deploy", "build-image.sh"), u.Family, filepath.Join(u.Source, "image")}
	for _, k := range append(slices.Clone(packagePins), pinReleased) {
		argv = append(argv, "--build-arg", k+"="+want[k])
	}
	bctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	out, err := u.Build(bctx, argv, []string{"HERMITATOR_DATA_DIR=" + u.DataDir})
	if err != nil && ctx.Err() != nil {
		// hermitator is stopping; the versions did not fail, so the next
		// process tries them again.
		return Result{Skipped: "stopped by a restart"}
	}
	if err != nil {
		u.writePins(failedFile, want)
		return Result{Changes: changes(have, want), Err: fmt.Errorf("%s: %w\n%s", describe(changes(have, want)), err, tail(string(out), 12))}
	}
	os.Remove(filepath.Join(u.Images.Dir, failedFile))
	built, err := u.Images.Current(u.Family)
	if err != nil || built == current {
		return Result{Err: fmt.Errorf("the build finished but %s is still current\n%s", current, tail(string(out), 12))}
	}
	return Result{Built: built, Changes: changes(have, want), Pruned: u.prune(ctx, built)}
}

func (u *Updater) writePins(name string, p Pins) {
	var b strings.Builder
	for _, k := range append(slices.Clone(packagePins), pinReleased) {
		fmt.Fprintf(&b, "%s=%s\n", k, p[k])
	}
	os.WriteFile(filepath.Join(u.Images.Dir, name), []byte(b.String()), 0o644)
}

// prune deletes versions of the family no agent is pinned to, keeping the
// current one and u.Keep before it. It deletes nothing if it cannot tell
// which are in use: an agent's lower layer must never disappear.
func (u *Updater) prune(ctx context.Context, current string) []string {
	u.defaults()
	if u.InUse == nil {
		return nil
	}
	inUse, err := u.InUse(ctx)
	if err != nil {
		u.Log.Warn("not pruning agent images", "err", err)
		return nil
	}
	versions := u.versions()
	var pruned []string
	kept := 0
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		switch {
		case v == current || inUse[v]:
			continue
		case kept < u.Keep:
			kept++
			continue
		}
		tag, _ := os.ReadFile(filepath.Join(u.Images.Dir, v, "IMAGE"))
		if err := os.RemoveAll(filepath.Join(u.Images.Dir, v)); err != nil {
			u.Log.Warn("prune agent image", "version", v, "err", err)
			continue
		}
		if t := strings.TrimSpace(string(tag)); t != "" {
			u.RemoveTag(ctx, t)
		}
		pruned = append(pruned, v)
	}
	slices.Sort(pruned)
	return pruned
}

// versions lists the family's built versions, oldest first.
func (u *Updater) versions() []string {
	entries, _ := os.ReadDir(u.Images.Dir)
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || sandbox.Family(name) != u.Family || strings.HasSuffix(name, ".partial") {
			continue
		}
		if _, err := os.Stat(filepath.Join(u.Images.Dir, name, "rootfs")); err == nil {
			out = append(out, name)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		if later(stamp(a), stamp(b)) {
			return 1
		}
		if later(stamp(b), stamp(a)) {
			return -1
		}
		return 0
	})
	return out
}

// stamp is the dated part of a version: "universal-20261017.2" is
// "20261017.2", which compares as numbers part by part.
func stamp(version string) string { return version[strings.LastIndex(version, "-")+1:] }

// Status describes the current image and the last look, for /host image.
func (u *Updater) Status() string {
	u.defaults()
	var b strings.Builder
	if current, err := u.Images.Current(u.Family); err == nil {
		b.WriteString("Image: " + current)
		if p, err := ReadPins(filepath.Join(u.Images.Dir, current, "VERSIONS")); err == nil {
			var s []string
			for _, k := range packagePins {
				if p[k] != "" {
					s = append(s, pinNames[k]+" "+p[k])
				}
			}
			b.WriteString(" (" + strings.Join(s, ", ") + ")")
		}
	}
	if err := u.Available(); err != nil {
		b.WriteString("\nUpdates: off on this host, " + err.Error())
		return b.String()
	}
	u.mu.Lock()
	running, since, last := u.running, u.since, u.last
	u.mu.Unlock()
	switch {
	case running:
		b.WriteString("\nUpdates: building since " + ago(since))
	case last.At.IsZero():
		b.WriteString("\nUpdates: looked for every " + every(u.Every) + "; not yet since the last restart")
	case last.Err != nil:
		b.WriteString("\nUpdates: the last look, " + ago(last.At) + ", failed: " + firstLine(last.Err.Error()))
	case last.Built != "":
		b.WriteString("\nUpdates: built " + last.Built + " " + ago(last.At))
	default:
		b.WriteString("\nUpdates: looked " + ago(last.At) + "; " + last.Skipped)
	}
	if n := len(u.versions()); n > 1 {
		b.WriteString("\nVersions on disk: " + strconv.Itoa(n))
	}
	return b.String()
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > 1500 {
		out = "…" + out[len(out)-1500:]
	}
	return out
}

func firstLine(s string) string {
	first, _, _ := strings.Cut(s, "\n")
	return first
}

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func every(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		if d == 24*time.Hour {
			return "day"
		}
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
	return d.String()
}
