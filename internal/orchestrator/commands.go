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
	"history":  "history",
	"preview":  "preview",
	"previews": "previews",
	"plan":     "plan",
	"todo":     "plan",
	"sessions": "sessions",
	"resume":   "resume",
	"fork":     "fork",
	"timezone": "timezone",
	"tz":       "timezone",
	"sh":       "sh",
	"compact":  "compact",
}

// harnessCommands are slash commands Claude Code and pi take at their own
// prompt. From the chat they reach the harness as plain text (stream-json
// and RPC input take no slash commands), so a message made of one would
// cost a turn and do nothing.
var harnessCommands = map[string]bool{
	"add-dir": true, "agents": true, "bug": true, "config": true, "context": true, "cost": true, "doctor": true,
	"export": true, "hooks": true, "init": true, "login": true, "logout": true, "mcp": true, "memory": true,
	"permissions": true, "pr-comments": true, "release-notes": true, "review": true, "vim": true,
}

// ParseCommand recognises hermitator's chat commands. Anything else goes
// to the agent untouched, unless NotACommand has something to say about
// it first.
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

// NotACommand explains a slash word hermitator does not know, when it is
// plainly meant as one: a message that is nothing but /word, or one that
// opens with a command of the harness's own. It reports whether it has
// anything to say; a path such as "/workspace is full" is left alone.
func NotACommand(text, harness string) (string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	word, _, _ := strings.Cut(text[1:], " ")
	lower := strings.ToLower(word)
	if word == "" || strings.ContainsAny(word, "/.") || commandAliases[lower] != "" {
		return "", false
	}
	switch {
	case lower == "cost" || lower == "context":
		return "/" + word + " is " + harness + "'s own command and does not work from the chat. /usage shows spending and allowances; /status shows the conversation's size.", true
	case harnessCommands[lower]:
		return "/" + word + " is " + harness + "'s own command and does not work from the chat, where messages reach it as text. Use the Terminal tab on the agent's page in hermitator, or ask the agent in plain words.", true
	case strings.TrimSpace(text[1:]) == word:
		return "There is no /" + word + " command; /help lists them. To send it to the agent as text, start the message with something else.", true
	}
	return "", false
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
/compact: summarise this conversation now, so the next messages re-read less
/sessions: past conversations; /resume N goes back to one
/fork: branch the next message off this conversation
/plan: the agent's task list for what it is doing
/usage: your Claude subscription use and spending
/history on|off: keep this chat's messages in a file the agent can search
/preview PORT: a link to a web server the agent runs; /previews lists them, /previews off turns them off
/model [NAME]: show or change the model (/model default to reset)
/effort [LEVEL]: show or change how hard it thinks
/once [MODEL] [EFFORT]: MESSAGE: use them for one message only
/switch claude|pi: change harness
/listen: show or change which messages wake the agent here
/unlink: disconnect this chat from the agent
/link CODE: connect this chat to an agent
/help: this message

Anything else goes to the agent.`
