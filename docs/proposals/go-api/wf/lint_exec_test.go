package wf_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type (
	xaAgent   struct{}
	xaOther   struct{}
	xaCompute struct{}
	xaDone    struct{}
	xaStop    struct{}
	xaOut     struct {
		Branch string `json:"branch" pattern:"^[a-z0-9][a-z0-9/._-]*$"`
		Title  string `json:"title"`
		Count  int    `json:"count"`
	}
	xaOtherOut struct {
		// Same name as xaOut's validated branch, free text: it can bind
		// {branch} too, whichever was stored last.
		Branch string `json:"Branch"`
	}
	xaComp struct {
		Path string `json:"path" pattern:"^/[A-Za-z0-9._/-]+$"`
		Text string `json:"text"`
	}
)

func execLines(d *wf.Def) []string {
	var got []string
	for _, p := range d.LintExec() {
		got = append(got, p.Line())
	}
	return got
}

// S3a: a placeholder is clean only when every stored type that can bind it
// is validated (a pattern-tagged string, an integer, an enum); free text is
// an error in any argv position, the program and a script's text included.
func TestLintExecTypes(t *testing.T) {
	build := func(withOther bool) *wf.Def {
		d := wf.New("ex")
		d.Param("slug")
		a := wf.NewAgent[xaAgent, xaOut](d)
		c := wf.NewCompute[xaCompute, xaComp](d, func(wf.Ctx) (xaComp, error) { return xaComp{}, nil })
		done := wf.NewTerminal[xaDone](d, wf.Success)
		stop := wf.NewTerminal[xaStop](d, wf.Stopped)
		a.Spec(wf.PromptText("write"), wf.Retry[xaAgent](wf.Const(1), a.To(stop, "failed")))
		toC := a.To(c, "written").With(
			shell.Effect(wf.KeyOf("co"), shell.Run("git", "checkout", "{branch}", "{count}", "{slug}", "{run}")),
			shell.Effect(wf.KeyOf("say"), shell.Run("bash", "-c", "echo {title}", "say")))
		a.Route(func(xaOut, wf.Ctx) wf.Edge[xaAgent] { return toC }, toC)
		var next wf.State = done
		if withOther {
			o := wf.NewAgent[xaOther, xaOtherOut](d)
			o.Spec(wf.PromptText("other"), wf.Retry[xaOther](wf.Const(1), o.To(stop, "failed")))
			toDone := o.To(done, "other done")
			o.Route(func(xaOtherOut, wf.Ctx) wf.Edge[xaOther] { return toDone }, toDone)
			next = o
		}
		toNext := c.To(next, "computed").With(shell.Effect(wf.KeyOf("cat"), shell.Run("cat", "{path}", "{text}")))
		c.Route(func(xaComp, wf.Ctx) wf.Edge[xaCompute] { return toNext }, toNext)
		return d
	}
	got := execLines(build(false))
	want := []string{
		`lint: exec-tainted ex/xaAgent edge "written" effect · {title} (script) can bind an unvalidated value: ex/xaAgent output`,
		`lint: exec-tainted ex/xaCompute edge "computed" effect · {text} (argument) can bind an unvalidated value: ex/xaCompute computed`,
	}
	match := len(got) == len(want)
	for i := 0; match && i < len(want); i++ {
		match = strings.HasPrefix(got[i], want[i])
	}
	if !match {
		t.Fatalf("exec lint:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// An unvalidated field of the same name anywhere in the run taints the
	// validated one (binding is by name, newest first, without regard to case).
	got = execLines(build(true))
	if len(got) != 3 || !strings.Contains(got[0], `{branch} (argument) can bind an unvalidated value: ex/xaOther output`) {
		t.Fatalf("a shadowing field:\n%s", strings.Join(got, "\n"))
	}
}

// A pattern tag is checked: an agent's value that does not match is refused
// in place, and a compute's is refused too.
func TestPatternTagIsValidated(t *testing.T) {
	d := wf.New("pat")
	a := wf.NewAgent[xaAgent, xaOut](d)
	done := wf.NewTerminal[xaDone](d, wf.Success)
	stop := wf.NewTerminal[xaStop](d, wf.Stopped)
	a.Spec(wf.PromptText("write"), wf.Retry[xaAgent](wf.Const(2), a.To(stop, "failed")))
	ok := a.To(done, "ok")
	a.Route(func(xaOut, wf.Ctx) wf.Edge[xaAgent] { return ok }, ok)
	tr := wftest.New(d, wftest.WithTB(t)).Run(
		wftest.SubmitAt("xaAgent", xaOut{Branch: "--upload-pack=evil"}),
		wftest.SubmitAt("xaAgent", xaOut{Branch: "task-1"}))
	if tr.Terminal() != "xaDone" || tr.RetriesSpent() != 1 {
		t.Fatalf("terminal %q, %d refusals: want the option-shaped branch refused, then accepted:\n%s", tr.Terminal(), tr.RetriesSpent(), tr)
	}

	d2 := wf.New("pat-compute")
	a2 := wf.NewAgent[xaAgent, xaOut](d2)
	c := wf.NewCompute[xaCompute, xaComp](d2, func(wf.Ctx) (xaComp, error) { return xaComp{Path: "relative; rm -rf /"}, nil })
	done2 := wf.NewTerminal[xaDone](d2, wf.Success)
	stop2 := wf.NewTerminal[xaStop](d2, wf.Stopped)
	a2.Spec(wf.PromptText("write"), wf.Retry[xaAgent](wf.Const(1), a2.To(stop2, "failed")))
	toC := a2.To(c, "written")
	a2.Route(func(xaOut, wf.Ctx) wf.Edge[xaAgent] { return toC }, toC)
	next := c.To(done2, "computed")
	c.Route(func(xaComp, wf.Ctx) wf.Edge[xaCompute] { return next }, next)
	c.Retry(wf.Retry[xaCompute](wf.Const(1), c.To(stop2, "refused")))
	if tr := wftest.New(d2, wftest.WithTB(t)).Run(wftest.SubmitAt("xaAgent", xaOut{Branch: "main"})); tr.Terminal() != "xaStop" {
		t.Fatalf("a compute's value against its pattern: terminal %q, want the refusal's stop:\n%s", tr.Terminal(), tr)
	}
}

type (
	xbRead  struct{}
	xbAgent struct{}
	xbOut   struct {
		LastGood string `json:"lastGood"`
		Opt      string `json:"opt" pattern:"^.*$"`
	}
)

// Review: a read runs its argv too (an agent's lastGood reached git diff
// there), and a pattern that admits an option is no validation.
func TestLintExecReadsAndOptionPatterns(t *testing.T) {
	d := wf.New("exr")
	a := wf.NewAgent[xbAgent, xbOut](d)
	diff := shell.Observe(shell.Run("bash", "-c", `git diff --name-only "$1" HEAD`, "amend-diff", "{lastGood}"))
	r := wf.NewCompute[xbRead, bool](d, func(c wf.Ctx) (bool, error) { _, err := wf.Observed(c, diff); return true, err }).Observes(diff)
	done := wf.NewTerminal[xaDone](d, wf.Success)
	stop := wf.NewTerminal[xaStop](d, wf.Stopped)
	a.Spec(wf.PromptText("w"), wf.Retry[xbAgent](wf.Const(1), a.To(stop, "failed")))
	toR := a.To(r, "drafted").With(shell.Effect(wf.KeyOf("o"), shell.Run("tool", "{opt}")))
	a.Route(func(xbOut, wf.Ctx) wf.Edge[xbAgent] { return toR }, toR)
	end := r.To(done, "read")
	r.Route(func(bool, wf.Ctx) wf.Edge[xbRead] { return end }, end)
	got := strings.Join(execLines(d), "\n")
	for _, want := range []string{
		`lint: exec-tainted exr/xbAgent edge "drafted" effect · {opt} (argument) can bind an unvalidated value: exr/xbAgent output`,
		`lint: exec-tainted exr/xbRead read · {lastGood} (argument) can bind an unvalidated value: exr/xbAgent output`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("exec lint lacks %q:\n%s", want, got)
		}
	}
}

type (
	xcCompute struct{}
	xcAgent   struct{}
	xcMap     struct {
		Vars map[string]string `json:"vars"`
	}
	xcOut struct {
		Branch *string `json:"branch" pattern:"^[a-z]+$"`
		Help   string  `json:"help" pattern:"^--help$"`
		Alt    string  `json:"alt" pattern:"^safe|help$"`
	}
)

// Cross-review repros: a map binds any key (tainted unless its values are
// typed), a pattern that can only match an option is no validation, and a
// *string field's pattern is checked at run time.
func TestLintExecMapsPointersAndOptionPatterns(t *testing.T) {
	d := wf.New("exm")
	c := wf.NewCompute[xcCompute, xcMap](d, func(wf.Ctx) (xcMap, error) { return xcMap{}, nil })
	a := wf.NewAgent[xcAgent, xcOut](d)
	done := wf.NewTerminal[xaDone](d, wf.Success)
	stop := wf.NewTerminal[xaStop](d, wf.Stopped)
	toA := c.To(a, "computed").With(shell.Effect(wf.KeyOf("m"), shell.Run("tool", "{vars.branch}")))
	c.Route(func(xcMap, wf.Ctx) wf.Edge[xcCompute] { return toA }, toA)
	a.Spec(wf.PromptText("w"), wf.Retry[xcAgent](wf.Const(2), a.To(stop, "failed")))
	end := a.To(done, "done").With(shell.Effect(wf.KeyOf("b"), shell.Run("git", "checkout", "{branch}", "{help}", "{alt}")))
	a.Route(func(xcOut, wf.Ctx) wf.Edge[xcAgent] { return end }, end)
	got := strings.Join(execLines(d), "\n")
	for _, want := range []string{"{vars.branch} (argument)", "{help} (argument)"} {
		if !strings.Contains(got, want) {
			t.Errorf("exec lint lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "{branch}") {
		t.Errorf("a validated *string field is reported:\n%s", got)
	}

	d2 := wf.New("exm-run")
	a2 := wf.NewAgent[xcAgent, xcOut](d2)
	done2 := wf.NewTerminal[xaDone](d2, wf.Success)
	stop2 := wf.NewTerminal[xaStop](d2, wf.Stopped)
	a2.Spec(wf.PromptText("w"), wf.Retry[xcAgent](wf.Const(2), a2.To(stop2, "failed")))
	end2 := a2.To(done2, "done")
	a2.Route(func(xcOut, wf.Ctx) wf.Edge[xcAgent] { return end2 }, end2)
	evil := "--upload-pack=evil"
	tr := wftest.New(d2, wftest.WithTB(t)).Run(
		wftest.SubmitAt("xcAgent", xcOut{Branch: &evil}),
		wftest.SubmitAt("xcAgent", xcOut{}))
	if tr.RetriesSpent() != 1 {
		t.Fatalf("a *string field off its pattern was not refused:\n%s", tr)
	}
	// ^safe|help$ is checked whole: --help (which help$ alone matches) is refused.
	tr = wftest.New(d2, wftest.WithTB(t)).Run(
		wftest.SubmitAt("xcAgent", xcOut{Alt: "--help"}),
		wftest.SubmitAt("xcAgent", xcOut{Alt: "help"}))
	if tr.RetriesSpent() != 1 || tr.Terminal() != "xaDone" {
		t.Fatalf("an unanchored alternative let an option through:\n%s", tr)
	}
}

type (
	xdAgent struct{}
	xdOut   struct {
		Promoted []string `json:"promoted" pattern:"^[a-z0-9][a-z0-9.-]*$"`
	}
)

// A pattern tag on a list of strings applies to each element: an id an agent
// lists cannot be read as an option by the command it reaches at run time.
func TestPatternTagOnAListAppliesToEachElement(t *testing.T) {
	d := wf.New("pat-list")
	a := wf.NewAgent[xdAgent, xdOut](d)
	done := wf.NewTerminal[xaDone](d, wf.Success)
	stop := wf.NewTerminal[xaStop](d, wf.Stopped)
	a.Spec(wf.PromptText("w"), wf.Retry[xdAgent](wf.Const(2), a.To(stop, "failed")))
	end := a.To(done, "done")
	a.Route(func(xdOut, wf.Ctx) wf.Edge[xdAgent] { return end }, end)
	tr := wftest.New(d, wftest.WithTB(t)).Run(
		wftest.SubmitAt("xdAgent", xdOut{Promoted: []string{"e1.2", "--force"}}),
		wftest.SubmitAt("xdAgent", xdOut{Promoted: []string{"e1.2"}}))
	if tr.Terminal() != "xaDone" || tr.RetriesSpent() != 1 {
		t.Fatalf("an option-shaped list element was not refused:\n%s", tr)
	}
}
