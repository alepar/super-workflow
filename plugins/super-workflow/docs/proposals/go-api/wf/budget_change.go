package wf

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// BudgetChange is a run --budget or resume --budget value: the limits it
// names, each replacing that limit of the budget it changes; a limit it does
// not name is kept. A named zero removes that limit.
type BudgetChange struct {
	Turns, Tokens *int
	Wall          *time.Duration
}

// ParseBudget parses "turns=N,tokens=N,wall=D": each key at most once, N a
// non-negative integer, D a Go duration (90m, 2h). An empty string is no
// change.
func ParseBudget(s string) (BudgetChange, error) {
	var b BudgetChange
	if strings.TrimSpace(s) == "" {
		return b, nil
	}
	for kv := range strings.SplitSeq(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok || v == "" {
			return BudgetChange{}, fmt.Errorf("--budget %q: %q is not key=value (turns=N,tokens=N,wall=D)", s, kv)
		}
		switch k {
		case "turns", "tokens":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return BudgetChange{}, fmt.Errorf("--budget %q: %s=%s is not a non-negative integer", s, k, v)
			}
			dst := &b.Turns
			if k == "tokens" {
				dst = &b.Tokens
			}
			if *dst != nil {
				return BudgetChange{}, fmt.Errorf("--budget %q: %s is given twice", s, k)
			}
			*dst = &n
		case "wall":
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return BudgetChange{}, fmt.Errorf("--budget %q: wall=%s is not a non-negative duration (90m, 2h)", s, v)
			}
			if b.Wall != nil {
				return BudgetChange{}, fmt.Errorf("--budget %q: wall is given twice", s)
			}
			b.Wall = &d
		default:
			return BudgetChange{}, fmt.Errorf("--budget %q: unknown limit %q (turns, tokens, wall)", s, k)
		}
	}
	return b, nil
}

// Over is base with the limits b names replaced.
func (b BudgetChange) Over(base RunBudget) RunBudget {
	if b.Turns != nil {
		base.Turns = *b.Turns
	}
	if b.Tokens != nil {
		base.Tokens = *b.Tokens
	}
	if b.Wall != nil {
		base.Wall = *b.Wall
	}
	return base
}

// String renders the budget as run --budget takes it; "none" for no limit.
func (b RunBudget) String() string {
	var parts []string
	if b.Turns > 0 {
		parts = append(parts, "turns="+strconv.Itoa(b.Turns))
	}
	if b.Tokens > 0 {
		parts = append(parts, "tokens="+strconv.Itoa(b.Tokens))
	}
	if b.Wall > 0 {
		parts = append(parts, "wall="+b.Wall.String())
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}
