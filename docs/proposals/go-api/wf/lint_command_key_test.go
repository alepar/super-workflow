package wf

import (
	"testing"
	"time"
)

type lintCmd struct{}

// commandKeyProblems lints a one-command definition after mod shapes the command.
func commandKeyProblems(mod func(c *Command[lintCmd])) []Problem {
	d := New("command-key-internal")
	c := NewCommand[lintCmd](d, "make")
	mod(c)
	var out []Problem
	for _, p := range d.Lint() {
		if p.Rule == RuleCommandKey {
			out = append(out, p)
		}
	}
	return out
}

func TestCommandKeyDetached(t *testing.T) {
	detached := func(c *Command[lintCmd]) { c.Detached(time.Hour) }
	for _, tc := range []struct {
		name string
		mod  func(c *Command[lintCmd])
		want int
	}{
		{"detached without a Key", detached, 1},
		{"detached with a Key but neither Idempotent nor a check", func(c *Command[lintCmd]) { detached(c); c.Key("k") }, 1},
		{"detached with a Key and Idempotent", func(c *Command[lintCmd]) { detached(c); c.Key("k").Idempotent() }, 0},
		{"detached with a Key and a DoneWhen check", func(c *Command[lintCmd]) {
			detached(c)
			c.Key("k")
			DoneWhen(c, Observe[bool]("x", "done", nil), "the target exists")
		}, 0},
		{"inline without a Key", func(c *Command[lintCmd]) {}, 0},
		{"Idempotent without a Key", func(c *Command[lintCmd]) { c.Idempotent() }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandKeyProblems(tc.mod); len(got) != tc.want {
				t.Fatalf("command-key problems = %v, want %d", got, tc.want)
			}
		})
	}
}
