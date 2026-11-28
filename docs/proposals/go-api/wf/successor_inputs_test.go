package wf_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/sink"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// shellWords splits a line the engine printed with sink.Quote.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	cmds, err := sink.SplitCommands(line)
	if err != nil {
		t.Fatalf("split %q: %v", line, err)
	}
	var words []string
	for i, c := range cmds {
		if i > 0 {
			words = append(words, "&&")
		}
		words = append(words, c...)
	}
	return words
}

// the regression case: a run started with --assets ended; the resume command status
// printed left --assets out, so run as printed it failed, and the
// coordinator had to add the flag itself. A successor reuses the recorded
// start's definition inputs (assets, snapshots, model map) when the call
// omits them, so the printed command runs as printed.
func TestPrintedResumeRunsAsPrintedAfterAssetsStart(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "prompt.md", "chosen")
	d := wf.New("succ-assets").Assets(filepath.Join(root, "missing"))
	d.RunKey(d.Param("topic"))
	a := wf.NewAgent[pfA, string](d)
	end := wf.NewTerminal[pfEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	edge := a.To(end, "done")
	a.Spec(wf.PromptFile("{assets}/prompt.md"), wf.Retry(wf.Const(1), edge))
	a.Then(edge)
	cfg := wf.MainConfig{Binary: "succ-assets", Defs: []wf.Registration{{Def: d}}}
	state, ws := filepath.Join(t.TempDir(), "state"), t.TempDir()
	call := func(args ...string) (int, string, string) {
		var out, er bytes.Buffer
		code := fixtureCLI(cfg, args, &out, &er)
		return code, out.String(), er.String()
	}
	must := func(args ...string) string {
		t.Helper()
		code, out, er := call(args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, er)
		}
		return out
	}
	first := strings.TrimSpace(must("run", d.Name(), "--state-dir", state, "--workspace", ws, "--assets", root, "--snapshots", "--paged-prompts", "--model-map", "all=m-recorded", "--delegate", "pfA=yes", "--param", "topic=x"))
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(must("wf", "next", "1", "--json", "--state-dir", state)), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("next: %+v (%v)", ins, err)
	}
	must("wf", "dev", "started", ins[0].Name, "--session", "s", "--agent", "a", "--state-dir", state)
	writeAllocated(t, ins[0].Output, `"ok"`)
	must("wf", "done", ins[0].Name, "--state-dir", state)

	var st wf.RunStatus
	if err := json.Unmarshal([]byte(must("wf", "status", "--json", "--state-dir", state, "--run-id", first)), &st); err != nil {
		t.Fatal(err)
	}
	if st.Phase != wf.RunEnded || st.Resume == "" {
		t.Fatalf("status = phase %s resume %q, want an ended run with a resume command", st.Phase, st.Resume)
	}
	words := shellWords(t, st.Resume)
	for i, w := range words { // past a leading "cd <dir> &&"
		if w == "&&" {
			words = words[i+1:]
			break
		}
	}
	code, out, er := call(words[1:]...) // the printed command, verbatim, past its binary
	if code != 0 {
		t.Fatalf("the printed resume command %q exited %d: %s", st.Resume, code, er)
	}
	second := strings.TrimSpace(strings.Split(out, "\n")[0])
	if second == first {
		t.Fatalf("the printed command attached %s, want a successor", first)
	}
	var started wf.RunStartedData
	for _, line := range strings.Split(strings.TrimSpace(must("wf", "journal", "--state-dir", state, "--run-id", second)), "\n") {
		var r wf.JournalRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.Kind == wf.JournalRunStarted {
			if err := json.Unmarshal(r.Data, &started); err != nil {
				t.Fatal(err)
			}
		}
	}
	if string(started.Predecessor) != first {
		t.Fatalf("successor predecessor %q, want %s", started.Predecessor, first)
	}
	// Each recorded start input that shapes the run, kept when omitted.
	if started.Assets != root {
		t.Errorf("successor assets %q, want the recorded %q", started.Assets, root)
	}
	if !started.Snapshots {
		t.Error("successor lost --snapshots")
	}
	if !started.PagedPrompts {
		t.Error("successor lost --paged-prompts")
	}
	if started.ModelMap["all"] != "m-recorded" {
		t.Errorf("successor model map %v, want all=m-recorded", started.ModelMap)
	}
	if started.Delegations["pfA"] != "yes" {
		t.Errorf("successor delegations %v, want pfA=yes", started.Delegations)
	}
	if started.Workspace != ws {
		t.Errorf("successor workspace %q, want %q", started.Workspace, ws)
	}
}

