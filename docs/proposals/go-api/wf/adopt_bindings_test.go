package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	abPre   struct{}
	abItems struct{}
	abRedo  struct{}
	abWork  struct{}
	abCheck struct{}
	abDone  struct{}
	abStop  struct{}
)

// abindDef is Pre (an agent), Items (a compute storing O), Work (an agent),
// then Check, a command running argv, as the workflow run's AmendItems, designer
// and AmendChecked. With redo O2 is not nil: Work routes to Redo (a compute
// storing O2) before Check, and its closure always takes that route, but it
// also declares the direct route to Check.
func abindDef[O, O2 any](argv []string, redo bool, retryToWork ...bool) *wf.Def {
	d := wf.New("abind")
	pre := wf.NewAgent[abPre, eOut](d)
	items := wf.NewCompute[abItems, O](d, func(wf.Ctx) (O, error) { var o O; return o, nil })
	work := wf.NewAgent[abWork, eOut](d)
	check := wf.NewCommand[abCheck](d, argv...)
	done := wf.NewTerminal[abDone](d, wf.Success)
	stop := wf.NewTerminal[abStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	pre.Spec(wf.PromptText("pre"), wf.Retry[abPre](wf.Const(1), pre.To(stop, "pre failed")))
	toItems := pre.To(items, "go")
	pre.Route(func(eOut, wf.Ctx) wf.Edge[abPre] { return toItems }, toItems)
	toWork := items.To(work, "items")
	items.Route(func(O, wf.Ctx) wf.Edge[abItems] { return toWork }, toWork)
	if len(retryToWork) > 0 && retryToWork[0] {
		items.Retry(wf.Retry[abItems](wf.Const(1), toWork))
	}
	work.Spec(wf.PromptText("work"), wf.Retry[abWork](wf.Const(1), work.To(stop, "work failed")))
	toCheck := work.To(check, "check")
	if redo {
		r := wf.NewCompute[abRedo, O2](d, func(wf.Ctx) (O2, error) { var o O2; return o, nil })
		toRedo := work.To(r, "redo")
		// Only the redo route is ever taken; the direct one is declared.
		work.Route(func(eOut, wf.Ctx) wf.Edge[abWork] { return toRedo }, toRedo, toCheck)
		fromRedo := r.To(check, "redone")
		r.Route(func(O2, wf.Ctx) wf.Edge[abRedo] { return fromRedo }, fromRedo)
	} else {
		work.Route(func(eOut, wf.Ctx) wf.Edge[abWork] { return toCheck }, toCheck)
	}
	check.RouteExit(check.To(done, "green"), check.To(stop, "red"))
	return d
}

// The versions declare their output types locally, under one name: the
// adoption shape sees the same type name, as it saw amendItemsOut across
// the workflow run's definitions.
func abindV1() *wf.Def {
	type itemsOut struct {
		N int `json:"n"`
	}
	return abindDef[itemsOut, itemsOut]([]string{"true"}, false)
}

func abindSweeps() *wf.Def {
	type itemsOut struct {
		N      int    `json:"n"`
		Sweeps string `json:"sweeps" pattern:"^[a-z,]*$"`
	}
	return abindDef[itemsOut, itemsOut]([]string{"true", "--sweeps", "{sweeps}"}, false)
}

func abindExtra() *wf.Def {
	type itemsOut struct {
		N     int    `json:"n"`
		Extra string `json:"extra" pattern:"^[a-z,]*$"`
	}
	return abindDef[itemsOut, itemsOut]([]string{"true", "--n", "{n}"}, false)
}

func abindRedo() *wf.Def {
	type itemsOut struct {
		N int `json:"n"`
	}
	type redoOut struct {
		Sweeps string `json:"sweeps" pattern:"^[a-z,]*$"`
	}
	return abindDef[itemsOut, redoOut]([]string{"true", "--sweeps", "{sweeps}"}, true)
}

// abindRun runs v1 and submits Pre; at Work it has also run Items.
func abindRun(t *testing.T, id string, atWork bool) (*harness, wf.RunID) {
	t.Helper()
	v1 := abindV1()
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: wf.RunID(id)})
	run := h.run(v1, nil)
	ins := h.next(run)
	if len(ins) != 1 || ins[0].State != "abPre" {
		t.Fatalf("first instruction %+v", ins)
	}
	if atWork {
		h.submit(run, ins[0], `{}`)
		if ins = h.next(run); len(ins) != 1 || ins[0].State != "abWork" {
			t.Fatalf("after Pre: %+v, want Work", ins)
		}
	}
	return h, run
}

