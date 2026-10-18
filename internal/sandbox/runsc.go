package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

// Runsc runs sandboxes with gVisor.
type Runsc struct {
	// Binary is the runsc executable. Its gvisor-bin sidecar directory
	// must sit next to it.
	Binary string
	// Root is runsc's state directory.
	Root string
	// Platform is the gVisor platform; systrap needs no KVM.
	Platform string
	Network  *Network
	Log      *slog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lock serialises lifecycle operations on one sandbox.
func (r *Runsc) lock(id string) func() {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = map[string]*sync.Mutex{}
	}
	l := r.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		r.locks[id] = l
	}
	r.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (r *Runsc) args(sub string, args ...string) []string {
	platform := r.Platform
	if platform == "" {
		platform = "systrap"
	}
	global := []string{
		"--root", r.Root,
		"--platform", platform,
		"--network", "sandbox",
		// Writes must land in our OverlayFS upper layer as ordinary
		// files, not in gVisor's own in-sandbox overlay.
		"--overlay2", "none",
	}
	return append(append(global, sub), args...)
}

func (r *Runsc) run(ctx context.Context, sub string, args ...string) ([]byte, error) {
	return command(ctx, r.Binary, r.args(sub, args...)...)
}

// containerStatus returns runsc's view: "running", "stopped", "created",
// or "" if runsc does not know the container.
func (r *Runsc) containerStatus(ctx context.Context, id string) (string, error) {
	out, err := r.run(ctx, "state", id)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "not found") {
			return "", nil
		}
		return "", err
	}
	var st struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return "", fmt.Errorf("runsc state: %w", err)
	}
	return st.Status, nil
}

// State implements Runtime.
func (r *Runsc) State(ctx context.Context, s *Spec) (string, error) {
	st, err := r.containerStatus(ctx, s.ID)
	if err != nil {
		return "", err
	}
	switch {
	case st == "running":
		return Running, nil
	case s.hasCheckpoint():
		return Checkpointed, nil
	default:
		return Stopped, nil
	}
}

// Wake implements Runtime.
func (r *Runsc) Wake(ctx context.Context, s *Spec) (string, error) {
	defer r.lock(s.ID)()
	st, err := r.containerStatus(ctx, s.ID)
	if err != nil {
		return "", err
	}
	if st == "running" {
		return WakeWarm, nil
	}
	if st != "" {
		// A leftover container record (stopped, or created and never
		// started) would make run and restore fail.
		r.run(ctx, "delete", "-force", s.ID)
	}
	if err := s.prepare(); err != nil {
		return "", err
	}
	if err := r.Network.Attach(ctx, s.ID, s.IP); err != nil {
		return "", fmt.Errorf("network: %w", err)
	}
	if err := r.mount(ctx, s); err != nil {
		return "", err
	}

	if s.hasCheckpoint() {
		err := r.restore(ctx, s)
		if err == nil {
			return WakeRestore, nil
		}
		// A checkpoint can become unusable, for example after a gVisor
		// upgrade. The upper layer still has every file, so fall back to a
		// cold boot and only lose what was in memory.
		r.Log.Warn("restore failed; booting cold", "agent", s.ID, "err", err)
		r.run(ctx, "delete", "-force", s.ID)
		r.discardCheckpoint(s)
		if err := r.Network.Attach(ctx, s.ID, s.IP); err != nil {
			return "", fmt.Errorf("network: %w", err)
		}
	}

	if err := writeSpec(s, ociSpec(s, r.Network.NamespacePath(s.ID))); err != nil {
		return "", err
	}
	if err := r.start(ctx, s, "run", "-detach", "-bundle", s.Dir, s.ID); err != nil {
		return "", err
	}
	return WakeCold, nil
}

func (r *Runsc) restore(ctx context.Context, s *Spec) error {
	// The bundle config is deliberately reused as is: it must describe the
	// same mounts the checkpoint was taken with.
	if err := r.start(ctx, s, "restore", "-detach", "-image-path", s.checkpoint(), "-bundle", s.Dir, s.ID); err != nil {
		return err
	}
	r.discardCheckpoint(s)
	return nil
}

