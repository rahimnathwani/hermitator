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