func adoptNext(h *harness, run wf.RunID, id string, d *wf.Def) ([]wf.Instruction, error) {
	return wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, RunID: wf.RunID(id), Adopt: true}).Next(run, "")
}

// the workflow run the regression case: a run past Items adopts a definition whose
// Check binds {sweeps}, a field the journaled Items output never had. The
// adoption is refused up front, naming the step, the placeholder and the
// state to rewind before, and nothing is journaled; before the check it was
// adopted and halted at Check as a definition error.
func TestAdoptRefusesABindingTheJournalCannotSupply(t *testing.T) {
	h, run := abindRun(t, "ab1", true)
	_, err := adoptNext(h, run, "ab1", abindSweeps())
	if !errors.Is(err, wf.ErrDefChanged) {
		t.Fatalf("adopt = %v, want ErrDefChanged", err)
	}
	for _, want := range []string{"abCheck binds {sweeps}", "rewind before abItems", "no producer can supply"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q", err, want)
		}
	}
	if n := len(h.kind(run, wf.JournalMigrated)); n != 0 {
		t.Fatalf("a refused adoption journaled %d migrated records", n)
	}
}

// A run that has not reached Items yet adopts the same definition: Items
// runs again under it and stores {sweeps} before Check binds it.
func TestAdoptWithTheProducerAhead(t *testing.T) {
	h, run := abindRun(t, "ab2", false)
	ins, err := adoptNext(h, run, "ab2", abindSweeps())
	if err != nil || len(ins) != 1 || ins[0].State != "abPre" {
		t.Fatalf("adopt = %+v, %v; want Pre carried on", ins, err)
	}
	if n := len(h.kind(run, wf.JournalMigrated)); n != 1 {
		t.Fatalf("%d migrated records, want the adoption journaled once", n)
	}
}

func TestAdoptProducerSuccessCanShareARetryEdge(t *testing.T) {
	type itemsOut struct {
		N      int    `json:"n"`
		Sweeps string `json:"sweeps" pattern:"^[a-z,]*$"`
	}
	h, run := abindRun(t, "ab-shared-edge", false)
	candidate := abindDef[itemsOut, itemsOut]([]string{"true", "--sweeps", "{sweeps}"}, false, true)
	if _, err := adoptNext(h, run, "ab-shared-edge", candidate); err != nil {
		t.Fatalf("adopt = %v, want Items' success route to supply sweeps", err)
	}
}

func TestAdoptChecksWorkWhenReturningToALiveState(t *testing.T) {
	build := func(addRead, loop bool) *wf.Def {
		d := wf.New("abind-return")
		pre := wf.NewAgent[abPre, eOut](d)
		done := wf.NewTerminal[abDone](d, wf.Success)
		stop := wf.NewTerminal[abStop](d, wf.Stopped)
		pre.Spec(wf.PromptText("pre"), wf.Retry[abPre](wf.Const(1), pre.To(stop, "pre failed")))
		if addRead {
			wf.DoneWhen(pre, shell.Observe(shell.Run("true", "{unknown}")), "already done")
		}
		if loop {
			pre.Then(pre.To(pre, "re-enter"))
		} else {
			pre.Then(pre.To(done, "done"))
		}
		return d
	}
	for _, loop := range []bool{true, false} {
		t.Run(fmt.Sprint(loop), func(t *testing.T) {
			v1 := build(false, loop)
			const id = "ab-reentry"
			h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: id})
			run := h.run(v1, nil)
			if ins := h.next(run); len(ins) != 1 || ins[0].State != "abPre" {
				t.Fatalf("first instruction %+v", ins)
			}
			_, err := adoptNext(h, run, id, build(true, loop))
			if loop {
				if !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "abPre binds {unknown}") {
					t.Fatalf("adopt = %v, want re-entry refusal", err)
				}
			} else if err != nil {
				t.Fatalf("adopt = %v, want initial handout to carry on", err)
			}
		})
	}
}

