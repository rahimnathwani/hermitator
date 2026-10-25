package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeChrome is just enough of a browser's DevTools: two kinds of target,
// sessions per page, a screencast that sends one frame per start, and a
// record of every command.
type fakeChrome struct {
	t    *testing.T
	srv  *httptest.Server
	mu   sync.Mutex
	got  []map[string]any
	conn *websocket.Conn
	acks chan float64
	ws   chan struct{}
}

func newFakeChrome(t *testing.T) *fakeChrome {
	f := &fakeChrome{t: t, acks: make(chan float64, 16), ws: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		// Chrome names its own idea of its address; Dial must not use it.
		w.Write([]byte(`{"webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/browser/abc"}`))
	})
	mux.HandleFunc("/devtools/browser/abc", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conn = c
		f.mu.Unlock()
		close(f.ws)
		ctx := context.Background()
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			json.Unmarshal(b, &m)
			f.mu.Lock()
			f.got = append(f.got, m)
			f.mu.Unlock()
			f.answer(ctx, m)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeChrome) write(ctx context.Context, v any) {
	b, _ := json.Marshal(v)
	f.mu.Lock()
	c := f.conn
	f.mu.Unlock()
	c.Write(ctx, websocket.MessageText, b)
}

func (f *fakeChrome) answer(ctx context.Context, m map[string]any) {
	id := m["id"]
	params, _ := m["params"].(map[string]any)
	session, _ := m["sessionId"].(string)
	result := map[string]any{}
	switch m["method"] {
	case "Target.getTargets":
		result["targetInfos"] = []map[string]any{
			{"targetId": "A", "type": "page", "title": "Login", "url": "https://a.test/login"},
			{"targetId": "W", "type": "service_worker", "url": "https://a.test/sw.js"},
		}
	case "Target.attachToTarget":
		result["sessionId"] = "S-" + params["targetId"].(string)
	case "Page.screencastFrameAck":
		f.acks <- params["sessionId"].(float64)
	case "Page.getNavigationHistory":
		result["currentIndex"] = 1
		result["entries"] = []map[string]any{{"id": 11}, {"id": 12}}
	}
	f.write(ctx, map[string]any{"id": id, "result": result})
	if m["method"] == "Page.startScreencast" {
		f.write(ctx, map[string]any{"method": "Page.screencastFrame", "sessionId": session, "params": map[string]any{
			"data": []byte("jpeg of " + session), "sessionId": 7,
			"metadata": map[string]any{"deviceWidth": 400, "deviceHeight": 800},
		}})
	}
}

func (f *fakeChrome) sent(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, m := range f.got {
		if m["method"] == method {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeChrome) waitFor(method string, n int) []map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.sent(method); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("no %d× %s; got %v", n, method, f.got)
	return nil
}

type panelMsg struct {
	typ  websocket.MessageType
	data []byte
}

func TestRelayShowsFollowsAndTaps(t *testing.T) {
	f := newFakeChrome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Dial(ctx, strings.TrimPrefix(f.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out := make(chan panelMsg, 64)
	in := make(chan []byte, 8)
	r := &Relay{C: c, Send: func(_ context.Context, typ websocket.MessageType, b []byte) error {
		out <- panelMsg{typ, b}
		return nil
	}}
	errc := make(chan error, 1)
	go func() { errc <- r.Run(ctx, in) }()

	next := func(typ websocket.MessageType) string {
		t.Helper()
		for {
			select {
			case m := <-out:
				if m.typ == typ {
					return string(m.data)
				}
			case err := <-errc:
				t.Fatalf("relay ended: %v", err)
			case <-ctx.Done():
				t.Fatal("timed out")
			}
		}
	}

	// The page, not the service worker, is shown, sized like the phone,
	// and its frame comes through and is acknowledged.
	if page := next(websocket.MessageText); !strings.Contains(page, `"url":"https://a.test/login"`) || strings.Contains(page, "sw.js") {
		t.Fatalf("page message = %s", page)
	}
	if frame := next(websocket.MessageBinary); frame != "jpeg of S-A" {
		t.Fatalf("frame = %q", frame)
	}
	if ack := <-f.acks; ack != 7 {
		t.Fatalf("acked frame %v", ack)
	}
	metrics := f.waitFor("Emulation.setDeviceMetricsOverride", 1)[0]["params"].(map[string]any)
	if metrics["mobile"] != true || metrics["width"].(float64) != 412 {
		t.Fatalf("viewport = %v", metrics)
	}

	// A tap at a quarter across and halfway down lands at those CSS
	// pixels of the 400×800 viewport the frame showed.
	in <- []byte(`{"type":"tap","x":0.25,"y":0.5}`)
	clicks := f.waitFor("Input.dispatchMouseEvent", 3)
	p := clicks[1]["params"].(map[string]any)
	if p["type"] != "mousePressed" || p["x"].(float64) != 100 || p["y"].(float64) != 400 || clicks[1]["sessionId"] != "S-A" {
		t.Fatalf("press = %v", clicks[1])
	}
	in <- []byte(`{"type":"tap","x":3,"y":0.5}`)
	in <- []byte(`{"type":"text","text":"hunter2"}`)
	if got := f.waitFor("Input.insertText", 1)[0]["params"].(map[string]any)["text"]; got != "hunter2" {
		t.Fatalf("typed %v", got)
	}
	if n := len(f.sent("Input.dispatchMouseEvent")); n != 3 {
		t.Fatalf("a tap off the picture was sent: %d mouse events", n)
	}
	in <- []byte(`{"type":"back"}`)
	if got := f.waitFor("Page.navigateToHistoryEntry", 1)[0]["params"].(map[string]any)["entryId"]; got.(float64) != 11 {
		t.Fatalf("back went to %v", got)
	}

	// A popup (the OAuth window) is shown as soon as it opens, and when it
	// closes the page behind it is shown again.
	f.write(ctx, map[string]any{"method": "Target.targetCreated", "params": map[string]any{"targetInfo": map[string]any{
		"targetId": "B", "type": "page", "title": "Sign in with Google", "url": "https://accounts.test/", "openerId": "A"}}})
	if frame := next(websocket.MessageBinary); frame != "jpeg of S-B" {
		t.Fatalf("popup frame = %q", frame)
	}
	if stops := f.sent("Page.stopScreencast"); len(stops) == 0 || stops[0]["sessionId"] != "S-A" {
		t.Fatalf("the first page's screencast went on: %v", stops)
	}
	f.write(ctx, map[string]any{"method": "Target.targetDestroyed", "params": map[string]any{"targetId": "B"}})
	if frame := next(websocket.MessageBinary); frame != "jpeg of S-A" {
		t.Fatalf("after the popup closed = %q", frame)
	}

	// Pages announced again by discovery are not switched to.
	f.write(ctx, map[string]any{"method": "Target.targetCreated", "params": map[string]any{"targetInfo": map[string]any{
		"targetId": "A", "type": "page", "title": "Login", "url": "https://a.test/login"}}})
	in <- []byte(`{"type":"reload"}`)
	f.waitFor("Page.reload", 1)
	if n := len(f.sent("Target.attachToTarget")); n != 3 {
		t.Fatalf("%d attaches, want 3 (A, B, A)", n)
	}
	close(in)
	if err := <-errc; err != nil {
		t.Fatalf("relay ended with %v", err)
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{" example.com/x ": "https://example.com/x", "http://a.test": "http://a.test", "": ""} {
		if got := normalizeURL(in); got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}
