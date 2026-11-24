package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	citeImplement struct{}
	citeReview    struct{}
	citeDone      struct{}
	citeStop      struct{}
)

type citeImplOut struct{ Status string }

type citeEvidence struct {
	Command  string `json:"command"`
	Location string `json:"location"`
}

type citeReviewOut struct {
	Verdict  string         `json:"verdict"`
	Evidence []citeEvidence `json:"evidence"`
}

// citeDef is the the regression case shape: Implement records an "evidence"
// directory artifact; Review returns evidence whose location cites a file the
// engine recorded in it, as task-reviewer-prompt.md tells the reviewer to.
func citeDef() *wf.Def {
	d := wf.New("cite")
	impl := wf.NewAgent[citeImplement, citeImplOut](d)
	review := wf.NewAgent[citeReview, citeReviewOut](d)
	done := wf.NewTerminal[citeDone](d, wf.Success)
	stop := wf.NewTerminal[citeStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	wf.NewArtifact[string](impl, "evidence").Optional() // a directory, as the regression case's
	toReview := impl.To(review, "implemented")
	impl.Spec(wf.PromptText("implement"), wf.Retry[citeImplement](wf.Const(1), impl.To(stop, "gave up")))
	impl.Route(func(citeImplOut, wf.Ctx) wf.Edge[citeImplement] { return toReview }, toReview)
	toDone := review.To(done, "reviewed")
	review.Spec(wf.PromptText("review"), wf.Retry[citeReview](wf.Const(1), review.To(stop, "gave up")))
	review.Route(func(citeReviewOut, wf.Ctx) wf.Edge[citeReview] { return toDone }, toDone)
	return d
}

// citeRun drives citeDef to an open Review attempt: Implement wrote its
// evidence directory with a test log and finished. It returns the review
// instruction and the log's absolute path.
func citeRun(t *testing.T) (h *harness, run wf.RunID, review wf.Instruction, log, state, workspace string) {
	return citeRunIn(t, false)
}

// citeRunIn is citeRun with the state directory inside the workspace when
// nested, as a super run keeps it (<repo>/.superpowers/state).
func citeRunIn(t *testing.T, nested bool) (h *harness, run wf.RunID, review wf.Instruction, log, state, workspace string) {
	return citeRunLayout(t, nested, "runs")
}

// citeRunLayout is citeRunIn with run directories at <state>/<runs>/<id>.
func citeRunLayout(t *testing.T, nested bool, runs string) (h *harness, run wf.RunID, review wf.Instruction, log, state, workspace string) {
	t.Helper()
	state, workspace = t.TempDir(), t.TempDir()
	if nested {
		state = filepath.Join(workspace, ".superpowers", "state")
	}
	d := citeDef()
	h = newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: workspace, StateRoot: state, RunDir: func(r wf.RunID) string { return filepath.Join(state, runs, string(r)) }})
	run = h.run(d, nil)
	impl, implPrompt := startedUnit(t, h, run, "implement")
	for _, f := range strings.Fields(implPrompt) {
		if strings.HasSuffix(f, "/prompt.md") {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			implPrompt = string(body)
			break
		}
	}
	_, after, _ := strings.Cut(implPrompt, "## Artifacts")
	var dir string
	for _, f := range strings.Fields(after) {
		if strings.HasSuffix(f, "/evidence") {
			dir = f
			break
		}
	}
	if dir == "" {
		t.Fatalf("no evidence artifact path in the implementer's prompt:\n%s", implPrompt)
	}
	log = filepath.Join(dir, "timeline-test.log")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := h.e.Done(run, impl.Name, wf.Submission{Token: impl.Token, Output: json.RawMessage(`{"Status":"implemented"}`)}); err != nil || res.Class != wf.Accepted {
		t.Fatalf("implement done: %+v %v", res, err)
	}
	review, _ = startedUnit(t, h, run, "review")
	return h, run, review, log, state, workspace
}

func citeOutput(location string) json.RawMessage {
	raw, _ := json.Marshal(citeReviewOut{Verdict: "CLEAN", Evidence: []citeEvidence{{Command: "go test ./internal/film", Location: location}}})
	return raw
}

