package wf

import (
	"fmt"
	"io"
)

// preflightChecks is the ordered check list of one registration: the engine's
// model-tiers check over the served definition, then the client's checks.
func preflightChecks(reg *Registration) []PreflightCheck {
	return append([]PreflightCheck{modelTiersCheck(reg.Def)}, reg.Preflight...)
}

// runPreflight is the preflight runner.
//
// Owns: the order of the checks (model-tiers, then the registration's), stopping
// at the first refusal, printing it ("refused: <check>: <cause>" and
// "fix: <remedy>" on stderr) and the exit code (exitUsage, 2). A Warn refusal
// prints "warning: <check>: <cause>" and its fix line and the checks go on. It
// returns exitOK when no check refuses.
// Does not: run inside Engine.Run or after the state dir is touched (the CLI
// calls it on the creation path only, before the engine exists), resolve the
// harness (its caller does), or know any check: a new check is a
// PreflightCheck value in a registration or modelTiersCheck, never code here.
func runPreflight(reg *Registration, ctx PreflightContext, stderr io.Writer) int {
	for _, c := range preflightChecks(reg) {
		if r := c.Run(ctx); r != nil && r.Warn {
			fmt.Fprintf(stderr, "warning: %s: %s\nfix: %s\n", c.Name, r.Cause, r.Remedy)
		} else if r != nil {
			fmt.Fprintf(stderr, "refused: %s: %s\nfix: %s\n", c.Name, r.Cause, r.Remedy)
			return exitUsage
		}
	}
	return exitOK
}
