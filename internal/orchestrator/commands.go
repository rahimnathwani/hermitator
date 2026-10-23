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
	"in":       "in",
	"at":       "at",
	"every":    "every",
	"get":      "get",
	"git":      "git",
	"md":       "md",
	"render":   "md",
	"img":      "md",
	"remember": "remember",
	"awake":    "awake",
	"agents":   "agents",
}

// commandHelp is /help, or with a command named, the lines of it that
// mention that command.
func commandHelp(arg string) string {
	name, _, _ := strings.Cut(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(arg), "/")), " ")
	if name == "" {
		return helpText
	}
	canon := commandAliases[name]
	if canon == "" {
		return "There is no /" + name + " command; /help lists them."
	}
	var lines []string
	for _, line := range strings.Split(helpText, "\n") {
		if strings.Contains(line, "/"+canon+" ") || strings.Contains(line, "/"+canon+":") || strings.Contains(line, "/"+name+")") ||
			strings.Contains(line, "/"+name+" ") || strings.Contains(line, "/"+name+":") {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return helpText
	}
	return strings.Join(lines, "\n")
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

*The agent*
/status: what it is doing
/stop: abort the running turn; /stop all also drops the messages waiting
/retry: run the last message again, after a failure or a restart
/plan: its task list for what it is doing
/usage: your Claude subscription use and spending
/sleep: suspend the sandbox now (alias /exit)
/awake 2h: keep the sandbox from sleeping, for a background job (/awake off)

*Without the model*
/sh COMMAND: run a shell command in the sandbox and see its output
/get PATH: send a file (or a folder, zipped) from the sandbox to this chat
/git [PATH]: uncommitted and unpushed work in the workspace's repositories
/md: the last reply again as a picture, for tables and wide output; reply to an older one with /md, or react 🖼 to it, to pick it (/md pdf, /md file)
/remember FACT: add a line to the agent's memory file, read at the start of every conversation
/preview PORT: a link to a web server the agent runs; /previews lists them, /previews off turns them off

*Later*
/in 2h MESSAGE, /at 9am MESSAGE, /every CRON: MESSAGE: schedule a message to the agent
/tasks: scheduled tasks (/tasks pause|resume|run|delete N)
/timezone [ZONE]: show or set the time zone schedules use

*Conversation*
/new: start a fresh conversation, keeping all files (alias /clear)
/compact: summarise this conversation now, so the next messages re-read less
/sessions: past conversations; /resume N goes back to one
/fork: branch the next message off this conversation
/history on|off: keep this chat's messages in a file the agent can search

*Model*
/model [NAME]: show or change the model (/model default to reset)
/effort [LEVEL]: show or change how hard it thinks
/once [MODEL] [EFFORT]: MESSAGE: use them for one message only
/switch claude|pi: change harness

*This chat*
/listen: show or change which messages wake the agent here
/link CODE or /link NAME: connect this chat to an agent; /agents lists yours
/unlink: disconnect this chat from the agent
/help [COMMAND]: this list, or the lines about one command

Anything else goes to the agent; commands work as replies too.`