// an earlier revision A: citing is not claiming. A review that returns, as evidence, the
// path of a file inside an artifact the engine recorded for this run (or the
// artifact itself) validates and is accepted.
// It holds with the state directory inside the workspace, as in the regression case.
func TestAResultMayCiteAnArtifactTheEngineRecorded(t *testing.T) {
	for _, nested := range []bool{false, true} {
		h, run, review, log, state, _ := citeRunIn(t, nested)
		for _, loc := range []string{log, filepath.Dir(log)} {
			res, err := h.e.Validate(run, review.Name, wf.Submission{Token: review.Token, Output: citeOutput(loc)})
			if err != nil || res.Class != wf.Accepted {
				t.Fatalf("nested=%v: validate citing %s: %+v %v", nested, loc, res, err)
			}
		}
		if nested {
			// Not cited, still run-owned (not "make it relative").
			other := filepath.Join(state, "runs", string(run), "records", "notes.md")
			_, err := h.e.Validate(run, review.Name, wf.Submission{Token: review.Token, Output: citeOutput(other)})
			if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "leave run-owned paths out") {
				t.Fatalf("nested: an unrecorded run path = %v, want the run-owned refusal", err)
			}
		}
		if res, err := h.e.Done(run, review.Name, wf.Submission{Token: review.Token, Output: citeOutput(log)}); err != nil || res.Class != wf.Accepted {
			t.Fatalf("nested=%v: done citing the recorded log: %+v %v", nested, res, err)
		}
	}
}

// an earlier revision A and B: a run-owned path the engine did not record stays refused,
// and the refusal names the field, the path, and what to give instead.
func TestARunOwnedPathTheEngineDidNotRecordIsRefusedByField(t *testing.T) {
	h, run, review, log, state, workspace := citeRun(t)
	scratch := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(log))), "scratch", "notes.md")
	if !strings.HasPrefix(scratch, state) {
		t.Fatalf("fixture: %s is not under the state root %s", scratch, state)
	}
	sibling := filepath.Dir(log) + "-other/x.log" // a prefix match is not inside the artifact
	for _, loc := range []string{scratch, sibling} {
		_, err := h.e.Validate(run, review.Name, wf.Submission{Token: review.Token, Output: citeOutput(loc)})
		msg := strings.TrimPrefix(err.Error(), "rejected: ")
		if !errors.Is(err, wf.ErrRejected) || !strings.Contains(msg, "evidence[0].location") || !strings.Contains(msg, loc) || !strings.Contains(msg, "leave run-owned paths out") || !strings.Contains(msg, "recorded") {
			t.Fatalf("validate citing unrecorded %s = %v; want a refusal naming the field, the path and what to give instead", loc, err)
		}
	}
	ws := filepath.Join(workspace, "internal", "film", "timeline.go")
	_, err := h.e.Validate(run, review.Name, wf.Submission{Token: review.Token, Output: citeOutput(ws)})
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "evidence[0].location") || !strings.Contains(err.Error(), ws) || !strings.Contains(err.Error(), "internal/film/timeline.go") {
		t.Fatalf("validate with an absolute workspace path = %v; want the field, the path and the relative form", err)
	}
}

// an earlier revision C: a refusal the child can fix tells it to fix and run the command
// again, and to stop only when the same refusal comes back twice; it never
// ends with the stop-and-report tail. A refusal it cannot fix in this
// attempt (done spent the attempt) keeps that tail.
func TestAFixableRefusalSaysFixAndRetryNotStop(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	state, workspace := t.TempDir(), t.TempDir()
	d := resultPathsDefinition()
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}}
	call := func(args ...string) (int, string, string) {
		var out, er bytes.Buffer
		code := fixtureCLI(cfg, append(args, "--state-dir", state), &out, &er)
		return code, out.String(), er.String()
	}
	_, o, _ := call("run", d.Name(), "--workspace", workspace, "--run-id", "fixable")
	run := strings.TrimSpace(o)
	var ins []wf.Instruction
	if _, o, er := call("next", run, "1", "--json"); json.Unmarshal([]byte(o), &ins) != nil || len(ins) != 1 {
		t.Fatalf("next: %s %s", o, er)
	}
	in := ins[0]
	call("dev", "started", run, in.Name, "--harness", "claude", "--session", "session", "--agent", "agent")
	call("prompt", run, in.Name)
	childExit := "end only your child task"
	coordinatorStop := "stop and report this to the person who started the run"
	for _, c := range []struct{ name, output string }{
		{"run-owned path", `{"Text":"` + state + `/records/spec.md"}`},
		{"schema", `{"Text":7}`},
	} {
		writeAllocated(t, in.Output, c.output)
		code, _, er := call("validate", run, in.Name)
		if code != 5 || strings.Contains(er, coordinatorStop) || !strings.Contains(er, wf.FixableRefusalTail) {
			t.Fatalf("%s: validate = %d\n%s\nwant child fix-and-retry guidance", c.name, code, er)
		}
	}
	writeAllocated(t, in.Output, `{"Text":7}`)
	code, _, er := call("done", run, in.Name)
	if code != 5 || !strings.Contains(er, "attempt is spent") || !strings.Contains(er, childExit) || strings.Contains(er, wf.FixableRefusalTail) || strings.Contains(er, coordinatorStop) {
		t.Fatalf("done spending the attempt = %d\n%s\nwant child-only exit guidance", code, er)
	}
}

