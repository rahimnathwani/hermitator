package gateway

import (
	"bytes"
	"context"
	"fmt"
	"github.com/rahimnathwani/hermitator/internal/store"
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
	case m.GetLocationMessage() != nil:
		l := m.GetLocationMessage()
		msg.Text, ctxInfo = locationText("Location", l.GetDegreesLatitude(), l.GetDegreesLongitude(), l.GetName(), l.GetAddress(), l.GetComment()), l.GetContextInfo()
	case m.GetLiveLocationMessage() != nil:
		l := m.GetLiveLocationMessage()
		msg.Text, ctxInfo = locationText("Live location", l.GetDegreesLatitude(), l.GetDegreesLongitude(), "", "", l.GetCaption()), l.GetContextInfo()
	case m.GetContactMessage() != nil:
		c := m.GetContactMessage()
		msg.Text, ctxInfo = "[Contact card: "+c.GetDisplayName()+"]", c.GetContextInfo()
		msg.Media = vcardMedia(c.GetDisplayName(), c.GetVcard())
	case m.GetContactsArrayMessage() != nil:
		cs := m.GetContactsArrayMessage()
		var names, cards []string
		for _, c := range cs.GetContacts() {
			names = append(names, c.GetDisplayName())
			cards = append(cards, strings.TrimSpace(c.GetVcard()))
		}
		msg.Text, ctxInfo = "[Contact cards: "+strings.Join(names, ", ")+"]", cs.GetContextInfo()
		msg.Media = vcardMedia(cs.GetDisplayName(), strings.Join(cards, "\n"))
	case m.GetPollUpdateMessage() != nil:
		text, ok := g.pollAnswer(ctx, cli, e)
		if !ok {
			return orchestrator.Message{}, false
		}
		// A vote on the agent's poll is addressed to the agent.
		msg.Text, msg.Mentioned = text, true
	default:
		return orchestrator.Message{}, false // reactions, stickers, protocol messages
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
		// Replying to one of the account's own messages addresses it too.
		if p, err := types.ParseJID(ctxInfo.GetParticipant()); err == nil && slices.Contains(own, p.ToNonAD().String()) {
			msg.Mentioned = true
		}
		if q := ctxInfo.GetQuotedMessage(); q != nil {
			if quoted := quotedText(q); quoted != "" {
				msg.Text = "> " + strings.ReplaceAll(quoted, "\n", "\n> ") + "\n\n" + msg.Text
			}
			// "What's in this?" in reply to a photo means the photo: bring it
			// along, from WhatsApp's servers, as if it came with this message.
			if msg.Media == nil {
				msg.Media = quotedMedia(q, download)
			}
		}
	}
	msg.ChatName, msg.GroupSize = g.chatName(ctx, cli, info)
	return msg, true
}

// locationText describes a shared location: coordinates the agent can
// use, the place's name and address when WhatsApp sent them, and a map
// link, so "find a pharmacy near here" works from a pin.
func locationText(kind string, lat, lon float64, name, address, comment string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s: %.5f, %.5f", kind, lat, lon)
	if place := strings.TrimSpace(strings.Join(compact(name, address), ", ")); place != "" {
		b.WriteString(" (" + place + ")")
	}
	fmt.Fprintf(&b, " https://maps.google.com/?q=%.5f,%.5f]", lat, lon)
	if comment = strings.TrimSpace(comment); comment != "" {
		b.WriteString(" " + comment)
	}
	return b.String()
}

