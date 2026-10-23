package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/spool"
)

// maxShellOutput is how much of a /sh command's output is sent back.
const maxShellOutput = 3000

// shell runs one command in the agent's sandbox and replies with its
// output, without involving the model: quick checks cost nothing and
// still work when the model is rate-limited or broken. Commands are
// owner-only, like every chat command.
func (m *Manager) shell(agentID, command string, reply func(string)) {
	command = strings.TrimSpace(command)
	if command == "" {
		reply("Usage: /sh COMMAND, for example /sh git -C /workspace log --oneline -5")
		return
	}
	argv := []string{"sh", "-lc", "cd /workspace 2>/dev/null; exec 2>&1; " + command}
	m.sandboxJob(agentID, "sh", argv, nil, reply, func(r jobResult) {
		reply(formatShell(string(r.out), r.exit))
	})
}

// jobResult is how a chat command's spool job went.
type jobResult struct {
	out  []byte
	exit spool.Exit
	spec *sandbox.Spec
}

// sandboxJob wakes the agent's sandbox and runs one spool job there, with
// no credentials in its environment, then calls done while the sandbox
// is still held awake. When the job cannot run it replies itself and done
// is not called.
func (m *Manager) sandboxJob(agentID, kind string, argv []string, stdin []byte, reply func(string), done func(jobResult)) {
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
	out, exit, err := spec.Spool().Run(ctx, spool.Job{ID: kind + "-" + ids.New(), Argv: argv, Stdin: stdin})
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reply(fmt.Sprintf("⚠️ Stopped after %s. For longer jobs, ask the agent to run them in the background.", m.cfg.ShellTimeout))
		return
	case err != nil:
		reply("⚠️ Could not run it: " + err.Error())
		return
	}
	done(jobResult{out, exit, spec})
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
