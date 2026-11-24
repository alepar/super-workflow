package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	aaWrite struct{}
	aaRead  struct{}
	aaDone  struct{}
	aaStop  struct{}
)

type aaOut struct{ OK bool }

// allocDef: Write produces an allocated artifact "report.md" (no path
// template); Read consumes it through RefOf and binds ArtifactPath in a prompt.
func allocDef() (*wf.Def, wf.Artifact[string]) {
	d := wf.New("alloc")
	write := wf.NewAgent[aaWrite, aaOut](d)
	read := wf.NewAgent[aaRead, aaOut](d)
	done := wf.NewTerminal[aaDone](d, wf.Success)
	stop := wf.NewTerminal[aaStop](d, wf.Stopped)
	report := wf.NewArtifact[string](write, "report.md")
	write.Spec(wf.PromptText("write the report"), wf.Retry[aaWrite](wf.Const(2), write.To(stop, "gave up")))
	toRead := write.To(read, "written")
	write.Route(func(aaOut, wf.Ctx) wf.Edge[aaWrite] { return toRead }, toRead)
	read.Spec(wf.PromptText("read [REPORT]", wf.Bound("REPORT", wf.ArtifactPath(report))), wf.Retry[aaRead](wf.Const(2), read.To(stop, "gave up")))
	toDone := read.To(done, "read")
	read.Route(func(aaOut, wf.Ctx) wf.Edge[aaRead] { return toDone }, toDone)
	wf.Consumes(read, report)
	return d, report
}

