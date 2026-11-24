package wf_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A submitted record is durable even if the process died before classifying it.
// New result validation must not change that historical decision or replace its
// output with bytes from the caller retrying after the crash.
func TestResultRunPathsReplayDurableSubmission(t *testing.T) {
	for _, rootKind := range []string{"state", "workspace"} {
		for _, verb := range []string{"Next", "Validate", "Done", "Submit"} {
			t.Run(rootKind+"/"+verb, func(t *testing.T) {
				state, workspace := t.TempDir(), t.TempDir()
				opts := wf.EngineOptions{StateRoot: state, Workspace: workspace}
				def := rsChainDef()
				h := newHarnessOpts(t, def, nil, opts)
				run := h.run(def, nil)
				in, _ := startedUnit(t, h, run, "child")
				issued := h.kind(run, wf.JournalInstructionIssued)[0]
				var instruction wf.InstructionIssuedData
				if err := json.Unmarshal(issued.Data, &instruction); err != nil {
					t.Fatal(err)
				}
				root := state
				if rootKind == "workspace" {
					root = workspace
				}
				oldOutput := mustJSON(t, rsOut{V: filepath.Join(root, "old-result")})
				newOutput := mustJSON(t, rsOut{V: filepath.Join(root, "new-result")})
				durable := wf.JournalRecord{Run: run, Instance: issued.Instance, State: issued.State, Attempt: issued.Attempt, DefHash: issued.DefHash, At: h.now, Kind: wf.JournalSubmitted, Data: mustJSON(t, wf.SubmittedData{Token: instruction.Token, Output: oldOutput})}
				unlock, err := h.j.Lock(run)
				if err != nil {
					t.Fatal(err)
				}
				err = h.j.Append(durable)
				unlock()
				if err != nil {
					t.Fatal(err)
				}
				// Restart with only the durable prefix, retaining both configured roots.
				opts.Defs = []*wf.Def{def}
				opts.Clock = func() time.Time { return h.now }
				h.e = wf.NewEngine(h.j, h.reg, opts)
				before := len(h.records(run))
				submission := wf.Submission{Child: in.Child, Token: instruction.Token, Output: newOutput}
				var result wf.SubmitResult
				switch verb {
				case "Next":
					_, err = h.e.Next(run, "")
				case "Validate":
					result, err = h.e.Validate(run, in.Name, submission)
				case "Done":
					result, err = h.e.Done(run, in.Name, submission)
				case "Submit":
					result, err = h.e.Submit(run, submission)
				}
				if err != nil {
					t.Fatalf("%s blocked durable replay: %v", verb, err)
				}
				if verb != "Next" && result.Class != wf.Accepted {
					t.Fatalf("%s result: %+v", verb, result)
				}
				if verb == "Validate" {
					if after := len(h.records(run)); after != before {
						t.Fatalf("Validate changed journal: %d -> %d", before, after)
					}
					if _, err := h.e.Next(run, ""); err != nil {
						t.Fatal(err)
					}
				}
				submitted := h.kind(run, wf.JournalSubmitted)
				if len(submitted) != 1 {
					t.Fatalf("replay journaled %d submissions", len(submitted))
				}
				var saved wf.SubmittedData
				if err := json.Unmarshal(submitted[0].Data, &saved); err != nil {
					t.Fatal(err)
				}
				if string(saved.Output) != string(oldOutput) {
					t.Fatalf("durable output replaced: %s", saved.Output)
				}
				accepted := h.kind(run, wf.JournalAccepted)
				if len(accepted) != 1 {
					t.Fatalf("accepted records: %d", len(accepted))
				}
				var decision wf.AcceptedData
				if err := json.Unmarshal(accepted[0].Data, &decision); err != nil {
					t.Fatal(err)
				}
				if string(decision.Output) != string(oldOutput) {
					t.Fatalf("replay classified new bytes: %s", decision.Output)
				}
			})
		}
	}
}

func TestResultRunPathsRejectsNewLateSettledDone(t *testing.T) {
	state, workspace := t.TempDir(), t.TempDir()
	def := fanDef("late-result-path", chItems(1, 2), 2, wf.Quorum(1))
	h := newHarnessOpts(t, def, nil, wf.EngineOptions{StateRoot: state, Workspace: workspace})
	run := h.run(def, nil)
	seats := h.handOut(run, 2)
	for _, seat := range seats {
		if err := h.e.Started(run, seat.Name, claim(seat.Name)); err != nil {
			t.Fatal(err)
		}
		if _, err := h.e.Prompt(run, seat.Name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.e.Done(run, seats[0].Name, jsonOut(t, `{"OK":true}`)); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{state, workspace} {
		before := len(h.records(run))
		output := mustJSON(t, map[string]any{"OK": true, "Path": filepath.Join(root, "new-late-result")})
		if _, err := h.e.Done(run, seats[1].Name, wf.Submission{Output: output}); !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("late result path accepted: %v", err)
		}
		if after := len(h.records(run)); after != before {
			t.Fatalf("late refusal appended records: %d -> %d", before, after)
		}
	}
}
