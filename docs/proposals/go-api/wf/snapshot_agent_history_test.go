package wf_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	ahFix    struct{}
	ahOffer  struct{}
	ahSecond struct{}
	ahDone   struct{}
	ahStop   struct{}
)

// ahDiagnosis is SweepFix's diagnosis in small: prose citing a test by
// absolute path, and a top-level field a later step may bind.
type ahDiagnosis struct {
	Where string
	Tests []struct{ Evidence string }
}

// agentHistoryDef: an agent diagnoses, an agent offer (where the fork is
// taken), then a command bound as argv says.
func agentHistoryDef(argv ...string) *wf.Def {
	d := wf.New("agent-history")
	fix := wf.NewAgent[ahFix, ahDiagnosis](d)
	offer := wf.NewAgent[ahOffer, string](d)
	second := wf.NewCommand[ahSecond](d, argv...)
	done := wf.NewTerminal[ahDone](d, wf.Success)
	stop := wf.NewTerminal[ahStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	fix.Spec(wf.PromptText("diagnose"), wf.Retry[ahFix](wf.Const(1), fix.To(stop, "gave up")))
	toOffer := fix.To(offer, "diagnosed")
	fix.Route(func(ahDiagnosis, wf.Ctx) wf.Edge[ahFix] { return toOffer }, toOffer)
	offer.Spec(wf.PromptText("go on"), wf.Retry[ahOffer](wf.Const(1), offer.To(stop, "gave up")))
	next := offer.To(second, "next")
	offer.Route(func(string, wf.Ctx) wf.Edge[ahOffer] { return next }, next)
	second.RouteExit(second.To(done, "ran"), second.To(stop, "red"))
	return d
}

// An agent's result whose prose names the source workspace is history: a
// run forks past it (an earlier revision; the SweepFix submission whose
// Tests[0].Evidence was '<source workspace>/x_test.go:17', refused at
// journal 111/112). Binding that text into an operational field in the fork
// is still refused (an earlier revision's bind-time guard), and passes nothing to an
// adapter.
func TestForkPastAnAgentResultWhoseProseNamesTheSource(t *testing.T) {
	for name, c := range map[string]struct {
		argv    []string
		refused bool
	}{
		"prose only":      {[]string{"true"}, false},
		"bound into argv": {[]string{"cat", "{Where}"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			ws, state := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			sh := &recordingShell{print: func(string) string { return "" }}
			reg := wf.NewAdapters()
			for _, rt := range []wf.AdapterRuntime{&snapshotRuntime{}, sh} {
				if err := reg.Register(rt); err != nil {
					t.Fatal(err)
				}
			}
			d := agentHistoryDef(c.argv...)
			opts := wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true,
				RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}
			e := wf.NewEngine(wf.FileJournal(state), reg, opts).(*wf.Engine)
			run, err := e.Run(d, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			ins, err := e.Next(run, "")
			if err != nil || len(ins) != 1 || ins[0].State != "ahFix" {
				t.Fatalf("fix: %+v %v", ins, err)
			}
			// A bare absolute path as a whole field is refused at submit
			// today; the prose that cites one is what reaches the journal
			// (the literal journal 111/112 value is covered at the journal
			// level, TestAgentResultsAndTheirCarriersAreHistory).
			evidence := "TestX fails at " + filepath.Join(ws, "x_test.go") + ":17"
			out := fmt.Sprintf(`{"Where":%q,"Tests":[{"Evidence":%q}]}`, evidence, evidence)
			if _, err := e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(out)}); err != nil {
				t.Fatal(err)
			}
			if ins, err = e.Next(run, ""); err != nil || len(ins) != 1 || ins[0].State != "ahOffer" {
				t.Fatalf("offer: %+v %v", ins, err)
			}
			recs, _ := wf.ReadJournal(state, run)
			var seq int64
			for _, r := range recs {
				if r.Kind == wf.JournalWorldSnapshot && r.State == "ahOffer" {
					seq = r.Seq
				}
			}
			fork, err := e.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
			if err != nil {
				t.Fatalf("fork past the diagnosis: %v", err)
			}
			opts.Workspace, opts.StateRoot = fork.Workspace, fork.StateDir
			opts.RunDir = func(r wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(r)) }
			fe := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, opts)
			ins, err = fe.Next(run, "")
			if err != nil || len(ins) != 1 || ins[0].State != "ahOffer" {
				t.Fatalf("fork offer: %+v %v", ins, err)
			}
			if _, err := fe.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"ok"`)}); err != nil {
				t.Fatal(err)
			}
			before := len(sh.runs)
			if _, err := fe.Next(run, ""); err != nil {
				t.Fatalf("next into the command: %v", err)
			}
			frecs, _ := wf.ReadJournal(fork.StateDir, run)
			ran, refusal := len(sh.runs) > before, false
			for _, r := range frecs {
				if r.State == "ahSecond" && r.Kind == wf.JournalEffectResult {
					ran = true
				}
				if r.State == "ahSecond" && strings.Contains(string(r.Data), "a root of the run this one was forked from") && strings.Contains(string(r.Data), "command.argv[1]") {
					refusal = true
				}
			}
			if c.refused && (ran || !refusal) {
				t.Fatalf("binding the prose into argv: ran %v (%v), refusal journaled %v", ran, sh.runs, refusal)
			}
			if !c.refused && (!ran || refusal) {
				t.Fatalf("the fork's command: ran %v, refused %v", ran, refusal)
			}
		})
	}
}
