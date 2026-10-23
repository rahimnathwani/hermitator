package wamd

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
)

var (
	listItem   = regexp.MustCompile(`^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$`)
	rule       = regexp.MustCompile(`^\s{0,3}(-(\s*-){2,}|\*(\s*\*){2,}|_(\s*_){2,})\s*$`)
	quoteLine  = regexp.MustCompile(`^\s{0,3}>\s?(.*)$`)
	italicUndr = regexp.MustCompile(`(^|[^\w])_([^_\s](?:[^_\n]*?[^_\s])?)_($|[^\w])`)
	taskBox    = regexp.MustCompile(`^\[([ xX])\]\s+`)
)

// HTML renders the Markdown that agents write as an HTML fragment, for
// turning a reply into an image. It covers the subset Format does, plus
// the structure WhatsApp cannot show: tables as tables, nested lists,
// quotes and rules. Everything is escaped; images become their alt text
// and links their text, since nothing in the result can be clicked.
func HTML(md string) string {
	var b strings.Builder
	writeBlocks(&b, strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n"))
	return b.String()
}

func writeBlocks(b *strings.Builder, lines []string) {
	var para []string
	flush := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + strings.Join(para, "<br>") + "</p>\n")
			para = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case fence.MatchString(line):
			flush()
			var code []string
			for i++; i < len(lines) && !fence.MatchString(lines[i]); i++ {
				code = append(code, html.EscapeString(lines[i]))
			}
			b.WriteString("<pre><code>" + strings.Join(code, "\n") + "</code></pre>\n")
		case tableLength(lines[i:]) > 0:
			flush()
			n := tableLength(lines[i:])
			writeTable(b, lines[i:i+n])
			i += n - 1
		case heading.MatchString(line):
			flush()
			level := len(strings.TrimLeft(line, " ")) - len(strings.TrimLeft(strings.TrimLeft(line, " "), "#"))
			fmt.Fprintf(b, "<h%d>%s</h%d>\n", level, inlineHTML(heading.FindStringSubmatch(line)[1]), level)
		case rule.MatchString(line):
			flush()
			b.WriteString("<hr>\n")
		case quoteLine.MatchString(line):
			flush()
			var inner []string
			for ; i < len(lines) && quoteLine.MatchString(lines[i]); i++ {
				inner = append(inner, quoteLine.FindStringSubmatch(lines[i])[1])
			}
			i--
			b.WriteString("<blockquote>\n")
			writeBlocks(b, inner)
			b.WriteString("</blockquote>\n")
		case listItem.MatchString(line):
			flush()
			n := listLength(lines[i:])
			writeList(b, lines[i:i+n])
			i += n - 1
		default:
			para = append(para, inlineHTML(strings.TrimSpace(line)))
		}
	}
	flush()
}

// listLength is the number of lines of the list starting at lines[0]:
// items, and lines indented under them, up to a blank line that is not
// followed by more of the list.
func listLength(lines []string) int {
	n := 0
	for n < len(lines) {
		line := lines[n]
		switch {
		case listItem.MatchString(line):
		case strings.TrimSpace(line) == "":
			if n+1 < len(lines) && (listItem.MatchString(lines[n+1]) || indent(lines[n+1]) >= 2) {
				break
			}
			return n
		case indent(line) >= 2:
		default:
			return n
		}
		n++
	}
	return n
}

func indent(line string) int {
	return len(strings.ReplaceAll(line, "\t", "    ")) - len(strings.TrimLeft(strings.ReplaceAll(line, "\t", "    "), " "))
}

