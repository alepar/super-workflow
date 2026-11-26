// The engine binary: framework + the engine example definitions.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/agentless"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/approval"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/broaden"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/checkrepair"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/critique"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/dispatch"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/escalation"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/fanout"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/feature"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/gatedchain"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/noprogress"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/orchestrator"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/park"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/resumable"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/router"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/stale"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tour"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tournament"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/typedexit"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/unavailable"
	"github.com/alepar/super-workflow/docs/proposals/go-api/fsx"
	"github.com/alepar/super-workflow/docs/proposals/go-api/gh"
	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func main() {
	defs := []*wf.Def{
		agentless.Definition(),
		approval.Definition(),
		broaden.Definition(),
		checkrepair.Definition(),
		critique.Definition(),
		dispatch.Definition(),
		escalation.Definition(),
		fanout.Definition(),
		feature.Definition(),
		gatedchain.Definition(),
		noprogress.Definition(),
		orchestrator.Definition(),
		park.Definition(),
		resumable.Definition(),
		router.Definition(),
		stale.Definition(),
		tour.Definition(),
		tournament.Definition(),
		typedexit.Definition(),
		unavailable.Definition(),
	}
	regs := make([]wf.Registration, len(defs))
	for i, d := range defs {
		regs[i] = wf.Registration{Def: d}
		if d.Name() == "feature" {
			regs[i] = feature.Registration()
		}
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "locating the running binary: %v\n", err)
		os.Exit(1)
	}
	wf.MainWith(wf.MainConfig{
		Defs:         regs,
		Adapters:     registry(),
		EffectRunner: shell.DetachedRunner(exe),
		Extra: map[string]func(args []string, stdout, stderr io.Writer) int{
			shell.DetachedVerb: shell.RunDetached,
		},
	})
}

// registry is the adapter registry of the engine binary: one runtime per tool,
// registered once here and nowhere else.
func registry() *wf.Adapters {
	a := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{
		fsx.Runtime(fsx.Config{}),
		shell.Runtime(shell.Config{}),
		git.Runtime(git.Config{}),
		gh.Runtime(gh.Config{}),
		bd.Runtime(bd.Config{}),
	} {
		if err := a.Register(rt); err != nil {
			panic(err)
		}
	}
	return a
}
