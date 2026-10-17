package spool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T) (Client, *Server) {
	t.Helper()
	root := t.TempDir()
	srv := &Server{Root: root, Poll: 10 * time.Millisecond, Env: os.Environ(), KillGrace: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return Client{Root: root, Poll: 10 * time.Millisecond}, srv
}

func TestRunStreamsOutputAndExitCode(t *testing.T) {
	c, _ := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job := Job{ID: "j1", Argv: []string{"sh", "-c", "read x; echo got:$x; echo two; echo err >&2; exit 3"}, Stdin: []byte("hello\n")}
	if err := c.Submit(job); err != nil {
		t.Fatal(err)
	}
	var lines []string
	exit, err := c.Follow(ctx, "j1", func(l []byte) { lines = append(lines, string(l)) })
	if err != nil {
		t.Fatal(err)
	}
	if exit.Code != 3 || exit.OK() {
		t.Fatalf("exit = %+v", exit)
	}
	if strings.Join(lines, "|") != "got:hello|two" {
		t.Fatalf("lines = %q", lines)
	}
	if got := c.Stderr("j1", 100); got != "err\n" {
		t.Fatalf("stderr = %q", got)
	}
	if _, err := os.Stat(jobPath(c.Root, "j1")); !os.IsNotExist(err) {
		t.Fatal("job file (which may hold credentials) was left on disk")
	}
}

func TestKillReachesTheWholeProcessGroup(t *testing.T) {
	c, srv := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.Submit(Job{ID: "j2", Argv: []string{"sh", "-c", "sleep 60 & sleep 60; wait"}})
	deadline := time.Now().Add(5 * time.Second)
	for srv.Running() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.Kill("j2"); err != nil {
		t.Fatal(err)
	}
	exit, err := c.Follow(ctx, "j2", func([]byte) {})
	if err != nil {
		t.Fatal(err)
	}
	if exit.Signal != "SIGTERM" {
		t.Fatalf("exit = %+v", exit)
	}
}

func TestStartFailureIsReported(t *testing.T) {
	c, _ := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, exit, err := c.Run(ctx, Job{ID: "j3", Argv: []string{"definitely-not-a-binary"}})
	if err != nil {
		t.Fatal(err)
	}
	if exit.Error == "" {
		t.Fatalf("exit = %+v", exit)
	}
}

func TestUnfinishedJobsAreMarkedLostOnColdStart(t *testing.T) {
	root := t.TempDir()
	Init(root)
	os.WriteFile(outPath(root, "old"), []byte("partial"), 0o600)
	srv := &Server{Root: root, Poll: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx)
	defer cancel()
	c := Client{Root: root, Poll: 10 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if exit, done, _ := c.Exit("old"); done {
			if exit.Error == "" {
				t.Fatalf("exit = %+v", exit)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("lost job never got an exit record")
}

func TestBackgroundChildDoesNotHoldUpTheExit(t *testing.T) {
	c, _ := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Like `npm run dev &` without a redirect: the background process
	// inherits the job's stdout and outlives it.
	start := time.Now()
	out, exit, err := c.Run(ctx, Job{ID: "bg", Argv: []string{"sh", "-c", "sleep 30 & echo started"}})
	if err != nil {
		t.Fatal(err)
	}
	if !exit.OK() || string(out) != "started\n" {
		t.Fatalf("exit = %+v, out = %q", exit, out)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("exit took %s; a background child held it up", took)
	}
}

func TestClientRefusesSymlinkEscapes(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	// What a hostile agent could do from inside the sandbox.
	if err := os.Symlink(outside, filepath.Join(root, "jobs")); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, "out"), 0o700)
	os.WriteFile(filepath.Join(outside, "secret"), []byte("host file"), 0o600)
	c := Client{Root: root}
	if err := c.Submit(Job{ID: "j", Argv: []string{"true"}, Env: []string{"TOKEN=secret"}}); err == nil {
		t.Fatal("Submit followed a symlink out of the spool")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 {
		t.Fatalf("something was written outside the spool: %v", entries)
	}

	os.Remove(filepath.Join(root, "jobs"))
	os.RemoveAll(filepath.Join(root, "out"))
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "out"))
	if got := c.Stderr("j", 100); got != "" {
		t.Fatalf("Stderr read through a symlink: %q", got)
	}
}
