package orchestrator

import "strings"

// Command is a chat command handled by hermitator itself; it never reaches
// the agent.
type Command struct {
	Name string
	Arg  string
}

var commandAliases = map[string]string{
	"link":     "link",
	"unlink":   "unlink",
	"sleep":    "sleep",
	"exit":     "sleep",
	"stop":     "stop",
	"new":      "new",
	"clear":    "new",
	"reset":    "new",
	"status":   "status",
	"listen":   "listen",
	"model":    "model",
	"effort":   "effort",
	"once":     "once",
	"switch":   "switch",
	"help":     "help",
	"retry":    "retry",
	"tasks":    "tasks",
	"usage":    "usage",
	"plan":     "plan",
	"todo":     "plan",
	"sessions": "sessions",
	"resume":   "resume",
	"fork":     "fork",
	"timezone": "timezone",
	"tz":       "timezone",
	"sh":       "sh",
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
/retry: run the last message again, after a failure or a restart
/sh COMMAND: run a shell command in the sandbox and see its output (no model)
/tasks: scheduled tasks (/tasks pause|resume|delete N)
/timezone [ZONE]: show or set the time zone schedules use
/new: start a fresh conversation, keeping all files (alias /clear)
/sessions: past conversations; /resume N goes back to one
/fork: branch the next message off this conversation
/plan: the agent's task list for what it is doing
/usage: your Claude subscription use and spending
/model [NAME]: show or change the model (/model default to reset)
/effort [LEVEL]: show or change how hard it thinks
/once [MODEL] [EFFORT]: MESSAGE: use them for one message only
/switch claude|pi: change harness
/listen: show or change which messages wake the agent here
/unlink: disconnect this chat from the agent
/link CODE: connect this chat to an agent
/help: this message

Anything else goes to the agent.`
