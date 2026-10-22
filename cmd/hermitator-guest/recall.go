package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

// recall searches the agent's past conversations where they lie: Claude
// Code's and pi's transcripts, and the chat history files hermitator
// keeps. No model is involved. A search lists the newest matches with a
// reference each; --show REF reads the messages around one. A compacted
// conversation's transcript still holds everything the summary dropped,
// and this is how the agent gets it back. Each chat has conversations of
// its own; from a turn, recall looks in that chat's unless given --all.

const recallUsage = `usage: hermitator-guest recall WORDS...            search past conversations
       hermitator-guest recall --show REF [--around N]  read the messages around a match
       hermitator-guest recall                          list the conversations

Searches your transcripts (Claude Code's and pi's) and the chat's
history file for messages containing every one of WORDS, ignoring case,
newest first. Each match has a reference, such as 3f9a1c:120, to read it
in context with --show.

  --all        search your other chats' conversations too
  --limit N    how many matches to list (default 10)
  --around N   how many messages before and after to show (default 5)`

// entry is one message of a past conversation.
type entry struct {
	when time.Time
	who  string
	text string
}

// source is a file holding a conversation.
type source struct {
	id   string // a short hash of its path, stable across calls
	path string
	kind string // claude, pi, chat
	mod  time.Time
}

func recall(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	show := fs.String("show", "", "")
	around := fs.Int("around", 5, "")
	limit := fs.Int("limit", 10, "")
	all := fs.Bool("all", false, "")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, recallUsage)
		return 2
	}
	sources := recallSources()
	if *show != "" {
		// A reference comes from a search already made, in whatever scope.
		return recallShow(out, sources, *show, max(*around, 0))
	}
	scoped := false
	if !*all {
		sources, scoped = thisChat(sources)
	}
	if fs.NArg() == 0 {
		return recallList(out, sources)
	}
	code := recallSearch(out, sources, fs.Args(), max(*limit, 1))
	if code != 0 && scoped {
		fmt.Fprintln(out, "That was this chat's conversations; --all searches your other chats too.")
	}
	return code
}

// thisChat keeps the sources of the chat whose turn recall runs in: its
// conversations and its history file. Outside a turn, as in the web
// terminal, there is no chat, and all of them stay.
func thisChat(sources []source) ([]source, bool) {
	list, ok := os.LookupEnv(turn.EnvSessions)
	if !ok {
		return sources, false
	}
	sessions := map[string]bool{}
	for _, id := range strings.Split(list, ",") {
		if id != "" {
			sessions[id] = true
		}
	}
	history := os.Getenv(turn.EnvHistory)
	var out []source
	for _, s := range sources {
		name := filepath.Base(s.path)
		switch s.kind {
		case "chat":
			if history != "" && name == filepath.Base(history) {
				out = append(out, s)
			}
		default:
			// Claude Code names a transcript SESSION.jsonl, pi TIME_SESSION.jsonl.
			id := strings.TrimSuffix(name, ".jsonl")
			if i := strings.LastIndexByte(id, '_'); i >= 0 {
				id = id[i+1:]
			}
			if sessions[id] {
				out = append(out, s)
			}
		}
	}
	return out, true
}

