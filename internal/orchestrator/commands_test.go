package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rahimnathwani/hermitator/internal/store"
)

// reply is msg sent as a WhatsApp reply to quoted, the way the gateway
// hands it over.
func reply(msg Message, quoted string) Message {
	msg.QuotedID, msg.QuotedText, msg.Text = "q-1", quoted, QuoteText(quoted, msg.Text)
	return msg
}

// Replying is how a phone points at a message, so a command sent as a
// reply must still be a command, not text for the agent.
func TestCommandsWorkAsReplies(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.PutSecret(ctx, e.user.ID, store.SecretClaudeToken, "tok")
	chat := "self@s.whatsapp.net"
	e.link(t, chat)

	e.m.HandleMessage(ctx, reply(e.msg(chat, "/status", true), "**echo:** earlier\nanswer"))
	if got := e.rec.wait(t, "text"); !strings.HasPrefix(got.body, "*bot*") {
		t.Fatalf("/status as a reply = %q", got.body)
	}
	if turns, _ := e.st.Turns(ctx, e.agent.ID, 1); len(turns) != 0 {
		t.Fatal("a command sent as a reply ran a turn")
	}

	// /once keeps the quote, so the agent knows what "this" is.
	e.m.HandleMessage(ctx, reply(e.msg(chat, "/once opus: explain this", true), "the earlier answer"))
	e.rec.wait(t, "text")
	e.idle(t)
	spec, _ := e.m.Spec(e.agent)
	prompts, _ := os.ReadFile(filepath.Join(spec.Dir, "home", "prompts"))
	if !strings.Contains(string(prompts), `\u003e the earlier answer\n\nexplain this`) {
		t.Fatalf("prompt = %s", prompts)
	}

	// Text that merely starts with a slash after a quote is still for the agent.
	e.m.HandleMessage(ctx, reply(e.msg(chat, "/workspace is full, see above", true), "disk report"))
	if got := e.rec.wait(t, "text"); !strings.Contains(got.body, "/workspace is full") {
		t.Fatalf("reply = %q", got.body)
	}
}