// An explicit --assets on the successor's run wins over the recorded one.
func TestExplicitAssetsWinOnASuccessor(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	writeFile(t, root, "prompt.md", "chosen")
	writeFile(t, other, "prompt.md", "other")
	d := wf.New("succ-assets-explicit").Assets(filepath.Join(root, "missing"))
	d.RunKey(d.Param("topic"))
	a := wf.NewAgent[pfA, string](d)
	end := wf.NewTerminal[pfEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	edge := a.To(end, "done")
	a.Spec(wf.PromptFile("{assets}/prompt.md"), wf.Retry(wf.Const(1), edge))
	a.Then(edge)
	cfg := wf.MainConfig{Binary: "succ-assets-explicit", Defs: []wf.Registration{{Def: d}}}
	state, ws := filepath.Join(t.TempDir(), "state"), t.TempDir()
	must := func(args ...string) string {
		t.Helper()
		var out, er bytes.Buffer
		if code := fixtureCLI(cfg, args, &out, &er); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, er.String())
		}
		return out.String()
	}
	must("run", d.Name(), "--state-dir", state, "--workspace", ws, "--assets", root, "--param", "topic=x")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(must("wf", "next", "1", "--json", "--state-dir", state)), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("next: %+v (%v)", ins, err)
	}
	must("wf", "dev", "started", ins[0].Name, "--session", "s", "--agent", "a", "--state-dir", state)
	writeAllocated(t, ins[0].Output, `"ok"`)
	must("wf", "done", ins[0].Name, "--state-dir", state)
	second := strings.TrimSpace(strings.Split(must("run", d.Name(), "--state-dir", state, "--workspace", ws, "--assets", other, "--param", "topic=x"), "\n")[0])
	var started wf.RunStartedData
	for _, line := range strings.Split(strings.TrimSpace(must("wf", "journal", "--state-dir", state, "--run-id", second)), "\n") {
		var r wf.JournalRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.Kind == wf.JournalRunStarted {
			_ = json.Unmarshal(r.Data, &started)
		}
	}
	if started.Assets != other {
		t.Errorf("successor assets %q, want the explicit %q", started.Assets, other)
	}
}

// runPrinted runs a resume command the engine printed: in its "cd <dir> &&"
// directory, past its binary, through cfg.
func runPrinted(t *testing.T, cfg wf.MainConfig, line string) (int, string, string) {
	t.Helper()
	words := shellWords(t, line)
	for i, w := range words {
		if w == "&&" {
			if i == 2 && words[0] == "cd" {
				t.Chdir(words[1])
			}
			words = words[i+1:]
			break
		}
	}
	var out, er bytes.Buffer
	code := fixtureCLI(cfg, words[1:], &out, &er)
	return code, out.String(), er.String()
}

// cliRunStarted is run's run_started payload.
func cliRunStarted(t *testing.T, cfg wf.MainConfig, state, run string) wf.RunStartedData {
	t.Helper()
	var out, er bytes.Buffer
	if code := fixtureCLI(cfg, []string{"wf", "journal", "--state-dir", state, "--run-id", run}, &out, &er); code != 0 {
		t.Fatalf("journal %s: %d %s", run, code, er.String())
	}
	var started wf.RunStartedData
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r wf.JournalRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.Kind == wf.JournalRunStarted {
			if err := json.Unmarshal(r.Data, &started); err != nil {
				t.Fatal(err)
			}
		}
	}
	return started
}

