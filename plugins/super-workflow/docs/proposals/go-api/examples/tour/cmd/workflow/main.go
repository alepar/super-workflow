// Command workflow is a development runner with the tour's checker fixtures
// and the shell adapter its live commands use.
package main

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tour/checker"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func main() {
	adapters := wf.NewAdapters()
	if err := adapters.Register(shell.Runtime(shell.Config{})); err != nil {
		panic(err)
	}
	wf.MainWith(wf.MainConfig{
		Defs:     []wf.Registration{checker.Registration()},
		Adapters: adapters,
	})
}
