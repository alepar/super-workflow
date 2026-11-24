package wftest_test

import (
	"encoding/json"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func init() {
	crashPoints[wf.JournalUntilUnitBound] = scenario(wf.JournalUntilUnitBound, simpleRun, func(h *drv) {
		h.at(wf.JournalUntilUnitBound, func() { h.must(h.e.(*wf.Engine).BindUntilUnit(h.id, wf.UntilUnitTarget{State: "crA"})) })
		// The binding append survives process death, without any new CLI flag.
		ins := h.next()
		if len(ins) != 1 || ins[0].Kind != wf.InstructionAgent {
			h.t.Fatalf("target was not offered: %+v", ins)
		}
		_, err := h.e.Submit(h.id, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`{"OK":true}`)})
		h.must(err)
		h.restart()
		status, err := h.e.(*wf.Engine).UntilUnitStatus(h.id)
		h.must(err)
		if !status.Bound || !status.Settled || status.Class != wf.Accepted {
			h.t.Fatalf("boundary did not survive restart: %+v", status)
		}
		stopped, err := h.e.Next(h.id, "")
		h.must(err)
		if len(stopped) != 1 || stopped[0].Kind != wf.InstructionStopped {
			h.t.Fatalf("boundary did not stop: %+v", stopped)
		}
		for _, record := range h.records() {
			if record.Kind == wf.JournalTransition {
				h.t.Fatal("successor ran after bound attempt")
			}
		}
	})
}
