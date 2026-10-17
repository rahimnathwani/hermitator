// Package gateway connects hermitator to WhatsApp.
//
// One process holds a whatsmeow client for every linked account. Each
// account is a companion device of its owner's phone, exactly like
// WhatsApp Web, so messages the owner sends from their phone arrive here
// too, flagged as their own.
package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"

	"github.com/rahimnathwani/hermitator/internal/orchestrator"
	hstore "github.com/rahimnathwani/hermitator/internal/store"
)

// staleAfter drops messages delivered late, such as the backlog that
// arrives after hermitator has been offline, so agents do not wake up to
// answer yesterday's chatter.
const staleAfter = 15 * time.Minute

// Handler receives incoming messages.
type Handler func(ctx context.Context, msg orchestrator.Message)

// Gateway manages every linked WhatsApp account.
type Gateway struct {
	container *sqlstore.Container
	store     *hstore.Store
	log       *slog.Logger
	handler   Handler

	ctx      context.Context
	mu       sync.Mutex
	accounts map[string]*account
	pairings map[string]*Pairing
	sent     recent
	names    nameCache
}

type account struct {
	id     string
	client *whatsmeow.Client
	queue  chan orchestrator.Message
}

func init() {
	// This is the name the owner sees under Linked Devices on their phone.
	store.SetOSInfo("hermitator", [3]uint32{1, 0, 0})
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
}

