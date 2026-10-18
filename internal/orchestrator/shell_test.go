package orchestrator

import (
	"strings"
	"testing"

	"github.com/rahimnathwani/hermitator/internal/spool"
)

func TestFormatShell(t *testing.T) {
	if got := formatShell("", spool.Exit{}); got != "_(no output)_" {
		t.Errorf("empty = %q", got)
	}
	if got := formatShell("a```b\n", spool.Exit{}); got != "```a'''b```" {
		t.Errorf("backticks = %q", got)
	}
	long := strings.Repeat("é", maxShellOutput)
	got := formatShell(long, spool.Exit{Code: 1})
	if !strings.HasPrefix(got, "```…") || !strings.Contains(got, "exit") || !strings.HasSuffix(strings.Split(got, "```")[1], "é") {
		t.Errorf("long output = %.40q…", got)
	}
}

func TestParseRisk(t *testing.T) {
	got := parseRisk("/workspace/shop\t3\t2\n/workspace/clean\t0\t0\ngarbage\n/workspace/new\t       1\t0\n")
	if len(got) != 2 || got[0] != (RepoAtRisk{"/workspace/shop", 3, 2}) || got[1] != (RepoAtRisk{"/workspace/new", 1, 0}) {
		t.Fatalf("parseRisk = %+v", got)
	}
}