// endOne drives the run's one ready unit to done and returns the status
// resume command once the run has ended.
func endOne(t *testing.T, cfg wf.MainConfig, state, run string) string {
	t.Helper()
	must := func(args ...string) string {
		t.Helper()
		var out, er bytes.Buffer
		if code := fixtureCLI(cfg, args, &out, &er); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, er.String())
		}
		return out.String()
	}
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(must("wf", "next", "1", "--json", "--state-dir", state, "--run-id", run)), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("next: %+v (%v)", ins, err)
	}
	must("wf", "dev", "started", ins[0].Name, "--session", "s", "--agent", "a", "--state-dir", state, "--run-id", run)
	writeAllocated(t, ins[0].Output, `"ok"`)
	must("wf", "done", ins[0].Name, "--state-dir", state, "--run-id", run)
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(must("wf", "status", "--json", "--state-dir", state, "--run-id", run)), &st); err != nil {
		t.Fatal(err)
	}
	if st.Phase != wf.RunEnded || st.Resume == "" {
		t.Fatalf("status = phase %s resume %q, want an ended run with a resume command", st.Phase, st.Resume)
	}
	return st.Resume
}

// Round 2 (1): a successor's params are derived from the recorded asset
// selection, not from the default tree of the cwd it runs in. The first run
// starts from a cwd whose relative default root holds version v1; the
// printed command runs in the workspace, whose same relative root holds v2.
func TestSuccessorDerivesParamsFromTheRecordedAssets(t *testing.T) {
	origin, ws := t.TempDir(), t.TempDir()
	for dir, v := range map[string]string{origin: "v1", ws: "v2"} {
		if err := os.MkdirAll(filepath.Join(dir, "rel-assets"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "rel-assets"), "prompt.md", "p")
		writeFile(t, filepath.Join(dir, "rel-assets"), "version.txt", v)
	}
	d := wf.New("succ-derive").Assets("rel-assets")
	d.RunKey(d.Param("topic"))
	d.Param("version")
	a := wf.NewAgent[pfA, string](d)
	end := wf.NewTerminal[pfEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	edge := a.To(end, "done")
	a.Spec(wf.PromptFile("{assets}/prompt.md"), wf.Retry(wf.Const(1), edge))
	a.Then(edge)
	cfg := wf.MainConfig{Binary: "succ-derive", Defs: []wf.Registration{{Def: d, Params: []wf.ParamSpec{{Name: "version", Derive: func(c wf.RunContext) (string, error) {
		b, err := os.ReadFile(filepath.Join(c.Assets, "version.txt"))
		return strings.TrimSpace(string(b)), err
	}}}}}}
	state := filepath.Join(t.TempDir(), "state")
	t.Chdir(origin)
	var out, er bytes.Buffer
	if code := fixtureCLI(cfg, []string{"run", d.Name(), "--state-dir", state, "--workspace", ws, "--param", "topic=x"}, &out, &er); code != 0 {
		t.Fatalf("run: %d %s", code, er.String())
	}
	first := strings.TrimSpace(out.String())
	if v := cliRunStarted(t, cfg, state, first).Params["version"]; v != "v1" {
		t.Fatalf("first run version %q, want v1", v)
	}
	code, sout, ser := runPrinted(t, cfg, endOne(t, cfg, state, first))
	if code != 0 {
		t.Fatalf("printed resume: exit %d: %s", code, ser)
	}
	second := strings.TrimSpace(strings.Split(sout, "\n")[0])
	st := cliRunStarted(t, cfg, state, second)
	if string(st.Predecessor) != first {
		t.Fatalf("the printed command did not succeed %s: predecessor %q", first, st.Predecessor)
	}
	if st.Params["version"] != "v1" {
		t.Errorf("successor version %q, want v1 from the recorded assets", st.Params["version"])
	}
}

type psAsk struct{}
type psSub struct{}
type psDone struct{}
type ksWork struct{}
type ksEnd struct{}

// Round 2 (2): a predecessor whose parent and child definitions declare
// separate roots, run without --assets, keeps its per-definition roots in
// its successor: the unchanged printed command lints and records the same
// AssetRoots.
func TestSuccessorKeepsPerDefinitionRoots(t *testing.T) {
	parentRoot, childRoot, ws := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, parentRoot, "p.md", "parent")
	writeFile(t, childRoot, "child.md", "child")
	kid := wf.New("succ-roots-kid").Assets(childRoot)
	w := wf.NewAgent[ksWork, string](kid)
	kend := wf.NewTerminal[ksEnd](kid, wf.Success)
	kid.OnFailure(wf.Blocked, kend)
	toK := w.To(kend, "done")
	w.Spec(wf.PromptFile("{assets}/child.md"), wf.Retry[ksWork](wf.Const(1), toK))
	w.Route(func(string, wf.Ctx) wf.Edge[ksWork] { return toK }, toK)
	wf.Yield(kend, func(wf.Ctx) chRes { return chRes{} })
	d := wf.New("succ-roots").Assets(parentRoot)
	d.RunKey(d.Param("topic"))
	a := wf.NewAgent[psAsk, string](d)
	end := wf.NewTerminal[psDone](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	s := wf.NewSub[psSub, chItem, chRes](d, wf.Typed[chItem, chRes](kid), func(wf.Ctx) chItem { return chItem{} })
	s.Exits(wf.OnExit[ksEnd](s.To(end, "child done")))
	toEnd, toSub := a.To(end, "done"), a.To(s, "go")
	a.Spec(wf.PromptFile("{assets}/p.md"), wf.Retry[psAsk](wf.Const(1), toEnd))
	a.Route(func(string, wf.Ctx) wf.Edge[psAsk] { return toEnd }, toEnd, toSub)
	cfg := wf.MainConfig{Binary: "succ-roots", Defs: []wf.Registration{{Def: d}}}
	state := filepath.Join(t.TempDir(), "state")
	var out, er bytes.Buffer
	if code := fixtureCLI(cfg, []string{"run", d.Name(), "--state-dir", state, "--workspace", ws, "--param", "topic=x"}, &out, &er); code != 0 {
		t.Fatalf("run: %d %s", code, er.String())
	}
	first := strings.TrimSpace(out.String())
	want := cliRunStarted(t, cfg, state, first).AssetRoots
	code, sout, ser := runPrinted(t, cfg, endOne(t, cfg, state, first))
	if code != 0 {
		t.Fatalf("printed resume: exit %d: %s", code, ser)
	}
	second := strings.TrimSpace(strings.Split(sout, "\n")[0])
	st := cliRunStarted(t, cfg, state, second)
	if string(st.Predecessor) != first {
		t.Fatalf("the printed command did not succeed %s: predecessor %q", first, st.Predecessor)
	}
	if len(want) < 2 {
		t.Fatalf("the predecessor recorded roots %v, want the parent's and the child's", want)
	}
	for k, v := range want {
		if st.AssetRoots[k] != v {
			t.Errorf("successor root %s = %q, want the recorded %q (all: %v)", k, st.AssetRoots[k], v, st.AssetRoots)
		}
	}
}

// versionedDef reads a relative default asset tree and derives its version
// param from it; keyed puts the version in the run key.
func versionedDef(name string, keyed bool) (*wf.Def, wf.MainConfig) {
	d := wf.New(name).Assets("rel-assets")
	topic, version := d.Param("topic"), d.Param("version")
	if keyed {
		d.RunKey(topic, version)
	} else {
		d.RunKey(topic)
	}
	a := wf.NewAgent[pfA, string](d)
	end := wf.NewTerminal[pfEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	edge := a.To(end, "done")
	a.Spec(wf.PromptFile("{assets}/prompt.md"), wf.Retry(wf.Const(1), edge))
	a.Then(edge)
	return d, wf.MainConfig{Binary: name, Defs: []wf.Registration{{Def: d, Params: []wf.ParamSpec{{Name: "version", Derive: func(c wf.RunContext) (string, error) {
		b, err := os.ReadFile(filepath.Join(c.Assets, "version.txt"))
		return strings.TrimSpace(string(b)), err
	}}}}}}
}

// versionTree writes dir/rel-assets with prompt.md and, unless v is "",
// version.txt holding v.
func versionTree(t *testing.T, dir, v string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "rel-assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "rel-assets"), "prompt.md", "p")
	if v != "" {
		writeFile(t, filepath.Join(dir, "rel-assets"), "version.txt", v)
	}
}

// startFrom runs `run` from cwd and returns the new run's id.
func startFrom(t *testing.T, cfg wf.MainConfig, d *wf.Def, cwd, state, ws string) string {
	t.Helper()
	t.Chdir(cwd)
	var out, er bytes.Buffer
	if code := fixtureCLI(cfg, []string{"run", d.Name(), "--state-dir", state, "--workspace", ws, "--param", "topic=x"}, &out, &er); code != 0 {
		t.Fatalf("run: %d %s", code, er.String())
	}
	return strings.TrimSpace(out.String())
}

// runIDs are the runs under state.
func runIDs(t *testing.T, state string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// Final round (1): with the version in the run key, the printed command
// continues its own chain from the recorded tree, not a run the workspace's
// default tree would key.
func TestPrintedResumeContinuesItsChainWithAVersionKey(t *testing.T) {
	origin, ws := t.TempDir(), t.TempDir()
	versionTree(t, origin, "v1")
	versionTree(t, ws, "v2")
	d, cfg := versionedDef("succ-keyed", true)
	state := filepath.Join(t.TempDir(), "state")
	first := startFrom(t, cfg, d, origin, state, ws)
	line := endOne(t, cfg, state, first)
	if !strings.Contains(line, "--continue "+first) && !strings.Contains(line, "--continue '"+first+"'") {
		t.Fatalf("the printed resume command does not name the run it continues: %s", line)
	}
	code, out, er := runPrinted(t, cfg, line)
	if code != 0 {
		t.Fatalf("printed resume: exit %d: %s", code, er)
	}
	st := cliRunStarted(t, cfg, state, strings.TrimSpace(strings.Split(out, "\n")[0]))
	if string(st.Predecessor) != first || st.Params["version"] != "v1" {
		t.Fatalf("the printed command started predecessor %q version %q, want the successor of %s at v1", st.Predecessor, st.Params["version"], first)
	}
}

// Final round (2): when the recorded tree now derives another key (v3),
// the printed command is refused before any run is created or attached.
func TestPrintedResumeRefusesAKeyOutsideItsChain(t *testing.T) {
	origin, ws := t.TempDir(), t.TempDir()
	versionTree(t, origin, "v1")
	versionTree(t, ws, "v1")
	d, cfg := versionedDef("succ-conflict", true)
	state := filepath.Join(t.TempDir(), "state")
	first := startFrom(t, cfg, d, origin, state, ws)
	line := endOne(t, cfg, state, first)
	writeFile(t, filepath.Join(origin, "rel-assets"), "version.txt", "v3")
	before := runIDs(t, state)
	code, _, er := runPrinted(t, cfg, line)
	if code == 0 || !strings.Contains(er, "--continue") || !strings.Contains(er, first) {
		t.Fatalf("printed resume with a conflicting key: exit %d: %s; want a refusal naming --continue and %s", code, er, first)
	}
	if after := runIDs(t, state); len(after) != len(before) {
		t.Fatalf("a refused resume created a run: %v -> %v", before, after)
	}
}

// Final round (3): with a fixed key and the recorded tree intact, the
// printed command does not depend on the default tree at all: its
// version.txt may be gone.
func TestPrintedResumeIgnoresTheDefaultTree(t *testing.T) {
	origin, ws := t.TempDir(), t.TempDir()
	versionTree(t, origin, "v1")
	versionTree(t, ws, "")
	d, cfg := versionedDef("succ-default-gone", false)
	state := filepath.Join(t.TempDir(), "state")
	first := startFrom(t, cfg, d, origin, state, ws)
	code, out, er := runPrinted(t, cfg, endOne(t, cfg, state, first))
	if code != 0 {
		t.Fatalf("printed resume with no default version.txt: exit %d: %s", code, er)
	}
	st := cliRunStarted(t, cfg, state, strings.TrimSpace(strings.Split(out, "\n")[0]))
	if string(st.Predecessor) != first || st.Params["version"] != "v1" {
		t.Fatalf("successor predecessor %q version %q, want %s at v1", st.Predecessor, st.Params["version"], first)
	}
}