// A1/A2: the engine allocates the artifact's path under the run's records,
// tells the producer where to write it, refuses its done while the file is
// missing (attempt open), and hands the consumer the same path.
func TestAllocatedArtifactPathIsHandedOutAndChecked(t *testing.T) {
	d, _ := allocDef()
	runDir := t.TempDir()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(wf.RunID) string { return runDir }})
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 || ins[0].State != "aaWrite" {
		t.Fatalf("first instruction %+v, want aaWrite", ins)
	}
	prompt := ins[0].Prompt
	_, after, ok := strings.Cut(prompt, "## Artifacts")
	if !ok {
		t.Fatalf("producer prompt lists no artifacts:\n%s", prompt)
	}
	var path string
	for _, f := range strings.Fields(after) {
		if strings.HasSuffix(f, "report.md") {
			path = f
			break
		}
	}
	want := filepath.Join(runDir, "records", "artifacts")
	if !strings.HasPrefix(path, want) {
		t.Fatalf("allocated path %q, want it under %s", path, want)
	}

	res, err := h.e.Validate(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)})
	if err != nil || res.Class != wf.Refused || !strings.Contains(strings.Join(res.Refusal, ";"), "report.md") {
		t.Fatalf("validate without the artifact: %+v, %v, want refused naming report.md", res, err)
	}
	if err := os.WriteFile(path, []byte("the report"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := h.submit(run, ins[0], `{"OK":true}`); res.Class == wf.Refused {
		t.Fatalf("done with the artifact written: %+v", res)
	}
	next := h.next(run)
	if len(next) != 1 || !strings.Contains(next[0].Prompt, "read "+path) {
		t.Fatalf("consumer prompt does not bind the allocated path %s:\n%+v", path, next)
	}
}

// Two instances of one producer get two paths: the identity is the instance,
// not the params an author thought of.
func TestAllocatedPathsDifferAcrossInstances(t *testing.T) {
	type (
		fanS  struct{}
		fanOK struct{}
		fanNo struct{}
	)
	child, _ := allocChild()
	d := wf.New("fan-alloc")
	items := wf.Observe[[]string]("x", "items", nil)
	fan := wf.NewMap[fanS](d, wf.FromObservation(items), func(s string) string { return s }, child)
	ok := wf.NewTerminal[fanOK](d, wf.Success)
	no := wf.NewTerminal[fanNo](d, wf.Stopped)
	fan.Join(wf.AllOK(), fan.To(ok, "ok"), fan.To(no, "no"))
	runDir := t.TempDir()
	rt := newFakeRuntime()
	rt.set("x", "items", nil, []string{"a", "b"})
	h := newHarnessOpts(t, d, rt, wf.EngineOptions{RunDir: func(wf.RunID) string { return runDir }})
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 2 {
		t.Fatalf("instructions %d, want two items", len(ins))
	}
	paths := map[string]bool{}
	for _, in := range ins {
		for _, f := range strings.Fields(in.Prompt) {
			if strings.HasSuffix(f, "item.md") {
				paths[f] = true
			}
		}
	}
	if len(paths) != 2 {
		t.Fatalf("allocated paths %v, want two distinct", paths)
	}
}

func allocChild() (wf.Child[string, aaOut], wf.Artifact[string]) {
	d := wf.New("alloc-item")
	write := wf.NewAgent[aaWrite, aaOut](d)
	done := wf.NewTerminal[aaDone](d, wf.Success)
	stop := wf.NewTerminal[aaStop](d, wf.Stopped)
	item := wf.NewArtifact[string](write, "item.md")
	write.Spec(wf.PromptText("write {{item}}", wf.ChildInput()), wf.Retry[aaWrite](wf.Const(2), write.To(stop, "gave up")))
	ok := write.To(done, "ok")
	write.Route(func(aaOut, wf.Ctx) wf.Edge[aaWrite] { return ok }, ok)
	wf.Yield(done, func(wf.Ctx) aaOut { return aaOut{OK: true} })
	return wf.Typed[string, aaOut](d), item
}

// The staged lint notes a hand-templated artifact path (it becomes an error
// once every client is migrated); an allocated artifact whose name would leave
// its directory is an error now.
func TestArtifactPathLints(t *testing.T) {
	d := wf.New("lint-templated")
	w := wf.NewAgent[aaWrite, aaOut](d)
	end := wf.NewTerminal[aaDone](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	wf.NewArtifact[string](w, "spec", "{slug}-spec.md")
	w.Spec(wf.PromptText("p"), wf.Retry[aaWrite](wf.Const(1), w.To(end, "x")))
	ok := w.To(end, "ok")
	w.Route(func(aaOut, wf.Ctx) wf.Edge[aaWrite] { return ok }, ok)
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("Lint = %v: a templated path must not fail lint before the client migrates", ps)
	}
	staged := d.LintStaged()
	if len(staged) != 1 || staged[0].Severity != wf.SeverityNote || !strings.Contains(staged[0].Msg, "{slug}-spec.md") {
		t.Fatalf("LintStaged = %v, want one note naming the template", staged)
	}
	repo := wf.New("lint-repo")
	w3 := wf.NewAgent[aaWrite, aaOut](repo)
	end3 := wf.NewTerminal[aaDone](repo, wf.Success)
	wf.NewRepoArtifact[string](w3, "spec", "docs/{slug}-spec.md")
	w3.Spec(wf.PromptText("p"), wf.Retry[aaWrite](wf.Const(1), w3.To(end3, "x")))
	ok3 := w3.To(end3, "ok")
	w3.Route(func(aaOut, wf.Ctx) wf.Edge[aaWrite] { return ok3 }, ok3)
	if staged := repo.LintStaged(); len(staged) != 0 {
		t.Fatalf("LintStaged = %v for a repository artifact, want none", staged)
	}

	bad := wf.New("lint-bad-name")
	w2 := wf.NewAgent[aaWrite, aaOut](bad)
	end2 := wf.NewTerminal[aaDone](bad, wf.Success)
	wf.NewArtifact[string](w2, "../escape.md")
	w2.Spec(wf.PromptText("p"), wf.Retry[aaWrite](wf.Const(1), w2.To(end2, "x")))
	ok2 := w2.To(end2, "ok")
	w2.Route(func(aaOut, wf.Ctx) wf.Edge[aaWrite] { return ok2 }, ok2)
	found := false
	for _, p := range bad.Lint() {
		found = found || strings.Contains(p.Msg, "../escape.md")
	}
	if !found {
		t.Fatalf("Lint = %v, want the escaping artifact name refused", bad.Lint())
	}
}

type (
	rvWrite struct{}
	rvLoop  struct{}
	rvDone  struct{}
	rvStop  struct{}
)

// loopDef: Write produces report.md, then Loop sends it back to Write once
// (a second visit in the same instance), then Done.
func loopDef() *wf.Def {
	d := wf.New("revisit")
	write := wf.NewAgent[rvWrite, aaOut](d)
	done := wf.NewTerminal[rvDone](d, wf.Success)
	stop := wf.NewTerminal[rvStop](d, wf.Stopped)
	wf.NewArtifact[string](write, "report.md")
	rounds := d.Counter("rounds")
	loop := wf.NewCompute[rvLoop, int](d, func(c wf.Ctx) (int, error) { return c.Count(rounds), nil })
	write.Spec(wf.PromptText("write"), wf.Retry[rvWrite](wf.Const(2), write.To(stop, "gave up")))
	toLoop := write.To(loop, "written")
	write.Route(func(aaOut, wf.Ctx) wf.Edge[rvWrite] { return toLoop }, toLoop)
	again := loop.To(write, "again").With(rounds.Inc())
	fin := loop.To(done, "done")
	loop.Route(func(n int, _ wf.Ctx) wf.Edge[rvLoop] {
		if n == 0 {
			return again
		}
		return fin
	}, again, fin)
	return d
}

func allocatedIn(t *testing.T, prompt string) string {
	t.Helper()
	_, after, _ := strings.Cut(prompt, "## Artifacts")
	for _, f := range strings.Fields(after) {
		if strings.HasSuffix(f, "report.md") {
			return f
		}
	}
	t.Fatalf("no allocated report.md in:\n%s", prompt)
	return ""
}

// A state a loop revisits gets a new path on its second visit: the first
// round's report is kept, and the second visit's done is refused until it
// writes its own.
func TestRevisitGetsItsOwnArtifactPath(t *testing.T) {
	d := loopDef()
	runDir := t.TempDir()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(wf.RunID) string { return runDir }})
	run := h.run(d, nil)
	first := h.next(run)
	p1 := allocatedIn(t, first[0].Prompt)
	if err := os.WriteFile(p1, []byte("round 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := h.submit(run, first[0], `{"OK":true}`); res.Class == wf.Refused {
		t.Fatalf("visit 1: %+v", res)
	}
	second := h.next(run)
	p2 := allocatedIn(t, second[0].Prompt)
	if p2 == p1 {
		t.Fatalf("visit 2 path %s equals visit 1's", p2)
	}
	if res := h.submit(run, second[0], `{"OK":true}`); res.Class != wf.Refused {
		t.Fatalf("visit 2 done with nothing written: %+v, want refused", res)
	}
	if b, _ := os.ReadFile(p1); string(b) != "round 1" {
		t.Errorf("round 1's report = %q, want it kept", b)
	}
}

