package form

import (
	"errors"
	"fmt"
	"strconv"
)

// Minted ids: a prefix letter and a counter, "F12". An id is minted from
// the recorded set, never derived from a title, a location or a message, and
// a reference is checked by membership in that set.

// Minted is a Grammar for ids of one prefix. Its Check is usually
// CheckMinted(g.Prefix(), s), and its Pattern MintedPattern(g.Prefix()).
type Minted interface {
	Grammar
	// Prefix is the id's leading letter, A to Z.
	Prefix() byte
}

// ErrNotMinted is the refusal of a string that is not a minted id.
var ErrNotMinted = errors.New("not a minted id")

// CheckMinted returns nil when s is prefix followed by a counter with no
// leading zero: ^P[1-9][0-9]*$.
func CheckMinted(prefix byte, s string) error {
	if prefix < 'A' || prefix > 'Z' {
		return fmt.Errorf("minted prefix %q is not a letter A to Z", prefix)
	}
	if _, ok := mintedCounter(prefix, s); !ok {
		return fmt.Errorf("%w: want %s", ErrNotMinted, MintedPattern(prefix))
	}
	return nil
}

// MintedPattern is the regular expression of ids with prefix.
func MintedPattern(prefix byte) string {
	return "^" + string(rune(prefix)) + "[1-9][0-9]*$"
}

func mintedCounter(prefix byte, s string) (int, bool) {
	if len(s) < 2 || s[0] != prefix || s[1] == '0' {
		return 0, false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s[1:])
	return n, err == nil
}

// Mint returns the next id after every id in recorded: the prefix and one
// more than the largest counter, or 1 when recorded has none. Absent values
// in recorded are ignored.
func Mint[G Minted](recorded []Of[G]) Of[G] {
	var g G
	max := 0
	for _, v := range recorded {
		if n, ok := mintedCounter(g.Prefix(), v.s); ok && n > max {
			max = n
		}
	}
	return MustParse[G](string(rune(g.Prefix())) + strconv.Itoa(max+1))
}

// Vocabularies: a closed set of words.

// ErrUnknownWord is the refusal of a word outside a vocabulary.
var ErrUnknownWord = errors.New("not a word of the vocabulary")

// CheckWord returns nil when s is one of words. An Enumerated grammar's
// Check is usually CheckWord(s, g.Values()...).
func CheckWord(s string, words ...string) error {
	for _, w := range words {
		if s == w {
			return nil
		}
	}
	return fmt.Errorf("%w %q", ErrUnknownWord, words)
}
