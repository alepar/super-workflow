package git_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	hcWork struct{}
	hcDone struct{}
	hcBad  struct{}
)

// hcOut is a typed result carrying a head.
type hcOut struct{ Head string }

func hcDef() *wf.Def {
	d := wf.New("headcheck")
	a := wf.NewAgent[hcWork, hcOut](d)
	done := wf.NewTerminal[hcDone](d, wf.Success)
	bad := wf.NewTerminal[hcBad](d, wf.Stopped)
	a.Spec(wf.PromptText("p"), wf.Retry[hcWork](wf.Const(3), a.To(bad, "gave up")),
		git.HeadCheck(".", func(o hcOut) string { return o.Head }))
	next := a.To(done, "done")
	a.Route(func(hcOut, wf.Ctx) wf.Edge[hcWork] { return next }, next)
	return d
}

type hcRun struct {
	t   *testing.T
	e   wf.Executor
	run wf.RunID
}

func startHC(t *testing.T, repo string, rt wf.AdapterRuntime) hcRun {
	t.Helper()
	reg := wf.NewAdapters()
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	d := hcDef()
	e := wf.NewEngine(wf.MemJournal(), reg, wf.EngineOptions{Workspace: repo, Defs: []*wf.Def{d}})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return hcRun{t: t, e: e, run: run}
}

// submit answers the open instruction with out.
func (h hcRun) submit(out hcOut) wf.SubmitResult {
	h.t.Helper()
	ins, err := h.e.Next(h.run, "")
	if err != nil || len(ins) != 1 || ins[0].Kind == wf.InstructionEnded {
		h.t.Fatalf("next: %v %+v", err, ins)
	}
	raw, _ := json.Marshal(out)
	res, err := h.e.Submit(h.run, wf.Submission{Token: ins[0].Token, Output: raw})
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func commitOn(t *testing.T, repo, branch, file string) string {
	t.Helper()
	sh(t, repo, "checkout", "-q", branch)
	writeFile(t, repo, file, file+"\n")
	sh(t, repo, "add", file)
	sh(t, repo, "commit", "-q", "-m", file)
	return sh(t, repo, "rev-parse", "HEAD")
}

// hcRepo: main is c0 - base; side forks at c0 (not a descendant of base);
// task forks at base with one commit.
func hcRepo(t *testing.T) (repo, base, side, task string) {
	repo = newRepo(t)
	sh(t, repo, "branch", "side")
	base = commitOn(t, repo, "main", "base.txt")
	side = commitOn(t, repo, "side", "side.txt")
	sh(t, repo, "branch", "task", base)
	task = commitOn(t, repo, "task", "task.txt")
	sh(t, repo, "checkout", "-q", "main")
	return repo, base, side, task
}

func refused(t *testing.T, res wf.SubmitResult, cond string) {
	t.Helper()
	if res.Class != wf.Refused || len(res.Refusal) != 1 || !strings.HasPrefix(res.Refusal[0], "head: ") || !strings.Contains(res.Refusal[0], cond) {
		t.Fatalf("result %+v, want refused naming %q", res, cond)
	}
}

func TestHeadCheck(t *testing.T) {
	repo, base, side, task := hcRepo(t)

	t.Run("missing", func(t *testing.T) {
		refused(t, startHC(t, repo, rt()).submit(hcOut{Head: strings.Repeat("e", 40)}), "missing")
	})
	t.Run("not a full SHA", func(t *testing.T) {
		sh(t, repo, "tag", "-a", "-m", "annotated", "ann", task)
		tagObj := sh(t, repo, "rev-parse", "ann")
		for _, head := range []string{"task", "HEAD", "main", task[:12], task + "~0", "-" + task[1:], "{taskBase}", "", strings.ToUpper(task)} {
			refused(t, startHC(t, repo, rt()).submit(hcOut{Head: head}), "head "+`"`+head+`"`+" is not a full commit SHA")
		}
		// 40 hex digits naming a tag object, not the commit it peels to.
		refused(t, startHC(t, repo, rt()).submit(hcOut{Head: tagObj}), "head "+tagObj+" is not a full commit SHA")
	})
	t.Run("any existing commit is accepted", func(t *testing.T) {
		// Ancestry is the agent's attestation, not the check's: a head off
		// the base, equal to it or on another branch passes the form check.
		for _, head := range []string{task, base, side} {
			if res := startHC(t, repo, rt()).submit(hcOut{Head: head}); res.Class != wf.Accepted {
				t.Fatalf("head %s: %+v", head, res)
			}
		}
	})
	t.Run("refusal keeps the attempt", func(t *testing.T) {
		h := startHC(t, repo, rt())
		refused(t, h.submit(hcOut{Head: "task"}), "not a full commit SHA")
		if res := h.submit(hcOut{Head: task}); res.Class != wf.Accepted {
			t.Fatalf("resubmission after refusal: %+v", res)
		}
	})
	t.Run("unavailable", func(t *testing.T) {
		h := startHC(t, repo, git.Runtime(git.Config{Bin: "git-not-installed-here"}))
		res := h.submit(hcOut{Head: task})
		if res.Class != wf.Unavailable {
			t.Fatalf("result %+v, want unavailable, not a refusal", res)
		}
	})
}
