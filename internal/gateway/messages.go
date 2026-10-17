package gateway

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/rahimnathwani/hermitator/internal/orchestrator"
)

// maxDownload caps attachments hermitator will fetch for an agent.
const maxDownload = 100 << 20

// convert turns a whatsmeow message into an orchestrator.Message.
func (g *Gateway) convert(ctx context.Context, a *account, e *events.Message) (orchestrator.Message, bool) {
	info := e.Info
	switch {
	case e.Message == nil, e.IsEdit:
		return orchestrator.Message{}, false
	case info.Chat.Server == types.BroadcastServer, info.Chat.Server == types.NewsletterServer:
		return orchestrator.Message{}, false // status updates and channels
	case g.sent.has(info.ID):
		return orchestrator.Message{}, false
	case !info.Timestamp.IsZero() && time.Since(info.Timestamp) > staleAfter:
		return orchestrator.Message{}, false
	}
	m := e.Message
	cli := a.client
	msg := orchestrator.Message{
		AccountID:   a.id,
		Chat:        canonicalChat(ctx, cli, info).String(),
		IsGroup:     info.IsGroup,
		Sender:      info.Sender.String(),
		SenderPhone: phoneOf(ctx, cli, info.Sender, info.SenderAlt),
		SenderName:  info.PushName,
		ID:          info.ID,
		FromMe:      info.IsFromMe,
		Time:        info.Timestamp,
	}
	var ctxInfo *waE2E.ContextInfo
	download := func(d whatsmeow.DownloadableMessage, size uint64) func(context.Context) ([]byte, error) {
		return func(ctx context.Context) ([]byte, error) {
			if size > maxDownload {
				return nil, fmt.Errorf("it is %d MB; hermitator fetches at most %d MB", size>>20, maxDownload>>20)
			}
			return cli.Download(ctx, d)
		}
	}
	switch {
	case m.GetConversation() != "":
		msg.Text = m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		msg.Text = m.GetExtendedTextMessage().GetText()
		ctxInfo = m.GetExtendedTextMessage().GetContextInfo()
	case m.GetImageMessage() != nil:
		im := m.GetImageMessage()
		msg.Text, ctxInfo = im.GetCaption(), im.GetContextInfo()
		msg.Media = &orchestrator.Media{Kind: "image", MIME: im.GetMimetype(), Size: int64(im.GetFileLength()), Fetch: download(im, im.GetFileLength())}
	case m.GetAudioMessage() != nil:
		au := m.GetAudioMessage()
		ctxInfo = au.GetContextInfo()
		msg.Media = &orchestrator.Media{Kind: "audio", MIME: au.GetMimetype(), Size: int64(au.GetFileLength()), Voice: au.GetPTT(), Fetch: download(au, au.GetFileLength())}
	case m.GetVideoMessage() != nil:
		v := m.GetVideoMessage()
		msg.Text, ctxInfo = v.GetCaption(), v.GetContextInfo()
		msg.Media = &orchestrator.Media{Kind: "video", MIME: v.GetMimetype(), Size: int64(v.GetFileLength()), Fetch: download(v, v.GetFileLength())}
	case m.GetDocumentMessage() != nil:
		d := m.GetDocumentMessage()
		msg.Text, ctxInfo = d.GetCaption(), d.GetContextInfo()
		msg.Media = &orchestrator.Media{Kind: "document", MIME: d.GetMimetype(), FileName: d.GetFileName(), Size: int64(d.GetFileLength()), Fetch: download(d, d.GetFileLength())}
	default:
		return orchestrator.Message{}, false // reactions, stickers, polls, protocol messages
	}
	if ctxInfo != nil {
		own := []string{}
		if cli.Store.ID != nil {
			own = append(own, cli.Store.ID.ToNonAD().String())
		}
		if !cli.Store.LID.IsEmpty() {
			own = append(own, cli.Store.LID.ToNonAD().String())
		}
		for _, j := range ctxInfo.GetMentionedJID() {
			if slices.Contains(own, j) {
				msg.Mentioned = true
			}
		}
		if q := ctxInfo.GetQuotedMessage(); q != nil {
			if quoted := quotedText(q); quoted != "" {
				msg.Text = "> " + strings.ReplaceAll(quoted, "\n", "\n> ") + "\n\n" + msg.Text
			}
		}
	}
	msg.ChatName = g.chatName(ctx, cli, info)
	return msg, true
}

// phoneOf returns the phone number behind a user JID, which WhatsApp may
// give as a hidden "LID" instead, with the phone number as its alternate.
func phoneOf(ctx context.Context, cli *whatsmeow.Client, jid, alt types.JID) string {
	switch {
	case jid.Server == types.DefaultUserServer:
		return jid.User
	case alt.Server == types.DefaultUserServer:
		return alt.User
	case jid.Server == types.HiddenUserServer:
		if pn, err := cli.Store.LIDs.GetPNForLID(ctx, jid.ToNonAD()); err == nil && !pn.IsEmpty() {
			return pn.User
		}
	}
	return ""
}

// canonicalChat names a direct chat by phone number when it can. WhatsApp
// addresses the same person sometimes by number and sometimes by LID, and
// a route has to keep matching either way.
func canonicalChat(ctx context.Context, cli *whatsmeow.Client, info types.MessageInfo) types.JID {
	chat := info.Chat.ToNonAD()
	if info.IsGroup || chat.Server != types.HiddenUserServer {
		return chat
	}
	alt := info.SenderAlt
	if info.IsFromMe {
		alt = info.RecipientAlt
	}
	if alt.Server == types.DefaultUserServer {
		return alt.ToNonAD()
	}
	if pn, err := cli.Store.LIDs.GetPNForLID(ctx, chat); err == nil && !pn.IsEmpty() {
		return pn.ToNonAD()
	}
	return chat
}

