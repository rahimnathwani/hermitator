package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/spool"
)

// maxShellOutput is how much of a /sh command's output is sent back.
const maxShellOutput = 3000

// shell runs one command in the agent's sandbox and replies with its
// output, without involving the model: quick checks cost nothing and
// still work when the model is rate-limited or broken. Commands are
// owner-only, like every chat command. The command runs as a spool job,
// with no credentials in its environment, and holds the sandbox awake
// only while it runs.
func (m *Manager) shell(agentID, command string, reply func(string)) {
	command = strings.TrimSpace(command)
	if command == "" {
		reply("Usage: /sh COMMAND, for example /sh git -C /workspace log --oneline -5")
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.ShellTimeout)
	defer cancel()
	release, err := m.Hold(ctx, agentID)
	if err != nil {
		reply("⚠️ Could not wake the sandbox: " + err.Error())
		return
	}
	defer release()
	ag, err := m.store.Agent(ctx, agentID)
	if err != nil {
		return
	}
	spec, err := m.Spec(ag)
	if err != nil {
		reply("⚠️ " + err.Error())
		return
	}
	job := spool.Job{ID: "sh-" + ids.New(), Argv: []string{"sh", "-lc", "cd /workspace 2>/dev/null; exec 2>&1; " + command}}
	out, exit, err := spec.Spool().Run(ctx, job)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reply(fmt.Sprintf("⚠️ Stopped after %s. For longer jobs, ask the agent to run them in the background.", m.cfg.ShellTimeout))
		return
	case err != nil:
		reply("⚠️ Could not run it: " + err.Error())
		return
	}
	reply(formatShell(string(out), exit))
}

func formatShell(out string, exit spool.Exit) string {
	out = strings.TrimRight(out, "\n")
	if len(out) > maxShellOutput {
		cut := len(out) - maxShellOutput
		for cut < len(out) && !utf8.RuneStart(out[cut]) {
			cut++
		}
		out = "…" + out[cut:]
	}
	// Backticks inside would end WhatsApp's code block early.
	out = strings.ReplaceAll(out, "```", "'''")
	status := ""
	if !exit.OK() {
		status = "\n" + exit.String()
	}
	if out == "" {
		return "_(no output)_" + status
	}
	return "```" + out + "```" + status
}
