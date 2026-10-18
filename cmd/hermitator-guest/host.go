package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/harness"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

func shared(parts ...string) string {
	return filepath.Join(append([]string{harness.Getenv("HERMITATOR_SHARED", turn.SharedDir)}, parts...)...)
}

// ask hands a request to the host and waits for its answer. The host
// looks for requests at least every 15 seconds while the sandbox is awake,
// and every second during a turn.
func ask(req turn.HostRequest, wait time.Duration) int {
	dir := shared(turn.RequestsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	id := fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	data, _ := json.Marshal(req)
	tmp := filepath.Join(dir, "."+id+".json")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.Rename(tmp, filepath.Join(dir, id+".json")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	answer := filepath.Join(dir, id+".answer.json")
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(answer); err == nil {
			os.Remove(answer)
			var a turn.HostAnswer
			if json.Unmarshal(b, &a) != nil {
				fmt.Fprintln(os.Stderr, "hermitator answered with something unreadable")
				return 1
			}
			if !a.OK {
				fmt.Fprintln(os.Stderr, a.Message)
				return 1
			}
			fmt.Println(a.Message)
			return 0
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Println("Handed to hermitator, which has not answered yet; check with `hermitator-guest tasks` in a minute.")
	return 0
}

const scheduleUsage = `usage: hermitator-guest schedule (--cron EXPR | --at TIME | --in DURATION) [--name NAME] PROMPT...

Runs PROMPT later, as a message to you in this chat, in this conversation.
Times are in the owner's time zone.

  --cron "0 9 * * 1-5"   on a schedule (minute hour day month weekday)
  --at "2026-10-19 09:00" or --at 09:00 or --at 9am   once
  --in 2h30m             once, after this long
  --name "CI check"      what the owner sees; defaults to the prompt

Runs at most every 5 minutes. List with: hermitator-guest tasks.
Cancel with: hermitator-guest unschedule ID.`

func schedule(args []string) int {
	fs := flag.NewFlagSet("schedule", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, scheduleUsage) }
	var req turn.HostRequest
	fs.StringVar(&req.Cron, "cron", "", "")
	fs.StringVar(&req.At, "at", "", "")
	fs.StringVar(&req.In, "in", "", "")
	fs.StringVar(&req.Name, "name", "", "")
	if fs.Parse(args) != nil {
		return 2
	}
	req.Op, req.Prompt = "schedule", strings.TrimSpace(strings.Join(fs.Args(), " "))
	n := 0
	for _, v := range []string{req.Cron, req.At, req.In} {
		if v != "" {
			n++
		}
	}
	if n != 1 || req.Prompt == "" {
		fs.Usage()
		return 2
	}
	return ask(req, 20*time.Second)
}

func unschedule(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: hermitator-guest unschedule ID   (IDs from: hermitator-guest tasks)")
		return 2
	}
	return ask(turn.HostRequest{Op: "unschedule", Task: args[0]}, 20*time.Second)
}

func tasks() int {
	b, err := os.ReadFile(shared(turn.TasksFile))
	var list []turn.TaskInfo
	if err == nil {
		err = json.Unmarshal(b, &list)
	}
	if err != nil || len(list) == 0 {
		fmt.Println("No scheduled tasks.")
		return 0
	}
	for _, t := range list {
		state := "next " + t.NextRun
		if t.Paused {
			state = "paused"
		}
		fmt.Printf("%s  %s  [%s, %s, set by %s]\n    %s\n", t.ID, t.Name, t.Schedule, state, t.By, t.Prompt)
	}
	return 0
}

// hold keeps the sandbox awake while a command runs, so background work
// progresses instead of freezing when the agent goes idle. The file it
// keeps fresh is what the host checks; a hold that stops refreshing it
// (because the sandbox was killed, say) lapses after a few minutes.
func hold(args []string) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: hermitator-guest hold [--] COMMAND [ARGS...]\n\nKeeps this machine awake until COMMAND exits (at most 12 hours).")
		return 2
	}
	dir := shared(turn.HoldsDir)
	os.MkdirAll(dir, 0o755)
	file := filepath.Join(dir, fmt.Sprintf("%d-%d", time.Now().Unix(), os.Getpid()))
	if err := os.WriteFile(file, []byte(strings.Join(args, " ")), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "hold:", err)
		return 1
	}
	defer os.Remove(file)
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-tick.C:
				os.Chtimes(file, now, now)
			}
		}
	}()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	close(done)
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "hold:", err)
		return 127
	}
	return 0
}
