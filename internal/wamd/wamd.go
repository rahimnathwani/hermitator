// Package wamd converts the GitHub-flavoured Markdown that coding agents
// write into WhatsApp's formatting, and splits long replies into messages.
package wamd

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	heading    = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.+?)\s*#*\s*$`)
	bullet     = regexp.MustCompile(`^(\s*)[*+]\s+`)
	image      = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)[^)]*\)`)
	link       = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)[^)]*\)`)
	boldStar   = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	boldUnder  = regexp.MustCompile(`__([^_\n]+?)__`)
	italicStar = regexp.MustCompile(`(^|[^*\w])\*([^*\s](?:[^*\n]*?[^*\s])?)\*($|[^*\w])`)
	strike     = regexp.MustCompile(`~~([^~\n]+?)~~`)
	fence      = regexp.MustCompile("^\\s*```")
)

// placeholder stands in for WhatsApp's bold marker while italics are
// rewritten, so `**x**` does not get read as two italic markers.
const placeholder = "\x00"

// Format rewrites Markdown for WhatsApp. Code blocks and inline code are
// left alone.
func Format(md string) string {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	inFence := false
	for i, line := range lines {
		if fence.MatchString(line) {
			inFence = !inFence
			// WhatsApp does not understand language tags.
			lines[i] = strings.TrimSpace(line)[:3]
			continue
		}
		if inFence {
			continue
		}
		if m := heading.FindStringSubmatch(line); m != nil {
			line = "*" + strings.Trim(m[1], "*_ ") + "*"
		} else {
			line = bullet.ReplaceAllString(line, "$1- ")
			line = formatInline(line)
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

func formatInline(line string) string {
	// Odd segments are inside `inline code`.
	parts := strings.Split(line, "`")
	for i := 0; i < len(parts); i += 2 {
		if i == len(parts)-1 && len(parts)%2 == 0 {
			break // unbalanced backtick; leave the tail alone
		}
		s := parts[i]
		s = image.ReplaceAllString(s, "$2")
		s = link.ReplaceAllStringFunc(s, func(m string) string {
			sub := link.FindStringSubmatch(m)
			if sub[1] == sub[2] {
				return sub[2]
			}
			return sub[1] + " (" + sub[2] + ")"
		})
		s = boldStar.ReplaceAllString(s, placeholder+"$1"+placeholder)
		s = boldUnder.ReplaceAllString(s, placeholder+"$1"+placeholder)
		s = italicStar.ReplaceAllString(s, "${1}_${2}_${3}")
		s = strike.ReplaceAllString(s, "~$1~")
		parts[i] = strings.ReplaceAll(s, placeholder, "*")
	}
	return strings.Join(parts, "`")
}

// Split breaks text into messages of at most max characters, preferring
// paragraph and line boundaries. A code block cut in two is closed at the
// end of one message and reopened at the start of the next.
func Split(text string, max int) []string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= max {
		if text == "" {
			return nil
		}
		return []string{text}
	}
	var chunks []string
	var cur strings.Builder
	curLen := 0
	inFence := false
	flush := func() {
		s := strings.TrimRight(cur.String(), "\n")
		if inFence {
			s += "\n```"
		}
		if strings.TrimSpace(s) != "" {
			chunks = append(chunks, s)
		}
		cur.Reset()
		curLen = 0
		if inFence {
			cur.WriteString("```\n")
			curLen = 4
		}
	}
	budget := max - 4 // room to close a code block
	for _, line := range strings.Split(text, "\n") {
		for utf8.RuneCountInString(line) > budget {
			if curLen > 0 {
				flush()
			}
			r := []rune(line)
			cut := budget - curLen
			if sp := strings.LastIndexByte(string(r[:cut]), ' '); sp > cut/2 {
				cut = utf8.RuneCountInString(string(r[:cut])[:sp])
			}
			cur.WriteString(string(r[:cut]))
			curLen += cut
			flush()
			line = strings.TrimLeft(string(r[cut:]), " ")
		}
		n := utf8.RuneCountInString(line) + 1
		if curLen+n > budget {
			flush()
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
		curLen += n
		if fence.MatchString(line) {
			inFence = !inFence
		}
	}
	if inFence {
		// The text itself left a block open; do not invent a close.
		inFence = false
	}
	flush()
	return chunks
}
