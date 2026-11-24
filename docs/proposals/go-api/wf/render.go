package wf

import (
	"fmt"
	"sort"
	"strings"
)

// Render prints the state graph for a terminal: one block per state with its
// kind line, checks, budgets, cases and labelled edges, recursing through Sub
// and Map children. It is a plain function over the shared walk (Inspect) and
// never executes a user closure. The stall annotation appears only when
// NoProgressAfter is explicitly set and lower than the attempt budget.
func (d *Def) Render() string { return renderView(Inspect(d)) }

func renderView(v DefView) string {
	var b strings.Builder
	start := "(none)"
	if len(v.States) > 0 {
		start = v.States[0].Name
	}
	fmt.Fprintf(&b, "%s v%d — %d states, start: %s\n\n", v.Name, v.Version, len(v.States), start)
	for i, s := range v.States {
		mark := "  "
		if i == 0 {
			mark = "▶ "
		}
		fmt.Fprintf(&b, "%s%-18s %s\n", mark, s.Name, kindLine(s))
		const in = "      "
		line := func(label, text string) {
			if text != "" {
				fmt.Fprintf(&b, "%s%s: %s\n", in, label, text)
			}
		}
		line("prompt", s.Prompt)
		line("inputs", strings.Join(s.Inputs, ", "))
		line("dispatch", s.Dispatch)
		line("checks", strings.Join(s.Checks, ", "))
		line("observes", strings.Join(s.Observes, ", "))
		line("done when", s.DoneWhen)
		line("presents", strings.Join(s.Presents, ", "))
		line("consumes", strings.Join(s.Consumes, ", "))
		line("produces", strings.Join(s.Produces, ", "))
		if c := s.Cases; c != nil {
			parts := make([]string, 0, len(c.Cases)+1)
			for _, kv := range c.Cases {
				target := kv[1]
				if rs, ok := c.Routes[kv[0]]; ok {
					target = "route(" + strings.Join(rs, " | ") + ")"
				}
				parts = append(parts, kv[0]+" → "+target)
			}
			if c.Otherwise != "" {
				parts = append(parts, "otherwise → "+c.Otherwise)
			}
			name := c.Enum
			if name == "" {
				name = "(open)"
			}
			line("cases "+name, strings.Join(parts, " | "))
		}
		if s.Route != nil {
			line("route", strings.Join(s.Route, " | "))
		}
		if s.Kind == "gate" {
			for _, value := range s.IntValues {
				text := fmt.Sprintf("%s [%d,%d], default proposal", value.Answer, value.Min, value.Max)
				if value.Minimum != "" {
					text += ", minimum " + value.Minimum
				}
				line("integer", text)
			}
			for _, p := range v.Policy {
				if p.Gate == s.Name {
					t := p.Answer + " when " + p.When
					if p.Park != "" {
						t += " (park: " + p.Park + ")"
					}
					line("policy", t)
				}
			}
		}
		if len(s.Exits) > 0 {
			keys := make([]string, 0, len(s.Exits))
			for k := range s.Exits {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, len(keys))
			for i, k := range keys {
				parts[i] = k + " → " + s.Exits[k]
			}
			line("exits", strings.Join(parts, ", "))
		}
		line("max visits", s.MaxVisits)
		line("holds", strings.Join(s.Locks, ", "))
		for j, e := range s.Edges {
			br := "├"
			if j == len(s.Edges)-1 {
				br = "└"
			}
			fmt.Fprintf(&b, "%s%s── %s ──▶ %s\n", in, br, edgeText(s, e), e.To)
		}
	}
	for _, c := range v.Children {
		fmt.Fprintf(&b, "  ┆ child %s\n", c.Name)
		for _, l := range strings.Split(strings.TrimRight(renderView(c), "\n"), "\n") {
			if l == "" {
				b.WriteString("\n")
			} else {
				b.WriteString("    " + l + "\n")
			}
		}
	}
	return b.String()
}

func kindLine(s StateView) string {
	switch s.Kind {
	case "agent":
		return "agent → " + s.Output
	case "command":
		l := "command " + strings.Join(s.Argv, " ")
		if s.Dir != "" {
			l += " (in " + s.Dir + ")"
		}
		if len(s.CommandKey) > 0 {
			l += " key=" + strings.Join(s.CommandKey, "/")
		}
		if s.Idempotent {
			l += " idempotent"
		}
		if s.Detached > 0 {
			l += " detached(" + s.Detached.String() + ")"
		}
		if s.Stdin != "" {
			l += " stdin=" + s.Stdin
		}
		return l
	case "compute":
		return "compute → " + s.Output
	case "gate":
		return fmt.Sprintf("gate %q [%s]", s.Question, s.Reservation)
	case "wait":
		l := "wait " + s.Cond
		if s.Poll > 0 {
			l += fmt.Sprintf(" (poll %s)", s.Poll)
		}
		return l
	case "map":
		par := ""
		if s.Parallel > 0 {
			par = fmt.Sprintf(" ×%d", s.Parallel)
		}
		return fmt.Sprintf("map over %s by %s%s join %s", s.Source, s.Key, par, s.Join)
	case "sub":
		return "sub " + s.Child
	case "terminal":
		l := "terminal ■ " + s.Outcome
		if s.PartialWhen {
			l += " (" + string(Partial) + " when not ready)"
		}
		if s.Result != "" {
			l += " → " + s.Result
		}
		return l
	}
	return s.Kind
}

// edgeText is "<label>[ [annotation]][ when <pred>][ after <timeout>][ {effects}]".
func edgeText(s StateView, e EdgeView) string {
	t := e.Label
	if t == "" {
		t = "ok"
	}
	switch {
	case e.Fallback == "exhausted" && s.Retry != nil:
		ann := "retry ≤" + s.Retry.Max
		if n := s.Retry.NoProgressAfter; n > 0 && n < s.Retry.MaxConst {
			ann += fmt.Sprintf(", stall after %d", n)
		}
		if s.Retry.Recovery {
			ann += ", then recovery offer"
		}
		t += " [" + ann + "]"
	case e.Fallback == "stalled":
		t += " [stalled]"
	}
	if e.Class != "" {
		t += " [on " + e.Class
		if b := s.Retry.backoffText(e.Class); b != "" {
			t += " after retries " + b
		}
		t += "]"
	}
	if e.When != "" {
		t += " when " + e.When
	}
	if e.Timeout > 0 {
		t += " after " + e.Timeout.String()
	}
	if len(e.Effects) > 0 {
		t += " {" + strings.Join(e.Effects, "; ") + "}"
	}
	return t
}

// backoffText is the class's schedule as "30s, 1m0s", or "" for none.
func (r *RetryView) backoffText(class string) string {
	if r == nil {
		return ""
	}
	parts := make([]string, 0, len(r.Backoff[class]))
	for _, d := range r.Backoff[class] {
		parts = append(parts, d.String())
	}
	return strings.Join(parts, ", ")
}