func abindSubmitRead(addRead bool) *wf.Def {
	d := wf.New("abind-submit-read")
	pre := wf.NewAgent[abPre, eOut](d)
	stop := wf.NewTerminal[abStop](d, wf.Stopped)
	done := wf.NewTerminal[abDone](d, wf.Success)
	pre.Spec(wf.PromptText("pre"), wf.Retry[abPre](wf.Const(1), pre.To(stop, "pre failed")))
	if addRead {
		pre.Observes(shell.Observe(shell.Run("true", "{unknown}")))
	}
	pre.Then(pre.To(done, "done"))
	return d
}

func TestAdoptChecksObservationOnInitialSubmission(t *testing.T) {
	v1 := abindSubmitRead(false)
	const id = "ab-submit-read"
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: id})
	run := h.run(v1, nil)
	h.next(run)
	_, err := adoptNext(h, run, id, abindSubmitRead(true))
	if !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "abPre binds {unknown}") {
		t.Fatalf("adopt = %v, want refusal for the initial submission's observation", err)
	}
}

func TestAgentInputBindingHaltStopsAgentWork(t *testing.T) {
	for _, atSubmit := range []bool{false, true} {
		t.Run(fmt.Sprint(atSubmit), func(t *testing.T) {
			v2 := abindSubmitRead(true)
			const id = "ab-input-halt"
			if atSubmit {
				v1 := abindSubmitRead(false)
				h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: id})
				run := h.run(v1, nil)
				in := h.next(run)[0]
				// A declared migration bypasses adoption compatibility checks.
				v2.Migrations(wf.Migration{FromHash: v1.Hash(), ToHash: v2.Hash()})
				h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{v2}, RunID: id})
				result, err := h.e.Submit(run, wf.Submission{Token: in.Token, Output: json.RawMessage(`{}`)})
				if err != nil || result.Class != wf.Blocked {
					t.Fatalf("submit = %+v, %v, want Blocked", result, err)
				}
				checkAgentInputHalt(t, h, run)
				return
			}
			h := newHarnessOpts(t, v2, nil, wf.EngineOptions{RunID: id})
			run := h.run(v2, nil)
			if ins := h.next(run); len(ins) != 0 {
				t.Fatalf("issued %+v after input binding failed", ins)
			}
			checkAgentInputHalt(t, h, run)
		})
	}
}

func checkAgentInputHalt(t *testing.T, h *harness, run wf.RunID) {
	t.Helper()
	recs := h.kind(run, wf.JournalRefused)
	if len(recs) != 1 {
		t.Fatalf("got %d refusal records, want one halt", len(recs))
	}
	var refusal wf.RefusedData
	if err := json.Unmarshal(recs[0].Data, &refusal); err != nil || recs[0].Class != wf.Blocked ||
		!refusal.Halt || !strings.Contains(strings.Join(refusal.Reasons, " "), "{unknown}") {
		t.Fatalf("refusal = %+v, %v, want a blocked halt naming {unknown}", refusal, err)
	}
	if n := len(h.kind(run, wf.JournalAccepted)); n != 0 {
		t.Fatalf("accepted %d times after input binding failed", n)
	}
}

