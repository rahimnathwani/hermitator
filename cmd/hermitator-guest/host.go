package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	return askOr(req, wait, "Handed to hermitator, which has not answered yet; check with `hermitator-guest tasks` in a minute.")
}

// askOr is ask with what to say when hermitator has not answered in time.
func askOr(req turn.HostRequest, wait time.Duration, pending string) int {
	a, answered, err := request(req, wait)
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		return 1
	case !answered:
		fmt.Println(pending)
		return 0
	case !a.OK:
		fmt.Fprintln(os.Stderr, a.Message)
		return 1
	}
	fmt.Println(a.Message)
	return 0
}

// request hands a request to the host and returns its answer, if one
// came within wait.
func request(req turn.HostRequest, wait time.Duration) (turn.HostAnswer, bool, error) {
	var a turn.HostAnswer
	dir := shared(turn.RequestsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return a, false, err
	}
	id := fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	req.Turn = os.Getenv(turn.EnvTurn)
	data, _ := json.Marshal(req)
	tmp := filepath.Join(dir, "."+id+".json")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return a, false, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, id+".json")); err != nil {
		return a, false, err
	}
	answer := filepath.Join(dir, id+".answer.json")
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(answer); err == nil {
			os.Remove(answer)
			if json.Unmarshal(b, &a) != nil {
				return a, false, errors.New("hermitator answered with something unreadable")
			}
			return a, true, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return a, false, nil
}

const previewUsage = `usage: hermitator-guest preview PORT [PATH]
       hermitator-guest preview http://localhost:PORT/PATH?QUERY

Prints a link the user can open to see what listens on PORT here. Given
a path, or the local URL an app printed (with its own tokens), the link
opens that page: send it exactly as printed, since it carries a secret
the page cannot be opened without. The server must listen on 0.0.0.0,
not localhost.`

// preview asks for a link to a port, for the user to open.
func preview(args []string) int {
	port, page, err := previewTarget(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error()+"\n\n"+previewUsage)
		return 2
	}
	// Agents' servers default to localhost, which the host cannot reach;
	// saying so here saves the owner a dead link and a round trip.
	if ip := sandboxIP(); ip != nil {
		outside, inside := listening(ip, port)
		if !outside && inside {
			fmt.Fprintf(os.Stderr, "Port %d answers on localhost only, so the link would not reach it. Restart the server listening on 0.0.0.0 (vite --host, next dev -H 0.0.0.0, python3 -m http.server --bind 0.0.0.0), then run this again.\n", port)
			return 1
		}
		if !outside {
			fmt.Fprintf(os.Stderr, "Nothing listens on port %d yet; start the server on 0.0.0.0 before sending the link.\n", port)
		}
	}
	a, answered, err := request(turn.HostRequest{Op: "preview", Port: port}, 20*time.Second)
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		return 1
	case !answered:
		fmt.Fprintln(os.Stderr, "hermitator has not answered; try again in a moment.")
		return 1
	case !a.OK:
		fmt.Fprintln(os.Stderr, a.Message)
		return 1
	}
	link, err := withPage(strings.TrimSpace(a.Message), page)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(link)
	return 0
}

// previewTarget reads PORT, PORT PATH, or a local URL such as the one a
// dev server prints. page holds the path, query and fragment to open, or
// is nil for the root.
func previewTarget(args []string) (port int, page *url.URL, err error) {
	if len(args) < 1 || len(args) > 2 {
		return 0, nil, errors.New("give a port or a URL")
	}
	if p, err := strconv.Atoi(args[0]); err == nil {
		if p < 1 || p > 65535 {
			return 0, nil, fmt.Errorf("%d is not a port", p)
		}
		if len(args) == 1 {
			return p, nil, nil
		}
		page, err := url.Parse(args[1])
		if err != nil || page.Scheme != "" || page.Host != "" || !strings.HasPrefix(page.Path, "/") {
			return 0, nil, fmt.Errorf("%q is not a path such as /app?x=1", args[1])
		}
		return p, page, nil
	}
	if len(args) != 1 {
		return 0, nil, errors.New("give a port and a path, or one URL")
	}
	u, err := url.Parse(args[0])
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return 0, nil, fmt.Errorf("%q is not a port or an http:// URL", args[0])
	}
	port = 80
	if s := u.Port(); s != "" {
		if port, err = strconv.Atoi(s); err != nil || port < 1 || port > 65535 {
			return 0, nil, fmt.Errorf("%q has no usable port", args[0])
		}
	}
	page = &url.URL{Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery, Fragment: u.Fragment}
	if page.Path == "" {
		page.Path = "/"
	}
	return port, page, nil
}

// withPage points a preview link at a page, keeping the link's secret
// and the page's own query (tokens included) as they were.
func withPage(link string, page *url.URL) (string, error) {
	if page == nil {
		return link, nil
	}
	u, err := url.Parse(link)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("hermitator answered with %q, not a link", link)
	}
	u.Path, u.RawPath, u.Fragment = page.Path, page.RawPath, page.Fragment
	if page.RawQuery != "" {
		u.RawQuery = page.RawQuery + "&" + u.RawQuery
	}
	return u.String(), nil
}

// sandboxIP is the address the host reaches this machine on: its first
// address that is not loopback, or nil without one.
func sandboxIP() net.IP {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP
		}
	}
	return nil
}

// listening says whether port answers on ip, as the host would reach it,
// and on localhost.
func listening(ip net.IP, port int) (outside, inside bool) {
	dial := func(host string) bool {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 2*time.Second)
		if err == nil {
			c.Close()
		}
		return err == nil
	}
	return dial(ip.String()), dial("127.0.0.1")
}

