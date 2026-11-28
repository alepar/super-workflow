package wf

import (
	"flag"
	"fmt"
)

func init() {
	DevSubcommands = append(DevSubcommands, Subcommand{
		Name:    "harvest",
		Flags:   []string{"--state-dir dir", "--workspace dir", "--into dir"},
		Summary: "save accepted agent results from <run> as corpus entries",
		handle:  (*cli).drive,
		subject: "run id",
		define:  func(fs *flag.FlagSet, f *flags) { fs.StringVar(&f.output, "into", "", "destination corpus directory") },
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			if f.output == "" {
				return c.failf(exitUsage, "dev harvest: --into is required")
			}
			j := c.budgeted(rootedFileJournal(stateDir, c.workspace))
			unlock, err := j.Lock(run)
			if err != nil {
				return c.fromError("dev harvest", err)
			}
			recs, err := j.Records(run)
			unlock()
			if err != nil {
				return c.fromError("dev harvest", err)
			}
			entries, err := Harvest(recs, f.output)
			if err != nil {
				return c.fromError("dev harvest", err)
			}
			fmt.Fprintf(c.stdout, "harvested %d accepted agent results into %s\n", len(entries), f.output)
			return exitOK
		},
	})
}
