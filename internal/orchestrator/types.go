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
	"time"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

// Message is an incoming chat message, as handed over by the gateway.
type Message struct {
	AccountID  string
	Chat       string
	ChatName   string
	IsGroup    bool
	Sender     string
	SenderName string
	ID         string
	FromMe     bool
	Mentioned  bool
	Text       string
	Time       time.Time
	Media      *Media
}

// Media is an attachment. It is only downloaded if the chat is routed.
type Media struct {
	Kind     string // image, audio, video, document, sticker
	MIME     string
	FileName string
	Size     int64
	Voice    bool
	Fetch    func(ctx context.Context) ([]byte, error)
}

// Messenger sends things back to a chat.
type Messenger interface {
	SendText(ctx context.Context, accountID, chat, text string) error
	SendFile(ctx context.Context, accountID, chat, path, caption string) error
	React(ctx context.Context, accountID, chat, sender, messageID, emoji string) error
	Typing(ctx context.Context, accountID, chat string, typing bool) error
}

// Transcriber turns a voice note into text.
type Transcriber interface {
	Available() bool
	Transcribe(ctx context.Context, path string) (string, error)
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