// an earlier revision B: a result of the wrong JSON type is refused in the result's own
// terms: the field, what it holds, and what to give instead.
func TestADecodeRefusalNamesTheFieldAndWhatToGive(t *testing.T) {
	h, run, review, _, _, _ := citeRun(t)
	res, err := h.e.Validate(run, review.Name, wf.Submission{Token: review.Token, Output: json.RawMessage(`{"verdict":"CLEAN","evidence":[{"command":1}]}`)})
	want := "undecodable submission: result field evidence.command is a JSON number: give a JSON string instead"
	if err != nil || res.Class != wf.Refused || len(res.Refusal) != 1 || res.Refusal[0] != want {
		t.Fatalf("validate = %+v %v, want %q", res, err, want)
	}
}

// an earlier revision round 2, #1: only engine-derived artifacts are citable. A path a
// caller supplied in its submission's Artifacts (no allocation behind it) is
// journaled but proves nothing: citing it is refused even after that
// submission was accepted.
func TestACallerSuppliedArtifactPathIsNotCitable(t *testing.T) {
	state, workspace := t.TempDir(), t.TempDir()
	d := citeDef()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: workspace, StateRoot: state, RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }})
	run := h.run(d, nil)
	impl, _ := startedUnit(t, h, run, "implement")
	claimed := filepath.Join(state, "runs", string(run), "records", "scratch", "claimed.log")
	if err := os.MkdirAll(filepath.Dir(claimed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claimed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := h.e.Done(run, impl.Name, wf.Submission{Token: impl.Token, Output: json.RawMessage(`{"Status":"implemented"}`), Artifacts: map[string]string{"claimed": claimed}})
	if err != nil || res.Class != wf.Accepted {
		t.Fatalf("implement done: %+v %v", res, err)
	}
	review, _ := startedUnit(t, h, run, "review")
	_, err = h.e.Validate(run, review.Name, wf.Submission{Token: review.Token, Output: citeOutput(claimed)})
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "leave run-owned paths out") {
		t.Fatalf("citing a caller-supplied artifact path = %v, want refused", err)
	}
}

// an earlier revision round 2, #3: an accepted citation is journaled as a typed
// run-relative reference, so the journal relocates (snapshot forks, fanout)
// and the citation resolves against the new run directory.
func TestACitationIsJournaledAsARunRelativeReference(t *testing.T) {
	h, run, review, log, state, workspace := citeRun(t)
	if res, err := h.e.Done(run, review.Name, wf.Submission{Token: review.Token, Output: citeOutput(log)}); err != nil || res.Class != wf.Accepted {
		t.Fatalf("done citing the recorded log: %+v %v", res, err)
	}
	recs := h.records(run)
	if err := wf.CheckJournalRelocation(recs, wf.PathRoots{Workspace: workspace, State: state}); err != nil {
		t.Fatalf("the journal no longer relocates: %v", err)
	}
	var accepted json.RawMessage
	for _, r := range recs {
		if r.Kind == wf.JournalAccepted && r.State == "citeReview" {
			var d wf.AcceptedData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				t.Fatal(err)
			}
			accepted = d.Output
		}
	}
	if !strings.Contains(string(accepted), `"$wf.path"`) || strings.Contains(string(accepted), state) {
		t.Fatalf("accepted output is not a typed reference: %s", accepted)
	}
	runDir := filepath.Join(state, "runs", string(run))
	rel, _ := filepath.Rel(runDir, log)
	newRunDir := filepath.Join(t.TempDir(), "elsewhere", "runs", string(run))
	var out citeReviewOut
	if err := wf.DecodeRunValue(recs, newRunDir, accepted, &out); err != nil || len(out.Evidence) != 1 || out.Evidence[0].Location != filepath.Join(newRunDir, rel) {
		t.Fatalf("decoded under a new run dir = %+v %v, want %s", out, err, filepath.Join(newRunDir, rel))
	}
}

