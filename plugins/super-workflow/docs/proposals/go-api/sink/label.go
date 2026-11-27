package sink

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Label is a bd label or key-label value. bd parses a --label or --labels
// value as CSV and trims each field of Unicode whitespace, so the
// constructor percent-encodes what that would split, unquote or trim: %
// itself, a comma, a double quote, CR, LF, and each whitespace rune
// (unicode.IsSpace) at either end, byte by byte. A value with none of them
// is unchanged. Several labels go to bd as Labels(...), never joined by hand.
type Label struct{ s string }

// String is the label as bd stores it.
func (l Label) String() string { return l.s }

// ErrEmptyLabel is the refusal of an empty label, which bd drops.
var ErrEmptyLabel = errors.New("empty bd label")

// NewLabel encodes s as one bd label. It refuses an empty value and one
// holding a NUL byte.
func NewLabel(s string) (Label, error) {
	if s == "" {
		return Label{}, ErrEmptyLabel
	}
	if err := validText(s); err != nil {
		return Label{}, err
	}
	// The edges are the leading and trailing runs of whitespace runes.
	lead := len(s) - len(strings.TrimLeftFunc(s, unicode.IsSpace))
	trail := len(s) - len(strings.TrimRightFunc(s, unicode.IsSpace))
	if lead == len(s) {
		trail = 0
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		edge := i < lead || i >= len(s)-trail
		if edge || c == '%' || c == ',' || c == '"' || c == '\r' || c == '\n' {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return Label{b.String()}, nil
}

// MustLabel is NewLabel for a constant the author wrote; it panics on a
// refused value.
func MustLabel(s string) Label {
	l, err := NewLabel(s)
	if err != nil {
		panic(fmt.Sprintf("sink.MustLabel(%q): %v", s, err))
	}
	return l
}

// Labels is the labels as one bd --labels value.
func Labels(ls ...Label) string {
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = l.s
	}
	return strings.Join(parts, ",")
}
