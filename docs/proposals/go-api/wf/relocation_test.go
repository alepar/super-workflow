package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestMovedRunUsesSuppliedWorkspaceAndStateRoots(t *testing.T) {
	base := t.TempDir()
	oldRoot, newRoot := filepath.Join(base, "old"), filepath.Join(base, "moved")
	oldWorkspace := filepath.Join(oldRoot, "workspace")
	if err := os.MkdirAll(oldWorkspace, 0755); err != nil {
		t.Fatal(err)
	}
	d := promptHashDef("workspace {{workspace}}, target {{workspace}}/{{target}}")
	d.Param("target")
	engine := func(root string) *wf.Engine {
		state := filepath.Join(root, "state")
		opts := wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: filepath.Join(root, "workspace"), RunID: "relocation", RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) }}
		// The JSON option makes the pre-feature engine exercise its old behavior.
		data, _ := json.Marshal(map[string]string{"StateRoot": state})
		if err := json.Unmarshal(data, &opts); err != nil {
			t.Fatal(err)
		}
		return wf.NewEngine(wf.FileJournal(state), nil, opts).(*wf.Engine)
	}
	e := engine(oldRoot)
	run, err := e.Run(d, map[string]string{"target": "result.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.HandOut(run, 1)
	if err != nil || len(ins) != 1 {
		t.Fatalf("handoff: %v %v", ins, err)
	}
	first, err := e.PromptFetch(run, ins[0].Name, "")
	if err != nil {
		t.Fatal(err)
	}
	records, err := wf.ReadJournal(filepath.Join(oldRoot, "state"), run)
	if err != nil {
		t.Fatal(err)
	}
	var start wf.RunStartedData
	if err := json.Unmarshal(records[0].Data, &start); err != nil || start.Workspace != oldWorkspace {
		t.Fatalf("decoded read: %+v %v", start, err)
	}
	if err := os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	records, err = wf.ReadJournalWithRoots(filepath.Join(newRoot, "state"), run, wf.PathRoots{Workspace: filepath.Join(newRoot, "workspace"), State: filepath.Join(newRoot, "state")})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(records[0].Data, &start); err != nil || start.Workspace != filepath.Join(newRoot, "workspace") {
		t.Fatalf("decoded moved read: %+v %v", start, err)
	}
	e = engine(newRoot)
	if _, err := e.Run(d, nil, nil); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("Run silently relocated: %v", err)
	}
	second, err := e.PromptFetch(run, ins[0].Name, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second.Prompt, oldRoot) || !strings.Contains(second.Prompt, filepath.Join(newRoot, "workspace", "result.txt")) {
		t.Fatalf("moved run still targets original paths: %s", second.Prompt)
	}
	if first.PromptShapeHash != second.PromptShapeHash {
		t.Fatal("moving run changed prompt shape")
	}
	if _, err := e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`"done"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldRoot); !os.IsNotExist(err) {
		t.Fatalf("resume recreated original root: %v", err)
	}
	journal, err := os.ReadFile(filepath.Join(newRoot, "state", "runs", string(run), "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// Historical prompt text may contain its original roots. Only known path
	// fields use root tokens; free strings are never guessed to be paths.
	for _, line := range strings.Split(string(journal), "\n") {
		if line == "" {
			continue
		}
		var rec wf.JournalRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Kind == wf.JournalPromptFetched {
			var fetched wf.PromptFetchedData
			if err := json.Unmarshal(rec.Data, &fetched); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(fetched.PromptDir, oldRoot) || strings.Contains(fetched.PromptDir, newRoot) {
				t.Fatalf("absolute prompt path: %s", fetched.PromptDir)
			}
		}
	}

}

func TestCLIMovedRunUsesExplicitRootsAndRememberedAssets(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old")
	newRoot := filepath.Join(base, "moved")
	workspace := filepath.Join(oldRoot, "workspace")
	assets := filepath.Join(workspace, "assets")
	if err := os.MkdirAll(assets, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, assets, "prompt.md", "work in {{workspace}}")
	d := wf.New("move-cli").Assets(filepath.Join(base, "missing"))
	a := wf.NewAgent[pfA, string](d)
	end := wf.NewTerminal[pfEnd](d, wf.Success)
	edge := a.To(end, "done")
	d.OnFailure(wf.Blocked, end)
	a.Spec(wf.PromptFile("{assets}/prompt.md"), wf.Retry(wf.Const(1), edge))
	a.Then(edge)
	cfg := wf.MainConfig{Binary: "move-cli", Defs: []wf.Registration{{Def: d}}}
	call := func(root string, args ...string) string {
		t.Helper()
		var out, er bytes.Buffer
		args = append(args, "--state-dir", filepath.Join(root, "state"), "--workspace", filepath.Join(root, "workspace"))
		if code := wf.CLI(cfg, args, &out, &er); code != 0 {
			t.Fatalf("%v: %d %s", args, code, &er)
		}
		return out.String()
	}
	call(oldRoot, "run", d.Name(), "--run-id", "move-cli", "--assets", assets)
	call(oldRoot, "wf", "next", "1", "--json")
	call(oldRoot, "wf", "prompt", "pfA#1", "--json")
	if err := os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	call(newRoot, "wf", "status", "--json")
	prompt := call(newRoot, "wf", "prompt", "pfA#1", "--json")
	if strings.Contains(prompt, oldRoot) || !strings.Contains(prompt, filepath.Join(newRoot, "workspace")) {
		t.Fatalf("wrong relocated prompt: %s", prompt)
	}
	call(newRoot, "wf", "failed", "pfA#1", "--reason", "test stopped")
	if _, err := os.Stat(oldRoot); !os.IsNotExist(err) {
		t.Fatalf("original root accessed: %v", err)
	}
}

func TestMovedRunRecoversCommandWorkingDirectory(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		for _, legacy := range []bool{false, true} {
			for _, missing := range []bool{false, true} {
				if keyed && missing {
					continue
				}
				t.Run(fmt.Sprint("keyed=", keyed, "/legacy=", legacy, "/missing=", missing), func(t *testing.T) {
					base := t.TempDir()
					old := filepath.Join(base, "old")
					moved := filepath.Join(base, "moved")
					if err := os.MkdirAll(old, 0700); err != nil {
						t.Fatal(err)
					}
					d, cmd := commandDef(func(c *wf.Command[cRun], fail *wf.Terminal[cFail]) wf.RetryPolicy[cRun] {
						return wf.Retry[cRun](wf.Const(1), c.To(fail, "failed")).OnClass(wf.Unavailable, c.To(fail, "missing directory"))
					})
					cmd.In("{workspace}")
					if keyed {
						cmd.Key("command-key")
					} else {
						cmd.Idempotent()
					}
					w := newWorld()
					reg := wf.NewAdapters()
					if err := reg.Register(directoryCheckingWorld{w}); err != nil {
						t.Fatal(err)
					}
					state := filepath.Join(base, "state")
					var journal wf.Journal = wf.FileJournal(state)
					if legacy {
						journal = unmarkedCommandJournal{journal}
					}
					cj := &crashJournal{Journal: journal, kind: wf.JournalEffectResult}
					opts := wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: old, StateRoot: state, RunID: "cwd"}
					if legacy {
						opts.StateRoot = ""
					}
					e := wf.NewEngine(cj, reg, opts)
					run, err := e.Run(d, map[string]string{"topic": "test"}, nil)
					if err != nil {
						t.Fatal(err)
					}
					cj.armed = true
					if _, err := e.Next(run, ""); !errors.Is(err, errCrash) {
						t.Fatalf("crash: %v", err)
					}
					if err := os.Rename(old, moved); err != nil {
						t.Fatal(err)
					}
					if missing {
						if err := os.Remove(moved); err != nil {
							t.Fatal(err)
						}
					}
					opts.Workspace = moved
					opts.StateRoot = state
					e = wf.NewEngine(wf.FileJournal(state), reg, opts)
					if _, err := e.Next(run, ""); err != nil {
						t.Fatal(err)
					}
					st, err := e.Status(run)
					if err != nil {
						t.Fatal(err)
					}
					if missing {
						if len(w.applied) != 1 || st.Terminal != "cFail" {
							t.Fatalf("launched missing cwd or lost unavailable route: %d %+v", len(w.applied), st)
						}
					} else if len(w.applied) != 2 || w.applied[1].Args["dir"] != moved {
						t.Fatalf("reapplied cwd: %+v", w.applied)
					}
				})
			}
		}

	}

}

type unmarkedCommandJournal struct{ wf.Journal }

func (j unmarkedCommandJournal) Append(rec wf.JournalRecord) error {
	if rec.Kind == wf.JournalEffectIntent {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rec.Data, &fields); err != nil {
			return err
		}
		delete(fields, "Command")
		rec.Data, _ = json.Marshal(fields)
	}
	return j.Journal.Append(rec)
}

type directoryCheckingWorld struct{ *world }

func (w directoryCheckingWorld) Check(workspace string, e wf.ExternalEffect) (wf.EffectStatus, error) {
	dir, _ := e.Args["dir"].(string)
	if _, err := os.Stat(dir); err != nil {
		return wf.EffectUnknown, err
	}
	return wf.EffectNotDone, nil
}

func TestMissingCommandDirectoryStillCleansPreviousProcessGroup(t *testing.T) {
	dir := t.TempDir()
	d := detDef(func(c *wf.Command[dRun]) { c.Idempotent().Detached().In(dir) })
	fr, w := newDetRunner(), newWorld()
	h := newHarnessOpts(t, d, directoryCheckingWorld{w}, detOpts(t, fr))
	run := h.run(d, map[string]string{"topic": "build"})
	h.next(run)
	intent := lastIntent(t, h, run)
	fr.set(intent, wf.DetachedDeadLive, wf.RunResult{})
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	h.next(run)
	if len(fr.terminations()) != 1 || fr.startCount() != 1 {
		t.Fatalf("cleanup skipped or relaunched: terminated=%v starts=%d", fr.terminations(), fr.startCount())
	}
	h.now = h.now.Add(11 * time.Second)
	h.next(run)
	results := resultsOf(t, h, run)
	if len(results) != 1 || results[0].Outcome != "unavailable" || fr.startCount() != 1 {
		t.Fatalf("results=%+v starts=%d", results, fr.startCount())
	}
}

func TestFakeDetachedRunnerRecoversWithoutHostWorkspace(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "virtual-workspace")
	d := detDef(func(c *wf.Command[dRun]) { c.Idempotent().Detached().In("{workspace}") })
	fr, w := newDetRunner(), newWorld()
	opts := detOpts(t, fr)
	opts.Workspace = workspace
	h := newHarnessOpts(t, d, w, opts)
	run := h.run(d, map[string]string{"topic": "build"})
	h.next(run)
	intent := lastIntent(t, h, run)
	fr.set(intent, wf.DetachedDead, wf.RunResult{})
	h.next(run)
	if fr.startCount() != 2 {
		t.Fatalf("fake world consulted host filesystem: starts=%d results=%+v", fr.startCount(), resultsOf(t, h, run))
	}
}

func (w directoryCheckingWorld) CheckCommandDirectory(workspace, dir string) error {
	if dir == "" || dir == "." {
		dir = workspace
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(workspace, dir)
	}
	info, err := os.Stat(dir)
	if err == nil && !info.IsDir() {
		return fmt.Errorf("not a directory: %s", dir)
	}
	return err
}