const scheduleUsage = `usage: hermitator-guest schedule (--cron EXPR | --at TIME | --in DURATION) [--name NAME] PROMPT...

Runs PROMPT later, as a message to you in this chat, in this conversation.
Times are in the owner's time zone.

  --cron "0 9 * * 1-5"   on a schedule (minute hour day month weekday)
  --at "2026-10-19 09:00" or --at 09:00 or --at 9am   once
  --in 2h30m             once, after this long
  --name "CI check"      what the owner sees; defaults to the prompt
  --if 'COMMAND'         run COMMAND here first, each time, and start the
                         turn only if it prints something (it comes with
                         PROMPT); no output, or a failure, starts nothing,
                         so a check costs no model call

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
	fs.StringVar(&req.If, "if", "", "")
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
		if t.If != "" {
			fmt.Printf("    if: %s\n", t.If)
		}
	}
	return 0
}

// hold keeps the sandbox awake while a command runs, so background work
// progresses instead of freezing when the agent goes idle.
func hold(args []string) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: hermitator-guest hold [--] COMMAND [ARGS...]\n\nKeeps this machine awake until COMMAND exits (at most 12 hours).")
		return 2
	}
	release, err := keepAwake(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hold:", err)
		return 1
	}
	defer release()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return exitCode("hold", cmd.Run())
}

// keepAwake holds the sandbox awake until release is called. The file it
// keeps fresh is what the host checks; a hold that stops refreshing it
// (because the sandbox was killed, say) lapses after a few minutes.
func keepAwake(args []string) (release func(), err error) {
	dir := shared(turn.HoldsDir)
	os.MkdirAll(dir, 0o755)
	file := filepath.Join(dir, fmt.Sprintf("%d-%d", time.Now().Unix(), os.Getpid()))
	if err := os.WriteFile(file, []byte(strings.Join(args, " ")), 0o644); err != nil {
		return nil, err
	}
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
	return func() { close(done); os.Remove(file) }, nil
}

func exitCode(what string, err error) int {
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	} else if err != nil {
		fmt.Fprintln(os.Stderr, what+":", err)
		return 127
	}
	return 0
}

const notifyUsage = `usage: hermitator-guest notify [--name NAME] [--] COMMAND [ARGS...]

Runs COMMAND, keeping this machine awake until it exits (at most 12
hours), then starts a new turn for you, in the chat you last talked in,
with its exit code and the end of its output. Start it in the
background and end your turn; don't wait or poll for it:

  nohup hermitator-guest notify --name "test suite" -- npm test > /tmp/tests.log 2>&1 &

COMMAND's output still goes where this command's does.`

// notifyTail is how much of a job's output its report carries.
const notifyTail = 3000

// notify runs a job the agent wants to hear back from, and hands the host
// a report on it when it ends. The report is handed over while the hold
// still keeps the sandbox awake, or the sandbox could sleep before the
// host had read it.
func notify(args []string) int {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, notifyUsage) }
	name := fs.String("name", "", "")
	if fs.Parse(args) != nil {
		return 2
	}
	args = fs.Args()
	if len(args) == 0 {
		fs.Usage()
		return 2
	}
	if *name == "" {
		*name = clipText(strings.Join(args, " "), 60)
	}
	release, err := keepAwake(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "notify:", err)
		return 1
	}
	defer release()
	out := &tailBuffer{max: notifyTail}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout, cmd.Stderr = io.MultiWriter(os.Stdout, out), io.MultiWriter(os.Stderr, out)
	start := time.Now()
	code := exitCode("notify", cmd.Run())
	took := time.Since(start).Round(time.Second)
	output := strings.TrimSpace(out.String())
	if output == "" {
		output = "(no output)"
	}
	report := fmt.Sprintf("[Background job %q, which you started with hermitator-guest notify, finished after %s with exit code %d. The end of its output:]\n```\n%s\n```\nTell the user how it went.",
		*name, took, code, output)
	askOr(turn.HostRequest{Op: "wake", Name: *name, Prompt: report}, time.Minute, "hermitator has not answered; the report may not have reached you.")
	return code
}

// checkOutput is how much of a check's output the host is given.
const checkOutput = 4000

// check runs a scheduled task's check for the host (the request on
// stdin) and prints how it went.
func check() int {
	var req turn.CheckRequest
	data, err := io.ReadAll(os.Stdin)
	if err == nil {
		err = json.Unmarshal(data, &req)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad check request:", err)
		return 2
	}
	stdout, stderr := &tailBuffer{max: checkOutput}, &tailBuffer{max: checkOutput}
	cmd := exec.Command("sh", "-c", req.Command)
	cmd.Dir = harness.Getenv("HERMITATOR_WORKSPACE", turn.Workspace)
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	code := exitCode("check", cmd.Run())
	json.NewEncoder(os.Stdout).Encode(turn.CheckResult{Exit: code, Stdout: stdout.String(), Stderr: stderr.String()})
	return 0
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ToValidUTF8(string(t.buf), "")
}

func clipText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// askPoll sends the user a WhatsApp poll.
func askPoll(args []string) int {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, `usage: hermitator-guest ask QUESTION OPTION OPTION [OPTION...]

Sends the user a single-choice WhatsApp poll (2 to 12 options). Their
choice arrives as their next message.`)
		return 2
	}
	return ask(turn.HostRequest{Op: "poll", Question: args[0], Options: args[1:]}, 20*time.Second)
}
