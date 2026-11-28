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

	// heartbeat's operator form: a person releases a claim whose child is
	// gone, from a plain shell, when no coordinator is left to answer.
	row, _ := findRow("heartbeat")
	normal := row.handle
	row.Summary = "operator-only died <name> or died --session <id>: lose a claim whose child is gone as an orphan, spending no retry"
	row.handle = func(c *cli, row Subcommand, args []string) int {
		if harness.Detect(c.env) != "generic" || childCaller(c.env) {
			return c.failf(exitRejected, "dev %s: reserved for a person; a coordinator answers a stall with heartbeat", row.Name)
		}
		c.operatorChild = true
		return normal(c, row, args)
	}
	DevSubcommands = append(DevSubcommands, row)
}

func (c *cli) refuseStarted() int {
	return c.failf(exitRejected, "started: reserved for a person; %s", childCommandFix)
}
