package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type resultPathAgent struct{}
type resultPathDone struct{}
type resultPathOutput struct{ Text string }

func TestResultRunPathsRefusedWithoutSpendingAttempt(t *testing.T) {
	for _, verb := range []string{"validate", "done", "submit"} {
		for _, shape := range []string{"state", "workspace", "nested", "escaped", "duplicate"} {
			t.Run(verb+"/"+shape, func(t *testing.T) {
				state, workspace := t.TempDir(), t.TempDir()
				d := resultPathsDefinition()
				h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: workspace, StateRoot: state, RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }})
				run := h.run(d, nil)
				in, _ := startedUnit(t, h, run, "agent")
				path := state + "/records/spec.md"
				if shape == "workspace" {
					path = workspace + "/src/file.go"
				}
				value := "read " + path + " now"
				if shape == "workspace" {
					value = path
				}
				q, _ := json.Marshal(value)
				raw := `{"Text":` + string(q) + `}`
				switch shape {
				case "nested":
					raw = `{"Text":"content","extra":[{"path":` + string(q) + `}]}`
				case "escaped":
					raw = strings.ReplaceAll(raw, "/", `\u002f`)
				case "duplicate":
					raw = `{"Text":` + string(q) + `,"Text":"content"}`
				}
				before, _ := json.Marshal(h.records(run))
				s := wf.Submission{Token: in.Token, Output: json.RawMessage(raw)}
				var err error
				switch verb {
				case "validate":
					_, err = h.e.Validate(run, in.Name, s)
				case "done":
					_, err = h.e.Done(run, in.Name, s)
				case "submit":
					_, err = h.e.Submit(run, s)
				}
				want := "leave run-owned paths out; the engine already records your artifacts"
				if shape == "workspace" {
					want = "give this path relative to the repository root"
				}
				if !errors.Is(err, wf.ErrRejected) || !strings.Contains(fmt.Sprint(err), want) {
					t.Fatalf("refusal: %v", err)
				}
				after, _ := json.Marshal(h.records(run))
				if string(before) != string(after) {
					t.Fatal("refusal changed journal")
				}
				// A root in a JSON object key is not a string value; outside paths and
				// ordinary relative filenames remain valid content.
				clean, _ := json.Marshal(map[string]any{"Text": "spec.md and /outside/file; read " + workspace + "/src/file.go", state: "content"})
				if res, err := h.e.Done(run, in.Name, wf.Submission{Output: clean}); err != nil || res.Class != wf.Accepted {
					t.Fatalf("same-attempt correction: %+v %v", res, err)
				}
				if len(h.kind(run, wf.JournalInstructionIssued)) != 1 || len(h.kind(run, wf.JournalSubmitted)) != 1 {
					t.Fatal("correction spent/reissued attempt")
				}
			})
		}
	}
}

func resultPathsDefinition() *wf.Def {
	d := wf.New("result-paths")
	a := wf.NewAgent[resultPathAgent, resultPathOutput](d)
	end := wf.NewTerminal[resultPathDone](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	edge := a.To(end, "done")
	a.Spec(wf.PromptText("return content"), wf.Retry[resultPathAgent](wf.Const(1), edge))
	a.Route(func(resultPathOutput, wf.Ctx) wf.Edge[resultPathAgent] { return edge }, edge)
	return d
}

func TestResultRunPathsCLIRefusalKeepsAttempt(t *testing.T) {
	state, workspace := t.TempDir(), t.TempDir()
	d := resultPathsDefinition()
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}}
	call := func(want int, args ...string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		code := fixtureCLI(cfg, append(args, "--state-dir", state), &out, &stderr)
		if code != want {
			t.Fatalf("%v: exit %d, want %d: %s", args, code, want, &stderr)
		}
		if want != 0 {
			return stderr.String()
		}
		return out.String()
	}
	run := strings.TrimSpace(call(0, "run", d.Name(), "--workspace", workspace, "--run-id", "paths-cli"))
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(call(0, "next", run, "1", "--json")), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("instructions: %+v %v", ins, err)
	}
	in := ins[0]
	call(0, "dev", "started", run, in.Name, "--harness", "claude", "--session", "session", "--agent", "agent")
	call(0, "prompt", run, in.Name)
	before := call(0, "journal", run)
	raw, _ := json.Marshal(resultPathOutput{Text: state + "/records/spec.md"})
	writeAllocated(t, in.Output, string(raw))
	for _, verb := range []string{"validate", "done"} {
		args := []string{verb, run, in.Name}
		if verb == "done" {
			args = append(args, "--escalated", "go test: cache: ran")
		}
		stderr := call(5, args...)
		if !strings.Contains(stderr, "leave run-owned paths out; the engine already records your artifacts") || !strings.Contains(stderr, "your attempt is still open") || strings.Contains(stderr, "attempt is spent") {
			t.Fatalf("%s: %s", verb, stderr)
		}
		if after := call(0, "journal", run); after != before {
			t.Fatalf("%s changed journal\nbefore: %s\nafter: %s", verb, before, after)
		}
	}
	writeAllocated(t, in.Output, `{"Text":"spec content"}`)
	call(0, "validate", run, in.Name)
	call(0, "done", run, in.Name)
}
