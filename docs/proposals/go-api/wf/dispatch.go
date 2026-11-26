package wf

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// GitAuthority is the git authority a run grants an agent state's units: the
// refs they may touch, the operations allowed and forbidden on them, and the
// name of the AGENTS.md section that states the same grant.
//
// Owns: carrying the run's git authority on an instruction (Instruction.GitAuthority),
// so the agent and an approvals reviewer read the same statement.
// Does not: compute, validate or enforce it (the client declares it per state
// with Agent.GitAuthority; the Engine only copies it), or know what any ref or
// operation means.
// Extend by: a new part of the statement is one field here; how a client builds
// or enforces it lives in the client.
// Role: data carried on an instruction.
type GitAuthority struct {
	Refs      []string `json:"refs,omitempty"`
	Allowed   []string `json:"allowed,omitempty"`
	Forbidden []string `json:"forbidden,omitempty"`
	// Section names the AGENTS.md section that states this authority.
	Section string `json:"section,omitempty"`
}

// DispatchHints are the per-agent hints of an agent state: the model, effort
// and agent type the coordinator should spawn its subagent with, and how long
// the subagent's transcript may stay quiet before it is reported stalled.
//
// Owns: the hints as definition data, the tier included. They feed render (the dispatch: line),
// the hash and RuleDispatchHint, and are copied into Instruction.Dispatch. The
// stall threshold is the one hint the Engine reads itself: it overrides the
// class default (3x the duration EWMA, else StallDefault).
// Does not: choose how a state runs (every agent instruction is "spawn a
// subagent"; Map.Parallel is a semantic limit only, the definition's own
// constraint, and throughput is the n the coordinator passes to wf next), or
// change at run time (the model map is applied by the Engine at issue time).
// Extend by: a new hint is one field here, one sealed DispatchOption and its
// lint check.
type DispatchHints struct {
	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"`
	AgentType string `json:"agentType,omitempty"`
	// Tier is the abstract model tier; the host resolves it to a model.
	Tier Tier `json:"tier,omitempty"`
	// StallAfter, when non-zero, is the state's stall threshold.
	StallAfter time.Duration `json:"stallAfter,omitempty"`
	// Family is the harness the state's units run on: empty for the
	// coordinator's own, FamilyOther, or a named harness. On an instruction
	// it is the resolved harness, set only when it is not the coordinator's.
	Family Family `json:"family,omitempty"`
}

// String is the canonical form that render and the hash read: fixed field
// order, empty fields omitted.
func (h DispatchHints) String() string {
	var parts []string
	if h.Model != "" {
		parts = append(parts, "model="+h.Model)
	}
	if h.Effort != "" {
		parts = append(parts, "effort="+h.Effort)
	}
	if h.AgentType != "" {
		parts = append(parts, "agent-type="+h.AgentType)
	}
	if h.Tier != "" {
		parts = append(parts, "tier="+string(h.Tier))
	}
	if h.StallAfter != 0 {
		parts = append(parts, "stall-after="+h.StallAfter.String())
	}
	if h.Family != "" {
		parts = append(parts, "family="+string(h.Family))
	}
	return strings.Join(parts, " ")
}

type dispatchField int

const (
	dispatchModel dispatchField = iota + 1
	dispatchEffort
	dispatchAgentType
	dispatchStallAfter
	dispatchTier
	dispatchFamily
)

// Tier is an abstract model tier.
type Tier string

const (
	TierLight    Tier = "light"
	TierStandard Tier = "standard"
	TierStrong   Tier = "strong"
	TierTop      Tier = "top"
)

var dispatchTiers = []Tier{TierLight, TierStandard, TierStrong, TierTop}

// DispatchOption sets one hint on an agent state. It is sealed: build one with
// Model, Effort, AgentType, ModelTier or StallAfter. See DispatchHints for what the hints own.
type DispatchOption struct {
	field dispatchField
	value string
	dur   time.Duration
}

// Model hints the model of the agent's subagent.
func Model(name string) DispatchOption { return DispatchOption{field: dispatchModel, value: name} }

// Effort hints the effort level: low, medium, high, xhigh or max.
func Effort(level string) DispatchOption { return DispatchOption{field: dispatchEffort, value: level} }

// AgentType hints the agent type of the agent's subagent.
func AgentType(name string) DispatchOption {
	return DispatchOption{field: dispatchAgentType, value: name}
}

// StallAfter sets the state's stall threshold and lease: a running subagent
// whose transcript has not changed for d, or that fetched its prompt and has
// shown no sign of life for d (no validate, heartbeat or re-fetch), is
// reported stalled, a question for its coordinator (never reissued). It
// overrides the class default (3x the duration EWMA, else StallDefault) and,
// for the lease, FetchedLease.
func StallAfter(d time.Duration) DispatchOption {
	return DispatchOption{field: dispatchStallAfter, dur: d}
}

