package orchestrator

import "strings"

// Command is a chat command handled by hermitator itself; it never reaches
// the agent.
type Command struct {
	Name string
	Arg  string
}

var commandAliases = map[string]string{
	"link":   "link",
	"unlink": "unlink",
	"sleep":  "sleep",
	"exit":   "sleep",
	"stop":   "stop",
	"new":    "new",
	"clear":  "new",
	"reset":  "new",
	"status": "status",
	"switch": "switch",
	"help":   "help",
}

// ParseCommand recognises hermitator's chat commands. Anything else that
// starts with a slash (such as Claude Code's own /compact) goes to the
// agent untouched.
func ParseCommand(text string) (Command, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return Command{}, false
	}
	name, arg, _ := strings.Cut(text[1:], " ")
	canon, ok := commandAliases[strings.ToLower(name)]
	if !ok {
		return Command{}, false
	}
	return Command{Name: canon, Arg: strings.TrimSpace(arg)}, true
}

const helpText = `*hermitator commands*
/status: what this agent is doing
/sleep: suspend the sandbox now (alias /exit)
/stop: abort the running turn
/new: start a fresh conversation, keeping all files (alias /clear)
/switch claude|pi: change harness
/unlink: disconnect this chat from the agent
/link CODE: connect this chat to an agent
/help: this message

Anything else goes to the agent.`
