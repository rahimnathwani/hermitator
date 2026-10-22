package gateway

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waCommon"
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

	// A shared location becomes text the agent can use; a contact card
	// becomes a note and a .vcf in its inbox.
	pin := textEvent("PIN", "", time.Now())
	pin.Message = &waE2E.Message{LocationMessage: &waE2E.LocationMessage{DegreesLatitude: proto.Float64(51.507351), DegreesLongitude: proto.Float64(-0.127758), Name: proto.String("Trafalgar Square"), Comment: proto.String("meet here?")}}
	card := textEvent("CARD", "", time.Now())
	card.Message = &waE2E.Message{ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("Ada Lovelace"), Vcard: proto.String("BEGIN:VCARD\nFN:Ada Lovelace\nEND:VCARD")}}
	for _, e := range []*events.Message{pin, card} {
		raw, _ := encodeEvent(e)
		st.Receive(ctx, "acct", e.Info.ID, raw)
	}
	g.replay(a)
	msg := <-got
	if msg.Text != "[Location: 51.50735, -0.12776 (Trafalgar Square) https://maps.google.com/?q=51.50735,-0.12776] meet here?" {
		t.Fatalf("pin = %q", msg.Text)
	}
	msg = <-got
	data, _ := msg.Media.Fetch(ctx)
	if msg.Text != "[Contact card: Ada Lovelace]" || msg.Media.FileName != "Ada Lovelace.vcf" || msg.Media.MIME != "text/vcard" || !strings.Contains(string(data), "FN:Ada") {
		t.Fatalf("card = %+v", msg)
	}

	// A reaction and an edit name the message they are about, and an
	// edit keeps being one through storage.
	react := textEvent("REACT", "", time.Now())
	react.Message = &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("ORIG")}, Text: proto.String("❌")}}
	edit := textEvent("EDIT", "", time.Now())
	edit.IsEdit = true
	edit.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key: &waCommon.MessageKey{ID: proto.String("ORIG")}, EditedMessage: &waE2E.Message{Conversation: proto.String("deploy to staging, not prod")}}}
	for _, e := range []*events.Message{react, edit} {
		raw, _ := encodeEvent(e)
		st.Receive(ctx, "acct", e.Info.ID, raw)
	}
	g.replay(a)
	if msg = <-got; msg.ReactsTo != "ORIG" || msg.Reaction != "❌" || msg.Text != "" {
		t.Fatalf("reaction = %+v", msg)
	}
	if msg = <-got; msg.Edits != "ORIG" || msg.Text != "deploy to staging, not prod" || msg.ID != "EDIT" {
		t.Fatalf("edit = %+v", msg)
	}
}