// ModelTier hints the abstract model tier of the agent's subagent. (The spec's
// name Tier is taken by the Tier type.)
func ModelTier(t Tier) DispatchOption { return DispatchOption{field: dispatchTier, value: string(t)} }

// Family is the harness an agent state's units run on: a named harness
// (FamilyNamed), or FamilyOther, the run's other family for the
// coordinator's harness. A unit of another harness than the coordinator's is
// started by the engine's spawn verb as a headless process of that harness's
// CLI (harness.Headless), never on the coordinator's harness: when that
// harness is unavailable the unit settles with class Unavailable.
type Family string

// FamilyOther is the run's other family: claude for a codex coordinator and
// codex for a claude one by default (run flag --family other=H for others).
const FamilyOther Family = "other"

// FamilyNamed is the family of one named harness, in its canonical name.
func FamilyNamed(h string) Family { return Family(harness.Canonical(h)) }

// OnFamily hints the harness the agent's units run on (Family). The default,
// no hint, is the coordinator's own harness.
func OnFamily(f Family) DispatchOption {
	return DispatchOption{field: dispatchFamily, value: string(f)}
}

// hintsOf folds the options; a repeated option is a lint problem, so which of
// its values wins does not matter.
func hintsOf(opts []DispatchOption) DispatchHints {
	var h DispatchHints
	for _, o := range opts {
		switch o.field {
		case dispatchModel:
			h.Model = o.value
		case dispatchEffort:
			h.Effort = o.value
		case dispatchAgentType:
			h.AgentType = o.value
		case dispatchStallAfter:
			h.StallAfter = o.dur
		case dispatchTier:
			h.Tier = Tier(o.value)
		case dispatchFamily:
			h.Family = Family(o.value)
		}
	}
	return h
}

var dispatchEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// lintDispatch is RuleDispatchHint: an empty model or agent type, an effort
// outside the closed set, a non-positive stall threshold, a repeated option or
// a zero DispatchOption.
func lintDispatch(n *node, opts []DispatchOption) []Problem {
	var out []Problem
	seen := map[dispatchField]bool{}
	for _, o := range opts {
		if seen[o.field] {
			out = append(out, n.problem(RuleDispatchHint, "dispatch option repeated: %s", hintsOf([]DispatchOption{o})))
		}
		seen[o.field] = true
		switch o.field {
		case dispatchModel:
			if o.value == "" {
				out = append(out, n.problem(RuleDispatchHint, "empty model hint"))
			}
		case dispatchAgentType:
			if o.value == "" {
				out = append(out, n.problem(RuleDispatchHint, "empty agent type hint"))
			}
		case dispatchEffort:
			if !slices.Contains(dispatchEfforts, o.value) {
				out = append(out, n.problem(RuleDispatchHint, "effort %q is not one of %s", o.value, strings.Join(dispatchEfforts, ", ")))
			}
		case dispatchTier:
			if !slices.Contains(dispatchTiers, Tier(o.value)) {
				out = append(out, n.problem(RuleDispatchHint, "tier %q is not one of light, standard, strong, top", o.value))
			}
		case dispatchFamily:
			if why := familyProblem(Family(o.value)); why != "" {
				out = append(out, n.problem(RuleDispatchHint, "%s", why))
			}
		case dispatchStallAfter:
			if o.dur <= 0 {
				out = append(out, n.problem(RuleDispatchHint, "stall-after must be positive"))
			}
		default:
			out = append(out, n.problem(RuleDispatchHint, "zero DispatchOption"))
		}
	}
	return out
}

// familyProblem is why f is not a family a state can run on: empty, or a
// named harness with no headless launch (harness.Headless). FamilyOther is
// resolved per run and always lints.
func familyProblem(f Family) string {
	switch {
	case f == "":
		return "empty family hint"
	case f == FamilyOther:
		return ""
	case string(f) != harness.Canonical(string(f)):
		return fmt.Sprintf("family %q is not a canonical harness name (want %q)", f, harness.Canonical(string(f)))
	case harness.HeadlessFor(string(f)) == nil:
		return fmt.Sprintf("family %q: harness %s has no headless launch (have: %s)", f, f, strings.Join(headlessNames(), ", "))
	}
	return ""
}

// headlessNames are the harnesses with a headless launch.
func headlessNames() []string {
	var out []string
	for _, n := range harness.Names() {
		if harness.HeadlessFor(n) != nil {
			out = append(out, n)
		}
	}
	return out
}
