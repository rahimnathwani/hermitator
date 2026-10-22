package gateway

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/rahimnathwani/hermitator/internal/orchestrator"
	hstore "github.com/rahimnathwani/hermitator/internal/store"
)

func textEvent(id, text string, at time.Time) *events.Message {
	chat := types.NewJID("15551234567", types.DefaultUserServer)
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: chat},
			ID:            id, PushName: "Sam", Timestamp: at,
		},
		Message: &waE2E.Message{Conversation: proto.String(text)},
	}
}

func TestStoredEventsRoundTrip(t *testing.T) {
	e := textEvent("ABC123", "hello", time.Now().Truncate(time.Second))
	e.Message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("look"), Mimetype: proto.String("image/jpeg"), MediaKey: []byte{1, 2, 3}}}
	raw, err := encodeEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	back, err := decodeEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Info.ID != "ABC123" || back.Info.Chat != e.Info.Chat || !back.Info.Timestamp.Equal(e.Info.Timestamp) || back.Info.PushName != "Sam" {
		t.Fatalf("info = %+v", back.Info)
	}
	if back.Message.GetImageMessage().GetCaption() != "look" || string(back.Message.GetImageMessage().GetMediaKey()) != "\x01\x02\x03" {
		t.Fatalf("message = %v", back.Message)
	}
}

// After a restart, messages no turn took are handed over again, however
// old, and end up done.
func TestReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	st, err := hstore.Open(ctx, filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	g, err := Open(ctx, filepath.Join(dir, "wa.db"), st, log)
	if err != nil {
		t.Fatal(err)
	}
	g.ctx = ctx
	got := make(chan orchestrator.Message, 4)
	g.SetHandler(func(_ context.Context, msg orchestrator.Message) bool { got <- msg; return false })
	// A device as Start finds it: saved, then loaded with its stores.
	device := g.container.NewDevice()
	jid := types.NewADJID("15550000000", 0, 1)
	device.ID = &jid
	device.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := device.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if device, err = g.container.GetDevice(ctx, jid); err != nil {
		t.Fatal(err)
	}
	client := whatsmeow.NewClient(device, nil)
	g.attach("acct", client)
	a := g.accounts["acct"]

	for _, e := range []*events.Message{textEvent("FRESH", "still wanted", time.Now().Add(-time.Minute)), textEvent("OLD", "from before the outage", time.Now().Add(-time.Hour))} {
		raw, _ := encodeEvent(e)
		st.Receive(ctx, "acct", e.Info.ID, raw)
	}
	g.replay(a)
	for _, want := range []string{"FRESH", "OLD"} {
		select {
		case msg := <-got:
			if msg.ID != want || msg.SenderName != "Sam" {
				t.Fatalf("replayed %+v, want %s", msg, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was not handed over", want)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		pending, _ := st.PendingInbound(ctx, "acct")
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still pending: %d", len(pending))
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case msg := <-got:
		t.Fatalf("handed over twice: %+v", msg)
	default:
	}
}
