package wftest_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type RealWrite struct{}
type RealDone struct{}
type RealStop struct{}
type RealOutput struct{ OK bool }

// Missing RunDir must not halt an ordinary allocated-artifact definition.
func TestSimAllocatesRunDirectoryByDefault(t *testing.T) {
	d := wf.New("real-artifact")
	a := wf.NewAgent[RealWrite, RealOutput](d)
	end := wf.NewTerminal[RealDone](d, wf.Success)
	stop := wf.NewTerminal[RealStop](d, wf.Stopped)
	wf.NewArtifact[string](a, "report.md").Optional()
	a.Spec(wf.PromptText("write"), wf.Retry[RealWrite](wf.Const(1), a.To(stop, "failed")))
	edge := a.To(end, "done")
	a.Route(func(RealOutput, wf.Ctx) wf.Edge[RealWrite] { return edge }, edge)
	tr := wftest.New(d, wftest.WithTB(t)).Run(wftest.Submit(a, RealOutput{OK: true}))
	wftest.AssertTerminal(t, tr, end)
}

func TestSimWritesDeclaredArtifactsInBothModes(t *testing.T) {
	for _, cap := range []int{0, 1} {
		t.Run(string(rune('0'+cap)), func(t *testing.T) {
			d := wf.New("write-read")
			a := wf.NewAgent[RealWrite, RealOutput](d)
			b := wf.NewAgent[RealRead, RealOutput](d)
			end := wf.NewTerminal[RealDone](d, wf.Success)
			stop := wf.NewTerminal[RealStop](d, wf.Stopped)
			artifact := wf.NewArtifact[string](a, "report.md")
			a.Spec(wf.PromptText("write"), wf.Retry[RealWrite](wf.Const(1), a.To(stop, "fail")))
			edge := a.To(b, "written")
			a.Route(func(RealOutput, wf.Ctx) wf.Edge[RealWrite] { return edge }, edge)
			b.Spec(wf.PromptText("read", artifact), wf.Retry[RealRead](wf.Const(1), b.To(stop, "fail")), wf.Checkf("content", func(RealOutput, wf.Ctx) error { return nil }))
			wf.Consumes(b, artifact)
			done := b.To(end, "done")
			b.Route(func(RealOutput, wf.Ctx) wf.Edge[RealRead] { return done }, done)
			opts := []wftest.Option{wftest.WithTB(t)}
			if cap > 0 {
				opts = append(opts, wftest.Cap(cap))
			}
			tr := wftest.New(d, opts...).Run(wftest.Submit(a, RealOutput{OK: true}).WithArtifacts(map[string]string{"report.md": "the report"}), wftest.Submit(b, RealOutput{OK: true}))
			wftest.AssertTerminal(t, tr, end)
			for _, rec := range tr.Journal() {
				if rec.Kind == wf.JournalSubmitted {
					var v wf.SubmittedData
					if json.Unmarshal(rec.Data, &v) == nil {
						if ref, ok := v.Artifacts["report.md"]; ok && ref.Hash == fmt.Sprintf("%x", sha256.Sum256([]byte("the report"))) {
							return
						}
					}
				}
			}
			t.Fatal("declared artifact was not recorded with its content hash")
		})
	}
}

type RealRead struct{}

func TestSimFailureUsesDeclaredClassRoute(t *testing.T) {
	for _, cap := range []int{0, 1} {
		t.Run(string(rune('0'+cap)), func(t *testing.T) {
			d := wf.New("blocked")
			a := wf.NewAgent[RealWrite, RealOutput](d)
			end := wf.NewTerminal[RealDone](d, wf.Success)
			stop := wf.NewTerminal[RealStop](d, wf.Stopped)
			blocked := a.To(stop, "blocked")
			a.Spec(wf.PromptText("work"), wf.Retry[RealWrite](wf.Const(2), blocked).OnClass(wf.Blocked, blocked))
			done := a.To(end, "done")
			a.Route(func(RealOutput, wf.Ctx) wf.Edge[RealWrite] { return done }, done)
			opts := []wftest.Option{wftest.WithTB(t)}
			if cap > 0 {
				opts = append(opts, wftest.Cap(cap))
			}
			tr := wftest.New(d, opts...).Run(wftest.Fail(a, wf.FailBlocked, "permission refused"))
			wftest.AssertTerminal(t, tr, stop)
			wftest.AssertClass(t, tr, "RealWrite", wf.Blocked)
			if !strings.Contains(tr.String(), "permission refused") {
				t.Fatalf("reason missing from trace: %s", tr)
			}
		})
	}
}

