// Package turn defines the JSON exchanged between hermitator and the
// hermitator-guest binary that runs a harness inside a sandbox.
//
// A turn request goes in on the guest's stdin. A stream of Events comes
// back on its stdout, one JSON object per line, ending with a "result" or
// an "error" event.
package turn

// Paths inside every sandbox. The guest reads overrides from the
// environment variables named alongside, which is how the unsandboxed
// development runtime points it somewhere else.
const (
	SpoolDir  = "/run/hermitator" // HERMITATOR_SPOOL
	SharedDir = "/shared"         // HERMITATOR_SHARED
	Workspace = "/workspace"      // HERMITATOR_WORKSPACE
	ToolsDir  = "/opt/hermitator"
)

// Paths relative to the shared directory.
const (
	InboxDir  = "inbox"
	OutboxDir = "outbox"
	// RequestsDir holds requests from guest commands, such as schedule,
	// for the host, and its answers to them.
	RequestsDir = "requests"
	// HoldsDir holds a file per running `hermitator-guest hold`, which
	// keeps the sandbox awake.
	HoldsDir = "holds"
	// TasksFile lists the agent's scheduled tasks, written by the host.
	TasksFile = "tasks.json"
)

// PiAuthPath is where pi keeps credentials, relative to $HOME.
const PiAuthPath = "~/.pi/agent/auth.json"

// Harness names.
const (
	Claude = "claude"
	Pi     = "pi"
)

// Request asks the guest to run one turn.
type Request struct {
	Harness   string `json:"harness"`
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt"`
	// Images are attached inline for vision. Relative paths are resolved
	// against the shared directory.
	Images []Image `json:"images,omitempty"`
	Model  string  `json:"model,omitempty"`
	// Effort is Claude Code's --effort or pi's --thinking level.
	Effort string `json:"effort,omitempty"`
	// Provider selects pi's model provider; ignored for Claude.
	Provider string `json:"provider,omitempty"`
	// Fork continues SessionID in a new session, leaving the old one as
	// it was. Claude Code only.
	Fork bool `json:"fork,omitempty"`
	// SteerID names the spool directory where messages sent during the
	// turn arrive. Claude Code only; pi's messages wait for the next turn.
	SteerID      string `json:"steer_id,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	// Env is added to the harness environment. Credentials travel here so
	// they never touch a command line.
	Env map[string]string `json:"env,omitempty"`
	// Files are written before the harness starts. Files marked Sync are
	// read back afterwards and reported in "file" events if they changed,
	// which is how refreshed OAuth tokens find their way home.
	Files   []File `json:"files,omitempty"`
	Workdir string `json:"workdir,omitempty"`
}

// Image is an attachment the harness should see inline.
type Image struct {
	Path string `json:"path"`
	MIME string `json:"mime"`
}

// File is written into the sandbox before a turn. A leading "~/" in Path
// is the guest's home directory.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    uint32 `json:"mode,omitempty"`
	Sync    bool   `json:"sync,omitempty"`
}

// Event types.
const (
	EventStarted = "started"
	EventText    = "text"
	EventTool    = "tool"
	EventFile    = "file"
	EventResult  = "result"
	EventError   = "error"
	EventUsage   = "usage"
	EventTodos   = "todos"
	// EventReply is an answer before the last: one to a message that
	// reached the harness while it was finishing its previous answer.
	EventReply = "reply"
	// EventSteered says the harness took in a message sent mid-turn.
	EventSteered = "steered"
)

// Steer is a message for a running turn, from the spool's steer directory.
type Steer struct {
	Text string `json:"text"`
}

// Event is one line of guest output.
type Event struct {
	Type      string  `json:"type"`
	Text      string  `json:"text,omitempty"`
	Tool      string  `json:"tool,omitempty"`
	Detail    string  `json:"detail,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	Model     string  `json:"model,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
	Path      string  `json:"path,omitempty"`
	Content   string  `json:"content,omitempty"`
	IsError   bool    `json:"is_error,omitempty"`
	// Windows are subscription usage windows, in "usage" events.
	Windows []UsageWindow `json:"windows,omitempty"`
	// Todos is the agent's task list, in "todos" events.
	Todos []Todo `json:"todos,omitempty"`
}

// UsageWindow is how much of a subscription's allowance for a period is
// used, as the provider reports it.
type UsageWindow struct {
	Name        string  `json:"name"` // five_hour, seven_day, ...
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resets_at"` // Unix seconds
}

// Todo is one item of the agent's own task list.
type Todo struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending, in_progress, completed
}
