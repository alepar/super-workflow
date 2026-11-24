package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	mdA      struct{}
	mdDerive struct{}
	mdWorld  struct{}
	mdSuite  struct{}
	mdEnd    struct{}
	mdDone   struct{}
	mdStop   struct{}
)

// mdDef: an agent, a compute routed only by the agent's output (no world
// reads), a compute that declares an observation (a world read), a command,
// then the target agent. Every routed state has two edges; the run takes
// edge[0] of each.
func mdDef() *wf.Def {
	d := wf.New("md")
	a := wf.NewAgent[mdA, string](d)
	derive := wf.NewCompute[mdDerive, string](d, func(c wf.Ctx) (string, error) { return wf.OutputOf(c, a), nil })
	probe := wf.Observe[bool]("probe", "exists", map[string]any{"path": "x"})
	world := wf.NewCompute[mdWorld, bool](d, func(c wf.Ctx) (bool, error) { return wf.Observed(c, probe) }).Observes(probe)
	suite := wf.NewCommand[mdSuite](d, "suite")
	end := wf.NewAgent[mdEnd, string](d)
	done := wf.NewTerminal[mdDone](d, wf.Success)
	stop := wf.NewTerminal[mdStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	a.Spec(wf.PromptText("a"), wf.Retry[mdA](wf.Const(1), a.To(stop, "gave up")))
	toDerive := a.To(derive, "next")
	a.Route(func(string, wf.Ctx) wf.Edge[mdA] { return toDerive }, toDerive)
	dGo, dStop := derive.To(world, "go"), derive.To(stop, "halt")
	derive.Route(func(string, wf.Ctx) wf.Edge[mdDerive] { return dGo }, dGo, dStop)
	wGo, wStop := world.To(suite, "go"), world.To(stop, "halt")
	world.Route(func(bool, wf.Ctx) wf.Edge[mdWorld] { return wGo }, wGo, wStop)
	suite.RouteExit(suite.To(end, "green"), suite.To(stop, "red"))
	end.Spec(wf.PromptText("end"), wf.Retry[mdEnd](wf.Const(1), end.To(stop, "gave up")))
	fin := end.To(done, "done")
	end.Route(func(string, wf.Ctx) wf.Edge[mdEnd] { return fin }, fin)
	return d
}

// mdDrive manufactures mdDef to mdEnd with steps, and returns its report.
func mdDrive(t *testing.T, steps string) *wf.ManufactureReport {
	t.Helper()
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{&snapshotRuntime{}, &recordingShell{print: func(string) string { return "" }}} {
		if err := reg.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	d := mdDef()
	s := script(t, `{"edge":"md/mdEnd/edge[0]","registration":"md","target_state":"md/mdEnd","hash":"x","steps":[`+steps+`],"trigger":{"op":"submit","output":"ok"}}`)
	s.Hash = d.Hash()
	e, run, _ := manufacturedRun(t, d, reg, ws, state, &wf.ManufacturedData{Edge: s.Edge, Until: s.Until, Hash: s.Hash, Script: wf.ScriptDigest(s)})
	return manufacture(t, e, run, s, false)
}

const (
	mdStepA      = `{"state":"md/mdA","edge":"md/mdA/edge[1]","step":{"op":"submit","output":"x"}}`
	mdDeriveOK   = `{"state":"md/mdDerive","edge":"md/mdDerive/edge[0]","step":{"op":"compute"}}`
	mdDeriveMiss = `{"state":"md/mdDerive","edge":"md/mdDerive/edge[1]","step":{"op":"compute"}}`
	mdWorldOK    = `{"state":"md/mdWorld","edge":"md/mdWorld/edge[0]","step":{"op":"compute"}}`
	mdWorldMiss  = `{"state":"md/mdWorld","edge":"md/mdWorld/edge[1]","step":{"op":"compute"}}`
	mdSuiteMiss  = `{"state":"md/mdSuite","edge":"md/mdSuite/edge[1]","step":{"op":"settle"}}`
)

func mdDivergence(t *testing.T, rep *wf.ManufactureReport, state string) *wf.ManufactureDivergence {
	t.Helper()
	if rep.Reached || rep.Divergence == nil || rep.Divergence.State != state {
		b, _ := json.Marshal(rep)
		t.Fatalf("report %s, want a divergence at %s", b, state)
	}
	return rep.Divergence
}

// A compute that declares no reads (no observation, no consumed artifact)
// is routed by what the run already holds: earlier outputs, options,
// params, effect outcomes. Its divergence is no_declared_reads, never
// needs-stub (an earlier revision; CoverageScope routes only on CoverageApply's output).
func TestManufactureComputeWithoutDeclaredReads(t *testing.T) {
	rep := mdDrive(t, mdStepA+","+mdDeriveMiss)
	div := mdDivergence(t, rep, "md/mdDerive")
	if div.NeedsStub || !div.NoDeclaredReads {
		t.Fatalf("divergence %+v, want no_declared_reads and not needs_stub", div)
	}
	for _, s := range rep.NeedsStub {
		if s == "md/mdDerive" {
			t.Fatalf("needs_stub lists %v", rep.NeedsStub)
		}
	}
}

// A compute that declares a world read may be steered by the world: a
// fixture fact or a stub (this one routes go whatever it observes; the
// label is about what it declares).
func TestManufactureComputeWithWorldReadsNeedsStub(t *testing.T) {
	div := mdDivergence(t, mdDrive(t, mdStepA+","+mdDeriveOK+","+mdWorldMiss), "md/mdWorld")
	if !div.NeedsStub || div.NoDeclaredReads {
		t.Fatalf("divergence %+v, want needs_stub", div)
	}
}

// A settle step's edge is the effect's outcome: needs-stub.
func TestManufactureSettleNeedsStub(t *testing.T) {
	div := mdDivergence(t, mdDrive(t, mdStepA+","+mdDeriveOK+","+mdWorldOK+","+mdSuiteMiss), "md/mdSuite")
	if !div.NeedsStub || div.NoDeclaredReads {
		t.Fatalf("divergence %+v, want needs_stub", div)
	}
}

type (
	mrWrite  struct{}
	mrDerive struct{}
	mrEnd    struct{}
	mrDone   struct{}
	mrStop   struct{}
)

// A compute that consumes an artifact reads the world through it (a repo
// artifact's file in the workspace): it declares reads, so its divergence is
// needs-stub, not no_declared_reads (an earlier revision review round 1, P1).
func TestManufactureComputeConsumingAnArtifactNeedsStub(t *testing.T) {
	d := wf.New("mr")
	write := wf.NewAgent[mrWrite, string](d)
	spec := wf.NewRepoArtifact[string](write, "spec", "spec.md")
	derive := wf.NewCompute[mrDerive, string](d, func(c wf.Ctx) (string, error) { return wf.Read(c, spec) })
	wf.Consumes(derive, spec)
	end := wf.NewAgent[mrEnd, string](d)
	done := wf.NewTerminal[mrDone](d, wf.Success)
	stop := wf.NewTerminal[mrStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	write.Spec(wf.PromptText("w"), wf.Retry[mrWrite](wf.Const(1), write.To(stop, "gave up")))
	toDerive := write.To(derive, "next")
	write.Route(func(string, wf.Ctx) wf.Edge[mrWrite] { return toDerive }, toDerive)
	dGo, dStop := derive.To(end, "go"), derive.To(stop, "halt")
	derive.Route(func(string, wf.Ctx) wf.Edge[mrDerive] { return dGo }, dGo, dStop)
	end.Spec(wf.PromptText("end"), wf.Retry[mrEnd](wf.Const(1), end.To(stop, "gave up")))
	fin := end.To(done, "done")
	end.Route(func(string, wf.Ctx) wf.Edge[mrEnd] { return fin }, fin)
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "spec.md"), []byte("the spec"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	s := script(t, `{"edge":"mr/mrEnd/edge[0]","registration":"mr","target_state":"mr/mrEnd","hash":"x","steps":[`+
		`{"state":"mr/mrWrite","edge":"mr/mrWrite/edge[1]","step":{"op":"submit","output":"x"}},`+
		`{"state":"mr/mrDerive","edge":"mr/mrDerive/edge[1]","step":{"op":"compute"}}],"trigger":{"op":"submit","output":"ok"}}`)
	s.Hash = d.Hash()
	e, run, _ := manufacturedRun(t, d, reg, ws, state, &wf.ManufacturedData{Edge: s.Edge, Until: s.Until, Hash: s.Hash, Script: wf.ScriptDigest(s)})
	div := mdDivergence(t, manufacture(t, e, run, s, false), "mr/mrDerive")
	if !div.NeedsStub || div.NoDeclaredReads {
		t.Fatalf("divergence %+v, want needs_stub", div)
	}
}

type (
	msSub    struct{}
	msDone   struct{}
	msKid    struct{}
	msKidEnd struct{}
	msStop   struct{}
)

// The driver finds a child state by the child definition's exact name, as
// the engine folds a spawn: a name holding '/' is found, so a compute there
// is labelled by what it declares (an earlier revision review round 1, P2).
func TestManufactureFindsAChildDefinitionNamedWithASlash(t *testing.T) {
	kid := wf.New("middle/group")
	k := wf.NewCompute[msKid, string](kid, func(wf.Ctx) (string, error) { return "x", nil })
	kend := wf.NewTerminal[msKidEnd](kid, wf.Success)
	kstop := wf.NewTerminal[msStop](kid, wf.Stopped)
	kGo, kStop := k.To(kend, "go"), k.To(kstop, "halt")
	k.Route(func(string, wf.Ctx) wf.Edge[msKid] { return kGo }, kGo, kStop)
	wf.Yield(kend, func(wf.Ctx) string { return "" })
	wf.Yield(kstop, func(wf.Ctx) string { return "" })
	d := wf.New("ms")
	sub := wf.NewSub[msSub](d, wf.Typed[string, string](kid), func(wf.Ctx) string { return "" })
	done := wf.NewTerminal[msDone](d, wf.Success)
	sub.Exits(wf.OnExit[msKidEnd](sub.To(done, "done")), wf.OnExit[msStop](sub.To(done, "stopped")))
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	s := script(t, `{"edge":"ms/msSub/edge[0]","registration":"ms","until":"ended","hash":"x","steps":[`+
		`{"state":"ms/middle/group/msKid","edge":"ms/middle/group/msKid/edge[1]","step":{"op":"compute"}}]}`)
	s.Hash = d.Hash()
	e, run, _ := manufacturedRun(t, d, reg, ws, state, &wf.ManufacturedData{Edge: s.Edge, Until: s.Until, Hash: s.Hash, Script: wf.ScriptDigest(s)})
	div := mdDivergence(t, manufacture(t, e, run, s, false), "ms/middle/group/msKid")
	if div.NeedsStub || !div.NoDeclaredReads {
		t.Fatalf("divergence %+v, want no_declared_reads", div)
	}
}