// A field no reachable step binds changes nothing: a run past Items adopts a
// definition that added one, while Check binds {n}, which the journal has.
func TestAdoptAnUnboundAddedField(t *testing.T) {
	h, run := abindRun(t, "ab3", true)
	ins, err := adoptNext(h, run, "ab3", abindExtra())
	if err != nil || len(ins) != 1 || ins[0].State != "abWork" {
		t.Fatalf("adopt = %+v, %v; want Work carried on", ins, err)
	}
}

// Early in r9 the live Design state had a path through the normal round
// producer and an opaque re-entry candidate that skipped it. A possible
// future producer is enough for adoption; the route runs with live inputs.
func TestAdoptWithOpaqueBypassOfAFutureProducer(t *testing.T) {
	h, run := abindRun(t, "ab4", false)
	ins, err := adoptNext(h, run, "ab4", abindRedo())
	if err != nil || len(ins) != 1 || ins[0].State != "abPre" {
		t.Fatalf("adopt = %+v, %v; want Pre carried on", ins, err)
	}
}

func TestAdoptRefusesARemovedJournaledProducer(t *testing.T) {
	h, run := abindRun(t, "ab-removed", true)
	d := wf.New("abind")
	work := wf.NewAgent[abWork, eOut](d)
	redo := wf.NewCompute[abRedo, abItemsOut](d, func(wf.Ctx) (abItemsOut, error) { return abItemsOut{N: 2}, nil })
	check := wf.NewCommand[abCheck](d, "true", "--n", "{n}")
	stop := wf.NewTerminal[abStop](d, wf.Stopped)
	done := wf.NewTerminal[abDone](d, wf.Success)
	work.Spec(wf.PromptText("work"), wf.Retry[abWork](wf.Const(1), work.To(stop, "work failed")))
	toRedo := work.To(redo, "redo")
	work.Route(func(eOut, wf.Ctx) wf.Edge[abWork] { return toRedo }, toRedo)
	redo.Then(redo.To(check, "check"))
	check.RouteExit(check.To(done, "green"), check.To(stop, "red"))
	_, err := adoptNext(h, run, "ab-removed", d)
	if !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "abItems") ||
		!strings.Contains(err.Error(), "{n}") {
		t.Fatalf("adopt = %v, want removed journaled producer abItems for {n}", err)
	}
	if n := len(h.kind(run, wf.JournalMigrated)); n != 0 {
		t.Fatalf("a refused adoption journaled %d migrated records", n)
	}
}

func TestAdoptRefusesAFieldRemovedFromAJournaledProducer(t *testing.T) {
	type itemsOut struct {
		Other int `json:"other"`
	}
	h, run := abindRun(t, "ab-removed-field", true)
	candidate := abindDef[itemsOut, abItemsOut]([]string{"true", "--n", "{n}"}, true)
	_, err := adoptNext(h, run, "ab-removed-field", candidate)
	if !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "{n}") ||
		!strings.Contains(err.Error(), "journaled producer abItems") {
		t.Fatalf("adopt = %v, want a refusal for the field removed from abItems", err)
	}
}

func TestAdoptRefusesANameWithNoProducer(t *testing.T) {
	h, run := abindRun(t, "ab-unknown", false)
	unknown := abindDef[abItemsOut, abItemsOut]([]string{"true", "--unknown", "{unknown}"}, false)
	_, err := adoptNext(h, run, "ab-unknown", unknown)
	if !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "abCheck binds {unknown}") {
		t.Fatalf("adopt = %v, want a refusal for unknown binding", err)
	}
}

