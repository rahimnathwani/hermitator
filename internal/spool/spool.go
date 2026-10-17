// Package spool runs processes inside a sandbox without holding any host
// file descriptor open across a checkpoint.
//
// gVisor kills every process descended from `runsc exec` when it restores a
// checkpoint, so anything that should survive a sleep has to be a child of
// the sandbox's PID 1. hermitator-guest runs as PID 1 and serves a spool
// directory shared with the host:
//
//	jobs/<id>.json   Job written by the host (atomically, via rename)
//	jobs/<id>.kill   request to terminate a running job
//	out/<id>.out     the job's stdout, appended as it runs
//	out/<id>.err     the job's stderr
//	out/<id>.exit    Exit, written once the process has been reaped
//
// The guest polls for work and the host polls for output. Both sides only
// ever create, rename and append regular files, which gVisor's gofer
// handles in shared mode without either side caching stale state.
package spool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Job describes a process to start.
type Job struct {
	ID    string   `json:"id"`
	Argv  []string `json:"argv"`
	Env   []string `json:"env,omitempty"`
	Dir   string   `json:"dir,omitempty"`
	Stdin []byte   `json:"stdin,omitempty"`
}

// Exit describes how a job ended.
type Exit struct {
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
	// Error is set when the job could not be started at all.
	Error string `json:"error,omitempty"`
}

// OK reports whether the job ran and exited zero.
func (e Exit) OK() bool { return e.Error == "" && e.Signal == "" && e.Code == 0 }

func (e Exit) String() string {
	switch {
	case e.Error != "":
		return "failed to start: " + e.Error
	case e.Signal != "":
		return "killed by " + e.Signal
	default:
		return fmt.Sprintf("exit status %d", e.Code)
	}
}

func jobsDir(root string) string { return filepath.Join(root, "jobs") }
func outDir(root string) string  { return filepath.Join(root, "out") }

func jobPath(root, id string) string  { return filepath.Join(jobsDir(root), id+".json") }
func killPath(root, id string) string { return filepath.Join(jobsDir(root), id+".kill") }
func outPath(root, id string) string  { return filepath.Join(outDir(root), id+".out") }
func errPath(root, id string) string  { return filepath.Join(outDir(root), id+".err") }
func exitPath(root, id string) string { return filepath.Join(outDir(root), id+".exit") }

// Init creates the spool layout under root.
func Init(root string) error {
	for _, d := range []string{jobsDir(root), outDir(root)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func validID(id string) bool {
	return id != "" && !strings.ContainsAny(id, "/\\.") && len(id) <= 64
}

// writeAtomic writes data to path via a temporary file and rename, so a
// poller on the other side of the gofer never sees a partial file.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeAtomic(path, b, 0o600)
}
