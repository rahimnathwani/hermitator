package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"

	"github.com/rahimnathwani/hermitator/internal/ids"
	hstore "github.com/rahimnathwani/hermitator/internal/store"
)

// PairEvent reports progress while linking a new account.
type PairEvent struct {
	Type      string `json:"type"` // qr, code, success, error
	QR        string `json:"qr,omitempty"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
	AccountID string `json:"account_id,omitempty"`
}

// Pairing is one attempt to link an account, by QR code or by phone
// number. Several browser tabs can watch it; each gets the latest event
// straight away.
type Pairing struct {
	ID     string
	UserID string

	client *whatsmeow.Client
	cancel context.CancelFunc

	mu     sync.Mutex
	latest *PairEvent
	subs   map[chan PairEvent]struct{}
	done   bool
}

func (p *Pairing) publish(ev PairEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.latest = &ev
	if ev.Type == "success" || ev.Type == "error" {
		p.done = true
	}
	for ch := range p.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe streams the pairing's events, starting with the latest one.
func (p *Pairing) Subscribe() (<-chan PairEvent, func()) {
	ch := make(chan PairEvent, 16)
	p.mu.Lock()
	if p.subs == nil {
		p.subs = map[chan PairEvent]struct{}{}
	}
	p.subs[ch] = struct{}{}
	if p.latest != nil {
		ch <- *p.latest
	}
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.subs, ch)
		p.mu.Unlock()
	}
}

// StartPairing begins linking a new account for userID.
func (g *Gateway) StartPairing(ctx context.Context, userID string) (*Pairing, error) {
	device := g.container.NewDevice()
	client := whatsmeow.NewClient(device, waLogger{g.log.With("component", "whatsmeow", "pairing", true)})
	pctx, cancel := context.WithTimeout(g.ctx, 10*time.Minute)
	qr, err := client.GetQRChannel(pctx)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := client.Connect(); err != nil {
		cancel()
		return nil, err
	}
	p := &Pairing{ID: ids.New(), UserID: userID, client: client, cancel: cancel}
	g.mu.Lock()
	g.pairings[p.ID] = p
	g.mu.Unlock()

	go func() {
		defer func() {
			g.mu.Lock()
			delete(g.pairings, p.ID)
			g.mu.Unlock()
		}()
		for item := range qr {
			switch item.Event {
			case whatsmeow.QRChannelEventCode:
				png, err := qrcode.Encode(item.Code, qrcode.Medium, 320)
				if err != nil {
					continue
				}
				p.publish(PairEvent{Type: "qr", QR: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)})
			case whatsmeow.QRChannelSuccess.Event:
				accountID, err := g.finishPairing(userID, client)
				if err != nil {
					p.publish(PairEvent{Type: "error", Message: err.Error()})
					client.Disconnect()
					return
				}
				p.publish(PairEvent{Type: "success", AccountID: accountID})
				return
			case whatsmeow.QRChannelTimeout.Event:
				p.publish(PairEvent{Type: "error", Message: "The code expired before it was scanned. Start again."})
				client.Disconnect()
				return
			case whatsmeow.QRChannelEventError:
				p.publish(PairEvent{Type: "error", Message: "Pairing failed: " + item.Error.Error()})
				client.Disconnect()
				return
			default:
				if strings.HasPrefix(item.Event, "err") {
					p.publish(PairEvent{Type: "error", Message: "Pairing failed: " + item.Event})
					client.Disconnect()
					return
				}
			}
		}
		p.mu.Lock()
		finished := p.done
		p.mu.Unlock()
		if !finished {
			p.publish(PairEvent{Type: "error", Message: "Pairing was cancelled."})
			client.Disconnect()
		}
	}()
	return p, nil
}

func (g *Gateway) finishPairing(userID string, client *whatsmeow.Client) (string, error) {
	ctx := g.ctx
	jid := client.Store.ID
	if jid == nil {
		return "", fmt.Errorf("WhatsApp reported success but sent no device ID")
	}
	// Linking the same phone again replaces the old device.
	if old, err := g.store.AccountByJID(ctx, jid.String()); err == nil {
		g.store.DeleteAccount(ctx, old.ID)
	}
	acct := &hstore.Account{
		UserID:   userID,
		JID:      jid.String(),
		Phone:    jid.User,
		PushName: client.Store.PushName,
		Status:   hstore.AccountConnected,
	}
	if err := g.store.CreateAccount(ctx, acct); err != nil {
		return "", err
	}
	g.attach(acct.ID, client)
	g.store.Audit(ctx, userID, userID, "account.link", "phone="+jid.User)
	g.log.Info("whatsapp account linked", "account", acct.ID, "user", userID)
	return acct.ID, nil
}

// Pairing returns a pairing in progress that belongs to userID.
func (g *Gateway) Pairing(id, userID string) (*Pairing, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.pairings[id]
	if p == nil || p.UserID != userID {
		return nil, ErrNotPaired
	}
	return p, nil
}

// PairPhone switches a pairing to an 8-character code the owner types into
// WhatsApp on their phone, for when there is no second screen to scan
// from.
func (g *Gateway) PairPhone(ctx context.Context, p *Pairing, phone string) (string, error) {
	digits := strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, phone)
	if len(digits) < 8 {
		return "", fmt.Errorf("enter the full number with country code")
	}
	code, err := p.client.PairPhone(ctx, digits, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		return "", err
	}
	p.publish(PairEvent{Type: "code", Code: code})
	return code, nil
}
