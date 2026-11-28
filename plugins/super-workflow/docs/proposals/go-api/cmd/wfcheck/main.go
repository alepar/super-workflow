// wfcheck is a development-only checker binary. It is not the shipped workflow runner.
package main

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/feature/checker"
	tourchecker "github.com/alepar/super-workflow/docs/proposals/go-api/examples/tour/checker"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func main() {
	wf.MainWith(wf.MainConfig{Defs: []wf.Registration{tourchecker.Registration(), checker.Registration()}})
}
