package wftest_test

import (
	"strconv"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func init() {
	for _, kind := range []wf.JournalKind{
		wf.JournalCoordinatorAttached,
		wf.JournalCoordinatorDetached,
		wf.JournalCoordinatorHandoff,
		wf.JournalCoordinatorContinued,
		wf.JournalCoordinatorStopStalled,
	} {
		crashPoints[kind] = scenario(kind, coordinatorCrashRun, func(h *drv) {
			e := func() *wf.Engine { return h.e.(*wf.Engine) }
			co := wf.Coordinator{Harness: "claude", Session: "main"}
			attach := func() {
				gen, err := e().AttachCoordinator(h.id, co)
				h.must(err)
				co.ExpectedAdmission = gen
			}
			if kind == wf.JournalCoordinatorAttached {
				h.at(kind, attach)
			}
			attach()
			switch kind {
			case wf.JournalCoordinatorDetached:
				h.at(kind, func() { h.must(e().DetachCoordinator(h.id, co, co.ExpectedAdmission)) })
				previous := co.ExpectedAdmission
				attach()
				if co.ExpectedAdmission == previous {
					h.t.Fatal("a detached generation was reused")
				}
			case wf.JournalCoordinatorHandoff:
				offer := func() {
					instructions, err := e().HandOutAs(h.id, 1, co)
					h.must(err)
					if len(instructions) != 1 || instructions[0].HandoffReceipt == 0 {
						h.t.Fatalf("handoff was not offered: %+v", instructions)
					}
				}
				h.at(kind, offer)
				offer()
				if got := kindCount(h.records(), kind); got != 1 {
					h.t.Fatalf("handoff was issued %d times after redelivery", got)
				}
			case wf.JournalCoordinatorContinued, wf.JournalCoordinatorStopStalled:
				count := 1
				if kind == wf.JournalCoordinatorStopStalled {
					count = 6
				}
				event := strconv.Itoa(count)
				record := func(id string) {
					_, err := e().RecordCoordinatorStop(h.id, co, id, "")
					h.must(err)
				}
				for i := 1; i < count; i++ {
					record(strconv.Itoa(i))
				}
				h.at(kind, func() { record(event) })
				record(event)
				if got := kindCount(h.records(), kind); got != 1 {
					h.t.Fatalf("%s was recorded %d times for one event", kind, got)
				}
			}
			h.must(e().Answer(h.id, "crTurnGate", "yes"))
			_, err := e().HandOutAs(h.id, 1, co)
			h.must(err)
		})
	}
}

type crTurnGate struct{}

func coordinatorCrashRun(t *testing.T, crash bool) *drv {
	d := wf.New("cr-turn")
	gate := wf.NewGate[crTurnGate, string](d, "continue?", wf.NewEnum(d, "answer", "yes"), wf.Reserved)
	end := wf.NewTerminal[crEnd](d, wf.Success)
	gate.Answers(wf.On("yes", gate.To(end, "approved")))
	return newDrv(t, crash, d, nil, func(opts *wf.EngineOptions) { opts.Harness = "claude" })
}
