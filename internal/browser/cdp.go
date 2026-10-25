// Package browser relays an agent's headless Chromium to the panel: it
// streams the page as JPEG frames and plays the user's taps and typing
// back into it, over the Chrome DevTools Protocol, so the user can sign in
// to a site for the agent from their phone.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Client is a connection to a browser's DevTools, at the browser level,
// with sessions to its pages multiplexed over it (flat mode).
type Client struct {
	conn *websocket.Conn

	mu      sync.Mutex
	next    int64
	pending map[int64]chan reply
	closed  error

	// Events are what the browser sends unasked. Screencast frames go to
	// Frames instead, newest only, so a slow phone sees the latest page
	// rather than falling behind.
	Events chan Event
	Frames chan Frame
	done   chan struct{}
}

// Event is a DevTools event; SessionID says which page it is about.
type Event struct {
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId"`
}

// Frame is one screencast frame of a page.
type Frame struct {
	SessionID string
	Data      []byte // JPEG
	Meta      FrameMeta
}

// FrameMeta is the viewport a frame shows, in CSS pixels.
type FrameMeta struct {
	DeviceWidth  float64 `json:"deviceWidth"`
	DeviceHeight float64 `json:"deviceHeight"`
	OffsetTop    float64 `json:"offsetTop"`
}

type reply struct {
	Result json.RawMessage
	Err    error
}

// Dial connects to the DevTools a browser serves at addr (host:port). The
// browser names its websocket by the address it thinks it has, so the
// host and port are put back to addr.
func Dial(ctx context.Context, addr string) (*Client, error) {
	hc := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/json/version", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	// The browser is the agent's, so what it answers is bounded and only
	// its path is used: the host never connects anywhere but addr.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v); err != nil || v.WS == "" {
		return nil, fmt.Errorf("the browser did not say where its DevTools are (%s)", resp.Status)
	}
	u, err := url.Parse(v.WS)
	if err != nil {
		return nil, err
	}
	u.Scheme, u.Host, u.User = "ws", addr, nil
	conn, _, err := websocket.Dial(ctx, u.String(), nil)
	if err != nil {
		return nil, err
	}
	// Frames of a large page are a few hundred kB of base64.
	conn.SetReadLimit(32 << 20)
	c := &Client{conn: conn, pending: map[int64]chan reply{}, Events: make(chan Event, 256), Frames: make(chan Frame, 1), done: make(chan struct{})}
	go c.read()
	return c, nil
}

// Close ends the connection; the browser keeps running.
func (c *Client) Close() error { return c.conn.Close(websocket.StatusNormalClosure, "") }

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) read() {
	defer close(c.done)
	ctx := context.Background()
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			c.fail(err)
			return
		}
		var msg struct {
			ID        int64           `json:"id"`
			Result    json.RawMessage `json:"result"`
			Error     *struct{ Message string }
			Method    string          `json:"method"`
			Params    json.RawMessage `json:"params"`
			SessionID string          `json:"sessionId"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if msg.ID != 0 {
			c.mu.Lock()
			ch := c.pending[msg.ID]
			delete(c.pending, msg.ID)
			c.mu.Unlock()
			if ch != nil {
				r := reply{Result: msg.Result}
				if msg.Error != nil {
					r.Err = errors.New(msg.Error.Message)
				}
				ch <- r
			}
			continue
		}
		if msg.Method == "Page.screencastFrame" {
			c.frame(msg.SessionID, msg.Params)
			continue
		}
		select {
		case c.Events <- Event{Method: msg.Method, Params: msg.Params, SessionID: msg.SessionID}:
		default:
			// Nobody is keeping up; dropping a title change is harmless,
			// and blocking here would stall every reply.
		}
	}
}

// frame acknowledges a screencast frame at once, so Chrome sends the next
// when the page changes, and keeps it as the newest.
func (c *Client) frame(session string, params json.RawMessage) {
	var p struct {
		Data      []byte    `json:"data"` // base64 in JSON
		Metadata  FrameMeta `json:"metadata"`
		SessionID int64     `json:"sessionId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	c.Send(session, "Page.screencastFrameAck", map[string]any{"sessionId": p.SessionID})
	f := Frame{SessionID: session, Data: p.Data, Meta: p.Metadata}
	for {
		select {
		case c.Frames <- f:
			return
		default:
		}
		select {
		case <-c.Frames:
		default:
		}
	}
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = err
	for id, ch := range c.pending {
		ch <- reply{Err: err}
		delete(c.pending, id)
	}
}

// Call sends a command and waits for its result. An empty session talks
// to the browser itself.
func (c *Client) Call(ctx context.Context, session, method string, params any) (json.RawMessage, error) {
	ch, err := c.send(ctx, session, method, params, true)
	if err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r.Result, r.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Send sends a command without waiting for its result, for input, whose
// result says nothing.
func (c *Client) Send(session, method string, params any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.send(ctx, session, method, params, false)
	return err
}

func (c *Client) send(ctx context.Context, session, method string, params any, wait bool) (chan reply, error) {
	c.mu.Lock()
	if c.closed != nil {
		c.mu.Unlock()
		return nil, c.closed
	}
	c.next++
	id := c.next
	var ch chan reply
	if wait {
		ch = make(chan reply, 1)
		c.pending[id] = ch
	}
	c.mu.Unlock()
	if params == nil {
		params = struct{}{}
	}
	msg := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		msg["sessionId"] = session
	}
	b, err := json.Marshal(msg)
	if err == nil {
		err = c.conn.Write(ctx, websocket.MessageText, b)
	}
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	return ch, nil
}
