package spool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Client submits jobs to a spool from the host side.
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

// Submit queues a job.
func (c Client) Submit(job Job) error {
	if !validID(job.ID) {
		return fmt.Errorf("invalid job id %q", job.ID)
	}
	if err := Init(c.Root); err != nil {
		return err
	}
	return writeJSON(jobPath(c.Root, job.ID), job)
}

// Kill asks the guest to terminate a job and everything it started.
func (c Client) Kill(id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid job id %q", id)
	}
	return writeAtomic(killPath(c.Root, id), nil, 0o600)
}

// Exit returns a job's exit record if it has finished.
func (c Client) Exit(id string) (Exit, bool, error) {
	data, err := os.ReadFile(exitPath(c.Root, id))
	if errors.Is(err, os.ErrNotExist) {
		return Exit{}, false, nil
	}
	if err != nil {
		return Exit{}, false, err
	}
	var e Exit
	if err := json.Unmarshal(data, &e); err != nil {
		return Exit{}, false, err
	}
	return e, true, nil
}

// Follow calls fn with each line the job writes to stdout, without the
// trailing newline, until the job exits or ctx is done.
func (c Client) Follow(ctx context.Context, id string, fn func(line []byte)) (Exit, error) {
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
			f, err = os.Open(outPath(c.Root, id))
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
		// only after the process (and so all its output) is gone.
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
	data, _ := os.ReadFile(errPath(c.Root, id))
	if len(data) > max {
		data = data[len(data)-max:]
	}
	return string(data)
}

// Remove deletes everything the spool holds for a job.
func (c Client) Remove(id string) {
	for _, p := range []string{jobPath(c.Root, id), killPath(c.Root, id), outPath(c.Root, id), errPath(c.Root, id), exitPath(c.Root, id)} {
		os.Remove(p)
	}
}

// Run submits a job, waits for it, and returns its stdout.
func (c Client) Run(ctx context.Context, job Job) ([]byte, Exit, error) {
	if err := c.Submit(job); err != nil {
		return nil, Exit{}, err
	}
	defer c.Remove(job.ID)
	var out bytes.Buffer
	exit, err := c.Follow(ctx, job.ID, func(line []byte) {
		out.Write(line)
		out.WriteByte('\n')
	})
	if err != nil {
		c.Kill(job.ID)
		return nil, Exit{}, err
	}
	return out.Bytes(), exit, nil
}