// recallSources finds every conversation file, newest first.
func recallSources() []source {
	home, _ := os.UserHomeDir()
	var out []source
	add := func(pattern, kind string) {
		matches, _ := filepath.Glob(pattern)
		for _, p := range matches {
			fi, err := os.Stat(p)
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			sum := sha256.Sum256([]byte(p))
			out = append(out, source{id: hex.EncodeToString(sum[:3]), path: p, kind: kind, mod: fi.ModTime()})
		}
	}
	add(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"), "claude")
	add(filepath.Join(home, ".pi", "agent", "sessions", "*", "*.jsonl"), "pi")
	add(shared("chats", "*.md"), "chat")
	sort.Slice(out, func(i, j int) bool { return out[i].mod.After(out[j].mod) })
	return out
}

func recallSearch(out io.Writer, sources []source, words []string, limit int) int {
	var res []*regexp.Regexp
	for _, w := range words {
		res = append(res, regexp.MustCompile("(?i)"+regexp.QuoteMeta(w)))
	}
	type hit struct {
		ref string
		e   entry
	}
	var hits []hit
	for _, s := range sources {
		for i, e := range readSource(s) {
			matched := true
			for _, re := range res {
				if !re.MatchString(e.text) {
					matched = false
					break
				}
			}
			if matched {
				hits = append(hits, hit{fmt.Sprintf("%s:%d", s.id, i), e})
			}
		}
	}
	if len(hits) == 0 {
		fmt.Fprintf(out, "No message in your past conversations contains all of: %s\n", strings.Join(words, " "))
		return 1
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].e.when.After(hits[j].e.when) })
	for _, h := range hits[:min(limit, len(hits))] {
		fmt.Fprintf(out, "%s  %s  %s: %s\n", h.ref, stamp(h.e.when), h.e.who, snippet(h.e.text, res[0], 240))
	}
	if len(hits) > limit {
		fmt.Fprintf(out, "\n%d matches in all; --limit %d shows more, or add words to narrow it.\n", len(hits), len(hits))
	}
	fmt.Fprintln(out, "\nRead one in context with: hermitator-guest recall --show REF")
	return 0
}

func recallShow(out io.Writer, sources []source, ref string, around int) int {
	id, n, ok := strings.Cut(ref, ":")
	i, err := strconv.Atoi(n)
	if !ok || err != nil || i < 0 {
		fmt.Fprintf(os.Stderr, "%q is not a reference; they look like 3f9a1c:120\n", ref)
		return 2
	}
	for _, s := range sources {
		if s.id != id {
			continue
		}
		entries := readSource(s)
		if i >= len(entries) {
			fmt.Fprintf(os.Stderr, "%s has only %d messages\n", s.path, len(entries))
			return 1
		}
		fmt.Fprintf(out, "%s (%s)\n\n", s.path, s.kind)
		for j := max(0, i-around); j <= min(len(entries)-1, i+around); j++ {
			e, mark, limit := entries[j], "  ", 1500
			if j == i {
				mark, limit = "> ", 8000
			}
			fmt.Fprintf(out, "%s%s:%d  %s  %s: %s\n\n", mark, s.id, j, stamp(e.when), e.who, clipText(e.text, limit))
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "no conversation %s; run hermitator-guest recall to list them\n", id)
	return 1
}

func recallList(out io.Writer, sources []source) int {
	if len(sources) == 0 {
		fmt.Fprintln(out, "No past conversations yet.")
		return 0
	}
	for _, s := range sources[:min(20, len(sources))] {
		entries := readSource(s)
		first := ""
		for _, e := range entries {
			if e.who == "user" || s.kind == "chat" {
				first = oneLine(e.text, 80)
				break
			}
		}
		fmt.Fprintf(out, "%s  %-6s  %s  %4d messages  %s\n", s.id, s.kind, s.mod.Format("2006-01-02 15:04"), len(entries), first)
	}
	if len(sources) > 20 {
		fmt.Fprintf(out, "… and %d older\n", len(sources)-20)
	}
	return 0
}

func readSource(s source) []entry {
	f, err := os.Open(s.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var out []entry
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			switch s.kind {
			case "claude":
				out = append(out, claudeEntries(line)...)
			case "pi":
				out = append(out, piEntries(line)...)
			case "chat":
				out = chatEntry(out, string(line))
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintln(os.Stderr, "reading", s.path+":", err)
			}
			return out
		}
	}
}

