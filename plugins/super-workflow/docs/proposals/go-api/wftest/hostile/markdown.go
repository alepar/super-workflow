package hostile

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Readable is what a text format reads back for s: each NUL and each byte of
// an invalid UTF-8 sequence as U+FFFD, byte by byte as encoding/json reads
// them.
func Readable(s string) string {
	s = strings.ReplaceAll(s, "\x00", "\uFFFD")
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			b.WriteRune(utf8.RuneError)
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// ReadInline reads Markdown inline source back to its text the way a
// CommonMark parser does for the constructs sink.Text, sink.Code and
// sink.Link write: backslash escapes, numeric character references and code
// spans. It refuses any syntax left unescaped (a line break, emphasis, a
// raw bracket or angle bracket, an entity it does not decode, a block marker
// at the start, edge whitespace a paragraph strips), so a value that reads
// back equal was carried as literal text.
//
// It is an oracle for those constructs, not a CommonMark parser, and it
// shares the writer's model of the syntax. It does not model: a heading's
// closing # sequence (an ATX heading strips a trailing run of # after a
// space), link destinations (a test reads those itself: backslash escapes
// and character references decode there too), and GitHub's extended
// autolinks (www.example.com becomes a link, its text unchanged). Tests of a
// renderer that places an Inline in one of those positions check them
// explicitly.
func ReadInline(md string) (string, error) {
	if strings.ContainsAny(md, "\r\n") {
		return "", fmt.Errorf("inline %q holds a line break", md)
	}
	if md != "" && strings.ContainsRune("#-+=>*", rune(md[0])) {
		return "", fmt.Errorf("inline %q starts with a block marker", md)
	}
	if n := len(md) - len(strings.TrimLeft(md, "0123456789")); n > 0 && n < len(md) && strings.ContainsRune(".)", rune(md[n])) {
		return "", fmt.Errorf("inline %q starts with an ordered list marker", md)
	}
	if strings.HasPrefix(md, " ") || strings.HasSuffix(md, " ") || strings.HasPrefix(md, "\t") {
		return "", fmt.Errorf("inline %q has edge whitespace a paragraph strips", md)
	}
	var b strings.Builder
	for i := 0; i < len(md); i++ {
		c := md[i]
		switch {
		case c == '\\' && i+1 < len(md) && isPunct(md[i+1]):
			b.WriteByte(md[i+1])
			i++
		case c == '&':
			end := strings.IndexByte(md[i:], ';')
			if end < 0 || !strings.HasPrefix(md[i:], "&#") {
				return "", fmt.Errorf("inline %q: raw & at %d", md, i)
			}
			n, err := strconv.Atoi(md[i+2 : i+end])
			if err != nil {
				return "", fmt.Errorf("inline %q: bad reference at %d", md, i)
			}
			b.WriteRune(rune(n))
			i += end
		case c == '`':
			run := len(md[i:]) - len(strings.TrimLeft(md[i:], "`"))
			rest := md[i+run:]
			end := closingFence(rest, run)
			if end < 0 {
				return "", fmt.Errorf("inline %q: unclosed code span", md)
			}
			content := rest[:end]
			if len(content) >= 2 && content[0] == ' ' && content[len(content)-1] == ' ' && strings.Trim(content, " ") != "" {
				content = content[1 : len(content)-1]
			}
			b.WriteString(content)
			i += run + end + run - 1
		case strings.IndexByte("*_[]<>!~\\", c) >= 0:
			return "", fmt.Errorf("inline %q: unescaped %q at %d", md, c, i)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// ReadRow splits one GitHub table row into its cells the way GitHub does
// (on each | not escaped as \|, then removing that escape and trimming), and
// reads each cell with ReadInline.
func ReadRow(row string) ([]string, error) {
	if !strings.HasPrefix(row, "|") || !strings.HasSuffix(row, "|") || strings.ContainsAny(row, "\r\n") {
		return nil, fmt.Errorf("row %q is not one table line", row)
	}
	var cells []string
	var cur strings.Builder
	for i := 1; i < len(row); i++ {
		if row[i] == '\\' && i+1 < len(row) && row[i+1] == '|' {
			cur.WriteByte('|')
			i++
			continue
		}
		if row[i] == '|' {
			// GitHub trims ASCII whitespace only; an NBSP stays.
			text, err := ReadInline(strings.Trim(cur.String(), " \t\v\f"))
			if err != nil {
				return nil, err
			}
			cells = append(cells, text)
			cur.Reset()
			continue
		}
		cur.WriteByte(row[i])
	}
	return cells, nil
}

// closingFence is the index in s of a backtick run exactly n long.
func closingFence(s string, n int) int {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		run := len(s[i:]) - len(strings.TrimLeft(s[i:], "`"))
		if run == n {
			return i
		}
		i += run
	}
	return -1
}

func isPunct(c byte) bool { return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", c) >= 0 }
