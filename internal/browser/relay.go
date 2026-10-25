package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

// Relay shows one of the browser's pages on the panel at a time. It
// follows new tabs and popups (an OAuth "Sign in with Google" window opens
// as one), and falls back to another page when the one shown closes.
//
// The panel sends JSON text messages:
//
//	{"type":"size","width":390,"height":700,"scale":3}   its viewport, in CSS pixels
//	{"type":"tap","x":0.5,"y":0.25}                       a tap, as fractions of the picture
//	{"type":"scroll","x":0.5,"y":0.5,"dy":300}
//	{"type":"text","text":"hello"}                        typed text
//	{"type":"key","key":"Enter"}                          Enter, Backspace, Tab, Escape, arrows, Delete
//	{"type":"navigate","url":"example.com"}, {"type":"back"}, {"type":"reload"}
//	{"type":"tab","id":"TARGET"}                          show another tab
//
// and gets binary messages, each a JPEG of the page, and text messages
// {"type":"page","url":..,"title":..,"tabs":[..]} when what it shows
// changes.
type Relay struct {
	C *Client
	// Send delivers a message to the panel.
	Send func(ctx context.Context, typ websocket.MessageType, data []byte) error

	pages   map[string]*target
	order   []string // oldest first
	current string
	session string
	meta    FrameMeta
	width   float64
	height  float64
	scale   float64
}

type target struct {
	ID    string `json:"targetId"`
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Viewport limits, so a confused client cannot ask for a huge one.
const (
	minSide  = 200
	maxWidth = 1600
	maxTall  = 3000
	maxTabs  = 100
)

// Run relays until ctx ends, in closes, or the browser goes away.
func (r *Relay) Run(ctx context.Context, in <-chan []byte) error {
	r.pages = map[string]*target{}
	if r.width == 0 {
		r.width, r.height, r.scale = 412, 800, 2
	}
	res, err := r.C.Call(ctx, "", "Target.getTargets", nil)
	if err != nil {
		return err
	}
	var list struct {
		Infos []target `json:"targetInfos"`
	}
	json.Unmarshal(res, &list)
	for _, t := range list.Infos {
		r.add(t)
	}
	// Pages that exist now are announced again as created; they are known
	// already, so only pages opened from now on are switched to.
	if _, err := r.C.Call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}); err != nil {
		return err
	}
	if len(r.order) == 0 {
		if _, err := r.C.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}); err != nil {
			return err
		}
	} else if err := r.attach(ctx, r.order[0]); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.C.Done():
			return errors.New("the browser closed")
		case b, ok := <-in:
			if !ok {
				return nil
			}
			if err := r.input(ctx, b); err != nil {
				return err
			}
		case ev := <-r.C.Events:
			if err := r.event(ctx, ev); err != nil {
				return err
			}
		case f := <-r.C.Frames:
			if f.SessionID != r.session || r.session == "" {
				continue
			}
			r.meta = f.Meta
			if err := r.Send(ctx, websocket.MessageBinary, f.Data); err != nil {
				return err
			}
		}
	}
}

func (r *Relay) add(t target) bool {
	if t.Type != "page" {
		return false
	}
	if p, ok := r.pages[t.ID]; ok {
		*p = t
		return false
	}
	if len(r.pages) >= maxTabs {
		// An agent opening tabs without end gets no more memory here.
		return false
	}
	r.pages[t.ID] = &t
	r.order = append(r.order, t.ID)
	return true
}

func (r *Relay) remove(id string) {
	delete(r.pages, id)
	for i, o := range r.order {
		if o == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// attach shows a page: it takes the page's own session, sizes it like the
// phone and starts its screencast, after stopping the previous one's.
func (r *Relay) attach(ctx context.Context, id string) error {
	if r.session != "" {
		r.C.Send(r.session, "Page.stopScreencast", nil)
		r.C.Send("", "Target.detachFromTarget", map[string]any{"sessionId": r.session})
		r.session = ""
	}
	res, err := r.C.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": id, "flatten": true})
	if err != nil {
		return err
	}
	var a struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(res, &a)
	r.current, r.session, r.meta = id, a.SessionID, FrameMeta{}
	if _, err := r.C.Call(ctx, r.session, "Page.enable", nil); err != nil {
		return err
	}
	r.C.Send("", "Target.activateTarget", map[string]any{"targetId": id})
	if err := r.screencast(ctx); err != nil {
		return err
	}
	return r.sendPage(ctx)
}

func (r *Relay) screencast(ctx context.Context) error {
	if _, err := r.C.Call(ctx, r.session, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": int(r.width), "height": int(r.height), "deviceScaleFactor": r.scale, "mobile": true,
	}); err != nil {
		return err
	}
	_, err := r.C.Call(ctx, r.session, "Page.startScreencast", map[string]any{
		"format": "jpeg", "quality": 70, "maxWidth": int(r.width * r.scale), "maxHeight": int(r.height * r.scale),
	})
	return err
}

func (r *Relay) sendPage(ctx context.Context) error {
	type tab struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		URL     string `json:"url"`
		Current bool   `json:"current"`
	}
	msg := struct {
		Type  string `json:"type"`
		URL   string `json:"url"`
		Title string `json:"title"`
		Tabs  []tab  `json:"tabs"`
	}{Type: "page", Tabs: []tab{}}
	for _, id := range r.order {
		p := r.pages[id]
		msg.Tabs = append(msg.Tabs, tab{ID: id, Title: p.Title, URL: p.URL, Current: id == r.current})
		if id == r.current {
			msg.URL, msg.Title = p.URL, p.Title
		}
	}
	b, _ := json.Marshal(msg)
	return r.Send(ctx, websocket.MessageText, b)
}

