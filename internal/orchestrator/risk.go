package orchestrator

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/spool"
)

// RepoAtRisk is a git repository in an agent's workspace with work that
// exists nowhere else.
type RepoAtRisk struct {
	Path        string
	Uncommitted int
	Unpushed    int
}

// riskScript lists repositories under /workspace with uncommitted files
// or commits on no remote. It runs inside the sandbox: git on the host,
// against a repository the agent controls, could be made to run anything.
const riskScript = `w=${HERMITATOR_WORKSPACE:-/workspace}
cd "$w" 2>/dev/null || exit 0
find . -maxdepth 4 -name .git -prune -print 2>/dev/null | head -n 30 | while read -r g; do
  d=${g%/.git}
  c=$(git -C "$d" status --porcelain 2>/dev/null | wc -l)
  u=$(git -C "$d" log --branches --not --remotes --oneline 2>/dev/null | wc -l)
  printf '%s\t%s\t%s\n' "/workspace/${d#./}" "$c" "$u"
done`

// WorkAtRisk looks for work that deleting the agent would lose. It wakes
// the agent for as long as the check takes.
func (m *Manager) WorkAtRisk(ctx context.Context, agentID string) ([]RepoAtRisk, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := m.Hold(ctx, agentID)
	if err != nil {
		return nil, err
	}
	defer release()
	ag, err := m.store.Agent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	spec, err := m.Spec(ag)
	if err != nil {
		return nil, err
	}
	out, _, err := spec.Spool().Run(ctx, spool.Job{ID: "risk-" + ids.New(), Argv: []string{"sh", "-c", riskScript}})
	if err != nil {
		return nil, err
	}
	return parseRisk(string(out)), nil
}

func parseRisk(out string) []RepoAtRisk {
	var repos []RepoAtRisk
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		c, _ := strconv.Atoi(strings.TrimSpace(f[1]))
		u, _ := strconv.Atoi(strings.TrimSpace(f[2]))
		if c > 0 || u > 0 {
			repos = append(repos, RepoAtRisk{Path: clip(f[0], 200), Uncommitted: c, Unpushed: u})
		}
	}
	return repos
}
