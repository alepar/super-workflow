package wf

import "fmt"

// ContextMode is how much of the coordinator's conversation a spawned
// subagent starts with: fresh (none) or inherited (a fork of it).
//
// Owns: the closed set fresh | inherited; the issued agent instruction's
// context field, which is always fresh; and the mode recorded on each unit's
// claim from wf started --context (empty when the flag was omitted, shown as
// unrecorded; an empty mode may be filled later, a conflicting one is rejected
// like any claim conflict), which wf status reports per unit.
// Does not: let a unit or definition choose its context. Every unit is issued
// fresh; inherited exists only so a coordinator can report what it actually
// did. Nor classify units: clients decide which modes matter.
// Extend by: a harness's spawn form for fresh is one case of the per-harness
// notes in Bootstrap (wf/bootstrap.go); nothing else changes.
type ContextMode string

// Context modes.
const (
	ContextFresh     ContextMode = "fresh"
	ContextInherited ContextMode = "inherited"
)

// contextUnrecorded is how status shows a claim with no reported mode.
const contextUnrecorded = "unrecorded"

// ParseContextMode parses a wf started --context value; anything outside the
// closed set is an error.
func ParseContextMode(s string) (ContextMode, error) {
	switch m := ContextMode(s); m {
	case ContextFresh, ContextInherited:
		return m, nil
	}
	return "", fmt.Errorf("context %q is not one of fresh, inherited", s)
}

// display is the mode as status shows it: unrecorded when empty.
func (m ContextMode) display() string {
	if m == "" {
		return contextUnrecorded
	}
	return string(m)
}