// claudeEntries reads one line of a Claude Code transcript.
func claudeEntries(line []byte) []entry {
	var rec struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		IsMeta    bool      `json:"isMeta"`
		Summary   bool      `json:"isCompactSummary"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.IsMeta || (rec.Type != "user" && rec.Type != "assistant") {
		return nil
	}
	who := "user"
	switch {
	case rec.Summary:
		who = "summary"
	case rec.Type == "assistant":
		who = "you"
	}
	var text string
	if json.Unmarshal(rec.Message.Content, &text) == nil {
		return oneEntry(rec.Timestamp, who, text)
	}
	var items []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Name    string          `json:"name"`
		Input   json.RawMessage `json:"input"`
		Content json.RawMessage `json:"content"`
	}
	json.Unmarshal(rec.Message.Content, &items)
	var out []entry
	for _, it := range items {
		switch it.Type {
		case "text":
			out = append(out, oneEntry(rec.Timestamp, who, it.Text)...)
		case "tool_use":
			out = append(out, oneEntry(rec.Timestamp, "tool", it.Name+" "+string(it.Input))...)
		case "tool_result":
			out = append(out, oneEntry(rec.Timestamp, "tool result", flatText(it.Content))...)
		}
	}
	return out
}

// piEntries reads one line of a pi session.
func piEntries(line []byte) []entry {
	var rec struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Summary   string    `json:"summary"`
		Message   struct {
			Role     string          `json:"role"`
			ToolName string          `json:"toolName"`
			Content  json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return nil
	}
	if rec.Type == "compaction" {
		return oneEntry(rec.Timestamp, "summary", rec.Summary)
	}
	if rec.Type != "message" {
		return nil
	}
	var items []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	json.Unmarshal(rec.Message.Content, &items)
	var out []entry
	for _, it := range items {
		switch {
		case it.Type == "text" && rec.Message.Role == "user":
			out = append(out, oneEntry(rec.Timestamp, "user", it.Text)...)
		case it.Type == "text" && rec.Message.Role == "assistant":
			out = append(out, oneEntry(rec.Timestamp, "you", it.Text)...)
		case it.Type == "text" && rec.Message.Role == "toolResult":
			out = append(out, oneEntry(rec.Timestamp, "tool result", it.Text)...)
		case it.Type == "toolCall":
			out = append(out, oneEntry(rec.Timestamp, "tool", it.Name+" "+string(it.Arguments))...)
		}
	}
	return out
}

var chatLine = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}) ([^:]+): (.*)$`)

// chatEntry adds one line of a chat history file: a message, or the
// indented continuation of the one before.
func chatEntry(out []entry, line string) []entry {
	line = strings.TrimRight(line, "\r\n")
	if m := chatLine.FindStringSubmatch(line); m != nil {
		when, _ := time.ParseInLocation("2006-01-02 15:04", m[1], time.Local)
		return append(out, entry{when: when, who: m[2], text: m[3]})
	}
	if rest, ok := strings.CutPrefix(line, "    "); ok && len(out) > 0 {
		out[len(out)-1].text += "\n" + rest
	}
	return out
}

func oneEntry(when time.Time, who, text string) []entry {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []entry{{when: when, who: who, text: text}}
}

// flatText is a tool result's text, which is a string or a list of parts.
func flatText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// snippet is the stretch of text around re's first match, on one line.
func snippet(text string, re *regexp.Regexp, width int) string {
	flat := strings.Join(strings.Fields(text), " ")
	loc := re.FindStringIndex(flat)
	if loc == nil || len(flat) <= width {
		return oneLine(flat, width)
	}
	start := max(0, loc[0]-width/3)
	for start > 0 && !utf8Start(flat[start]) {
		start--
	}
	end := min(len(flat), start+width)
	for end < len(flat) && !utf8Start(flat[end]) {
		end++
	}
	s := flat[start:end]
	if start > 0 {
		s = "…" + s
	}
	if end < len(flat) {
		s += "…"
	}
	return s
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func oneLine(s string, n int) string {
	return clipText(strings.Join(strings.Fields(s), " "), n)
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "????-??-?? ??:??"
	}
	return t.Local().Format("2006-01-02 15:04")
}
