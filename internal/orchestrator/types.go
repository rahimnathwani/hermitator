// Package orchestrator connects chats to agents: it routes incoming
// messages, wakes sandboxes, runs turns, delivers replies and puts idle
// sandboxes back to sleep.
//
// Each agent is served by one goroutine (an actor) that owns its state:
// the queue of messages waiting for a turn, whether a turn is running, and
// what is holding the sandbox awake. Everything that changes an agent goes
// through its actor, so there is no locking around agent state.
package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/store"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// Message is an incoming chat message, as handed over by the gateway.
type Message struct {
	AccountID string
	Chat      string
	ChatName  string
	IsGroup   bool
	Sender    string
	// SenderPhone is the sender's number in international format without
	// the plus, when WhatsApp reveals it.
	SenderPhone string
	SenderName  string
	ID          string
	FromMe      bool
	// Owner is set by the orchestrator: the message came from the account
	// itself or from one of its owner's numbers.
	Owner bool
	// Mentioned means the message @mentions the account or replies to it.
	Mentioned bool
	// Queued is set by the actor when the message had to wait for a turn
	// in progress, so its answer will come a while after it.
	Queued bool
	// ReactsTo and Reaction say this is a reaction (an emoji, or nothing
	// when one was taken back) to the message with that ID.
	ReactsTo string
	Reaction string
	// Edits says this is the new text of the message with that ID.
	Edits string
	// GroupSize is the number of participants, for groups.
	GroupSize int
	// Text is what the agent reads: for a reply, the quoted message comes
	// first (see QuoteText).
	Text string
	// QuotedID and QuotedText are the message this one replies to.
	QuotedID   string
	QuotedText string
	Time       time.Time
	Media      *Media
	// Model and Effort override the agent's settings for this message's
	// turn; /once sets them.
	Model  string
	Effort string
	// Prompt, when set, is sent as it is instead of Text: /retry replays
	// a stored prompt this way.
	Prompt string
	// TaskName is set when a scheduled task sent the message; the
	// agent's reply is headed with it.
	TaskName string
	// TaskID is the task's, so a run is not queued twice.
	TaskID string
	// JobName is set when a background job the agent started with
	// hermitator-guest notify reports back; the reply is headed with it.
	JobName string
	// ThreadAccount and ThreadChat, on a message typed in the web console,
	// name the chat whose conversation it continues. The reply is shown
	// only in the console. Empty, it continues the console's own.
	ThreadAccount string
	ThreadChat    string
}

// QuoteText is text as the agent reads a reply to quoted: the quote first,
// as in an email.
func QuoteText(quoted, text string) string {
	return "> " + strings.ReplaceAll(quoted, "\n", "\n> ") + "\n\n" + text
}

// typed is what the sender wrote, without the message it replies to.
func (m Message) typed() string {
	if m.QuotedText == "" {
		return m.Text
	}
	return strings.TrimPrefix(m.Text, QuoteText(m.QuotedText, ""))
}

// thread is the conversation a message belongs to: its chat's, or for a
// console message, the one picked in the console.
func (m Message) thread(agentID string) store.ThreadKey {
	if m.AccountID == "" {
		return store.ThreadKey{AgentID: agentID, AccountID: m.ThreadAccount, ChatJID: m.ThreadChat}
	}
	return store.ThreadKey{AgentID: agentID, AccountID: m.AccountID, ChatJID: m.Chat}
}

// Media is an attachment. It is only downloaded if the chat is routed.
type Media struct {
	Kind     string // image, audio, video, document, sticker
	MIME     string
	FileName string
	Size     int64
	Voice    bool
	// Quoted means it came with the message being replied to, not this one.
	Quoted bool
	Fetch  func(ctx context.Context) ([]byte, error)
}

// Messenger sends things back to a chat.
type Messenger interface {
	// SendText sends text and returns the message's ID.
	SendText(ctx context.Context, accountID, chat, text string) (string, error)
	// SendReply sends text quoting an earlier message of the chat.
	SendReply(ctx context.Context, accountID, chat, text string, q Quote) (string, error)
	// SendNote sends text and returns the message's ID, for Edit.
	SendNote(ctx context.Context, accountID, chat, text string) (string, error)
	// Edit replaces the text of a message this account sent recently.
	Edit(ctx context.Context, accountID, chat, messageID, text string) error
	SendFile(ctx context.Context, accountID, chat, name string, data []byte, caption string) error
	// SendPoll sends a single-choice poll; votes come back as messages.
	SendPoll(ctx context.Context, accountID, chat, question string, options []string) error
	React(ctx context.Context, accountID, chat, sender, messageID, emoji string) error
	Typing(ctx context.Context, accountID, chat string, typing bool) error
	// MarkRead sends a read receipt, where the account wants them.
	MarkRead(ctx context.Context, accountID, chat, sender, messageID string) error
}

// Quote names a message a reply attaches to, with its text for the
// preview WhatsApp shows above the reply.
type Quote struct {
	MessageID string
	Sender    string
	Text      string
}

// Speaker turns text into a voice note: Opus audio in an Ogg container.
type Speaker interface {
	Available() bool
	Speak(ctx context.Context, text string) ([]byte, error)
}

// Transcriber turns a voice note into text.
type Transcriber interface {
	Available() bool
	// Transcribe turns audio into text; prompt, which may be empty, lists
	// words to get right.
	Transcribe(ctx context.Context, path, prompt string) (string, error)
}

// UIEvent is published to web clients watching an agent.
type UIEvent struct {
	Kind   string      `json:"kind"` // state, turn_start, event, turn_end
	State  string      `json:"state,omitempty"`
	TurnID string      `json:"turn_id,omitempty"`
	Input  string      `json:"input,omitempty"`
	Event  *turn.Event `json:"event,omitempty"`
	Status string      `json:"status,omitempty"`
	Output string      `json:"output,omitempty"`
}
