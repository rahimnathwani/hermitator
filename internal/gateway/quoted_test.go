package gateway

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestQuotedMedia(t *testing.T) {
	fetched := 0
	download := func(whatsmeow.DownloadableMessage, uint64) func(context.Context) ([]byte, error) {
		return func(context.Context) ([]byte, error) { fetched++; return []byte("jpeg"), nil }
	}
	q := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(4)}}
	md := quotedMedia(q, download)
	if md == nil || md.Kind != "image" || !md.Quoted || md.MIME != "image/jpeg" {
		t.Fatalf("quoted image = %+v", md)
	}
	if data, err := md.Fetch(context.Background()); err != nil || string(data) != "jpeg" || fetched != 1 {
		t.Fatalf("fetch = %q, %v", data, err)
	}
	if md := quotedMedia(&waE2E.Message{Conversation: proto.String("just text")}, download); md != nil {
		t.Fatalf("quoted text came with media: %+v", md)
	}
}