// With snapshots on, the refusal names the checkpoint to rewind to: the last
// one before the run's visit to Items, so the fork runs Items again under the
// new definition.
func TestAdoptRefusalNamesTheCheckpointBeforeTheProducer(t *testing.T) {
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	opts := func(d *wf.Def, adopt bool) wf.EngineOptions {
		return wf.EngineOptions{Defs: []*wf.Def{d}, RunID: "ab5", Workspace: ws, StateRoot: state, Snapshots: true, Adopt: adopt,
			RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}
	}
	v1 := abindV1()
	j := wf.FileJournal(state)
	e := wf.NewEngine(j, reg, opts(v1, false))
	run, err := e.Run(v1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("Pre = %+v, %v", ins, err)
	}
	if _, err := e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if ins, err = e.Next(run, ""); err != nil || len(ins) != 1 || ins[0].State != "abWork" {
		t.Fatalf("after Pre: %+v, %v", ins, err)
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var cp int64
	for _, r := range recs {
		if r.State == "abItems" {
			break
		}
		var w wf.WorldSnapshotData
		if r.Kind == wf.JournalWorldSnapshot && json.Unmarshal(r.Data, &w) == nil && w.Unavailable == "" {
			cp = r.Seq
		}
	}
	if cp == 0 {
		t.Fatal("no checkpoint before Items")
	}
	_, err = wf.NewEngine(j, reg, opts(abindSweeps(), true)).Next(run, "")
	if want := fmt.Sprintf("rewind before abItems (wf dev rewind --to %d)", cp); !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), want) {
		t.Fatalf("adopt = %v, want a refusal naming %q", err, want)
	}
	t.Log(err)
}

type abFan struct{}

// abindAlt is abindDef with Pre storing P and no Redo. If failToCheck,
// the only route to Check is the Items retry's exhausted edge; fan puts a
// Map (whose children yield chRes{N}) between Work and Check.
func abindAlt[P, O any](argv []string, failToCheck, fan bool) *wf.Def {
	d := wf.New("abind")
	pre := wf.NewAgent[abPre, P](d)
	items := wf.NewCompute[abItems, O](d, func(wf.Ctx) (O, error) { var o O; return o, nil })
	work := wf.NewAgent[abWork, eOut](d)
	check := wf.NewCommand[abCheck](d, argv...)
	done := wf.NewTerminal[abDone](d, wf.Success)
	stop := wf.NewTerminal[abStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	pre.Spec(wf.PromptText("pre"), wf.Retry[abPre](wf.Const(1), pre.To(stop, "pre failed")))
	toItems := pre.To(items, "go")
	pre.Route(func(P, wf.Ctx) wf.Edge[abPre] { return toItems }, toItems)
	if failToCheck {
		items.Retry(wf.Retry[abItems](wf.Const(1), items.To(check, "items failed")))
	}
	var toWork wf.Edge[abItems]
	if failToCheck {
		toWork = items.To(stop, "items succeeded")
	} else {
		toWork = items.To(work, "items")
	}
	items.Route(func(O, wf.Ctx) wf.Edge[abItems] { return toWork }, toWork)
	work.Spec(wf.PromptText("work"), wf.Retry[abWork](wf.Const(1), work.To(stop, "work failed")))
	next := work.To(check, "check")
	if fan {
		m := wf.NewMap[abFan, chItem, string, chRes](d, chItems(0, 1), func(i chItem) string { return fmt.Sprint(i.N) }, chChild())
		m.Parallel(1).Join(wf.AllSettled(), m.To(check, "met"), m.To(stop, "partial"))
		next = work.To(m, "fan")
	}
	work.Route(func(eOut, wf.Ctx) wf.Edge[abWork] { return next }, next)
	check.RouteExit(check.To(done, "green"), check.To(stop, "red"))
	return d
}

type abPreOut struct {
	OK string `json:"ok" pattern:"^[a-z]*$"`
}

type abItemsOut struct {
	N int `json:"n"`
}

// Review round 1 (1): the live state is Pre, an issued agent whose required
// result field ok feeds Check on every path. Its result is not stored yet, so
// it is the producer: the adoption is accepted, and the run binds {ok} at
// Check and ends green.
func TestAdoptWithThePendingStateAsProducer(t *testing.T) {
	v1 := abindAlt[abPreOut, abItemsOut]([]string{"true"}, false, false)
	h := newHarnessOpts(t, v1, shell.Runtime(shell.Config{}), wf.EngineOptions{RunID: "ab6"})
	run := h.run(v1, nil)
	if ins := h.next(run); len(ins) != 1 || ins[0].State != "abPre" {
		t.Fatalf("first instruction %+v", ins)
	}
	v2 := abindAlt[abPreOut, abItemsOut]([]string{"true", "--ok", "{ok}"}, false, false)
	h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{v2}, RunID: "ab6", Adopt: true, Clock: func() time.Time { return h.now }})
	ins, err := h.e.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].State != "abPre" {
		t.Fatalf("adopt = %+v, %v; want Pre carried on", ins, err)
	}
	h.submit(run, ins[0], `{"ok":"yes"}`)
	ins = h.next(run)
	if len(ins) != 1 || ins[0].State != "abWork" {
		t.Fatalf("after Pre: %+v", ins)
	}
	h.submit(run, ins[0], `{}`)
	h.next(run)
	st, err := h.e.Status(run)
	if err != nil || st.Terminal != "abDone" {
		t.Fatalf("status = %+v, %v; want the run green at abDone", st, err)
	}
}