func compact(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// vcardMedia is a shared contact as a .vcf file for the agent's inbox.
func vcardMedia(name, vcard string) *orchestrator.Media {
	if strings.TrimSpace(vcard) == "" {
		return nil
	}
	if name = strings.TrimSpace(name); name == "" {
		name = "contact"
	}
	data := []byte(vcard)
	return &orchestrator.Media{Kind: "document", MIME: "text/vcard", FileName: name + ".vcf", Size: int64(len(data)),
		Fetch: func(context.Context) ([]byte, error) { return data, nil }}
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

// quotedMedia is the attachment of a quoted message, or nil.
func quotedMedia(q *waE2E.Message, download func(whatsmeow.DownloadableMessage, uint64) func(context.Context) ([]byte, error)) *orchestrator.Media {
	var md *orchestrator.Media
	switch {
	case q.GetImageMessage() != nil:
		im := q.GetImageMessage()
		md = &orchestrator.Media{Kind: "image", MIME: im.GetMimetype(), Size: int64(im.GetFileLength()), Fetch: download(im, im.GetFileLength())}
	case q.GetAudioMessage() != nil:
		au := q.GetAudioMessage()
		md = &orchestrator.Media{Kind: "audio", MIME: au.GetMimetype(), Size: int64(au.GetFileLength()), Voice: au.GetPTT(), Fetch: download(au, au.GetFileLength())}
	case q.GetVideoMessage() != nil:
		v := q.GetVideoMessage()
		md = &orchestrator.Media{Kind: "video", MIME: v.GetMimetype(), Size: int64(v.GetFileLength()), Fetch: download(v, v.GetFileLength())}
	case q.GetDocumentMessage() != nil:
		d := q.GetDocumentMessage()
		md = &orchestrator.Media{Kind: "document", MIME: d.GetMimetype(), FileName: d.GetFileName(), Size: int64(d.GetFileLength()), Fetch: download(d, d.GetFileLength())}
	default:
		return nil
	}
	md.Quoted = true
	return md
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
	case q.GetLocationMessage() != nil:
		l := q.GetLocationMessage()
		return locationText("Location", l.GetDegreesLatitude(), l.GetDegreesLongitude(), l.GetName(), l.GetAddress(), l.GetComment())
	case q.GetContactMessage() != nil:
		return "[contact card: " + q.GetContactMessage().GetDisplayName() + "]"
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
	size int
	at   time.Time
}

// chatName returns a chat's display name and, for groups, its size.
func (g *Gateway) chatName(ctx context.Context, cli *whatsmeow.Client, info types.MessageInfo) (string, int) {
	key := info.Chat.String()
	g.names.mu.Lock()
	if c, ok := g.names.names[key]; ok && time.Since(c.at) < time.Hour {
		g.names.mu.Unlock()
		return c.name, c.size
	}
	g.names.mu.Unlock()

	var name string
	var size int
	switch {
	case info.IsGroup:
		if gi, err := cli.GetGroupInfo(ctx, info.Chat); err == nil {
			name, size = gi.Name, len(gi.Participants)
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
	g.names.names[key] = cachedName{name, size, time.Now()}
	g.names.mu.Unlock()
	return name, size
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

// SendReply implements orchestrator.Messenger: text that quotes an
// earlier message, so the phone shows what it answers.
func (g *Gateway) SendReply(ctx context.Context, accountID, chat, text string, q orchestrator.Quote) error {
	cli, jid, err := g.target(accountID, chat)
	if err != nil {
		return err
	}
	info := &waE2E.ContextInfo{StanzaID: proto.String(q.MessageID), QuotedMessage: &waE2E.Message{Conversation: proto.String(q.Text)}}
	if sender, err := types.ParseJID(q.Sender); err == nil && !sender.IsEmpty() {
		info.Participant = proto.String(sender.ToNonAD().String())
	}
	resp, err := cli.SendMessage(ctx, jid, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: info}})
	if err == nil {
		g.sent.add(resp.ID)
	}
	return err
}

// SendPoll implements orchestrator.Messenger: a single-choice poll,
// remembered so its votes can be read back.
func (g *Gateway) SendPoll(ctx context.Context, accountID, chat, question string, options []string) error {
	cli, jid, err := g.target(accountID, chat)
	if err != nil {
		return err
	}
	resp, err := cli.SendMessage(ctx, jid, cli.BuildPollCreation(question, options, 1))
	if err != nil {
		return err
	}
	g.sent.add(resp.ID)
	return g.store.PutPoll(ctx, &store.Poll{MessageID: resp.ID, AccountID: accountID, ChatJID: chat, Question: question, Options: options})
}

// pollAnswer turns a vote on one of the account's polls into text.
func (g *Gateway) pollAnswer(ctx context.Context, cli *whatsmeow.Client, e *events.Message) (string, bool) {
	key := e.Message.GetPollUpdateMessage().GetPollCreationMessageKey()
	poll, err := g.store.Poll(ctx, key.GetID())
	if err != nil {
		return "", false // not ours, or forgotten
	}
	vote, err := cli.DecryptPollVote(ctx, e)
	if err != nil {
		g.log.Warn("decrypt poll vote", "poll", poll.MessageID, "err", err)
		return "", false
	}
	chosen := chosenOptions(poll.Options, vote.GetSelectedOptions())
	if len(chosen) == 0 {
		return "", false // a vote taken back
	}
	return fmt.Sprintf("[Answer to your poll %q: %s]", poll.Question, strings.Join(chosen, ", ")), true
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

// chosenOptions maps a vote's option hashes back to the options' names.
func chosenOptions(options []string, selected [][]byte) []string {
	var chosen []string
	hashes := whatsmeow.HashPollOptions(options)
	for _, h := range selected {
		for i, opt := range hashes {
			if bytes.Equal(h, opt) {
				chosen = append(chosen, options[i])
			}
		}
	}
	return chosen
}
