package wf

import (
	"fmt"
)

func init() {
	Subcommands = append(Subcommands, Subcommand{Name: "check", Summary: "check <definition> without a model: lint, calibration and simulation", handle: checkDefinition})
}

func checkDefinition(c *cli, _ Subcommand, args []string) int {
	if len(args) != 1 {
		return c.failf(exitUsage, "check: expected <definition>")
	}
	reg := c.registration(args[0])
	if reg == nil {
		return c.failf(exitUsage, "check: unknown definition %s", args[0])
	}
	problems := reg.Def.LintServed()
	problems = append(problems, reg.Def.LintSound()...)
	problems = append(problems, reg.Def.LintChecks()...)
	problems = append(problems, reg.Def.LintReachability()...)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(c.stderr, "lint: "+p.String())
		}
		return exitCheckFailed
	}
	if reg.Check == nil {
		return c.failf(exitUsage, "check %s: missing checker; register calibration and simulation fixtures", args[0])
	}
	if err := reg.Check(c.stdout); err != nil {
		return c.failf(exitCheckFailed, "check %s: %v", args[0], err)
	}
	fmt.Fprintf(c.stdout, "check %s: passed\n", args[0])
	return exitOK
}