// writeList writes a list, nesting items indented under others.
func writeList(b *strings.Builder, lines []string) {
	first := listItem.FindStringSubmatch(lines[0])
	base := indent(lines[0])
	ordered := first[2][0] >= '0' && first[2][0] <= '9'
	if ordered {
		start, _ := strconv.Atoi(strings.TrimRight(first[2], ".)"))
		if start != 1 {
			fmt.Fprintf(b, "<ol start=\"%d\">\n", start)
		} else {
			b.WriteString("<ol>\n")
		}
	} else {
		b.WriteString("<ul>\n")
	}
	for i := 0; i < len(lines); {
		m := listItem.FindStringSubmatch(lines[i])
		if m == nil || indent(lines[i]) > base {
			i++ // only reached for stray lines; nested ones are taken below
			continue
		}
		text := m[3]
		box := ""
		if t := taskBox.FindStringSubmatch(text); t != nil {
			box = "☐ "
			if t[1] != " " {
				box = "☑ "
			}
			text = text[len(t[0]):]
		}
		b.WriteString("<li>" + box + inlineHTML(text))
		j := i + 1
		var sub []string
		for ; j < len(lines); j++ {
			if listItem.MatchString(lines[j]) && indent(lines[j]) <= base {
				break
			}
			sub = append(sub, lines[j])
		}
		// Continuation lines join the item; indented items nest.
		for k := 0; k < len(sub); k++ {
			if listItem.MatchString(sub[k]) {
				writeList(b, trimBlank(sub[k:]))
				break
			}
			if t := strings.TrimSpace(sub[k]); t != "" {
				b.WriteString("<br>" + inlineHTML(t))
			}
		}
		b.WriteString("</li>\n")
		i = j
	}
	if ordered {
		b.WriteString("</ol>\n")
	} else {
		b.WriteString("</ul>\n")
	}
}

func trimBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func writeTable(b *strings.Builder, lines []string) {
	header := tableCells(lines[0])
	var align []string
	for _, c := range tableCells(lines[1]) {
		switch {
		case strings.HasPrefix(c, ":") && strings.HasSuffix(c, ":"):
			align = append(align, ` style="text-align:center"`)
		case strings.HasSuffix(c, ":"):
			align = append(align, ` style="text-align:right"`)
		default:
			align = append(align, "")
		}
	}
	at := func(i int) string {
		if i < len(align) {
			return align[i]
		}
		return ""
	}
	b.WriteString("<table>\n<thead><tr>")
	for i, c := range header {
		fmt.Fprintf(b, "<th%s>%s</th>", at(i), inlineHTML(c))
	}
	b.WriteString("</tr></thead>\n<tbody>\n")
	for _, line := range lines[2:] {
		cells := tableCells(line)
		b.WriteString("<tr>")
		for i := range header {
			c := ""
			if i < len(cells) {
				c = cells[i]
			}
			fmt.Fprintf(b, "<td%s>%s</td>", at(i), inlineHTML(c))
		}
		b.WriteString("</tr>\n")
	}
	b.WriteString("</tbody>\n</table>\n")
}

// inlineHTML renders one line's inline Markdown, escaped.
func inlineHTML(line string) string {
	// Odd segments are inside `inline code`.
	parts := strings.Split(line, "`")
	if len(parts)%2 == 0 {
		// An unbalanced backtick is a literal one.
		parts[len(parts)-2] += "`" + parts[len(parts)-1]
		parts = parts[:len(parts)-1]
	}
	for i, s := range parts {
		s = html.EscapeString(s)
		if i%2 == 1 {
			parts[i] = "<code>" + s + "</code>"
			continue
		}
		s = image.ReplaceAllString(s, "[$1]")
		s = link.ReplaceAllString(s, "<u>$1</u>")
		s = boldStar.ReplaceAllString(s, "<strong>$1</strong>")
		s = boldUnder.ReplaceAllString(s, "<strong>$1</strong>")
		s = italicStar.ReplaceAllString(s, "$1<em>$2</em>$3")
		s = italicUndr.ReplaceAllString(s, "$1<em>$2</em>$3")
		s = strike.ReplaceAllString(s, "<del>$1</del>")
		parts[i] = s
	}
	return strings.Join(parts, "")
}

// Widest is the width in characters of the widest table in md, or of its
// longest code line: what a rendered page has to fit across.
func Widest(md string) int {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	widest, inFence := 0, false
	for i := 0; i < len(lines); i++ {
		if fence.MatchString(lines[i]) {
			inFence = !inFence
			continue
		}
		if inFence {
			widest = max(widest, len([]rune(lines[i])))
			continue
		}
		if n := tableLength(lines[i:]); n > 0 {
			widths := map[int]int{}
			for _, line := range append([]string{lines[i]}, lines[i+2:i+n]...) {
				for j, c := range tableCells(line) {
					widths[j] = max(widths[j], len([]rune(plainCell(c))))
				}
			}
			total := 0
			for _, w := range widths {
				total += w + 3
			}
			widest = max(widest, total)
			i += n - 1
		}
	}
	return widest
}
