package sink

import "strings"

// Argv is a command as its argument vector. Execute it without a shell
// (exec with the slice); Line renders it for a reader to paste into one.
// Long text goes on stdin or in a file, not in an argument. An argument
// that might hold a NUL is checked with CheckArg first.
type Argv []string

// Line is the command as one POSIX shell line whose words are exactly the
// Argv, each quoted by Quote.
func (a Argv) Line() string {
	words := make([]string, len(a))
	for i, s := range a {
		words[i] = Quote(s)
	}
	return strings.Join(words, " ")
}

// Quote is s as one POSIX shell word: unchanged when it holds only
// characters no shell treats specially, otherwise single-quoted with each
// single quote closed, escaped and reopened. A # is special only at the
// start of a word, and so is = (zsh expands =cmd to its path). Quote
// cannot carry a NUL byte; a value that might hold one goes through
// CheckArg first.
func Quote(s string) string {
	if s != "" && s[0] != '#' && s[0] != '=' && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-./:=@%+,#", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CheckArg refuses a value no command argument can carry: one holding a
// NUL byte.
func CheckArg(s string) error { return validText(s) }
