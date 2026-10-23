package orchestrator

import (
	"fmt"
	"strconv"
	"strings"
)

// Chat commands that answer from the sandbox without the model: each is
// one spool job, so it costs a wake and a few seconds, not a turn.

// getScript queues a file for the chat the way the agent would, zipping
// a directory first. Paths are relative to the workspace.
const getScript = `cd "${HERMITATOR_WORKSPACE:-/workspace}" 2>/dev/null
p=$1
if [ -d "$p" ]; then
  n=$(basename "$(cd "$p" && pwd)")
  [ "$n" = / ] && n=files
  z=/tmp/hermitator-get/$n.zip
  rm -rf /tmp/hermitator-get && mkdir -p /tmp/hermitator-get || exit 1
  (cd "$p" && zip -qr "$z" .) 2>&1 || exit 1
  p=$z
fi
whatsapp-send "$p" 2>&1 >/dev/null
s=$?
rm -rf /tmp/hermitator-get
exit $s`

// getFile sends a file from the sandbox to this chat now. The agent
// could, but that costs a turn and it may make the file again.
func (m *Manager) getFile(agentID string, chat chatRef, path string, reply func(string)) {
	path = strings.TrimSpace(path)
	if path == "" {
		reply("Usage: /get PATH, for example /get report.pdf (from /workspace) or /get /tmp/screenshot.png. A folder is sent as a zip.")
		return
	}
	m.sandboxJob(agentID, "get", []string{"sh", "-c", getScript, "get", path}, nil, reply, func(r jobResult) {
		if !r.exit.OK() {
			reply(formatShell(string(r.out), r.exit))
			return
		}
		// Straight to this chat, rather than at the next background tick.
		if m.deliverOutbox(m.ctx, r.spec, chat) == 0 {
			reply("⚠️ Could not send " + path + ".")
		}
	})
}

// gitScript describes each repository under the workspace, one per line:
// path, branch, uncommitted files, commits on no remote, remote, and the
// last commit's subject and age.
const gitScript = `w=${HERMITATOR_WORKSPACE:-/workspace}
cd "$w" 2>/dev/null || exit 0
find . -maxdepth 4 -name .git -prune -print 2>/dev/null | sort | head -n 30 | while read -r g; do
  d=${g%/.git}
  b=$(git -C "$d" branch --show-current 2>/dev/null)
  c=$(git -C "$d" status --porcelain 2>/dev/null | wc -l)
  u=$(git -C "$d" log --branches --not --remotes --oneline 2>/dev/null | wc -l)
  r=$(git -C "$d" remote 2>/dev/null | head -n 1)
  l=$(git -C "$d" log -1 --format='%cr%x09%s' 2>/dev/null)
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "${d#.}" "$b" "$c" "$u" "$r" "$l"
done`

// gitCommand answers /git: which repositories have work that is not
// committed or not pushed, or, given one, its status and recent commits.
func (m *Manager) gitCommand(agentID, arg string, reply func(string)) {
	if arg = strings.TrimSpace(arg); arg != "" {
		argv := []string{"sh", "-c", `cd "${HERMITATOR_WORKSPACE:-/workspace}" 2>/dev/null; exec 2>&1; git -C "$1" status --short --branch && echo && git -C "$1" log --oneline --decorate -8`, "git", arg}
		m.sandboxJob(agentID, "git", argv, nil, reply, func(r jobResult) {
			reply(formatShell(string(r.out), r.exit))
		})
		return
	}
	m.sandboxJob(agentID, "git", []string{"sh", "-c", gitScript}, nil, reply, func(r jobResult) {
		reply(formatRepos(string(r.out)))
	})
}

func formatRepos(out string) string {
	var b strings.Builder
	n := 0
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 5 {
			continue
		}
		n++
		path := "/workspace" + strings.TrimSuffix(f[0], "/")
		uncommitted, _ := strconv.Atoi(strings.TrimSpace(f[2]))
		unpushed, _ := strconv.Atoi(strings.TrimSpace(f[3]))
		fmt.Fprintf(&b, "\n*%s*", clip(path, 120))
		if f[1] != "" {
			fmt.Fprintf(&b, " (%s)", clip(f[1], 60))
		}
		var state []string
		if uncommitted > 0 {
			state = append(state, count(uncommitted, "uncommitted file"))
		}
		switch {
		case f[4] == "" && unpushed > 0:
			state = append(state, "no remote, "+count(unpushed, "commit"))
		case unpushed > 0:
			state = append(state, count(unpushed, "unpushed commit"))
		}
		if len(state) == 0 {
			state = append(state, "✅ clean and pushed")
		}
		b.WriteString(": " + strings.Join(state, ", "))
		if len(f) >= 7 && f[6] != "" {
			fmt.Fprintf(&b, "\n   last commit %s: %s", f[5], clip(f[6], 80))
		}
	}
	if n == 0 {
		return "No git repositories under /workspace."
	}
	return strings.TrimPrefix(b.String(), "\n") + "\n\n/git PATH shows one repository's changes and recent commits."
}

// count is "1 file" or "3 files".
func count(n int, what string) string { return strconv.Itoa(n) + " " + what + plural(n) }
