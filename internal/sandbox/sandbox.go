// Package sandbox runs agent sandboxes.
//
// The production runtime is gVisor (runsc) over an OverlayFS root: the
// image is a shared read-only lower layer and each agent owns its upper
// layer. A sandbox is in one of three states:
//
//	stopped       no processes, no RAM; the upper layer holds all state
//	running       booted or restored
//	checkpointed  memory image on disk, no RAM; restores in tens of ms
//
// Suspend picks between the last two: a sandbox whose only process is the
// guest init is simply stopped, and one with background processes (a dev
// server, a database) is checkpointed so they survive.
package sandbox

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/rahimnathwani/hermitator/internal/spool"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// States.
const (
	Stopped      = "stopped"
	Running      = "running"
	Checkpointed = "checkpointed"
)

// How a sandbox came to be running.
const (
	WakeWarm    = "warm"    // it already was
	WakeRestore = "restore" // from a checkpoint
	WakeCold    = "cold"    // booted from the upper layer
)

// Spec describes one agent's sandbox.
type Spec struct {
	ID string
	// Dir holds everything the sandbox owns on the host.
	Dir string
	// Image is the root filesystem used as the read-only lower layer.
	Image Image
	// IP is the agent's address on the hermitator bridge.
	IP       netip.Addr
	MemoryMB int
	// Tools is the guest bundle to mount at /opt/hermitator on a cold boot.
	Tools string
}

// Image is an exported base image.
type Image struct {
	Name   string
	Rootfs string
	Env    []string
}

// Process is one entry from the sandbox's process table.
type Process struct {
	PID  int
	PPID int
	Cmd  string
}

// Runtime manages sandboxes.
type Runtime interface {
	// State reports a sandbox's current state.
	State(ctx context.Context, s *Spec) (string, error)
	// Wake makes sure a sandbox is running and says how it got there.
	Wake(ctx context.Context, s *Spec) (string, error)
	// Suspend frees a running sandbox's memory, by stopping it when only
	// the guest init is left and checkpointing it otherwise. It returns
	// the resulting state.
	Suspend(ctx context.Context, s *Spec) (string, error)
	// Stop kills a sandbox and discards any checkpoint.
	Stop(ctx context.Context, s *Spec) error
	// Destroy stops a sandbox and deletes everything it owns.
	Destroy(ctx context.Context, s *Spec) error
	// Processes lists what is running inside.
	Processes(ctx context.Context, s *Spec) ([]Process, error)
	// Shell returns a command that runs an interactive login shell inside
	// a running sandbox. The caller attaches it to a pseudo-terminal.
	Shell(ctx context.Context, s *Spec, env []string) (*exec.Cmd, error)
}

// Layout of Spec.Dir.
func (s *Spec) upper() string      { return filepath.Join(s.Dir, "upper") }
func (s *Spec) work() string       { return filepath.Join(s.Dir, "work") }
func (s *Spec) merged() string     { return filepath.Join(s.Dir, "merged") }
func (s *Spec) checkpoint() string { return filepath.Join(s.Dir, "checkpoint") }
func (s *Spec) logPath() string    { return filepath.Join(s.Dir, "guest.log") }

// SharedDir is the host side of the sandbox's /shared.
func (s *Spec) SharedDir() string { return filepath.Join(s.Dir, "shared") }

// SpoolDir is the host side of the sandbox's job spool.
func (s *Spec) SpoolDir() string { return filepath.Join(s.Dir, "spool") }

// Spool returns a client for submitting jobs to the sandbox.
func (s *Spec) Spool() spool.Client { return spool.Client{Root: s.SpoolDir()} }

// InboxDir is where incoming media is written for the agent.
func (s *Spec) InboxDir() string { return filepath.Join(s.SharedDir(), turn.InboxDir) }

// OutboxDir is where the agent leaves files to be sent.
func (s *Spec) OutboxDir() string { return filepath.Join(s.SharedDir(), turn.OutboxDir) }

// prepare creates the host-side directories a sandbox needs.
func (s *Spec) prepare() error {
	for _, d := range []string{s.upper(), s.work(), s.merged(), s.InboxDir(), s.OutboxDir(), filepath.Join(s.OutboxDir(), "sent")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return spool.Init(s.SpoolDir())
}

func (s *Spec) hasCheckpoint() bool {
	_, err := os.Stat(filepath.Join(s.checkpoint(), "checkpoint.img"))
	return err == nil
}

// IdleProcesses reports whether ps shows nothing but the guest init.
func IdleProcesses(ps []Process) bool {
	for _, p := range ps {
		if p.PID != 1 {
			return false
		}
	}
	return true
}

// ErrUnsupported is returned by runtimes that cannot do something.
var ErrUnsupported = errors.New("not supported by this runtime")
