package sink

import (
	"errors"
	"fmt"
	"strings"
)

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

// ErrNotLiteral is the refusal of a shell line Split cannot read without
// running a shell: an unquoted operator other than &&, an expansion ($ or a
// backtick), an unquoted line break, or an unclosed quote.
var ErrNotLiteral = errors.New("not a line of literal shell words")

// Split is the words of line, one POSIX shell command of literal words: the
// inverse of Argv.Line, Split(a.Line()) == a for every Argv a without a NUL.
// It reads what a shell would: bare characters, '…' runs, "…" runs (where
// \ escapes only $ ` " \ and a line break), and \x outside quotes. A
// backslash before a line break continues the line and is dropped, as in a
// shell. An unquoted # that begins a word starts a comment to the end of the
// line. A glob character or a leading ~ stays literal: a reader asks which
// command a line names, not which files it would touch. Any other operator,
// && too, is ErrNotLiteral; SplitCommands reads a chain joined by &&.
func Split(line string) ([]string, error) {
	cmds, err := SplitCommands(line)
	switch {
	case err != nil:
		return nil, err
	case len(cmds) > 1:
		return nil, fmt.Errorf("%w: an unquoted &&", ErrNotLiteral)
	case len(cmds) == 0:
		return nil, nil
	}
	return cmds[0], nil
}

// SplitCommands is Split for a chain of commands joined by unquoted &&: the
// words of each command in order. A chain with an empty command (a leading,
// trailing or doubled &&) is ErrNotLiteral. A NUL is ErrNUL.
func SplitCommands(line string) ([][]string, error) {
	if err := validText(line); err != nil {
		return nil, err
	}
	refuse := func(why string) ([][]string, error) {
		return nil, fmt.Errorf("%w: %s", ErrNotLiteral, why)
	}
	var cmds [][]string
	var words []string
	var word strings.Builder
	started := false
	endWord := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch c {
		case '\\':
			if i+1 == len(line) {
				return refuse("a trailing backslash")
			}
			i++
			if line[i] == '\n' {
				continue // a continuation
			}
			word.WriteByte(line[i])
			started = true
		case '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				return refuse("an unclosed single quote")
			}
			word.WriteString(line[i+1 : i+1+j])
			i += j + 1
			started = true
		case '"':
			closed := false
			for i++; i < len(line); i++ {
				d := line[i]
				if d == '"' {
					closed = true
					break
				}
				switch d {
				case '$', '`':
					return refuse("an expansion inside double quotes")
				case '\\':
					if i+1 < len(line) && strings.IndexByte("$`\"\\\n", line[i+1]) >= 0 {
						i++
						if line[i] == '\n' {
							continue
						}
						d = line[i]
					}
				}
				word.WriteByte(d)
			}
			if !closed {
				return refuse("an unclosed double quote")
			}
			started = true
		case ' ', '\t':
			endWord()
		case '#':
			if !started {
				i = len(line) // a comment
				continue
			}
			word.WriteByte(c)
		case '&':
			if i+1 == len(line) || line[i+1] != '&' {
				return refuse("an unquoted &")
			}
			endWord()
			if len(words) == 0 {
				return refuse("an empty command before &&")
			}
			cmds, words = append(cmds, words), nil
			i++
		case '\n', '\r', ';', '|', '<', '>', '(', ')', '$', '`':
			return refuse(fmt.Sprintf("an unquoted %q", c))
		default:
			word.WriteByte(c)
			started = true
		}
	}
	endWord()
	if len(words) == 0 {
		if len(cmds) > 0 {
			return refuse("an empty command after &&")
		}
		return nil, nil
	}
	return append(cmds, words), nil
}