// Review round 1 (2), as main ruled: a field a state still ahead may omit
// counts as produced. Items stores sweeps,omitempty; adopting at Pre, before
// Items runs, is accepted. Whether Items fills it is the new definition's
// own contract, which a fresh run meets the same way (a lint matter).
func TestAdoptCountsAnOmittableFieldOfAProducerAhead(t *testing.T) {
	type itemsOut struct {
		N      int    `json:"n"`
		Sweeps string `json:"sweeps,omitempty" pattern:"^[a-z,]*$"`
	}
	h, run := abindRun(t, "ab7", false)
	if _, err := adoptNext(h, run, "ab7", abindAlt[eOut, itemsOut]([]string{"true", "--sweeps", "{sweeps}"}, false, false)); err != nil {
		t.Fatalf("adopt = %v, want accepted: Items is ahead and declares sweeps", err)
	}
}

// Review round 1 (3): a Map stores its keyed join, never its children's
// result fields flat, so a Map before Check does not supply {N}.
func TestAdoptMapSuppliesNoFlatChildField(t *testing.T) {
	h, run := abindRun(t, "ab8", false)
	_, err := adoptNext(h, run, "ab8", abindAlt[eOut, struct{}]([]string{"true", "--n", "{N}"}, false, true))
	if !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "abCheck binds {N}") {
		t.Fatalf("adopt = %v, want refused: the Map stores no flat N", err)
	}
}

// A producer's failure edge stores nothing. If its successful edge leaves
// the consumer unreachable, even a future producer cannot supply the value.
func TestAdoptProducerFailureEdgeSuppliesNothing(t *testing.T) {
	type itemsOut struct {
		N      int    `json:"n"`
		Sweeps string `json:"sweeps" pattern:"^[a-z,]*$"`
	}
	argv := []string{"true", "--sweeps", "{sweeps}"}
	h, run := abindRun(t, "ab9", false)
	if _, err := adoptNext(h, run, "ab9", abindAlt[eOut, itemsOut](argv, false, false)); err != nil {
		t.Fatalf("adopt with Items' success edge only = %v, want accepted", err)
	}
	h2, run2 := abindRun(t, "ab10", false)
	_, err := adoptNext(h2, run2, "ab10", abindAlt[eOut, itemsOut](argv, true, false))
	if !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "abCheck binds {sweeps}") {
		t.Fatalf("adopt = %v, want refused: Items' exhausted edge reaches Check with no sweeps", err)
	}
}

