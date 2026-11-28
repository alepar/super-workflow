package wf

import (
	"errors"
	"fmt"
	"strings"
)

// Effect is something that happens when an edge is taken (counter increment,
// external write). It is sealed.
//
// Owns: the closed set of things a traversal may do; effects run when the
// transition is taken and are journaled intent then result.
// Does not: run at lint or render time.
// Extend by: adapter packages build ExternalEffect values; framework effects
// (counters) live here.
type Effect interface{ effect() string }

// Key is an idempotency key for an external effect. It is sealed: construct it
// with KeyOf or NonIdempotent; adapters take one as a positional argument.
//
// Owns: whether an effect can be checked for prior completion after a crash.
// Does not: perform the check (executor territory).
// Extend by: a new key shape is a new constructor here.
type Key interface{ key() string }

// idemKey is a key's parts, each escaped by escapeKeyPart, joined by "/"
// (joinKeyParts): splitKeyParts reads them back.
type idemKey string

// key is the parts joined unescaped: the key as a definition shows it (and
// its hash pins), and as one part's key is bound.
func (k idemKey) key() string { return legacyKeyJoin(k.parts()) }

func (k idemKey) parts() []string {
	parts, err := splitKeyParts(string(k))
	if err != nil {
		panic("wf: an effect key KeyOf did not make: " + err.Error()) // only KeyOf makes one
	}
	return parts
}

// KeyOf builds a stable key, e.g. KeyOf("fix", finding.ID). Its parts are
// bound (placeholders in them filled) one by one, and a key of two or more
// parts is their join with each part's "/" and "%" escaped (%2F, %25), so
// KeyOf("fix", "a/b") and KeyOf("fix", "a", "b") are different keys. A key
// of one part is that part, bound, as it is spelled.
func KeyOf(parts ...string) Key { return idemKey(joinKeyParts(parts)) }

// escapeKeyPart is part with each "%" written %25 and each "/" %2F: a part
// that holds neither is unchanged.
func escapeKeyPart(part string) string {
	return strings.NewReplacer("%", "%25", "/", "%2F").Replace(part)
}

// joinKeyParts is the key of parts: each escaped, joined by "/".
func joinKeyParts(parts []string) string {
	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = escapeKeyPart(p)
	}
	return strings.Join(escaped, "/")
}

// legacyKeyJoin is parts joined with no escaping: how a key was spelled
// before parts were escaped, and how a definition still shows it.
func legacyKeyJoin(parts []string) string { return strings.Join(parts, "/") }

// errKeyEscape is a joined key whose % does not begin %25 or %2F.
var errKeyEscape = errors.New("effect key: % not followed by 25 or 2F")

// splitKeyParts is the parts of a key joinKeyParts wrote.
func splitKeyParts(key string) ([]string, error) {
	var parts []string
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		switch c := key[i]; {
		case c == '/':
			parts = append(parts, b.String())
			b.Reset()
		case c != '%':
			b.WriteByte(c)
		case i+2 < len(key) && key[i+1] == '2' && key[i+2] == '5':
			b.WriteByte('%')
			i += 2
		case i+2 < len(key) && key[i+1] == '2' && key[i+2] == 'F':
			b.WriteByte('/')
			i += 2
		default:
			return nil, fmt.Errorf("%w: %q", errKeyEscape, key)
		}
	}
	return append(parts, b.String()), nil
}

// boundKey is a key as an effect intent journals it, already bound and
// joined: it is passed to the adapter as it is spelled.
type boundKey string

func (k boundKey) key() string { return string(k) }

type nonIdem string

func (k nonIdem) key() string { return "" }

// NonIdempotent declares an effect that cannot be checked for prior completion;
// after a crash it routes to a manual verification instruction instead of re-running.
func NonIdempotent(reason string) Key { return nonIdem(reason) }

// ExternalEffect is the write-side twin of Observation: a described adapter
// operation with an idempotency key.
//
// Owns: the descriptor of one external write (adapter, op, args, key).
// Does not: perform the write; adapter packages construct it with a positional
// Key, and a nil Key is a lint problem (RuleEffectKey).
// Extend by: a new operation is a constructor in an adapter package; no
// framework change.
type ExternalEffect struct {
	Adapter, Op string
	Args        map[string]any
	Key         Key
	// Outside marks an effect outside the run (a push, a merge to base): when
	// it is left uncertain a person settles it, not the coordinator. Set it
	// with Outside.
	Outside bool

	argTemplates []EffectArgTemplate
	// legacyKey is EffectData.LegacyKey of the bound effect (KeyIDs).
	legacyKey string
}

// Outside marks e as an effect outside the run: a person, not the
// coordinator, settles it when its intent has no result. Every other
// uncertain effect is the coordinator's to check and settle on evidence.
func Outside(e ExternalEffect) ExternalEffect {
	e.Outside = true
	return e
}

func (e ExternalEffect) effect() string { return e.Adapter + "." + e.Op }

// runtimeEffect marks an external effect attached by a running route.
// Its strings are data unless the caller explicitly marks templates.
type runtimeEffect struct{ ExternalEffect }

// Counter is a declared, monotone, journaled counter. A scoped counter (see
// ScopedTo) counts only the Incs since its scope counter last rose.
type Counter struct {
	name, scope   string
	def, scopeDef *Def // declaring Def of the counter and of its scope
}

// Name returns the counter name.
func (c Counter) Name() string { return c.name }

// Scope returns the name of the counter c is scoped to, "" for a plain counter.
func (c Counter) Scope() string { return c.scope }

// label is the rendered form: "name", or "name/scope" for a scoped counter.
func (c Counter) label() string {
	if c.scope == "" {
		return c.name
	}
	return c.name + "/" + c.scope
}

type incEffect struct{ c Counter }

func (i incEffect) effect() string { return "inc " + i.c.label() }

// Inc is the only way to change a counter. A plain counter never resets; a
// scoped counter is zeroed by the executor when its scope counter is
// incremented. When one edge increments a scope counter and a counter scoped
// to it, the scope Inc and the zeroing it causes apply before the scoped Inc,
// whatever the declaration order (see applyOrder).
func (c Counter) Inc() Effect { return incEffect{c} }

// applyOrder returns effs in the order the executor applies them: declaration
// order, except that an Inc of counter X moves to just before the first scoped
// Inc (of a counter scoped to X) that precedes it in the list.
func applyOrder(effs []Effect) []Effect {
	out := make([]Effect, 0, len(effs))
	done := make([]bool, len(effs))
	for i, ef := range effs {
		if done[i] {
			continue
		}
		if x, ok := ef.(incEffect); ok && x.c.scope != "" {
			for j := i + 1; j < len(effs); j++ {
				if y, ok := effs[j].(incEffect); ok && !done[j] && y.c.name == x.c.scope {
					out = append(out, effs[j])
					done[j] = true
					break
				}
			}
		}
		out = append(out, ef)
		done[i] = true
	}
	return out
}
