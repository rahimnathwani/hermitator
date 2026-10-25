package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

// The agent's own browser: a headless Chromium that keeps running between
// turns, with a profile that persists, so the user can sign in to a site
// once from the panel's Browser tab and the agent can use the session
// afterwards. Chrome serves DevTools on loopback only; a forwarder run
// beside it makes that reachable from the host (see turn.BrowserPort).

const browserUsage = `usage: hermitator-guest browser [URL]
       hermitator-guest browser --status | --close | --forget

Starts this machine's own Chromium, unless it is running, and opens URL in
it. The user sees and uses it from the Browser tab of your page in
hermitator: that is how they sign in to a site for you (a login form, a
CAPTCHA, a code sent to their phone). It keeps cookies and saved logins in
~/.hermitator/browser. Use it, and the sessions in it, from Playwright:

  browser = playwright.chromium.connect_over_cdp("http://127.0.0.1:9222")
  context = browser.contexts[0]

  --status  whether it is running, and the pages it has open
  --close   quit it; sessions that last only until the browser closes end
  --forget  quit it and delete its profile: every cookie and saved login`

func browserCmd(args []string) int {
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, browserUsage)
		return 2
	}
	arg := ""
	if len(args) == 1 {
		arg = args[0]
	}
	switch arg {
	case "-h", "--help":
		fmt.Println(browserUsage)
		return 0
	case "--serve":
		return browserServe()
	case "--status":
		return browserStatus()
	case "--close":
		if err := browserClose(); err != nil {
			fmt.Fprintln(os.Stderr, "browser:", err)
			return 1
		}
		fmt.Println("The browser is closed.")
		return 0
	case "--forget":
		if err := browserClose(); err != nil {
			fmt.Fprintln(os.Stderr, "browser:", err)
			return 1
		}
		if err := os.RemoveAll(browserProfile()); err != nil {
			fmt.Fprintln(os.Stderr, "browser:", err)
			return 1
		}
		fmt.Println("The browser is closed and its profile deleted: no cookies or saved logins are left.")
		return 0
	}
	if strings.HasPrefix(arg, "-") {
		fmt.Fprintln(os.Stderr, browserUsage)
		return 2
	}
	target := ""
	if arg != "" {
		var err error
		if target, err = browserURL(arg); err != nil {
			fmt.Fprintln(os.Stderr, "browser:", err)
			return 2
		}
	}
	started := false
	if !browserUp() {
		if err := browserStart(target); err != nil {
			fmt.Fprintln(os.Stderr, "browser:", err)
			return 1
		}
		started = true
	}
	if target != "" && !started {
		if err := browserOpen(target); err != nil {
			fmt.Fprintln(os.Stderr, "browser:", err)
			return 1
		}
	}
	switch {
	case target != "":
		fmt.Printf("Opened %s in the browser.\n", target)
	case started:
		fmt.Println("The browser is running.")
	default:
		fmt.Println("The browser was already running.")
	}
	fmt.Printf("DevTools: http://%s (Playwright: chromium.connect_over_cdp)\n", browserDebugAddr())
	return 0
}

// browserURL accepts a URL with or without its scheme.
func browserURL(s string) (string, error) {
	if !strings.Contains(s, "://") && !strings.HasPrefix(s, "about:") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "about" && u.Host == "") {
		return "", fmt.Errorf("%q is not a URL", s)
	}
	return u.String(), nil
}

func browserHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/root"
	}
	return filepath.Join(home, ".hermitator")
}

func browserProfile() string { return filepath.Join(browserHome(), "browser") }
func browserPIDFile() string { return filepath.Join(browserHome(), "browser.pid") }
func browserLog() string     { return filepath.Join(browserHome(), "browser.log") }

// browserDebugAddr is where Chrome serves DevTools. Tests move it off the
// usual port, which a developer's own Chrome may hold.
func browserDebugAddr() string {
	if a := os.Getenv("HERMITATOR_BROWSER_DEBUG_ADDR"); a != "" {
		return a
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(turn.BrowserDebugPort))
}

func browserListenAddr() string {
	if a := os.Getenv(turn.BrowserListenEnv); a != "" {
		return a
	}
	return net.JoinHostPort("0.0.0.0", strconv.Itoa(turn.BrowserPort))
}

var browserHTTP = &http.Client{Timeout: 5 * time.Second}

func browserUp() bool {
	c := &http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + browserDebugAddr() + "/json/version")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// browserStart starts the daemon that runs Chrome, in a session of its
