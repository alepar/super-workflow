package wf

import "strings"

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

type idemKey string

func (k idemKey) key() string { return string(k) }

// KeyOf builds a stable key, e.g. KeyOf("fix", finding.ID).
func KeyOf(parts ...string) Key { return idemKey(strings.Join(parts, "/")) }

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