// an earlier revision round 2, #4: a number of the wrong range or kind for an integer
// field names the Go type, not "a JSON number" (it already is one).
func TestADecodeRefusalOfANumberNamesItsTargetType(t *testing.T) {
	type intOut struct{ N int8 }
	type intS struct{}
	type intEnd struct{}
	d := wf.New("int-out")
	a := wf.NewAgent[intS, intOut](d)
	end := wf.NewTerminal[intEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	edge := a.To(end, "done")
	a.Spec(wf.PromptText("n"), wf.Retry[intS](wf.Const(1), edge))
	a.Route(func(intOut, wf.Ctx) wf.Edge[intS] { return edge }, edge)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{})
	run := h.run(d, nil)
	in, _ := startedUnit(t, h, run, "a")
	for _, out := range []string{`{"N":1.5}`, `{"N":300}`} {
		res, err := h.e.Validate(run, in.Name, wf.Submission{Token: in.Token, Output: json.RawMessage(out)})
		if err != nil || len(res.Refusal) != 1 || !strings.Contains(res.Refusal[0], "result field N") || !strings.Contains(res.Refusal[0], "int8") || !strings.Contains(res.Refusal[0], "-128 to 127") || strings.Contains(res.Refusal[0], "give a JSON number instead") {
			t.Fatalf("%s: %+v %v", out, res, err)
		}
	}
}

// an earlier revision round 2, #2: whether a refusal is fixable is the engine's
// disposition, not text: a definition's own refusal reason that contains the
// open-attempt phrase, on a done that spends the attempt, still ends with the
// stop tail.
func TestASpentRefusalQuotingTheOpenPhraseStillStops(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	state, workspace := t.TempDir(), t.TempDir()
	d := wf.New("phrase")
	a := wf.NewAgent[resultPathAgent, resultPathOutput](d)
	end := wf.NewTerminal[resultPathDone](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	edge := a.To(end, "done")
	a.Spec(wf.PromptText("return content"), wf.Retry[resultPathAgent](wf.Const(1), edge),
		wf.Checkf("phrase", func(resultPathOutput, wf.Ctx) error {
			return errors.New("your attempt is still open: not really")
		}))
	a.Route(func(resultPathOutput, wf.Ctx) wf.Edge[resultPathAgent] { return edge }, edge)
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}}
	call := func(args ...string) (int, string, string) {
		var out, er bytes.Buffer
		code := fixtureCLI(cfg, append(args, "--state-dir", state), &out, &er)
		return code, out.String(), er.String()
	}
	_, o, _ := call("run", d.Name(), "--workspace", workspace, "--run-id", "phrase")
	run := strings.TrimSpace(o)
	var ins []wf.Instruction
	if _, o, er := call("next", run, "1", "--json"); json.Unmarshal([]byte(o), &ins) != nil || len(ins) != 1 {
		t.Fatalf("next: %s %s", o, er)
	}
	in := ins[0]
	call("dev", "started", run, in.Name, "--harness", "claude", "--session", "session", "--agent", "agent")
	call("prompt", run, in.Name)
	writeAllocated(t, in.Output, `{"Text":"x"}`)
	code, _, er := call("done", run, in.Name)
	if code != 5 || !strings.Contains(er, "attempt is spent") || !strings.Contains(er, "end only your child task") || strings.Contains(er, wf.FixableRefusalTail) {
		t.Fatalf("done spending the attempt = %d\n%s\nwant child-only exit guidance", code, er)
	}
}

// an earlier revision round 2: the run-relative reference is the engine's form for a
// citation it accepted; a result that writes one itself is refused, so an
// unrecorded run path cannot be smuggled in as a reference.
func TestAResultMayNotWriteThePathMarkerItself(t *testing.T) {
	h, run, review, _, _, _ := citeRun(t)
	raw := json.RawMessage(`{"verdict":"CLEAN","evidence":[{"command":"x","location":{"$wf.path":{"base":"run","path":"records/scratch/notes.md"}}}]}`)
	_, err := h.e.Validate(run, review.Name, wf.Submission{Token: review.Token, Output: raw})
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "evidence[0].location") || !strings.Contains(err.Error(), "$wf.path") {
		t.Fatalf("a hand-written path marker = %v, want refused naming the field", err)
	}
}

