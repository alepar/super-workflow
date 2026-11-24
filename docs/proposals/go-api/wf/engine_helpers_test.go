package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// harness is the shared engine test harness: NewEngine over MemJournal, a
// registry holding one runtime and a fixed clock.
type harness struct {
	t       *testing.T
	j       wf.Journal
	reg     *wf.Adapters
	rt      wf.AdapterRuntime
	e       wf.Executor
	now     time.Time
	results []wf.SubmitResult
	// manualRecovery leaves recovery offers to the test; by default next
	// answers each with give-up, the coordinator that takes the failure route.
	manualRecovery bool
}

// newHarness builds NewEngine over MemJournal, a registry holding rt, and a fixed clock.
func newHarness(t *testing.T, d *wf.Def, rt wf.AdapterRuntime) *harness {
	return newHarnessOpts(t, d, rt, wf.EngineOptions{})
}

func newHarnessOpts(t *testing.T, d *wf.Def, rt wf.AdapterRuntime, opts wf.EngineOptions) *harness {
	t.Helper()
	h := &harness{t: t, j: wf.MemJournal(), reg: wf.NewAdapters(), rt: rt, now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	if rt != nil {
		if err := h.reg.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	opts.Clock = func() time.Time { return h.now }
	opts.Defs = append(opts.Defs, d)
	h.e = wf.NewEngine(h.j, h.reg, opts)
	return h
}

func (h *harness) run(d *wf.Def, params map[string]string) wf.RunID {
	h.t.Helper()
	id, err := h.e.Run(d, params, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) next(run wf.RunID) []wf.Instruction {
	h.t.Helper()
	for i := 0; ; i++ {
		ins, err := h.e.Next(run, "")
		if err != nil {
			h.t.Fatal(err)
		}
		answered := false
		for _, in := range ins {
			if in.Kind == wf.InstructionRecovery && !h.manualRecovery && i < 100 {
				if err := h.e.Recover(run, in.Name, wf.RecoveryGiveUp); err != nil {
					h.t.Fatalf("give up %s: %v", in.Name, err)
				}
				answered = true
			}
		}
		if !answered {
			return withoutEnded(ins)
		}
	}
}

// giveUpOffers answers every open recovery offer of the run with give-up
// (unless manualRecovery), as the coordinator that takes the failure route
// does, so a status read after a failure shows where the route went.
func (h *harness) giveUpOffers(run wf.RunID) {
	h.t.Helper()
	if h.manualRecovery {
		return
	}
	for i := 0; i < 100; i++ {
		st, err := h.e.Status(run)
		if err != nil || st.Phase == wf.RunEnded {
			return
		}
		answered := false
		for _, r := range st.Recoveries {
			if err := h.e.Recover(run, r.Unit, wf.RecoveryGiveUp); err != nil {
				h.t.Fatalf("give up %s: %v", r.Unit, err)
			}
			answered = true
		}
		if !answered {
			return
		}
	}
}

// withoutEnded drops the single ended instruction an ended run hands out, so a
// test that loops until nothing is left to do stops on an ended run.
func withoutEnded(ins []wf.Instruction) []wf.Instruction {
	var out []wf.Instruction
	for _, in := range ins {
		if in.Kind != wf.InstructionEnded {
			out = append(out, in)
		}
	}
	return out
}

func (h *harness) status(run wf.RunID) wf.RunStatus {
	h.t.Helper()
	h.giveUpOffers(run)
	st, err := h.e.Status(run)
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func (h *harness) submit(run wf.RunID, in wf.Instruction, out string) wf.SubmitResult {
	h.t.Helper()
	res, err := h.e.Submit(run, wf.Submission{Token: in.Token, Output: json.RawMessage(out)})
	if err != nil {
		h.t.Fatal(err)
	}
	h.results = append(h.results, res)
	return res
}

func (h *harness) records(run wf.RunID) []wf.JournalRecord {
	h.t.Helper()
	unlock, err := h.j.Lock(run)
	if err != nil {
		h.t.Fatal(err)
	}
	defer unlock()
	recs, err := h.j.Records(run)
	if err != nil {
		h.t.Fatal(err)
	}
	return recs
}

func (h *harness) kind(run wf.RunID, k wf.JournalKind) []wf.JournalRecord {
	var out []wf.JournalRecord
	for _, r := range h.records(run) {
		if r.Kind == k {
			out = append(out, r)
		}
	}
	return out
}

// drive answers open agent instructions from answers (state to JSON) until no
// instruction is open, failing on an instruction it has no answer for, and
// returns the final RunStatus. It answers every exhaustion halt with stop, as
// the human of an interactive run who takes the exhausted edge would.
func (h *harness) drive(run wf.RunID, answers map[string][]string) wf.RunStatus {
	h.t.Helper()
	queue := map[string][]string{}
	for k, v := range answers {
		queue[k] = append([]string(nil), v...)
	}
	for i := 0; i < 100; i++ {
		ins := h.next(run)
		if len(ins) == 0 {
			if h.stopExhausted(run) {
				continue
			}
			return h.status(run)
		}
		for _, in := range ins {
			q := queue[in.State]
			if len(q) == 0 {
				h.t.Fatalf("no answer left for instruction at %s (attempt %d)", in.State, in.Attempt)
			}
			queue[in.State] = q[1:]
			h.submit(run, in, q[0])
		}
	}
	h.t.Fatal("drive did not settle")
	return wf.RunStatus{}
}

// exhaustionHalted lists the instances of the run held at an exhaustion halt.
func exhaustionHalted(st wf.RunStatus) []string {
	var out []string
	if st.Phase != wf.RunBlocked {
		return nil
	}
	for _, in := range st.Instances {
		if in.Phase == "blocked" && strings.HasPrefix(in.Reason, "retry exhausted: ") {
			out = append(out, in.ID)
		}
	}
	return out
}

// stopExhausted answers stop to the first exhaustion halt the run is held at
// and reports whether there was one; answering one may settle the others, so
// callers loop.
func (h *harness) stopExhausted(run wf.RunID) bool {
	h.t.Helper()
	ids := exhaustionHalted(h.status(run))
	if len(ids) == 0 {
		return false
	}
	if err := h.e.Retry(run, ids[0], true); err != nil {
		h.t.Fatalf("Retry(%q, stop): %v", ids[0], err)
	}
	return true
}

// scriptedRuntime serves "*": reads by adapter, op and args JSON, records applies in
// order, and checks by key.
type scriptedRuntime struct {
	mu      sync.Mutex
	reads   map[string]json.RawMessage
	down    map[string]bool
	applied []wf.ExternalEffect
	status  map[string]wf.EffectStatus
	failOp  map[string]bool
}

func newFakeRuntime() *scriptedRuntime {
	return &scriptedRuntime{reads: map[string]json.RawMessage{}, down: map[string]bool{}, status: map[string]wf.EffectStatus{}, failOp: map[string]bool{}}
}

func readKey(adapter, op string, args map[string]any) string {
	b, _ := json.Marshal(args)
	return adapter + "." + op + "|" + string(b)
}

func (f *scriptedRuntime) set(adapter, op string, args map[string]any, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(v)
	f.reads[readKey(adapter, op, args)] = b
}

func (f *scriptedRuntime) Ops() []string { return []string{"*"} }

func (f *scriptedRuntime) Read(_ string, d wf.ObservationDesc) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[d.Adapter+"."+d.Op] {
		return nil, fmt.Errorf("%w: %s down", wf.ErrUnavailable, d.Adapter)
	}
	v, ok := f.reads[readKey(d.Adapter, d.Op, d.Args)]
	if !ok {
		return nil, errors.New("no scripted read for " + readKey(d.Adapter, d.Op, d.Args))
	}
	return v, nil
}

func (f *scriptedRuntime) Apply(_ string, e wf.ExternalEffect) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, e)
	if f.failOp[e.Adapter+"."+e.Op] {
		return nil, errors.New("apply failed")
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (f *scriptedRuntime) Check(_ string, e wf.ExternalEffect) (wf.EffectStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.status[e.KeyID()]; ok {
		return s, nil
	}
	return wf.EffectUnknown, nil
}