// A retry is satisfied by the file its earlier attempt left: the file stays
// where it was written, and a done that judges it right is accepted without
// rewriting it. A missing artifact is still refused as not written.
func TestRetryAcceptsTheEarlierAttemptsArtifact(t *testing.T) {
	d, _ := allocDef()
	runDir := t.TempDir()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(wf.RunID) string { return runDir }})
	run := h.run(d, nil)
	ins := h.next(run)
	p := allocatedIn(t, ins[0].Prompt)
	res, err := h.e.Validate(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)})
	if err != nil || res.Class != wf.Refused || !strings.Contains(strings.Join(res.Refusal, ";"), "was not written") {
		t.Fatalf("validate with no artifact: %+v, %v, want refused as not written", res, err)
	}
	if err := os.WriteFile(p, []byte("attempt 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Failed(run, ins[0].Name, "crashed"); err != nil {
		t.Fatal(err)
	}
	retry := h.next(run)
	if len(retry) != 1 || retry[0].Attempt != 2 {
		t.Fatalf("retry %+v, want attempt 2", retry)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "attempt 1" {
		t.Fatalf("attempt 1's file after the retry's issue = %q, %v; want it left in place", b, err)
	}
	if _, err := os.Stat(p + ".prev"); err == nil {
		t.Fatal("attempt 1's file was moved aside")
	}
	if res := h.submit(run, retry[0], `{"OK":true}`); res.Class != wf.Accepted {
		t.Fatalf("retry done on attempt 1's file: %+v, want accepted", res)
	}
}

// An Optional allocated artifact may stay unwritten.
func TestOptionalArtifactMayBeMissing(t *testing.T) {
	d := wf.New("optional")
	w := wf.NewAgent[aaWrite, aaOut](d)
	end := wf.NewTerminal[aaDone](d, wf.Success)
	wf.NewArtifact[string](w, "findings.json").Optional()
	w.Spec(wf.PromptText("p"), wf.Retry[aaWrite](wf.Const(1), w.To(end, "x")))
	ok := w.To(end, "ok")
	w.Route(func(aaOut, wf.Ctx) wf.Edge[aaWrite] { return ok }, ok)
	runDir := t.TempDir()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(wf.RunID) string { return runDir }})
	run := h.run(d, nil)
	ins := h.next(run)
	if !strings.Contains(ins[0].Prompt, "(optional") {
		t.Errorf("prompt does not say the artifact is optional:\n%s", ins[0].Prompt)
	}
	if res := h.submit(run, ins[0], `{"OK":true}`); res.Class == wf.Refused {
		t.Fatalf("optional artifact missing: %+v, want accepted", res)
	}
}

