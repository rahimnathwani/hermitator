package spool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Client submits jobs to a spool from the host side.
//
// The spool directory is writable from inside the sandbox, so its
// contents are untrusted: an agent could swap jobs/ or out/ for a symlink
// pointing anywhere on the host. Every operation therefore goes through
// an os.Root, which refuses to resolve a path that leaves the spool.
type Client struct {
	Root string
	// Poll is how often output files are checked. The default is 50ms.
	Poll time.Duration
}

func (c Client) poll() time.Duration {
	if c.Poll == 0 {
		return 50 * time.Millisecond
	}
	return c.Poll
}

func (c Client) open() (*os.Root, error) {
	if err := os.MkdirAll(c.Root, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(c.Root)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"jobs", "out"} {
		if err := root.MkdirAll(d, 0o700); err != nil {
			root.Close()
			return nil, err
		}
	}
	return root, nil
}

// Prepare creates the spool layout.
func (c Client) Prepare() error {
	root, err := c.open()
	if err != nil {
		return err
	}
	return root.Close()
}

// with runs fn with the spool's root.
func (c Client) with(fn func(*os.Root) error) error {
	root, err := c.open()
	if err != nil {
		return err
	}
	defer root.Close()
	return fn(root)
}

func rel(dir, id, ext string) string { return dir + "/" + id + ext }

func writeAtomicIn(root *os.Root, name string, data []byte) error {
	dir, base, _ := strings.Cut(name, "/")
	tmp := dir + "/." + base + ".tmp"
	if err := root.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

// Submit queues a job.
func (c Client) Submit(job Job) error {
	if !validID(job.ID) {
		return fmt.Errorf("invalid job id %q", job.ID)
	}
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return c.with(func(root *os.Root) error {
		return writeAtomicIn(root, rel("jobs", job.ID, ".json"), data)
	})
}

// Kill asks the guest to terminate a job and everything it started.
func (c Client) Kill(id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid job id %q", id)
	}
	return c.with(func(root *os.Root) error {
		return writeAtomicIn(root, rel("jobs", id, ".kill"), nil)
	})
}

// Exit returns a job's exit record if it has finished.
func (c Client) Exit(id string) (Exit, bool, error) {
	var e Exit
	var done bool
	err := c.with(func(root *os.Root) error {
		data, err := root.ReadFile(rel("out", id, ".exit"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		done = true
		return json.Unmarshal(data, &e)
	})
	return e, done, err
}

// Follow calls fn with each line the job writes to stdout, without the
// trailing newline, until the job exits or ctx is done.
func (c Client) Follow(ctx context.Context, id string, fn func(line []byte)) (Exit, error) {
	if !validID(id) {
		return Exit{}, fmt.Errorf("invalid job id %q", id)
	}
	root, err := c.open()
	if err != nil {
		return Exit{}, err
	}
	defer root.Close()

	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	var pending []byte
	buf := make([]byte, 64*1024)
	drain := func() error {
		if f == nil {
			var err error
			f, err = root.Open(rel("out", id, ".out"))
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
		}
		for {
			n, err := f.Read(buf)
			pending = append(pending, buf[:n]...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				fn(pending[:i])
				pending = pending[i+1:]
			}
			if err == io.EOF || n == 0 {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}

	t := time.NewTicker(c.poll())
	defer t.Stop()
	for {
		// Check for the exit record before draining: the guest writes it
		// only after the job's output is complete.
		exit, done, err := c.Exit(id)
		if err != nil {
			return Exit{}, err
		}
		if err := drain(); err != nil {
			return Exit{}, err
		}
		if done {
			if len(pending) > 0 {
				fn(pending)
			}
			return exit, nil
		}
		select {
		case <-ctx.Done():
			return Exit{}, ctx.Err()
		case <-t.C:
		}
	}
}

// Stderr returns up to the last max bytes a job wrote to stderr.
func (c Client) Stderr(id string, max int) string {
	var data []byte
	c.with(func(root *os.Root) error {
		data, _ = root.ReadFile(rel("out", id, ".err"))
		return nil
	})
	if len(data) > max {
		data = data[len(data)-max:]
	}
	return string(data)
}

// Cancel kills a job, waits up to grace for it to exit, then removes its
// files. Removing them straight after Kill would delete the kill request
// before the guest saw it.
func (c Client) Cancel(id string, grace time.Duration) {
	c.Kill(id)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if _, done, _ := c.Exit(id); done {
			break
		}
		time.Sleep(c.poll())
	}
	c.Remove(id)
}

// Remove deletes everything the spool holds for a job.
func (c Client) Remove(id string) {
	if !validID(id) {
		return
	}
	c.with(func(root *os.Root) error {
		for _, name := range []string{rel("jobs", id, ".json"), rel("jobs", id, ".kill"), rel("out", id, ".out"), rel("out", id, ".err"), rel("out", id, ".exit")} {
			root.Remove(name)
		}
		return nil
	})
}

// Sweep removes every finished job, for cleaning up after a restart.
func (c Client) Sweep() {
	var done []string
	c.with(func(root *os.Root) error {
		dir, err := root.Open("out")
		if err != nil {
			return err
		}
		defer dir.Close()
		names, _ := dir.Readdirnames(-1)
		for _, n := range names {
			if id, ok := strings.CutSuffix(n, ".exit"); ok && validID(id) {
				done = append(done, id)
			}
		}
		return nil
	})
	for _, id := range done {
		c.Remove(id)
	}
}

// Run submits a job, waits for it, and returns its stdout.
func (c Client) Run(ctx context.Context, job Job) ([]byte, Exit, error) {
	if err := c.Submit(job); err != nil {
		return nil, Exit{}, err
	}
	var out bytes.Buffer
	exit, err := c.Follow(ctx, job.ID, func(line []byte) {
		out.Write(line)
		out.WriteByte('\n')
	})
	if err != nil {
		c.Cancel(job.ID, 15*time.Second)
		return nil, Exit{}, err
	}
	c.Remove(job.ID)
	return out.Bytes(), exit, nil
}

// SteerDir is where a running job finds messages sent to it after it
// started, relative to the spool: steer/ID/SEQ.json.
func SteerDir(spool, id string) string { return spool + "/steer/" + id }

// Steer hands a running job one more message, numbered seq so the job
// reads them in order.
func (c Client) Steer(id string, seq int, data []byte) error {
	if !validID(id) {
		return fmt.Errorf("invalid job id %q", id)
	}
	return c.with(func(root *os.Root) error {
		dir := "steer/" + id
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		name := fmt.Sprintf("%06d", seq)
		tmp := dir + "/." + name + ".tmp"
		if err := root.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		return root.Rename(tmp, dir+"/"+name+".json")
	})
}

// Unsteered returns, in order, the messages a job never read, and removes
// them; they are for the next turn.
func (c Client) Unsteered(id string) [][]byte {
	if !validID(id) {
		return nil
	}
	var out [][]byte
	c.with(func(root *os.Root) error {
		dir := "steer/" + id
		f, err := root.Open(dir)
		if err != nil {
			return nil
		}
		names, _ := f.Readdirnames(-1)
		f.Close()
		sort.Strings(names)
		for _, n := range names {
			if strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".json") {
				continue
			}
			if fi, err := root.Lstat(dir + "/" + n); err != nil || !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
				continue
			}
			if data, err := root.ReadFile(dir + "/" + n); err == nil {
				out = append(out, data)
			}
		}
		return root.RemoveAll(dir)
	})
	return out
}
