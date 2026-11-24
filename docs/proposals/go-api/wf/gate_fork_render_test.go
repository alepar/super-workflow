package wf_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	gfAsk  struct{}
	gfDone struct{}
	gfStop struct{}
	gfAns  string
)

// gateFanDef maps two items onto a child that starts at a person gate of
// reservation res: two sibling gates at one qualified state.
func gateFanDef(res wf.Reservation) *wf.Def {
	child := wf.New("gf-item")
	ans := wf.NewEnum(child, "gf-ans", gfAns("yes"), gfAns("no"))
	g := wf.NewGate[gfAsk, gfAns](child, "Keep?", ans, res)
	ok := wf.NewTerminal[gfDone](child, wf.Success)
	no := wf.NewTerminal[gfStop](child, wf.Stopped)
	child.OnFailure(wf.Blocked, no)
	g.Answers(wf.On(gfAns("yes"), g.To(ok, "yes")), wf.On(gfAns("no"), g.To(no, "no")))
	res2 := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res2)
	wf.Yield(no, res2)
	d := wf.New("gf")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, wf.Typed[chItem, chRes](child))
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	return d
}

type gateFan struct {
	cfg       wf.MainConfig
	e         *wf.Engine
	d         *wf.Def
	ws, state string
	run       wf.RunID
	targets   []wf.FanoutTarget
	adapters  *wf.Adapters
}

// newGateFan runs gateFanDef with snapshots to its two offered gates and
// plans a fanout of every gate visit.
func newGateFan(t *testing.T, res wf.Reservation) gateFan {
	t.Helper()
	g := gateFan{ws: t.TempDir(), state: t.TempDir(), d: gateFanDef(res), adapters: wf.NewAdapters()}
	if err := os.WriteFile(filepath.Join(g.ws, "data"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.adapters.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	g.cfg = wf.MainConfig{Adapters: g.adapters, Defs: []wf.Registration{{Def: g.d}}, Binary: "super"}
	g.e = wf.NewEngine(wf.FileJournal(g.state), g.adapters, wf.EngineOptions{Defs: []*wf.Def{g.d}, Workspace: g.ws, StateRoot: g.state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(g.state, "runs", string(r)) }}).(*wf.Engine)
	var err error
	if g.run, err = g.e.Run(g.d, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = g.e.Next(g.run, ""); err != nil {
		t.Fatal(err)
	}
	plan, err := g.e.PlanFanout(g.run, "all")
	if err != nil || len(plan.Targets) != 2 {
		t.Fatalf("plan = %+v %v, want both sibling gates", plan, err)
	}
	g.targets = plan.Targets
	return g
}

// fork forks target as dev fanout does, bound to it, and renders the
// fork's next as its coordinator sees it.
func (g gateFan) fork(t *testing.T, target wf.FanoutTarget) (wf.SnapshotFork, string) {
	t.Helper()
	fork, err := g.e.ForkSnapshot(g.run, target.SnapshotSeq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	fe := wf.NewEngine(wf.FileJournal(fork.StateDir), g.adapters, wf.EngineOptions{Defs: []*wf.Def{g.d}, Workspace: fork.Workspace, StateRoot: fork.StateDir, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(r)) }}).(*wf.Engine)
	if err := fe.BindFanoutTarget(g.run, target); err != nil {
		t.Fatalf("bind %+v: %v", target, err)
	}
	code, out, er := cliCall(g.cfg, "wf", "next", "--until-unit", target.Unit, "--state-dir", fork.StateDir, "--workspace", fork.Workspace, "--run-id", string(g.run))
	if code != 0 && code != 7 { // 7: next stopped at a person's gate
		t.Fatalf("fork next %d:\n%s\n%s", code, out, er)
	}
	return fork, out
}

var answerChild = regexp.MustCompile(`--child '?([^' ]+)'? --visit (\d+)`)

// In a fanout fork bound to either sibling gate, of either kind, the
// rendered next tells the coordinator to choose the answer itself and
// prints the answer command for that very sibling and visit.
func TestFanoutGateForkRendersTheCoordinatorsChoice(t *testing.T) {
	for _, res := range []wf.Reservation{wf.Reserved, wf.PolicyAnswerable} {
		g := newGateFan(t, res)
		for _, target := range g.targets {
			_, out := g.fork(t, target)
			if strings.Contains(out, "ask yours") || strings.Contains(out, "Ask the person") {
				t.Errorf("%v %s: rendered next leaves the answer to a person:\n%s", res, target.Instance, out)
			}
			if !strings.Contains(strings.ToLower(out), "choose") {
				t.Errorf("%v %s: rendered next does not tell the coordinator to choose:\n%s", res, target.Instance, out)
			}
			m := answerChild.FindStringSubmatch(out)
			if m == nil || m[1] != target.Instance || m[2] != fmt.Sprint(target.Visit) {
				t.Errorf("%v %s: printed answer command %q names another sibling or visit:\n%s", res, target.Instance, m, out)
			}
		}
	}
}

// In the source run, a gate bound by wf next --until-unit stays the
// person's: only a fanout gate target is the coordinator's to answer.
func TestASourceGateBoundByUntilUnitStaysThePersons(t *testing.T) {
	for _, res := range []wf.Reservation{wf.Reserved, wf.PolicyAnswerable} {
		g := newGateFan(t, res)
		target := g.targets[0]
		code, out, er := cliCall(g.cfg, "wf", "next", "--until-unit", target.Unit, "--state-dir", g.state, "--workspace", g.ws, "--run-id", string(g.run))
		if code != 0 && code != 7 {
			t.Fatalf("source next %d:\n%s\n%s", code, out, er)
		}
		if strings.Contains(strings.ToLower(out), "choose the answer yourself") || strings.Contains(out, "choose it yourself") {
			t.Errorf("%v: a source gate bound by until-unit was handed to the coordinator:\n%s", res, out)
		}
	}
}
