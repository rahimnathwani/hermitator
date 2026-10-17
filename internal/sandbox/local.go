package sandbox

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Local runs the guest as an ordinary host process with no isolation at
// all. It exists so hermitator can be developed and tested on machines
// without gVisor (macOS, CI). Never point it at an agent you do not trust.
type Local struct {
	Log *slog.Logger

	mu    sync.Mutex
	procs map[string]*localProc
}

type localProc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (s *Spec) localHome() string      { return filepath.Join(s.Dir, "home") }
func (s *Spec) localWorkspace() string { return filepath.Join(s.Dir, "workspace") }

func (l *Local) env(s *Spec) []string {
	env := os.Environ()
	return append(env,
		"HOME="+s.localHome(),
		"PATH="+filepath.Join(s.Tools, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HERMITATOR_SPOOL="+s.SpoolDir(),
		"HERMITATOR_SHARED="+s.SharedDir(),
		"HERMITATOR_WORKSPACE="+s.localWorkspace(),
	)
}

func (l *Local) running(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.procs[id]
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// State implements Runtime.
func (l *Local) State(ctx context.Context, s *Spec) (string, error) {
	if l.running(s.ID) {
		return Running, nil
	}
	return Stopped, nil
}

// Wake implements Runtime.
func (l *Local) Wake(ctx context.Context, s *Spec) (string, error) {
	if l.running(s.ID) {
		return WakeWarm, nil
	}
	if err := s.prepare(); err != nil {
		return "", err
	}
	for _, d := range []string{s.localHome(), s.localWorkspace()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	logf, err := os.OpenFile(s.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	cmd := exec.Command(filepath.Join(s.Tools, "bin", "hermitator-guest"), "init")
	cmd.Env = l.env(s)
	cmd.Dir = s.localWorkspace()
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return "", err
	}
	p := &localProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		logf.Close()
		close(p.done)
	}()
	l.mu.Lock()
	if l.procs == nil {
		l.procs = map[string]*localProc{}
	}
	l.procs[s.ID] = p
	l.mu.Unlock()
	return WakeCold, nil
}

// Suspend implements Runtime. Local sandboxes cannot be checkpointed, so
// they are always stopped.
func (l *Local) Suspend(ctx context.Context, s *Spec) (string, error) {
	return Stopped, l.Stop(ctx, s)
}

// Stop implements Runtime.
func (l *Local) Stop(ctx context.Context, s *Spec) error {
	l.mu.Lock()
	p := l.procs[s.ID]
	delete(l.procs, s.ID)
	l.mu.Unlock()
	if p == nil {
		return nil
	}
	syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
	return nil
}

// Destroy implements Runtime.
func (l *Local) Destroy(ctx context.Context, s *Spec) error {
	l.Stop(ctx, s)
	return os.RemoveAll(s.Dir)
}

// Processes implements Runtime.
func (l *Local) Processes(ctx context.Context, s *Spec) ([]Process, error) {
	if !l.running(s.ID) {
		return nil, ErrNotRunning
	}
	return []Process{{PID: 1, Cmd: "hermitator-guest"}}, nil
}

// Shell implements Runtime.
func (l *Local) Shell(ctx context.Context, s *Spec, env []string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "/bin/bash", "-l")
	cmd.Env = append(l.env(s), env...)
	cmd.Dir = s.localWorkspace()
	return cmd, nil
}

var _ Runtime = (*Local)(nil)
