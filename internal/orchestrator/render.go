package orchestrator

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rahimnathwani/hermitator/internal/ids"
	"github.com/rahimnathwani/hermitator/internal/turn"
	"github.com/rahimnathwani/hermitator/internal/wamd"
)

// /md sends a reply again as a picture, or a PDF when it is long. WhatsApp
// has no tables and wraps monospace at the width of a phone, so a wide
// table, or wide /sh output, arrives scrambled; a picture keeps the grid
// and can be zoomed. The page is drawn inside the sandbox, by the
// Chromium the image ships for the agent's own screenshots: the host runs
// no browser, and the Markdown it renders came from the agent.

const (
	// keepReplies is how many replies per chat /md can go back to.
	keepReplies = 10
	keepFor     = 24 * time.Hour
)

// sentReplies remembers the Markdown behind the last replies sent to each
// chat, and the IDs of the messages they went out as, so a reply to one
// of them can name it.
type sentReplies struct {
	mu    sync.Mutex
	chats map[string][]sentReply
}

type sentReply struct {
	md  string
	ids []string
	at  time.Time
}

func (s *sentReplies) add(chat, md string, ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chats == nil {
		s.chats = map[string][]sentReply{}
	}
	list := s.chats[chat]
	for len(list) > 0 && (len(list) >= keepReplies || time.Since(list[0].at) > keepFor) {
		list = list[1:]
	}
	s.chats[chat] = append(list, sentReply{md, ids, time.Now()})
}

// find returns the Markdown of the reply that went out as message id, or
// of the latest reply when id is empty.
func (s *sentReplies) find(chat, id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.chats[chat]
	for i := len(list) - 1; i >= 0; i-- {
		if time.Since(list[i].at) > keepFor {
			break
		}
		for _, sent := range list[i].ids {
			if id == "" || sent == id {
				return list[i].md, true
			}
		}
	}
	return "", false
}

// renderReactions are the emoji that, from the owner on one of the
// agent's replies, mean /md for that reply.
var renderReactions = map[string]bool{"🖼": true, "📷": true, "📸": true}

// renderCommand handles /md: which reply, and in what form.
func (m *Manager) renderCommand(ctx context.Context, agentID string, chat chatRef, msg Message, arg string) {
	kind := strings.ToLower(strings.TrimSpace(arg))
	switch kind {
	case "", "auto":
		kind = "auto"
	case "png", "image", "img", "picture":
		kind = "png"
	case "pdf":
	case "file", "md", "text":
		kind = "md"
	default:
		chat.send("Usage: /md sends my last reply again as a picture (or a PDF, when it is long); reply to an earlier one with /md to pick it. /md pdf, /md png and /md file (the Markdown as a document) choose the form.")
		return
	}
	key := msg.AccountID + "\x00" + msg.Chat
	md, ok := m.replies.find(key, msg.QuotedID)
	switch {
	case ok:
	case msg.QuotedText != "":
		// Sent before hermitator last started: what WhatsApp quotes is
		// the formatted text, which still renders.
		md = msg.QuotedText
	default:
		if t, err := m.store.LastChatTurn(ctx, agentID, msg.AccountID, msg.Chat); err == nil && strings.TrimSpace(t.Output) != "" {
			md = t.Output
		}
	}
	if strings.TrimSpace(md) == "" {
		chat.send("There is no reply here to render yet.")
		return
	}
	m.render(agentID, chat, md, kind)
}

// render sends md to the chat as a picture, a PDF or a Markdown file.
// When drawing it fails, the Markdown file goes instead.
func (m *Manager) render(agentID string, chat chatRef, md, kind string) {
	if kind == "md" {
		m.sendMarkdownFile(chat, md, "")
		return
	}
	width := min(max(720, 80+9*wamd.Widest(md)), 1600)
	name := turn.RenderDir + "/" + ids.New()
	failed := func(why string) {
		m.sendMarkdownFile(chat, md, "⚠️ Could not draw it ("+why+"), so here it is as a Markdown file.")
	}
	ag, err := m.store.Agent(m.ctx, agentID)
	if err != nil {
		return
	}
	spec, err := m.Spec(ag)
	if err == nil {
		err = writeShared(spec, name+".html", []byte(renderPage(md, width)))
	}
	if err != nil {
		failed(err.Error())
		return
	}
	defer removeShared(spec, name+".html", name+".png", name+".pdf")
	argv := []string{"python3", "-", name + ".html", name, kind, strconv.Itoa(width)}
	m.sandboxJob(agentID, "render", argv, []byte(renderScript), func(text string) { failed(strings.TrimPrefix(text, "⚠️ ")) }, func(r jobResult) {
		got := strings.TrimSpace(string(r.out))
		got = got[strings.LastIndexByte(got, '\n')+1:]
		if !r.exit.OK() || (got != "png" && got != "pdf") {
			failed(clip(strings.TrimSpace(string(r.out)), 300) + " " + r.exit.String())
			return
		}
		data, err := readShared(r.spec, name+"."+got)
		if err == nil {
			err = chat.sendFile("reply."+got, data, "")
		}
		if err != nil {
			failed(err.Error())
		}
	})
}

