package wamd

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFormat(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**bold** and *italic*", "*bold* and _italic_"},
		{"__bold__", "*bold*"},
		{"## Summary", "*Summary*"},
		{"# **Done** #", "*Done*"},
		{"* one\n* two", "- one\n- two"},
		{"  + nested", "  - nested"},
		{"~~gone~~", "~gone~"},
		{"see [the docs](https://x.dev/a) now", "see the docs (https://x.dev/a) now"},
		{"[https://x.dev](https://x.dev)", "https://x.dev"},
		{"![chart](https://x.dev/c.png)", "https://x.dev/c.png"},
		{"run `**not bold**` please", "run `**not bold**` please"},
		{"```go\nfunc **x**() {}\n```", "```\nfunc **x**() {}\n```"},
		{"2 * 3 * 4 = 24", "2 * 3 * 4 = 24"},
		{"a*b*c", "a*b*c"},
	}
	for _, c := range cases {
		if got := Format(c.in); got != c.want {
			t.Errorf("Format(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestSplit(t *testing.T) {
	if got := Split("short", 100); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short: %q", got)
	}
	if got := Split("   ", 100); len(got) != 0 {
		t.Fatalf("blank: %q", got)
	}

	para := strings.Repeat("word ", 30)
	text := para + "\n\n" + para + "\n\n" + para
	for _, c := range Split(text, 200) {
		if n := utf8.RuneCountInString(c); n > 200 {
			t.Fatalf("chunk of %d runes exceeds 200: %q", n, c)
		}
	}

	code := "intro\n```\n" + strings.Repeat("line of code\n", 40) + "```\noutro"
	chunks := Split(code, 120)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if strings.Count(c, "```")%2 != 0 {
			t.Fatalf("chunk %d has an unbalanced code fence:\n%s", i, c)
		}
		if utf8.RuneCountInString(c) > 120 {
			t.Fatalf("chunk %d too long", i)
		}
	}

	long := strings.Repeat("é", 500)
	for _, c := range Split(long, 100) {
		if utf8.RuneCountInString(c) > 100 || !utf8.ValidString(c) {
			t.Fatalf("bad chunk %q", c)
		}
	}
}