// Open opens (creating if needed) whatsmeow's device database.
func Open(ctx context.Context, path string, st *hstore.Store, log *slog.Logger) (*Gateway, error) {
	q := url.Values{}
	for _, p := range []string{"foreign_keys(1)", "journal_mode(WAL)", "busy_timeout(10000)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	container := sqlstore.NewWithDB(db, "sqlite3", waLogger{log.With("component", "whatsmeow")})
	if err := container.Upgrade(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade %s: %w", path, err)
	}
	return &Gateway{
		container: container,
		store:     st,
		log:       log,
		accounts:  map[string]*account{},
		pairings:  map[string]*Pairing{},
	}, nil
}

// SetHandler sets where incoming messages go.
func (g *Gateway) SetHandler(h Handler) { g.handler = h }

// Start connects every linked account.
func (g *Gateway) Start(ctx context.Context) error {
	g.ctx = ctx
	accts, err := g.store.Accounts(ctx, "")
	if err != nil {
		return err
	}
	for _, a := range accts {
		if a.JID == "" || a.Status == hstore.AccountLoggedOut {
			continue
		}
		jid, err := types.ParseJID(a.JID)
		if err != nil {
			g.log.Error("bad account jid", "account", a.ID, "jid", a.JID)
			continue
		}
		device, err := g.container.GetDevice(ctx, jid)
		if err != nil || device == nil {
			g.log.Warn("device missing from whatsmeow store; marking logged out", "account", a.ID)
			g.store.SetAccountStatus(ctx, a.ID, hstore.AccountLoggedOut, "")
			continue
		}
		client := whatsmeow.NewClient(device, waLogger{g.log.With("component", "whatsmeow", "account", a.ID)})
		g.attach(a.ID, client)
		if err := client.Connect(); err != nil {
			g.log.Error("connect", "account", a.ID, "err", err)
		}
	}
	go func() {
		<-ctx.Done()
		g.Close()
	}()
	return nil
}

// attach starts delivering a client's events for an account.
func (g *Gateway) attach(accountID string, client *whatsmeow.Client) {
	a := &account{id: accountID, client: client, queue: make(chan orchestrator.Message, 256)}
	g.mu.Lock()
	g.accounts[accountID] = a
	g.mu.Unlock()
	client.AddEventHandler(func(evt any) { g.handleEvent(a, evt) })
	// whatsmeow calls handlers synchronously; hand messages to a goroutine
	// that preserves their order.
	go func() {
		for {
			select {
			case <-g.ctx.Done():
				return
			case msg := <-a.queue:
				if g.handler != nil {
					g.handler(g.ctx, msg)
				}
			}
		}
	}()
}

// Close disconnects every client.
func (g *Gateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range g.accounts {
		a.client.Disconnect()
	}
	for _, p := range g.pairings {
		p.cancel()
	}
}

func (g *Gateway) client(accountID string) (*whatsmeow.Client, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.accounts[accountID]
	if a == nil {
		return nil, fmt.Errorf("account %s is not connected", accountID)
	}
	return a.client, nil
}

// Connected reports whether an account's client is online.
func (g *Gateway) Connected(accountID string) bool {
	c, err := g.client(accountID)
	return err == nil && c.IsConnected() && c.IsLoggedIn()
}

// Remove logs an account out of WhatsApp and forgets it.
func (g *Gateway) Remove(ctx context.Context, accountID string) error {
	g.mu.Lock()
	a := g.accounts[accountID]
	delete(g.accounts, accountID)
	g.mu.Unlock()
	if a != nil {
		if a.client.IsLoggedIn() {
			if err := a.client.Logout(ctx); err != nil {
				g.log.Warn("logout", "account", accountID, "err", err)
			}
		}
		a.client.Disconnect()
		if a.client.Store.ID != nil {
			g.container.DeleteDevice(ctx, a.client.Store)
		}
	}
	return g.store.DeleteAccount(ctx, accountID)
}

func (g *Gateway) handleEvent(a *account, evt any) {
	ctx := g.ctx
	switch e := evt.(type) {
	case *events.Message:
		msg, ok := g.convert(ctx, a, e)
		if !ok {
			return
		}
		select {
		case a.queue <- msg:
		default:
			g.log.Warn("message queue full; dropping message", "account", a.id)
		}
	case *events.Connected:
		g.store.SetAccountStatus(ctx, a.id, hstore.AccountConnected, a.client.Store.PushName)
		g.log.Info("whatsapp connected", "account", a.id)
	case *events.Disconnected:
		g.store.SetAccountStatus(ctx, a.id, hstore.AccountDisconnected, "")
	case *events.LoggedOut:
		g.log.Warn("whatsapp logged out (device removed on the phone?)", "account", a.id, "reason", e.Reason)
		g.store.SetAccountStatus(ctx, a.id, hstore.AccountLoggedOut, "")
		g.mu.Lock()
		delete(g.accounts, a.id)
		g.mu.Unlock()
	case *events.StreamReplaced:
		g.log.Error("whatsapp session opened elsewhere; is another hermitator running with this database?", "account", a.id)
	case *events.TemporaryBan:
		g.log.Error("whatsapp temporary ban", "account", a.id, "detail", e.String())
	}
}

// recent remembers IDs of messages hermitator sent, so their echoes are
// never mistaken for the owner typing.
type recent struct {
	mu   sync.Mutex
	ids  map[string]time.Time
	last time.Time
}

func (r *recent) add(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ids == nil {
		r.ids = map[string]time.Time{}
	}
	now := time.Now()
	r.ids[id] = now
	if now.Sub(r.last) > time.Minute {
		r.last = now
		for k, t := range r.ids {
			if now.Sub(t) > time.Hour {
				delete(r.ids, k)
			}
		}
	}
}

func (r *recent) has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.ids[id]
	return ok
}

// waLogger adapts slog to whatsmeow's logger.
type waLogger struct{ l *slog.Logger }

func (w waLogger) Warnf(msg string, args ...any)  { w.l.Warn(fmt.Sprintf(msg, args...)) }
func (w waLogger) Errorf(msg string, args ...any) { w.l.Error(fmt.Sprintf(msg, args...)) }
func (w waLogger) Infof(msg string, args ...any)  { w.l.Debug(fmt.Sprintf(msg, args...)) }
func (w waLogger) Debugf(msg string, args ...any) {}
func (w waLogger) Sub(module string) waLog.Logger {
	return waLogger{w.l.With("module", strings.ToLower(module))}
}

// ErrNotPaired is returned for operations on an unknown pairing.
var ErrNotPaired = errors.New("no such pairing in progress")