func (m *Manager) sendMarkdownFile(chat chatRef, md, note string) {
	if note != "" {
		chat.send(note)
	}
	if err := chat.sendFile("reply.md", []byte(md), ""); err != nil {
		chat.send("⚠️ Could not send it: " + err.Error())
	}
}

// renderScript draws the page with Playwright: a full-page PNG, or for a
// page much taller than it is wide (unreadable as one picture), a PDF.
// Nothing but the page itself may load, though it holds no scripts or
// links anyway: wamd.HTML escapes everything. It prints the form it chose.
const renderScript = `import os, sys
from playwright.sync_api import sync_playwright
shared = os.environ.get("HERMITATOR_SHARED", "/shared")
src, out, kind, width = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
with sync_playwright() as p:
    browser = p.chromium.launch()
    page = browser.new_page(viewport={"width": width, "height": 600}, device_scale_factor=2)
    page.route("**/*", lambda route: route.continue_() if route.request.url.startswith("file:") else route.abort())
    page.goto("file://" + os.path.join(shared, src))
    height = page.evaluate("document.documentElement.scrollHeight")
    if kind == "auto":
        kind = "pdf" if height > 3 * width else "png"
    if kind == "pdf":
        page.pdf(path=os.path.join(shared, out + ".pdf"), width=f"{width}px", height=f"{round(width * 1.414)}px", print_background=True)
    else:
        page.screenshot(path=os.path.join(shared, out + ".png"), full_page=True)
    browser.close()
print(kind)
`

// renderPage is the HTML document for md, laid out width CSS pixels wide.
func renderPage(md string, width int) string {
	return `<!doctype html><html><head><meta charset="utf-8"><title>reply</title><style>
*{box-sizing:border-box}
html{background:#fff}
body{margin:0;width:` + strconv.Itoa(width) + `px;padding:28px 32px;color:#1f2328;font:18px/1.5 -apple-system,"Segoe UI","Noto Sans","Liberation Sans","DejaVu Sans",sans-serif,"Noto Color Emoji"}
h1,h2,h3,h4,h5,h6{margin:1em 0 .4em;line-height:1.25}h1{font-size:1.6em}h2{font-size:1.35em}h3{font-size:1.15em}
p,ul,ol,pre,table,blockquote{margin:0 0 .8em}
ul,ol{padding-left:1.6em}li{margin:.15em 0}
code,pre{font-family:"DejaVu Sans Mono","Liberation Mono",Menlo,monospace;font-size:.88em}
code{background:#eff1f3;border-radius:4px;padding:.1em .3em}
pre{background:#f6f8fa;border:1px solid #d1d9e0;border-radius:6px;padding:12px 14px;white-space:pre-wrap;overflow-wrap:anywhere}
pre code{background:none;padding:0;font-size:1em}
table{border-collapse:collapse;font-size:.95em}
th,td{border:1px solid #d1d9e0;padding:6px 10px;vertical-align:top;text-align:left}
th{background:#f3f5f7;font-weight:600}tbody tr:nth-child(even){background:#fafbfc}
blockquote{margin-left:0;padding:0 1em;color:#59636e;border-left:4px solid #d1d9e0}
hr{border:0;border-top:2px solid #d1d9e0;margin:1.2em 0}
u{color:#0969da}del{color:#59636e}
body>:first-child{margin-top:0}body>:last-child{margin-bottom:0}
</style></head><body>
` + wamd.HTML(md) + `</body></html>
`
}
