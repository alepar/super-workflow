package wf_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type cleanupItem struct{ ID, Workdir string }

func TestWorktreeRemovalWaitsForOpenUnitWorkspace(t *testing.T) {
	for _, state := range []string{"pending", "handed", "claimed", "recovery", "sibling-directory", "quorum", "quorum-live", "quorum-failed", "crash-applied", "intent-recovery", "apply-failed"} {
		t.Run(state, func(t *testing.T) {
			claimed := state == "claimed" || state == "recovery" || (state == "quorum-live" || state == "quorum-failed")
			root := t.TempDir()
			target := filepath.Join(root, "task")
			child := wf.New("cleanup-item")
			a := wf.NewAgent[eA, eOut](child)
			done, bad := twoTerminals(child)
			a.Spec(wf.PromptText("work {{item}}", wf.ChildInput()), wf.Retry(wf.Const(1), a.To(bad, "failed")))
			a.Then(a.To(done, "clean up").With(wf.ExternalEffect{Adapter: "git", Op: "worktree_remove", Args: map[string]any{"path": target}, Key: wf.KeyOf("remove", "{id}")}))
			wf.Yield(done, func(wf.Ctx) bool { return true })
			wf.Yield(bad, func(wf.Ctx) bool { return false })
			d := wf.New("cleanup-open")
			other := filepath.Join(target, "subdir")
			if state == "sibling-directory" {
				other = target + "-other"
			}
			m := wf.NewMap[chFan, cleanupItem, string, bool](d, wf.FromValues(cleanupItem{"one", target}, cleanupItem{"two", other}), func(i cleanupItem) string { return i.ID }, wf.Typed[cleanupItem, bool](child))
			end := wf.NewTerminal[chMerged](d, wf.Success)
			policy := wf.AllSettled()
			if state == "quorum" || (state == "quorum-live" || state == "quorum-failed") {
				policy = wf.Quorum(1)
			}
			m.Parallel(2).Join(policy, m.To(end, "done"), m.To(end, "failed"))
			w := newWorld()
			w.status["remove/one"] = wf.EffectNotDone
			w.status["remove/two"] = wf.EffectNotDone
			h := newHarness(t, d, w)
			h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: root})
			run := h.run(d, nil)
			count := 2
			if state == "pending" {
				count = 1
			}
			units := h.handOut(run, count)
			if len(units) != count {
				t.Fatalf("units: %+v", units)
			}
			if err := h.e.Started(run, units[0].Name, claim("one")); err != nil {
				t.Fatal(err)
			}
			if claimed {
				if err := h.e.Started(run, units[1].Name, claim("two")); err != nil {
					t.Fatal(err)
				}
			}
			var cj *crashJournal
			if state == "intent-recovery" {
				cj = &crashJournal{Journal: h.j, kind: wf.JournalCleanupDeferred, armed: true}
				h.e = wf.NewEngine(cj, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}})
			}
			_, firstErr := h.e.Done(run, units[0].Name, wf.Submission{Output: json.RawMessage(`{}`)})
			if state == "intent-recovery" {
				// The crash is in the drive after done's result was
				// accepted: done reports the result, not the crash.
				if firstErr != nil || cj.armed {
					t.Fatalf("crash before defer: err %v, crashed %v", firstErr, !cj.armed)
				}
				// Model an older/crashed engine that already wrote the effect intent.
				payload, _ := json.Marshal(wf.EffectIntentData{Index: 0, Effect: wf.EffectData{Kind: "external", Adapter: "git", Op: "worktree_remove", Key: "remove/one", Args: map[string]any{"path": target}}})
				unlock, err := h.j.Lock(run)
				if err != nil {
					t.Fatal(err)
				}
				err = h.j.Append(wf.JournalRecord{Run: run, Instance: units[0].Child, State: "eA", Kind: wf.JournalEffectIntent, Data: payload})
				unlock()
				if err != nil {
					t.Fatal(err)
				}
				h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}})
			} else if firstErr != nil {
				t.Fatal(firstErr)
			}
			h.handOut(run, 0)
			if state == "quorum" || (state == "quorum-live" || state == "quorum-failed") {
				statusOf(t, h.status(run), wf.RunEnded, "chMerged")
				if state == "quorum-live" || state == "quorum-failed" {
					if len(w.applied) != 0 || len(h.status(run).PendingCleanups) != 1 {
						t.Fatal("ended run lost live workspace protection or pending cleanup")
					}
					h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}})
					h.handOut(run, 0)
					if len(w.applied) != 0 {
						t.Fatal("restart removed live workspace of settled child")
					}
					var err error
					if state == "quorum-failed" {
						_, err = h.e.Failed(run, units[1].Name, "failed")
					} else {
						_, err = h.e.Done(run, units[1].Name, wf.Submission{Output: json.RawMessage(`{}`)})
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if len(w.applied) != 1 || len(h.status(run).PendingCleanups) != 0 || len(h.kind(run, wf.JournalCleanupApplied)) != 1 {
					t.Fatal("quorum cleanup did not drain")
				}
				return
			}
			wantApplied := 0
			if state == "sibling-directory" {
				wantApplied = 1
			}
			if len(w.applied) != wantApplied || len(h.kind(run, wf.JournalEffectIntent)) != wantApplied+map[bool]int{true: 1}[state == "intent-recovery"] {
				t.Fatal("removed worktree while another open unit still uses it")
			}
			if state == "recovery" {
				pending := h.status(run).PendingCleanups
				if len(pending) != 1 || pending[0].Deferred == 0 || pending[0].Instance != units[0].Child || pending[0].Effect != "git.worktree_remove" {
					t.Fatalf("pending cleanup status: %+v", pending)
				}
				h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}})
				h.handOut(run, 0)
				if len(w.applied) != 0 || len(h.status(run).PendingCleanups) != 1 {
					t.Fatal("restart lost deferred cleanup")
				}
			}
			if state == "pending" {
				more := h.handOut(run, 1)
				if len(more) != 1 {
					t.Fatalf("pending handout: %+v", more)
				}
				units = append(units, more[0])
			}
			if !claimed {
				if err := h.e.Started(run, units[1].Name, claim("two")); err != nil {
					t.Fatal(err)
				}
			}
			if state == "crash-applied" {
				cj := &crashJournal{Journal: h.j, kind: wf.JournalCleanupApplied, armed: true}
				h.e = wf.NewEngine(cj, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}})
			}
			if state == "apply-failed" {
				w.failOp["git.worktree_remove"] = true
			}
			_, doneErr := h.e.Done(run, units[1].Name, wf.Submission{Output: json.RawMessage(`{}`)})
			if state == "crash-applied" {
				if doneErr != nil {
					t.Fatalf("cleanup crash after the accepted result surfaced: %v", doneErr)
				}
				if len(h.kind(run, wf.JournalCleanupApplied)) != 0 || len(h.status(run).PendingCleanups) != 1 {
					t.Fatal("crash falsely marked cleanup applied")
				}
				w.status["remove/one"] = wf.EffectDone
				h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}})
			} else if state == "apply-failed" {
				if doneErr != nil || len(h.kind(run, wf.JournalCleanupApplied)) != 0 || len(h.status(run).PendingCleanups) != 1 {
					t.Fatal("failed cleanup was not retained")
				}
				w.failOp["git.worktree_remove"] = false
				h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}})
			} else if doneErr != nil {
				t.Fatal(doneErr)
			}
			h.handOut(run, 0)
			statusOf(t, h.status(run), wf.RunEnded, "chMerged")
			wantAttempts := 2
			if state == "apply-failed" {
				wantAttempts++
			}
			if len(w.applied) != wantAttempts {
				t.Fatalf("cleanup did not resume: %d effects", len(w.applied))
			}
			if state == "claimed" {
				replay, err := wf.Replay(h.records(run), d, "")
				if err != nil || replay.Stop.Kind != "ended" {
					t.Fatalf("deferred cleanup replay: %+v, %v", replay, err)
				}
			}
		})
	}
}

