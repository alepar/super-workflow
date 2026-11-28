// Package form holds data text: strings with a format, each made only by
// its grammar's parser. A value of type Of[G] can only be obtained from
// Parse[G] (or from a kind's constructor such as Mint, which runs the same
// check), so a function that takes an Of[G] cannot receive a string that was
// assembled by concatenation or recovered by splitting.
//
// Prose is never parsed and has no type here. The formats with their own
// syntax that a value is written into (shell, Markdown, bd labels, path
// components) are in package sink.
//
// Every grammar is registered, and Parse refuses to run for one that is
// not, so the corpus test (formtest.CorpusTest) covers every format a module
// can parse.
//
// The package has no dependencies, so the engine and workflow modules can
// all import it.
package form

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"unicode/utf8"
)

// A Grammar names one text format and checks a candidate against it. Its
// methods must work on the zero value, and its type must be comparable (an
// empty struct is the usual shape): Parse[G] calls them on a zero G.
type Grammar interface {
	// Name is the format's stable name; it appears in errors and in the
	// corpus report.
	Name() string
	// Check returns nil when s is in the format.
	Check(s string) error
	// Samples are values in the format, used by the corpus test for the
	// round trip and as the example shown to agents. At least one.
	Samples() []string
}

// Patterned is a Grammar whose format is also a regular expression, shown
// to agents in an output schema. Check stays the authority.
type Patterned interface {
	Grammar
	Pattern() string
}

// Enumerated is a Grammar over a closed set of words, shown to agents as
// the schema's enum.
type Enumerated interface {
	Grammar
	Values() []string
}

// Of is a string known to satisfy G. The zero value is absent: no value.
type Of[G Grammar] struct{ s string }

// ErrEmpty is the refusal of an empty string. Absence is the zero Of, never
// an empty string.
var ErrEmpty = errors.New("empty value")

// ErrInvalidUTF8 is the refusal of a string that is not valid UTF-8, which
// JSON could not carry back unchanged.
var ErrInvalidUTF8 = errors.New("value is not valid UTF-8")

// Error is a refusal by a grammar's parser.
type Error struct {
	Grammar string
	Value   string
	Err     error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s %s: %v", e.Grammar, strconv.Quote(e.Value), e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Parse returns s as a value of G, or an *Error. It panics when G is not
// registered.
func Parse[G Grammar](s string) (Of[G], error) {
	var g G
	mustRegistered(g)
	if err := check(g, s); err != nil {
		return Of[G]{}, &Error{Grammar: g.Name(), Value: s, Err: err}
	}
	return Of[G]{s}, nil
}

// MustParse is Parse for a constant the author wrote; it panics on a
// refused value.
func MustParse[G Grammar](s string) Of[G] {
	v, err := Parse[G](s)
	if err != nil {
		panic("form.MustParse: " + err.Error())
	}
	return v
}

func check(g Grammar, s string) error {
	if s == "" {
		return ErrEmpty
	}
	if !utf8.ValidString(s) {
		return ErrInvalidUTF8
	}
	return g.Check(s)
}

// String is the value as written; "" when absent.
func (v Of[G]) String() string { return v.s }

// IsZero reports an absent value. encoding/json's omitzero uses it.
func (v Of[G]) IsZero() bool { return v.s == "" }

// MarshalText writes the value. An absent value has no text and is an
// error: a field that may be absent is tagged omitzero or is a pointer.
func (v Of[G]) MarshalText() ([]byte, error) {
	if v.s == "" {
		var g G
		return nil, &Error{Grammar: g.Name(), Err: errors.New("absent value has no text")}
	}
	return []byte(v.s), nil
}

// UnmarshalText parses text with G, so decoding any structure that holds an
// Of[G] validates it.
func (v *Of[G]) UnmarshalText(text []byte) error {
	p, err := Parse[G](string(text))
	if err != nil {
		return err
	}
	*v = p
	return nil
}

// Schema returns what an output schema states about G.
func (v Of[G]) Schema() Schema {
	var g G
	return schemaOf(g)
}

// Schema is what an output schema shows an agent about a grammar.
type Schema struct {
	Grammar string
	Pattern string   // "" when the grammar has none
	Values  []string // nil unless the grammar is Enumerated
	Example string
}

func schemaOf(g Grammar) Schema {
	s := Schema{Grammar: g.Name()}
	if p, ok := g.(Patterned); ok {
		s.Pattern = p.Pattern()
	}
	if e, ok := g.(Enumerated); ok {
		s.Values = append([]string(nil), e.Values()...)
	}
	if samples := g.Samples(); len(samples) > 0 {
		s.Example = samples[0]
	}
	return s
}

// Registry.

var (
	mu       sync.RWMutex
	registry = map[string]entry{}
)

type entry struct {
	g     Grammar
	probe func(s string) (string, error)
}

// Register adds G to the registry. It panics on a second grammar with the
// same name, and on one whose samples it refuses. Call it from an init
// function in the package that defines G.
func Register[G Grammar]() {
	var g G
	name := g.Name()
	if name == "" {
		panic(fmt.Sprintf("form.Register(%T): empty name", g))
	}
	mu.Lock()
	if prev, ok := registry[name]; ok {
		mu.Unlock()
		if prev.g == Grammar(g) {
			return
		}
		panic(fmt.Sprintf("form.Register(%T): name %q is taken by %T", g, name, prev.g))
	}
	registry[name] = entry{g: g, probe: probe[G]}
	mu.Unlock()
	if len(g.Samples()) == 0 {
		panic(fmt.Sprintf("form.Register(%T): no samples", g))
	}
	for _, s := range g.Samples() {
		if err := check(g, s); err != nil {
			panic(fmt.Sprintf("form.Register(%T): sample %q refused: %v", g, s, err))
		}
	}
}

func mustRegistered(g Grammar) {
	mu.RLock()
	e, ok := registry[g.Name()]
	mu.RUnlock()
	if !ok || e.g != g {
		panic(fmt.Sprintf("form: grammar %T (%q) is not registered; call form.Register from its package's init", g, g.Name()))
	}
}

// probe parses s with G and returns the text of the value, written and read
// back through every encoding Of[G] has.
func probe[G Grammar](s string) (string, error) {
	v, err := Parse[G](s)
	if err != nil {
		return "", err
	}
	text, err := v.MarshalText()
	if err != nil {
		return "", err
	}
	var back Of[G]
	if err := back.UnmarshalText(text); err != nil {
		return "", err
	}
	if back != v {
		return "", fmt.Errorf("text round trip gave %q", back.s)
	}
	return v.String(), nil
}

// Registered is one registered grammar, with its parser in a form that
// needs no type parameter.
type Registered struct {
	Grammar Grammar
	// Probe parses s and writes the value back through its text encoding;
	// it returns the resulting text, or the refusal.
	Probe func(s string) (string, error)
}

// Grammars returns every registered grammar, sorted by name.
func Grammars() []Registered {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Registered, 0, len(registry))
	for _, e := range registry {
		out = append(out, Registered{Grammar: e.g, Probe: e.probe})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Grammar.Name() < out[j].Grammar.Name() })
	return out
}