func (r *Relay) event(ctx context.Context, ev Event) error {
	var p struct {
		TargetInfo target `json:"targetInfo"`
		TargetID   string `json:"targetId"`
		SessionID  string `json:"sessionId"`
	}
	json.Unmarshal(ev.Params, &p)
	switch ev.Method {
	case "Target.targetCreated":
		if r.add(p.TargetInfo) {
			return r.attach(ctx, p.TargetInfo.ID)
		}
	case "Target.targetInfoChanged":
		if _, ok := r.pages[p.TargetInfo.ID]; ok {
			r.add(p.TargetInfo)
			if p.TargetInfo.ID == r.current {
				return r.sendPage(ctx)
			}
		}
	case "Target.targetDestroyed":
		if _, ok := r.pages[p.TargetID]; !ok {
			return nil
		}
		r.remove(p.TargetID)
		if p.TargetID != r.current {
			return r.sendPage(ctx)
		}
		r.current, r.session = "", ""
		if len(r.order) == 0 {
			// Its targetCreated brings it up.
			_, err := r.C.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"})
			return err
		}
		return r.attach(ctx, r.order[len(r.order)-1])
	case "Target.detachedFromTarget":
		if p.SessionID == r.session && r.current != "" {
			r.session = ""
			return r.attach(ctx, r.current)
		}
	}
	return nil
}

var keys = map[string]struct {
	code string
	vk   int
	text string
}{
	"Enter": {"Enter", 13, "\r"}, "Backspace": {"Backspace", 8, ""}, "Tab": {"Tab", 9, ""},
	"Escape": {"Escape", 27, ""}, "Delete": {"Delete", 46, ""},
	"ArrowLeft": {"ArrowLeft", 37, ""}, "ArrowUp": {"ArrowUp", 38, ""}, "ArrowRight": {"ArrowRight", 39, ""}, "ArrowDown": {"ArrowDown", 40, ""},
}

func (r *Relay) input(ctx context.Context, b []byte) error {
	var m struct {
		Type   string  `json:"type"`
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
		DY     float64 `json:"dy"`
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
		Scale  float64 `json:"scale"`
		Text   string  `json:"text"`
		Key    string  `json:"key"`
		URL    string  `json:"url"`
		ID     string  `json:"id"`
	}
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	if r.session == "" && m.Type != "size" {
		return nil
	}
	s := r.session
	switch m.Type {
	case "size":
		r.width, r.height = clamp(m.Width, minSide, maxWidth), clamp(m.Height, minSide, maxTall)
		r.scale = clamp(m.Scale, 1, 3)
		if s == "" {
			return nil
		}
		r.C.Send(s, "Page.stopScreencast", nil)
		return r.screencast(ctx)
	case "tap":
		x, y, ok := r.point(m.X, m.Y)
		if !ok {
			return nil
		}
		r.C.Send(s, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x, "y": y})
		for _, t := range []string{"mousePressed", "mouseReleased"} {
			r.C.Send(s, "Input.dispatchMouseEvent", map[string]any{"type": t, "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": 1})
		}
	case "scroll":
		x, y, ok := r.point(m.X, m.Y)
		if !ok {
			return nil
		}
		r.C.Send(s, "Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": x, "y": y, "deltaX": 0, "deltaY": clamp(m.DY, -5000, 5000)})
	case "text":
		if m.Text != "" && len(m.Text) <= 4096 {
			r.C.Send(s, "Input.insertText", map[string]any{"text": m.Text})
		}
	case "key":
		k, ok := keys[m.Key]
		if !ok {
			return nil
		}
		down := map[string]any{"type": "keyDown", "key": m.Key, "code": k.code, "windowsVirtualKeyCode": k.vk}
		if k.text != "" {
			down["text"] = k.text
		}
		r.C.Send(s, "Input.dispatchKeyEvent", down)
		r.C.Send(s, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": m.Key, "code": k.code, "windowsVirtualKeyCode": k.vk})
	case "navigate":
		if u := normalizeURL(m.URL); u != "" {
			r.C.Send(s, "Page.navigate", map[string]any{"url": u})
		}
	case "reload":
		r.C.Send(s, "Page.reload", nil)
	case "back":
		res, err := r.C.Call(ctx, s, "Page.getNavigationHistory", nil)
		if err != nil {
			return nil
		}
		var h struct {
			Current int `json:"currentIndex"`
			Entries []struct {
				ID int `json:"id"`
			} `json:"entries"`
		}
		if json.Unmarshal(res, &h) == nil && h.Current > 0 && h.Current <= len(h.Entries)-1 {
			r.C.Send(s, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[h.Current-1].ID})
		}
	case "tab":
		if _, ok := r.pages[m.ID]; ok && m.ID != r.current {
			return r.attach(ctx, m.ID)
		}
	}
	return nil
}

// point turns fractions of the picture into the page's CSS pixels, using
// the viewport the newest frame showed.
func (r *Relay) point(fx, fy float64) (x, y float64, ok bool) {
	if r.meta.DeviceWidth <= 0 || r.meta.DeviceHeight <= 0 || fx < 0 || fx > 1 || fy < 0 || fy > 1 {
		return 0, 0, false
	}
	return fx * r.meta.DeviceWidth, fy * r.meta.DeviceHeight, true
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// normalizeURL accepts what someone types in an address bar.
func normalizeURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") && !strings.HasPrefix(s, "about:") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return u.String()
}
