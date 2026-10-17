package spool

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Server runs jobs out of a spool directory. It is meant to be PID 1 in a
// sandbox, so it also reaps orphaned processes that get reparented to it.
type Server struct {
	Root string
	// Poll is how often the jobs directory is scanned.
	Poll time.Duration
	// Env is the base environment for every job.
	Env []string
	Log *slog.Logger
	// KillGrace is how long a job gets between SIGTERM and SIGKILL.
	KillGrace time.Duration

	mu    sync.Mutex
	byID  map[string]*proc
	byPID map[int]*proc
}

type proc struct {
	id      string
	process *os.Process
	killing bool
}

// Serve runs until ctx is cancelled, then terminates its jobs.
func (s *Server) Serve(ctx context.Context) error {
	if s.Poll == 0 {
		s.Poll = 100 * time.Millisecond
	}
	if s.KillGrace == 0 {
		s.KillGrace = 10 * time.Second
	}
	if s.Log == nil {
		s.Log = slog.New(slog.DiscardHandler)
	}
	s.mu.Lock()
	s.byID = map[string]*proc{}
	s.byPID = map[int]*proc{}
	s.mu.Unlock()
	if err := Init(s.Root); err != nil {
		return err
	}
	s.markLost()

	chld := make(chan os.Signal, 32)
	signal.Notify(chld, syscall.SIGCHLD)
	defer signal.Stop(chld)
	tick := time.NewTicker(s.Poll)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			s.terminateAll()
			return nil
		case <-chld:
			s.reap()
		case <-tick.C:
			s.scan()
			// SIGCHLD can coalesce; reaping on every tick is cheap insurance.
			s.reap()
		}
	}
}

// markLost closes out jobs that were running when the sandbox last
// stopped. A restored checkpoint resumes inside Serve and never gets here,
// so anything unfinished at startup is gone for good.
func (s *Server) markLost() {
	entries, _ := os.ReadDir(outDir(s.Root))
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".out")
		if !ok || !validID(id) {
			continue
		}
		if _, err := os.Stat(exitPath(s.Root, id)); errors.Is(err, os.ErrNotExist) {
			writeJSON(exitPath(s.Root, id), Exit{Code: -1, Error: "sandbox restarted while the job was running"})
		}
	}
}

func (s *Server) scan() {
	entries, err := os.ReadDir(jobsDir(s.Root))
	if err != nil {
		s.Log.Error("read jobs", "err", err)
		return
	}
	var kills []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if id, ok := strings.CutSuffix(name, ".kill"); ok {
			kills = append(kills, id)
			continue
		}
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || !validID(id) {
			continue
		}
		path := filepath.Join(jobsDir(s.Root), name)
		data, err := os.ReadFile(path)
		// Jobs can carry credentials; they never stay on disk once claimed.
		os.Remove(path)
		if err != nil {
			continue
		}
		var job Job
		if err := json.Unmarshal(data, &job); err != nil || job.ID != id {
			writeJSON(exitPath(s.Root, id), Exit{Code: -1, Error: "malformed job"})
			continue
		}
		if _, err := os.Stat(killPath(s.Root, id)); err == nil {
			writeJSON(exitPath(s.Root, id), Exit{Code: -1, Signal: "SIGTERM"})
			continue
		}
		s.start(&job)
	}
	for _, id := range kills {
		os.Remove(killPath(s.Root, id))
		s.kill(id)
	}
}

func (s *Server) start(job *Job) {
	fail := func(err error) {
		s.Log.Warn("job failed to start", "job", job.ID, "err", err)
		writeJSON(exitPath(s.Root, job.ID), Exit{Code: -1, Error: err.Error()})
	}
	if len(job.Argv) == 0 {
		fail(errors.New("empty argv"))
		return
	}
	env := append(append([]string{}, s.Env...), job.Env...)
	bin := job.Argv[0]
	if !strings.Contains(bin, "/") {
		resolved, err := lookPath(bin, env)
		if err != nil {
			fail(err)
			return
		}
		bin = resolved
	}

	stdout, err := os.OpenFile(outPath(s.Root, job.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fail(err)
		return
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(errPath(s.Root, job.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fail(err)
		return
	}
	defer stderr.Close()

	var stdin *os.File
	if len(job.Stdin) > 0 {
		r, w, err := os.Pipe()
		if err != nil {
			fail(err)
			return
		}
		defer r.Close()
		go func() {
			w.Write(job.Stdin)
			w.Close()
		}()
		stdin = r
	} else {
		devnull, err := os.Open(os.DevNull)
		if err != nil {
			fail(err)
			return
		}
		defer devnull.Close()
		stdin = devnull
	}

	dir := job.Dir
	if dir == "" {
		dir = "/"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := os.StartProcess(bin, job.Argv, &os.ProcAttr{
		Dir:   dir,
		Env:   env,
		Files: []*os.File{stdin, stdout, stderr},
		// A process group per job lets a kill reach everything it spawned.
		Sys: &syscall.SysProcAttr{Setpgid: true},
	})
	if err != nil {
		fail(err)
		return
	}
	pr := &proc{id: job.ID, process: p}
	s.byID[job.ID] = pr
	s.byPID[p.Pid] = pr
	s.Log.Info("job started", "job", job.ID, "pid", p.Pid, "argv0", job.Argv[0])
}

func (s *Server) kill(id string) {
	s.mu.Lock()
	pr := s.byID[id]
	if pr == nil || pr.killing {
		s.mu.Unlock()
		return
	}
	pr.killing = true
	pid := pr.process.Pid
	s.mu.Unlock()
	syscall.Kill(-pid, syscall.SIGTERM)
	time.AfterFunc(s.KillGrace, func() {
		s.mu.Lock()
		alive := s.byPID[pid] == pr
		s.mu.Unlock()
		if alive {
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
}

func (s *Server) reap() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
		s.mu.Lock()
		pr := s.byPID[pid]
		if pr != nil {
			delete(s.byPID, pid)
			delete(s.byID, pr.id)
		}
		s.mu.Unlock()
		if pr == nil {
			continue // an orphan reparented to us; reaping it is all we owe it
		}
		pr.process.Release()
		exit := Exit{Code: ws.ExitStatus()}
		if ws.Signaled() {
			exit.Signal = unixSignalName(ws.Signal())
		}
		writeJSON(exitPath(s.Root, pr.id), exit)
		s.Log.Info("job exited", "job", pr.id, "pid", pid, "exit", exit.String())
	}
}

func (s *Server) terminateAll() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.byID))
	for id := range s.byID {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.kill(id)
	}
}

// Running reports how many jobs are in flight.
func (s *Server) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

func lookPath(name string, env []string) (string, error) {
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		candidate := filepath.Join(dir, name)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func unixSignalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGABRT:
		return "SIGABRT"
	default:
		return sig.String()
	}
}
