package wftest_test

import (
	"encoding/json"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func init() {
	kind := wf.JournalBreakpointHalted
	crashPoints[kind] = func(t *testing.T) {
		build := func(t *testing.T, crash bool) *drv {
			return newDrv(t, crash, crSimpleDef(), nil, func(o *wf.EngineOptions) {
				o.HaltBefore = []string{"crDone"}
			})
		}
		for _, cut := range []struct {
			name  string
			crash func(*drv, wf.JournalKind, func())
		}{
			{"before append", (*drv).atBefore},
			{"after append", (*drv).at},
		} {
			t.Run(cut.name, scenario(kind, build, func(h *drv) {
				a := h.next()[0]
				cut.crash(h, kind, func() {
					_, err := h.e.Submit(h.id, wf.Submission{Token: a.Token, Output: json.RawMessage(`{"OK":true}`)})
					h.must(err)
				})
				ins := h.next()
				if len(ins) != 1 || ins[0].Breakpoint == nil || ins[0].Breakpoint.State != "crDone" {
					h.t.Fatalf("next = %+v, want the breakpoint before crDone", ins)
				}
				h.must(h.e.(*wf.Engine).Resume(h.id))
			}))
		}
	}
}
