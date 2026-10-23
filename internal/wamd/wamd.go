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
	// tableRule is the line under a table's header: |---|:--:|.
	tableRule = regexp.MustCompile(`^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)*\|?\s*$`)
)

// placeholder stands in for WhatsApp's bold marker while italics are
// rewritten, so `**x**` does not get read as two italic markers.
const placeholder = "\x00"

// Format rewrites Markdown for WhatsApp. Code blocks and inline code are
// left alone.
func Format(md string) string {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var out []string
	inFence := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if fence.MatchString(line) {
			inFence = !inFence
			// WhatsApp does not understand language tags.
			out = append(out, strings.TrimSpace(line)[:3])
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}
		if n := tableLength(lines[i:]); n > 0 {
			out = append(out, formatTable(lines[i:i+n])...)
			i += n - 1
			continue
		}
		if m := heading.FindStringSubmatch(line); m != nil {
			line = "*" + strings.Trim(m[1], "*_ ") + "*"
		} else {
			line = bullet.ReplaceAllString(line, "$1- ")
			line = formatInline(line)
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// tableLength is the number of lines of the pipe table starting at
// lines[0], or 0 if none starts there: a header row, a rule, and rows
// until the first line without a pipe.
func tableLength(lines []string) int {
	if len(lines) < 2 || !strings.Contains(lines[0], "|") || !tableRule.MatchString(lines[1]) || len(tableCells(lines[0])) == 0 {
		return 0
	}
	n := 2
	for n < len(lines) && strings.Contains(lines[n], "|") && strings.TrimSpace(lines[n]) != "" {
		n++
	}
	return n
}

// tableCells splits a table row into its cells, trimmed.
func tableCells(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	cells := strings.Split(s, "|")
	for i, c := range cells {
		cells[i] = strings.TrimSpace(c)
	}
	return cells
}

// maxMonoWidth is the widest a monospace line can be before WhatsApp
// wraps it on a phone held upright, scrambling a table's columns.
const maxMonoWidth = 34

// formatTable renders a pipe table for a phone. Tables do not exist in
// WhatsApp, and agents write them however often they are told not to.
// Two columns read well as a list of "*key*: value" lines. Wider tables
// go in a monospace block with their columns lined up when that fits
// across a phone, and otherwise become one record per row, headed by its
// first cell, with a "Header: value" line for each other cell. /md draws
// the table itself.
func formatTable(lines []string) []string {
	header := tableCells(lines[0])
	var rows [][]string
	for _, line := range lines[2:] {
		rows = append(rows, tableCells(line))
	}
	cell := func(row []string, i int) string {
		if i < len(row) {
			return row[i]
		}
		return ""
	}
	var out []string
	switch len(header) {
	case 1:
		for _, row := range rows {
			out = append(out, "- "+formatInline(cell(row, 0)))
		}
	case 2:
		for _, row := range rows {
			key := strings.ReplaceAll(formatInline(cell(row, 0)), "*", "")
			value := formatInline(cell(row, 1))
			switch {
			case key == "":
				out = append(out, "- "+value)
			case value == "":
				out = append(out, "*"+key+"*")
			default:
				out = append(out, "*"+key+"*: "+value)
			}
		}
	default:
		all := append([][]string{header}, rows...)
		widths := make([]int, len(header))
		for _, row := range all {
			for i := range widths {
				widths[i] = max(widths[i], utf8.RuneCountInString(plainCell(cell(row, i))))
			}
		}
		total := 0
		for _, w := range widths {
			total += w + 2
		}
		if total-2 > maxMonoWidth {
			return tableRecords(header, rows)
		}
		out = append(out, "```")
		for _, row := range all {
			var b strings.Builder
			for i, w := range widths {
				c := plainCell(cell(row, i))
				if i == len(widths)-1 {
					b.WriteString(c)
				} else {
					b.WriteString(c + strings.Repeat(" ", w-utf8.RuneCountInString(c)+2))
				}
			}
			out = append(out, strings.TrimRight(b.String(), " "))
		}
		out = append(out, "```")
	}
	return out
}

// tableRecords writes each row of a table as a record: its first cell
// in bold, then a line per other cell that has something in it.
func tableRecords(header []string, rows [][]string) []string {
	var out []string
	for i, row := range rows {
		if i > 0 {
			out = append(out, "")
		}
		title := ""
		if len(row) > 0 {
			title = strings.ReplaceAll(formatInline(row[0]), "*", "")
		}
		if title != "" {
			out = append(out, "*"+title+"*")
		}
		for j := 1; j < len(header) && j < len(row); j++ {
			if row[j] == "" {
				continue
			}
			name := strings.ReplaceAll(formatInline(header[j]), "*", "")
			if name == "" {
				out = append(out, formatInline(row[j]))
			} else {
				out = append(out, name+": "+formatInline(row[j]))
			}
		}
	}
	return out
}

// plainCell strips inline markup, which a monospace block would show.
func plainCell(s string) string {
	s = image.ReplaceAllString(s, "$2")
	s = link.ReplaceAllString(s, "$1")
	for _, mark := range []string{"**", "__", "`", "~~"} {
		s = strings.ReplaceAll(s, mark, "")
	}
	return s
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
