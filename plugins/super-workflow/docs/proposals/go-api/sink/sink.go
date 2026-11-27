// Package sink holds the types a value passes through before it reaches a
// format with its own syntax: a shell line, Markdown, a bd label, a path
// component. Each constructor escapes or refuses at construction, so a sink
// function that accepts only these types cannot receive a value assembled
// by string concatenation.
//
// Model output is untrusted input: it goes through the same constructors.
// JSON has no type here: it is written only through encoding/json.
//
// The package has no dependencies, so the engine's adapters and workflow
// modules can all import it.
package sink

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// ErrNUL is the refusal of a value holding a NUL byte where the format
// cannot carry one: an argument, a path or a label.
var ErrNUL = errors.New("value holds a NUL byte")

// validText refuses what no argument, path or label can carry.
func validText(s string) error {
	if strings.IndexByte(s, 0) >= 0 {
		return ErrNUL
	}
	return nil
}

// toValidUTF8 replaces each byte of an invalid UTF-8 sequence (including an
// encoded unpaired surrogate) with U+FFFD, byte by byte as encoding/json
// does, so a value reads the same in Markdown and in JSON. Text formats
// cannot carry those bytes.
func toValidUTF8(s string) string {
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
