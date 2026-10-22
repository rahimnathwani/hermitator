package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/spool"
	"github.com/rahimnathwani/hermitator/internal/store"
)

// Starting an agent from a repository: the host clones it into the
// agent's workspace as a spool job, then, if asked, gives the agent a
// first message to get it ready to work on.

var (
	githubShort = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	repoDir     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// ParseRepo reads a repository as a user types it: owner/repo for GitHub,
// or an https URL. It returns the URL to clone and the directory name.
func ParseRepo(s string) (cloneURL, dir string, err error) {
	s = strings.TrimSpace(s)
	if githubShort.MatchString(s) {
		s = "https://github.com/" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("give a repository as owner/repo or an https:// URL")
	}
	if u.User != nil {
		return "", "", errors.New("leave credentials out of the URL; hermitator uses your GitHub token from Settings")
	}
	dir = strings.TrimSuffix(path.Base(strings.TrimRight(u.Path, "/")), ".git")
	if !repoDir.MatchString(dir) || dir == "." || dir == ".." {
		return "", "", errors.New("that URL does not name a repository")
	}
	return u.String(), dir, nil
}

// cloneScript clones $REPO into $DIR in the workspace. The GitHub token,
// if any, arrives on stdin and is offered to github.com only.
const cloneScript = `exec 2>&1
IFS= read -r t || true
cd "${HERMITATOR_WORKSPACE:-/workspace}" || exit 1
if [ -e "$DIR" ]; then echo "$DIR already exists in the workspace" >&2; exit 1; fi
if [ -n "$t" ]; then
	export GH_TOKEN="$t"
	exec git -c 'credential.https://github.com.helper=!f() { test "$1" = get && echo username=x-access-token && echo "password=$GH_TOKEN"; }; f' clone --quiet -- "$REPO" "$DIR"
fi
GIT_TERMINAL_PROMPT=0 exec git clone --quiet -- "$REPO" "$DIR"`

// StartFromRepo clones a repository into a new agent's workspace in the
// background and, with setup, asks the agent to get it ready. If the
// clone fails, the agent is told why, so the owner sees it and the agent
// can try itself.
func (m *Manager) StartFromRepo(agentID, cloneURL, dir string, setup bool) {
	m.bg.Add(1)
	go func() {
		defer m.bg.Done()
		err := m.cloneRepo(m.ctx, agentID, cloneURL, dir)
		switch {
		case err != nil:
			m.log.Warn("clone", "agent", agentID, "repo", cloneURL, "err", err)
			m.Prompt(store.ThreadKey{AgentID: agentID}, fmt.Sprintf("hermitator tried to clone %s into %s for you and failed: %v\nTry cloning it yourself, then tell me what happened.", cloneURL, path.Join("/workspace", dir), err))
		case setup:
			m.Prompt(store.ThreadKey{AgentID: agentID}, setupPrompt(cloneURL, dir))
		}
	}()
}

func setupPrompt(cloneURL, dir string) string {
	return fmt.Sprintf(`You were created to work on %s, which is cloned into /workspace/%s. Get it ready to work on:
1. Read its README and any contributor docs.
2. Install what it needs to build and run, as its docs say.
3. Run its tests, if it has any, and note which fail.
4. Write down in a few lines how to build, test and run it, in the repository's CLAUDE.md or AGENTS.md if it has one (do not commit it), or in your own memory otherwise.
Then reply with a short summary: what it is, whether it builds and the tests pass, and anything that needs me.`, cloneURL, dir)
}

func (m *Manager) cloneRepo(ctx context.Context, agentID, cloneURL, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	release, err := m.Hold(ctx, agentID)
	if err != nil {
		return err
	}
	defer release()
	ag, err := m.store.Agent(ctx, agentID)
	if err != nil {
		return err
	}
	spec, err := m.Spec(ag)
	if err != nil {
		return err
	}
	// The token a turn would get: a placeholder when the owner keeps
	// credentials out of the sandbox.
	creds, problem := m.credentials(ctx, ag)
	if problem != "" {
		m.extraCredentials(ctx, ag, &creds)
	}
	m.usePlaceholders(ctx, ag, spec, &creds)
	client := spec.Spool()
	out, exit, err := client.Run(ctx, spool.Job{ID: "clone-" + ids.New(), Argv: []string{"sh", "-c", cloneScript},
		Env: []string{"REPO=" + cloneURL, "DIR=" + dir}, Stdin: []byte(creds.env["GH_TOKEN"] + "\n")})
	if err != nil {
		return err
	}
	if !exit.OK() {
		detail := newRedactor(creds.redact).apply(strings.TrimSpace(string(out)))
		if len(detail) > 500 {
			detail = detail[len(detail)-500:]
		}
		if detail == "" {
			detail = exit.String()
		}
		return errors.New(detail)
	}
	return nil
}