func TestSimMismatchPrintsNamedJournalTrace(t *testing.T) {
	d := wf.New("mismatch")
	a := wf.NewAgent[RealWrite, RealOutput](d)
	end := wf.NewTerminal[RealDone](d, wf.Success)
	done := a.To(end, "done")
	a.Spec(wf.PromptText("work"), wf.Retry[RealWrite](wf.Const(1), done))
	a.Route(func(RealOutput, wf.Ctx) wf.Edge[RealWrite] { return done }, done)
	rec := &recordingTB{TB: t}
	wftest.New(d, wftest.WithTB(rec)).Run(wftest.Submit(a, RealOutput{}), wftest.SubmitAt("unknown", RealOutput{}))
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0], "accepted") || !strings.Contains(rec.msgs[0], "run_ended") || !strings.Contains(rec.msgs[0], "transition") {
		t.Fatalf("trace missing named kinds/classes: %q", rec.msgs)
	}
}

func TestSimTransientFailureThenReplacementSucceeds(t *testing.T) {
	for _, cap := range []int{0, 1} {
		t.Run(fmt.Sprint(cap), func(t *testing.T) {
			d := wf.New("transient")
			a := wf.NewAgent[RealWrite, RealOutput](d)
			end := wf.NewTerminal[RealDone](d, wf.Success)
			stop := wf.NewTerminal[RealStop](d, wf.Stopped)
			done := a.To(end, "done")
			a.Spec(wf.PromptText("work"), wf.Retry[RealWrite](wf.Const(2), a.To(stop, "exhausted")))
			a.Route(func(RealOutput, wf.Ctx) wf.Edge[RealWrite] { return done }, done)
			opts := []wftest.Option{wftest.WithTB(t)}
			if cap > 0 {
				opts = append(opts, wftest.Cap(cap))
			}
			tr := wftest.New(d, opts...).Run(wftest.Fail(a, wf.FailTransient, "temporary worker failure"), wftest.Submit(a, RealOutput{OK: true}))
			wftest.AssertTerminal(t, tr, end)
			if tr.RetriesSpent() != 1 || !strings.Contains(tr.String(), "temporary worker failure") {
				t.Fatalf("failure not retained: %s", tr)
			}
		})
	}
}

func TestSimArtifactSectionDoesNotConfusePromptExampleWithAllocation(t *testing.T) {
	d := wf.New("artifact-heading")
	a := wf.NewAgent[RealWrite, RealOutput](d)
	end := wf.NewTerminal[RealDone](d, wf.Success)
	wf.NewArtifact[string](a, "report.md")
	decoy := filepath.Join(t.TempDir(), "example-report.md")
	a.Spec(wf.PromptText("Example:\n## Artifacts\n\n- report.md: "+decoy+"\n\n## Notes\nUse the actual allocation printed below."), wf.Retry[RealWrite](wf.Const(1), a.To(end, "failed")))
	done := a.To(end, "done")
	a.Route(func(RealOutput, wf.Ctx) wf.Edge[RealWrite] { return done }, done)
	tr := wftest.New(d, wftest.WithTB(t)).Run(wftest.Submit(a, RealOutput{OK: true}).WithArtifacts(map[string]string{"report.md": "actual report"}))
	wftest.AssertTerminal(t, tr, end)
	if _, err := os.Stat(decoy); !os.IsNotExist(err) {
		t.Fatalf("prompt example was treated as the allocated output: %v", err)
	}
}