// Final review: Check binds {Stdout} in an effect on its exit-0 case, where
// its own stored RunResult supplies it, and on its retry's exhausted edge,
// where the command failed and stored nothing. Both are edges the engine
// takes itself, so their effects are static. The supplied occurrence, listed
// first, must not hide the unsupplied one: adopting at Pre is refused.
func TestAdoptChecksEachEdgeOccurrenceOfABinding(t *testing.T) {
	build := func(onFailure bool) *wf.Def {
		d := wf.New("abind")
		pre := wf.NewAgent[abPre, eOut](d)
		check := wf.NewCommand[abCheck](d, "true")
		done := wf.NewTerminal[abDone](d, wf.Success)
		stop := wf.NewTerminal[abStop](d, wf.Stopped)
		d.OnFailure(wf.Blocked, stop)
		pre.Spec(wf.PromptText("pre"), wf.Retry[abPre](wf.Const(1), pre.To(stop, "pre failed")))
		toCheck := pre.To(check, "go")
		pre.Route(func(eOut, wf.Ctx) wf.Edge[abPre] { return toCheck }, toCheck)
		effect := func(name string) wf.Effect {
			return shell.Effect(wf.KeyOf("out", name), shell.Run("true", "--out", "{Stdout}"))
		}
		failed := check.To(stop, "check failed")
		if onFailure {
			failed = failed.With(effect("failed"))
		}
		check.Retry(wf.Retry[abCheck](wf.Const(1), failed))
		// A case table is static: the engine takes the case's own edge.
		exits := wf.NewEnum(d, "ab-exit", cExit(0), cExit(1))
		check.RouteCases(wf.Cases[abCheck, wf.RunResult](wf.ExitAs[cExit], exits,
			wf.On(cExit(0), check.To(done, "green").With(effect("green"))),
			wf.On(cExit(1), check.To(stop, "red"))))
		return d
	}
	h, run := abindRun(t, "ab11", false)
	if _, err := adoptNext(h, run, "ab11", build(false)); err != nil {
		t.Fatalf("adopt with the effect on the exit-0 case only = %v, want accepted", err)
	}
	h2, run2 := abindRun(t, "ab12", false)
	_, err := adoptNext(h2, run2, "ab12", build(true))
	if !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "abCheck binds {Stdout}") || !strings.Contains(err.Error(), `edge "check failed" effect`) {
		t.Fatalf("adopt = %v, want refused naming the exhausted edge's effect", err)
	}
}

// A closure's candidates only declare their effects: the closure returns its
// own edge with effects bound to its values, as the fork's SweepRepairBeads
// and LedgerDecide do. A template placeholder there ({anything}) is no
// evidence, whether the closure is a Route or a case table's OnRoute.
func TestAdoptIgnoresClosureCandidateEffects(t *testing.T) {
	effect := shell.Effect(wf.KeyOf("tmpl"), shell.Run("true", "--set", "{anything}"))
	for i, onRoute := range []bool{false, true} {
		d := wf.New("abind")
		pre := wf.NewAgent[abPre, eOut](d)
		check := wf.NewCommand[abCheck](d, "true")
		done := wf.NewTerminal[abDone](d, wf.Success)
		stop := wf.NewTerminal[abStop](d, wf.Stopped)
		d.OnFailure(wf.Blocked, stop)
		pre.Spec(wf.PromptText("pre"), wf.Retry[abPre](wf.Const(1), pre.To(stop, "pre failed")))
		toCheck := pre.To(check, "go")
		pre.Route(func(eOut, wf.Ctx) wf.Edge[abPre] { return toCheck }, toCheck)
		green := check.To(done, "green")
		if onRoute {
			exits := wf.NewEnum(d, "ab-exit", cExit(0), cExit(1))
			check.RouteCases(wf.Cases[abCheck, wf.RunResult](wf.ExitAs[cExit], exits,
				wf.OnRoute(cExit(0), func(wf.Ctx) wf.Edge[abCheck] { return green }, green.With(effect)),
				wf.On(cExit(1), check.To(stop, "red"))))
		} else {
			red := check.To(stop, "red")
			check.Route(func(wf.RunResult, wf.Ctx) wf.Edge[abCheck] { return green }, green.With(effect), red)
		}
		id := fmt.Sprintf("ab2%d", i)
		h, run := abindRun(t, id, false)
		if _, err := adoptNext(h, run, id, d); err != nil {
			t.Fatalf("onRoute=%v: adopt = %v, want accepted: a closure candidate's effects are templates", onRoute, err)
		}
	}
}