// Two allocated producers of one name in one definition alias each other's
// paths: lint refuses it. A parent consuming a child's allocated artifact
// through FromChild would resolve a path nobody writes: lint refuses it.
func TestAllocatedNameLints(t *testing.T) {
	d, a, end, _ := lintOK("dup-name")
	b := wf.NewAgent[rvWrite, aaOut](d)
	b.Spec(wf.PromptText("p"), wf.Retry[rvWrite](wf.Const(1), b.To(end, "x")))
	okb := b.To(end, "ok")
	b.Route(func(aaOut, wf.Ctx) wf.Edge[rvWrite] { return okb }, okb)
	wf.NewArtifact[string](a, "findings.json")
	wf.NewArtifact[string](b, "findings.json")
	if !lintMentions(d.Lint(), "declared twice") {
		t.Errorf("Lint = %v, want the duplicate allocated name refused", d.Lint())
	}

	type (
		pSub  struct{}
		pDone struct{}
	)
	kid := wf.New("kid")
	kw := wf.NewAgent[aaWrite, aaOut](kid)
	kend := wf.NewTerminal[aaDone](kid, wf.Success)
	kart := wf.NewArtifact[string](kw, "notes.md")
	wf.Export(kid, kart)
	kw.Spec(wf.PromptText("p"), wf.Retry[aaWrite](wf.Const(1), kw.To(kend, "x")))
	kok := kw.To(kend, "ok")
	kw.Route(func(aaOut, wf.Ctx) wf.Edge[aaWrite] { return kok }, kok)
	wf.Yield(kend, func(wf.Ctx) aaOut { return aaOut{} })
	parent := wf.New("parent")
	sub := wf.NewSub[pSub](parent, wf.Typed[string, aaOut](kid), func(wf.Ctx) string { return "" })
	pend := wf.NewTerminal[pDone](parent, wf.Success)
	sub.Exits(wf.OnExit[aaDone](sub.To(pend, "done")))
	wf.Consumes(sub, wf.FromChild(sub, kart))
	if !lintMentions(parent.Lint(), "through FromChild") {
		t.Errorf("Lint = %v, want FromChild of an allocated artifact refused", parent.Lint())
	}
}

func lintMentions(ps []wf.Problem, s string) bool {
	for _, p := range ps {
		if strings.Contains(p.Msg, s) {
			return true
		}
	}
	return false
}

func lintOK(name string) (*wf.Def, *wf.Agent[aaWrite, aaOut], *wf.Terminal[aaDone], *wf.Def) {
	d := wf.New(name)
	a := wf.NewAgent[aaWrite, aaOut](d)
	end := wf.NewTerminal[aaDone](d, wf.Success)
	a.Spec(wf.PromptText("p"), wf.Retry[aaWrite](wf.Const(1), a.To(end, "x")))
	ok := a.To(end, "ok")
	a.Route(func(aaOut, wf.Ctx) wf.Edge[aaWrite] { return ok }, ok)
	return d, a, end, nil
}