func TestPrintedCommandEscapesDeletedWorkingDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace with ' quote")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	d, _ := simple("deleted-cwd", wf.PromptText("work"))
	h := newHarness(t, d, nil)
	h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: root,
		SubmitLine: func(wf.RunID, string) string { return "pwd" },
		VerbLine:   func(wf.RunID, string, ...string) string { return "pwd" },
	})
	run := h.run(d, nil)
	// A restarted caller can have a different cwd or Workspace option. Commands
	// must keep using the stable root recorded when this run was created.
	h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: t.TempDir(),
		SubmitLine: func(wf.RunID, string) string { return "pwd" },
		VerbLine:   func(wf.RunID, string, ...string) string { return "pwd" },
	})
	ins := h.next(run)
	if len(ins) != 1 {
		t.Fatalf("instructions: %+v", ins)
	}
	units := h.handOut(h.run(d, nil), 1)
	if len(units) != 1 {
		t.Fatalf("units: %+v", units)
	}
	if command, ok := wf.BriefCommand(units[0].Brief); !ok || !strings.HasPrefix(command, "cd ") {
		t.Fatalf("workspace advice broke brief command extraction: %q, %v", command, ok)
	}
	doomed := filepath.Join(t.TempDir(), "removed")
	if err := os.Mkdir(doomed, 0755); err != nil {
		t.Fatal(err)
	}
	// The shell remains in the directory after removing its last name. The
	// printed command must move to the stable run root before doing work.
	cmd := exec.Command("sh", "-c", "cd "+quoteForSh(doomed)+" && rmdir "+quoteForSh(doomed)+" && "+ins[0].Submit)
	got, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(got)) != root {
		t.Fatalf("printed command %q from deleted cwd: %v, %q", ins[0].Submit, err, got)
	}
}

func TestBootstrapNamesToolWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	d, _ := simple("bootstrap-cwd", wf.PromptText("work"))
	h := newHarness(t, d, nil)
	h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: root})
	run := h.run(d, nil)
	text := wf.Bootstrap(h.status(run), "/bin/workflow", t.TempDir())
	if !strings.Contains(text, "shell tool working-directory") || !strings.Contains(text, root) || !strings.Contains(text, "cd "+root+" && ") {
		t.Fatalf("bootstrap lacks stable directory instruction:\n%s", text)
	}
}