// own so that ending or aborting the turn that started it does not reach
// it, and waits until DevTools answers.
func browserStart(target string) error {
	if err := os.MkdirAll(browserHome(), 0o700); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(browserLog(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	argv := []string{"browser", "--serve"}
	cmd := exec.Command(exe, argv...)
	// Stdout must not be the job's: the spool would wait for it to close.
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Env = append(os.Environ(), "HERMITATOR_BROWSER_OPEN="+target)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if browserUp() {
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("the browser did not start: %s", tailFile(browserLog(), 600))
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("the browser did not answer within 45 seconds: %s", tailFile(browserLog(), 600))
}

// browserOpen opens target in a new tab and brings it to the front, so
// the panel shows it.
func browserOpen(target string) error {
	base := "http://" + browserDebugAddr()
	req, _ := http.NewRequest(http.MethodPut, base+"/json/new?"+url.QueryEscape(target), nil)
	resp, err := browserHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var page struct {
		ID string `json:"id"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&page) != nil {
		return fmt.Errorf("could not open a tab: %s", resp.Status)
	}
	if r, err := browserHTTP.Get(base + "/json/activate/" + page.ID); err == nil {
		r.Body.Close()
	}
	// The blank tab Chrome starts with is only in the way.
	for _, p := range browserPages() {
		if p.ID != page.ID && p.URL == "about:blank" {
			if r, err := browserHTTP.Get(base + "/json/close/" + p.ID); err == nil {
				r.Body.Close()
			}
		}
	}
	return nil
}

type browserPage struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

func browserPages() []browserPage {
	resp, err := browserHTTP.Get("http://" + browserDebugAddr() + "/json/list")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var all []browserPage
	json.NewDecoder(resp.Body).Decode(&all)
	var pages []browserPage
	for _, p := range all {
		if p.Type == "page" {
			pages = append(pages, p)
		}
	}
	return pages
}

func browserStatus() int {
	if !browserUp() {
		fmt.Println("The browser is not running. hermitator-guest browser URL starts it.")
		return 0
	}
	fmt.Printf("The browser is running. DevTools: http://%s\n", browserDebugAddr())
	for _, p := range browserPages() {
		fmt.Printf("- %s %s\n", clipText(p.Title, 60), p.URL)
	}
	return 0
}

// browserClose ends the daemon, which quits Chrome cleanly so it writes
// its cookies out.
func browserClose() error {
	pid := browserPID()
	if pid == 0 {
		return nil
	}
	syscall.Kill(pid, syscall.SIGTERM)
	for range 150 {
		if syscall.Kill(pid, 0) != nil {
			os.Remove(browserPIDFile())
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("the browser did not quit within 15 seconds")
}

// browserPID is the running daemon's, or 0.
func browserPID() int {
	b, err := os.ReadFile(browserPIDFile())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 || syscall.Kill(pid, 0) != nil {
		return 0
	}
	return pid
}

// browserServe is the daemon: it runs Chrome and the forwarder until
// Chrome exits, or until it is told to stop.
func browserServe() int {
	if pid := browserPID(); pid != 0 && pid != os.Getpid() {
		fmt.Fprintln(os.Stderr, "browser: already running as", pid)
		return 1
	}
	chrome, err := chromiumPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "browser:", err)
		return 1
	}
	profile := browserProfile()
	if err := os.MkdirAll(profile, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "browser:", err)
		return 1
	}
	// A browser that was killed leaves its lock behind, naming a PID that
	// may since belong to something else, and Chrome would refuse the
	// profile. Nothing else uses it while no daemon runs.
	for _, f := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		os.Remove(filepath.Join(profile, f))
	}
	host, port, _ := net.SplitHostPort(browserDebugAddr())
	args := []string{
		"--headless=new", "--no-sandbox", "--disable-setuid-sandbox", "--disable-dev-shm-usage", "--disable-gpu",
		"--remote-debugging-address=" + host, "--remote-debugging-port=" + port,
		"--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check", "--password-store=basic",
		"--window-size=412,915",
	}
	// Sites refuse sign-ins from a browser that calls itself headless.
	if ua := desktopUserAgent(chrome); ua != "" {
		args = append(args, "--user-agent="+ua)
	}
	if target := os.Getenv("HERMITATOR_BROWSER_OPEN"); target != "" {
		args = append(args, target)
	}
	ln, err := net.Listen("tcp", browserListenAddr())
	if err != nil {
		fmt.Fprintln(os.Stderr, "browser: forwarder:", err)
		return 1
	}
	defer ln.Close()
	cmd := exec.Command(chrome, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "browser:", err)
		return 1
	}
	os.WriteFile(browserPIDFile(), []byte(strconv.Itoa(os.Getpid())), 0o600)
	defer os.Remove(browserPIDFile())
	go forward(ln, browserDebugAddr())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		time.Sleep(10 * time.Second)
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}()
	err = cmd.Wait()
	fmt.Fprintln(os.Stderr, "browser: chromium exited:", err)
	return 0
}

// chromiumPath finds Playwright's Chromium, the full browser rather than
// its headless shell, which has no profile to keep.
func chromiumPath() (string, error) {
	if p := os.Getenv("HERMITATOR_CHROMIUM"); p != "" {
		return p, nil
	}
	root := os.Getenv("PLAYWRIGHT_BROWSERS_PATH")
	if root == "" {
		root = "/opt/ms-playwright"
	}
	matches, _ := filepath.Glob(filepath.Join(root, "chromium-*", "chrome-linux*", "chrome"))
	if len(matches) == 0 {
		return "", fmt.Errorf("no Playwright Chromium under %s", root)
	}
	return matches[len(matches)-1], nil
}

var chromeVersion = regexp.MustCompile(`\b(\d+)\.\d+\.\d+\.\d+\b`)

// desktopUserAgent is what Chrome of the same version on Linux sends.
func desktopUserAgent(chrome string) string {
	out, err := exec.Command(chrome, "--version").Output()
	if err != nil {
		return ""
	}
	m := chromeVersion.FindStringSubmatch(string(out))
	if m == nil {
		return ""
	}
	return "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + m[1] + ".0.0.0 Safari/537.36"
}

// forward relays every connection to ln on to addr.
func forward(ln net.Listener, addr string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			d, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				return
			}
			defer d.Close()
			var once sync.Once
			done := make(chan struct{})
			pipe := func(dst, src net.Conn) {
				io.Copy(dst, src)
				once.Do(func() { close(done) })
			}
			go pipe(d, c)
			go pipe(c, d)
			<-done
		}()
	}
}

func tailFile(path string, n int) string {
	b, _ := os.ReadFile(path)
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	if s == "" {
		return "no output"
	}
	return s
}