func quotedText(q *waE2E.Message) string {
	switch {
	case q.GetConversation() != "":
		return q.GetConversation()
	case q.GetExtendedTextMessage() != nil:
		return q.GetExtendedTextMessage().GetText()
	case q.GetImageMessage() != nil:
		return "[image] " + q.GetImageMessage().GetCaption()
	case q.GetDocumentMessage() != nil:
		return "[file " + q.GetDocumentMessage().GetFileName() + "]"
	}
	return ""
}

// nameCache keeps group names so every message does not cost a request.
type nameCache struct {
	mu    sync.Mutex
	names map[string]cachedName
}

type cachedName struct {
	name string
	at   time.Time
}

func (g *Gateway) chatName(ctx context.Context, cli *whatsmeow.Client, info types.MessageInfo) string {
	key := info.Chat.String()
	g.names.mu.Lock()
	if c, ok := g.names.names[key]; ok && time.Since(c.at) < time.Hour {
		g.names.mu.Unlock()
		return c.name
	}
	g.names.mu.Unlock()

	var name string
	switch {
	case info.IsGroup:
		if gi, err := cli.GetGroupInfo(ctx, info.Chat); err == nil {
			name = gi.Name
		}
	case cli.Store.ID != nil && (info.Chat.User == cli.Store.ID.User || info.Chat.User == cli.Store.LID.User):
		name = "Message yourself"
	default:
		if c, err := cli.Store.Contacts.GetContact(ctx, info.Chat); err == nil && c.Found {
			name = firstNonEmpty(c.FullName, c.PushName, c.BusinessName)
		}
		if name == "" && !info.IsFromMe {
			name = info.PushName
		}
	}
	g.names.mu.Lock()
	if g.names.names == nil {
		g.names.names = map[string]cachedName{}
	}
	g.names.names[key] = cachedName{name, time.Now()}
	g.names.mu.Unlock()
	return name
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// SendText implements orchestrator.Messenger.
func (g *Gateway) SendText(ctx context.Context, accountID, chat, text string) error {
	cli, jid, err := g.target(accountID, chat)
	if err != nil {
		return err
	}
	resp, err := cli.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(text)})
	if err == nil {
		g.sent.add(resp.ID)
	}
	return err
}

// SendFile implements orchestrator.Messenger. Photos go as images and
// everything else as a document, which WhatsApp delivers unrecompressed.
func (g *Gateway) SendFile(ctx context.Context, accountID, chat, name string, data []byte, caption string) error {
	cli, jid, err := g.target(accountID, chat)
	if err != nil {
		return err
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	base, _, _ := strings.Cut(mimeType, ";")

	var msg *waE2E.Message
	switch base {
	case "image/jpeg", "image/png", "image/webp":
		up, err := cli.Upload(ctx, data, whatsmeow.MediaImage)
		if err != nil {
			return err
		}
		msg = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Caption: proto.String(caption), Mimetype: proto.String(base),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
		}}
	case "video/mp4":
		up, err := cli.Upload(ctx, data, whatsmeow.MediaVideo)
		if err != nil {
			return err
		}
		msg = &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			Caption: proto.String(caption), Mimetype: proto.String(base),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
		}}
	default:
		up, err := cli.Upload(ctx, data, whatsmeow.MediaDocument)
		if err != nil {
			return err
		}
		msg = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			Caption: proto.String(caption), Mimetype: proto.String(mimeType),
			FileName: proto.String(name), Title: proto.String(name),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
		}}
	}
	resp, err := cli.SendMessage(ctx, jid, msg)
	if err == nil {
		g.sent.add(resp.ID)
	}
	return err
}

// React implements orchestrator.Messenger.
func (g *Gateway) React(ctx context.Context, accountID, chat, sender, messageID, emoji string) error {
	cli, jid, err := g.target(accountID, chat)
	if err != nil {
		return err
	}
	senderJID, _ := types.ParseJID(sender)
	resp, err := cli.SendMessage(ctx, jid, cli.BuildReaction(jid, senderJID, messageID, emoji))
	if err == nil {
		g.sent.add(resp.ID)
	}
	return err
}

// MarkRead implements orchestrator.Messenger. Only agent numbers send read
// receipts; on the owner's own account they would mark messages as read
// before the owner had seen them.
func (g *Gateway) MarkRead(ctx context.Context, accountID, chat, sender, messageID string) error {
	acct, err := g.store.Account(ctx, accountID)
	if err != nil || acct.Personal() {
		return err
	}
	cli, jid, err := g.target(accountID, chat)
	if err != nil {
		return err
	}
	senderJID, _ := types.ParseJID(sender)
	return cli.MarkRead(ctx, []types.MessageID{messageID}, time.Now(), jid, senderJID)
}

// Typing implements orchestrator.Messenger.
func (g *Gateway) Typing(ctx context.Context, accountID, chat string, typing bool) error {
	cli, jid, err := g.target(accountID, chat)
	if err != nil {
		return err
	}
	state := types.ChatPresencePaused
	if typing {
		state = types.ChatPresenceComposing
	}
	return cli.SendChatPresence(ctx, jid, state, types.ChatPresenceMediaText)
}

func (g *Gateway) target(accountID, chat string) (*whatsmeow.Client, types.JID, error) {
	cli, err := g.client(accountID)
	if err != nil {
		return nil, types.JID{}, err
	}
	jid, err := types.ParseJID(chat)
	if err != nil {
		return nil, types.JID{}, err
	}
	return cli, jid, nil
}

var _ orchestrator.Messenger = (*Gateway)(nil)
