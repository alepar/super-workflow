package wf

import "github.com/alepar/super-workflow/docs/proposals/go-api/harness"

// Explicit operator forms retain name-based repair without granting an agent
// a fallback around the child protocol. Environment identity is cooperative.
func init() {
	DevSubcommands = append(DevSubcommands, devStarted)
	// started has no agent-facing route; its operator form shares the guard.
	started := len(DevSubcommands) - 1
	normalStarted := DevSubcommands[started].handle
	DevSubcommands[started].handle = func(c *cli, row Subcommand, args []string) int {
		if harness.Detect(c.env) != "generic" || childCaller(c.env) {
			return c.refuseStarted()
		}
		c.operatorChild = true
		return normalStarted(c, row, args)
	}

	for _, name := range []string{"prompt", "validate", "done", "failed"} {
		row, _ := findRow(name)
		normal := row.handle
		row.Summary = "operator-only by-name " + name
		row.handle = func(c *cli, row Subcommand, args []string) int {
			if harness.Detect(c.env) != "generic" || childCaller(c.env) {
				return c.failf(exitRejected, "dev %s: reserved for a person; %s", row.Name, childClaimFix)
			}
			c.operatorChild = true
			return normal(c, row, args)
		}
		DevSubcommands = append(DevSubcommands, row)
	}
}

func (c *cli) refuseStarted() int {
	return c.failf(exitRejected, "started: reserved for a person; %s", childCommandFix)
}