// start runs `runsc run` or `runsc restore` with the guest's output going
// to its log file.
//
// Both streams must be files: the sandbox processes runsc leaves behind
// inherit them, so a pipe would never reach EOF and Run would never
// return.
func (r *Runsc) start(ctx context.Context, s *Spec, sub string, args ...string) error {
	logf, err := os.OpenFile(s.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	before, _ := logf.Seek(0, io.SeekEnd)
	cmd := exec.CommandContext(ctx, r.Binary, r.args(sub, args...)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("runsc %s: %w: %s", sub, err, lastLine(readFrom(s.logPath(), before)))
	}
	return nil
}

// readFrom returns what a file gained after offset.
func readFrom(path string, offset int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	f.Seek(offset, io.SeekStart)
	b, _ := io.ReadAll(io.LimitReader(f, 64<<10))
	return string(b)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func (r *Runsc) discardCheckpoint(s *Spec) {
	os.RemoveAll(s.checkpoint())
}

// Suspend implements Runtime.
func (r *Runsc) Suspend(ctx context.Context, s *Spec) (string, error) {
	defer r.lock(s.ID)()
	st, err := r.containerStatus(ctx, s.ID)
	if err != nil {
		return "", err
	}
	if st != "running" {
		if st != "" {
			r.run(ctx, "delete", "-force", s.ID)
		}
		if s.hasCheckpoint() {
			return Checkpointed, nil
		}
		return Stopped, nil
	}
	ps, err := r.processes(ctx, s.ID)
	if err != nil {
		return "", err
	}
	if IdleProcesses(ps) {
		r.kill(ctx, s.ID)
		return Stopped, nil
	}

	tmp := s.checkpoint() + ".partial"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", err
	}
	if _, err := r.run(ctx, "checkpoint", "-image-path", tmp, s.ID); err != nil {
		os.RemoveAll(tmp)
		// If the checkpoint failed the sandbox is still running.
		return Running, err
	}
	r.discardCheckpoint(s)
	if err := os.Rename(tmp, s.checkpoint()); err != nil {
		return "", err
	}
	r.run(ctx, "delete", "-force", s.ID)
	return Checkpointed, nil
}

func (r *Runsc) kill(ctx context.Context, id string) {
	r.run(ctx, "kill", id, "KILL")
	r.run(ctx, "delete", "-force", id)
}

// Stop implements Runtime. It also unmounts the root, so the next boot
// mounts it afresh, possibly over a different image.
func (r *Runsc) Stop(ctx context.Context, s *Spec) error {
	defer r.lock(s.ID)()
	r.kill(ctx, s.ID)
	r.discardCheckpoint(s)
	if mounted(s.merged()) {
		if _, err := command(ctx, "umount", s.merged()); err != nil {
			return err
		}
	}
	return nil
}

// Destroy implements Runtime.
func (r *Runsc) Destroy(ctx context.Context, s *Spec) error {
	defer r.lock(s.ID)()
	r.kill(ctx, s.ID)
	if mounted(s.merged()) {
		if _, err := command(ctx, "umount", s.merged()); err != nil {
			return err
		}
	}
	if err := r.Network.Detach(ctx, s.ID); err != nil {
		return err
	}
	return os.RemoveAll(s.Dir)
}

// Processes implements Runtime.
func (r *Runsc) Processes(ctx context.Context, s *Spec) ([]Process, error) {
	return r.processes(ctx, s.ID)
}

func (r *Runsc) processes(ctx context.Context, id string) ([]Process, error) {
	// `runsc ps --format json` only lists PIDs, so parse the table.
	out, err := r.run(ctx, "ps", id)
	if err != nil {
		return nil, err
	}
	return parsePS(out), nil
}

func parsePS(out []byte) []Process {
	var ps []Process
	sc := bufio.NewScanner(bytes.NewReader(out))
	header := true
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if header {
			header = false
			continue
		}
		if len(f) < 9 {
			continue
		}
		pid, err1 := strconv.Atoi(f[1])
		ppid, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil {
			continue
		}
		ps = append(ps, Process{PID: pid, PPID: ppid, Cmd: strings.Join(f[8:], " ")})
	}
	return ps
}

// Shell implements Runtime. The environment, which carries credentials,
// goes to runsc in a process spec file rather than on its command line,
// where any process on the host could read it.
func (r *Runsc) Shell(ctx context.Context, s *Spec, env []string) (*exec.Cmd, error) {
	proc := specs.Process{
		Args: []string{"/bin/bash", "-l"},
		Env:  append(guestEnv(s.Image), env...),
		Cwd:  turn.Workspace,
		Capabilities: &specs.LinuxCapabilities{
			Bounding:  capabilities,
			Effective: capabilities,
			Permitted: capabilities,
		},
	}
	b, err := json.Marshal(proc)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(s.Dir, ".exec-*.json")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	// runsc reads the file as it starts; it need not outlive that.
	time.AfterFunc(10*time.Second, func() { os.Remove(path) })
	return exec.CommandContext(ctx, r.Binary, r.args("exec", "-process", path, s.ID)...), nil
}

func (r *Runsc) mount(ctx context.Context, s *Spec) error {
	if mounted(s.merged()) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(s.Image.Rootfs, "bin")); err != nil {
		return fmt.Errorf("image %s is missing: %w", s.Image.Name, err)
	}
	_, err := command(ctx, "mount", "-t", "overlay", "overlay", "-o", overlayOptions(s), s.merged())
	return err
}

// overlayOptions are the OverlayFS mount options for a sandbox's root.
//
// index=off: with the index feature (on by default in some kernels, Arch's
// among them) an upper layer records the lower layer it was first used
// with and refuses any other, which would make moving an agent to a newer
// image impossible. hermitator needs nothing the index provides
// (hard-link consistency on copy-up, NFS export).
func overlayOptions(s *Spec) string {
	return "lowerdir=" + s.Image.Rootfs + ",upperdir=" + s.upper() + ",workdir=" + s.work() + ",index=off"
}

// mounted reports whether path is a mount point.
func mounted(path string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) > 4 && unescapeMount(f[4]) == path {
			return true
		}
	}
	return false
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

// Check verifies that runsc works on this host.
func (r *Runsc) Check(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(filepath.Dir(resolve(r.Binary)), "gvisor-bin")); err != nil {
		return fmt.Errorf("runsc sidecar binaries (gvisor-bin/) not found next to %s", r.Binary)
	}
	_, err := command(ctx, r.Binary, "--version")
	return err
}

func resolve(bin string) string {
	if p, err := exec.LookPath(bin); err == nil {
		bin = p
	}
	if p, err := filepath.EvalSymlinks(bin); err == nil {
		return p
	}
	return bin
}

// GuestShared implements Runtime.
func (r *Runsc) GuestShared(s *Spec) string { return turn.SharedDir }

// GuestTools implements Runtime.
func (r *Runsc) GuestTools(s *Spec) string { return turn.ToolsDir }

var _ Runtime = (*Runsc)(nil)

// ErrNotRunning is returned for operations that need a running sandbox.
var ErrNotRunning = errors.New("sandbox is not running")
