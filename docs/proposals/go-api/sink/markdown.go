package sink

import (
	"fmt"
	"strconv"
	"strings"
)

// Markdown is raw Markdown: text whose syntax is meant. It is a distinct
// type so that text becomes Markdown only by a deliberate conversion
// (Markdown(s) for a constant or a template the author wrote) or through
// Inline, whose constructors escape.
type Markdown string

// Inline is Markdown inline content that reads back as exactly the text it
// was built from. It is always one line, so it cannot start a block, end a
// list item or add a table row wherever it is placed. Build it with Text,
// Code or Link; the zero value is empty text.
type Inline struct{ md string }

// String is the Inline's Markdown source.
func (i Inline) String() string { return i.md }

// Markdown is the Inline as raw Markdown.
func (i Inline) Markdown() Markdown { return Markdown(i.md) }

// Text is s as literal inline text. It escapes what Markdown would read as
// syntax: backslash escapes for inline punctuation, a block marker at the
// start, and character references for line breaks, tabs and edge spaces, so
// a value with newlines stays on one line and parses back with them. Invalid
// UTF-8 and NUL read back as U+FFFD, as in every Markdown parser.
func Text(s string) Inline {
	s = toValidUTF8(strings.ReplaceAll(s, "\x00", "�"))
	var b strings.Builder
	start := true
	// Leading and trailing spaces are stripped from a paragraph; keep them.
	lead := len(s) - len(strings.TrimLeft(s, " "))
	trail := len(s) - len(strings.TrimRight(s, " "))
	if lead == len(s) {
		trail = 0
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' && (i < lead || i >= len(s)-trail):
			b.WriteString("&#32;")
		case c == '\n':
			b.WriteString("&#10;")
		case c == '\r':
			b.WriteString("&#13;")
		case c < ' ' || c == 0x7f:
			// A tab or other control byte: a paragraph trims some of them
			// at its edges and a tab can indent, so write each as a
			// reference.
			b.WriteString("&#" + strconv.Itoa(int(c)) + ";")
		case strings.IndexByte("\\`*_[]<>&~!", c) >= 0:
			b.WriteByte('\\')
			b.WriteByte(c)
		case start && strings.IndexByte("#-+=", c) >= 0:
			// A block marker or a table/definition line at the start of a
			// line: the Inline may be placed there.
			b.WriteByte('\\')
			b.WriteByte(c)
		case start && c >= '0' && c <= '9':
			// An ordered list marker: digits then . or ).
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			b.WriteString(s[i:j])
			if j < len(s) && (s[j] == '.' || s[j] == ')') {
				b.WriteByte('\\')
				b.WriteByte(s[j])
				j++
			}
			i = j - 1
		default:
			b.WriteByte(c)
		}
		start = false
	}
	return Inline{b.String()}
}

// Code is s as an inline code span, fenced by more backticks than any run
// in s. A code span cannot carry a line break, so a value with one is Text
// instead: the value stays intact, only the monospace is lost.
func Code(s string) Inline {
	s = toValidUTF8(strings.ReplaceAll(s, "\x00", "�"))
	if s == "" || strings.ContainsAny(s, "\r\n") {
		return Text(s)
	}
	run, longest := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	// One space on each side is stripped when both are there, so pad when
	// the content starts or ends with a backtick or a space.
	if s[0] == '`' || s[len(s)-1] == '`' || s[0] == ' ' && s[len(s)-1] == ' ' && strings.Trim(s, " ") != "" {
		s = " " + s + " "
	}
	return Inline{fence + s + fence}
}

// Command is a command line as Markdown a reader copies it from: an inline
// code span (Code) when it is one line, otherwise a fenced code block on
// lines of its own, fenced by more backticks than any run in it, so a line
// break in the command cannot end the span or the block early. It is for
// text the program writes whole, such as a report or a status line, not for
// a value bound into a template. The block starts and ends with a line
// break, so it can follow text on a top-level line; its fence is at column
// 0, so inside a list item or a block quote it ends that container. An
// empty command is empty.
func Command(line string) Markdown {
	line = toValidUTF8(strings.ReplaceAll(line, "\x00", "�"))
	if !strings.ContainsAny(line, "\r\n") {
		return Code(line).Markdown()
	}
	run, fence := 0, 3
	for i := 0; i < len(line); i++ {
		if line[i] == '`' {
			run++
			fence = max(fence, run+1)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", fence)
	return Markdown("\n" + f + "\n" + line + "\n" + f + "\n")
}

// Link is a link to url with text as its text. The destination is written
// in angle brackets. Inside them a link destination still reads backslash
// escapes and character references, so \ < > & are backslash-escaped, and
// spaces and control bytes, which it cannot hold, are percent-encoded: any
// URL stays one destination and reads back as itself.
func Link(text Inline, url string) Inline {
	return Inline{"[" + text.md + "](" + destination(url) + ")"}
}

// destination is url as an angle-bracket link destination.
func destination(url string) string {
	url = toValidUTF8(url)
	var b strings.Builder
	b.WriteByte('<')
	for i := 0; i < len(url); i++ {
		c := url[i]
		switch {
		case c <= ' ' || c == 0x7f:
			fmt.Fprintf(&b, "%%%02X", c)
		case c == '\\' || c == '<' || c == '>' || c == '&':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('>')
	return b.String()
}

// Item is a bullet list item.
func Item(text Inline) Markdown { return Markdown("- " + text.md) }

// Row is a GitHub table row. Every | in a cell, which the table splits on
// even inside a code span, is escaped; the table removes that escape before
// reading the cell.
func Row(cells ...Inline) Markdown {
	var b strings.Builder
	b.WriteString("|")
	for _, c := range cells {
		b.WriteString(" " + strings.ReplaceAll(c.md, "|", `\|`) + " |")
	}
	return Markdown(b.String())
}