// citeAccepted drives citeRun to an accepted review citing loc(log), and
// returns what replay and harvest need.
func citeAccepted(t *testing.T, loc func(log string) string) (*harness, wf.RunID, string) {
	t.Helper()
	h, run, review, log, state, _ := citeRun(t)
	if res, err := h.e.Done(run, review.Name, wf.Submission{Token: review.Token, Output: citeOutput(loc(log))}); err != nil || res.Class != wf.Accepted {
		t.Fatalf("done citing %s: %+v %v", loc(log), res, err)
	}
	return h, run, filepath.Join(state, "runs", string(run))
}

var citeLocations = map[string]func(string) string{
	"file in the artifact": func(log string) string { return log },
	"the artifact itself":  filepath.Dir,
}

// an earlier revision round 3, #1: a citation is a reference, not a read. Replay of the
// same definition (which recreates artifacts its own way) resolves it to a
// path string and decodes the review as the run did: the replay ends.
func TestAReplayResolvesACitationAsAPath(t *testing.T) {
	for name, loc := range citeLocations {
		h, run, runDir := citeAccepted(t, loc)
		rep, err := wf.Replay(h.records(run), citeDef(), runDir)
		if err != nil || rep.Stop.Kind != "ended" || len(rep.Differences) != 0 {
			t.Fatalf("%s: replay = %s, %v; want it to end as the run did", name, rep, err)
		}
	}
}

// an earlier revision round 3, #2: a harvested review that cites an artifact checks
// against its state as recorded: the citation decodes as a path string.
func TestAHarvestedCitationDecodesForCalibration(t *testing.T) {
	for name, loc := range citeLocations {
		h, run, _ := citeAccepted(t, loc)
		entries, err := wf.Harvest(h.records(run), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		checked := 0
		for _, e := range entries {
			if !strings.HasSuffix(e.State, "citeReview") {
				continue
			}
			for _, state := range wf.Inspect(citeDef()).States {
				if state.Name == e.State {
					if failures, err := state.CheckRecordedOutput(e.Output, e.Context); err != nil || len(failures) != 0 {
						t.Fatalf("%s: calibration of the harvested review = %v %v", name, failures, err)
					}
					checked++
				}
			}
		}
		if checked != 1 {
			t.Fatalf("%s: checked %d harvested reviews, want 1", name, checked)
		}
	}
}

// an earlier revision round 3, #3: a result is unambiguous before it is accepted: a key
// given twice, or two keys that differ only in case (Go would decode
// either into one field), are refused naming the field.
func TestAnAmbiguousResultIsRefused(t *testing.T) {
	h, run, review, log, _, _ := citeRun(t)
	q, _ := json.Marshal(log)
	for _, raw := range []string{
		`{"verdict":"other","Verdict":"live","evidence":[{"command":"x","location":` + string(q) + `}]}`,
		`{"verdict":"CLEAN","verdict":"other","evidence":[]}`,
		`{"verdict":"CLEAN","evidence":[{"command":"x","Command":"y","location":` + string(q) + `}]}`,
		// U+017F LATIN SMALL LETTER LONG S folds to s, as encoding/json matches.
		`{"verdict":"CLEAN","evidence":[{"command":"x","location":` + string(q) + `}],"ſtatus":"other","status":"live"}`,
	} {
		_, err := h.e.Validate(run, review.Name, wf.Submission{Token: review.Token, Output: json.RawMessage(raw)})
		if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "more than once") {
			t.Fatalf("%s = %v, want refused as ambiguous", raw, err)
		}
	}
}

// an earlier revision final round, #2: a harvested citation is the original path, with the
// run directory the run recorded, whatever layout the engine's RunDir uses.
func TestAHarvestedCitationIsTheOriginalPath(t *testing.T) {
	// The last layout holds the allocation segment itself.
	for _, runs := range []string{"runs", "jobs", filepath.Join("records", "artifacts", "jobs")} {
		h, run, review, log, _, _ := citeRunLayout(t, false, runs)
		if res, err := h.e.Done(run, review.Name, wf.Submission{Token: review.Token, Output: citeOutput(log)}); err != nil || res.Class != wf.Accepted {
			t.Fatalf("%s: done: %+v %v", runs, res, err)
		}
		entries, err := wf.Harvest(h.records(run), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range entries {
			if !strings.HasSuffix(e.State, "citeReview") {
				continue
			}
			var out citeReviewOut
			if err := json.Unmarshal(e.Output, &out); err != nil || len(out.Evidence) != 1 || out.Evidence[0].Location != log {
				t.Fatalf("%s: harvested citation = %+v %v, want the original %s", runs, out, err, log)
			}
			found = true
		}
		if !found {
			t.Fatalf("%s: no harvested review", runs)
		}
	}
}
