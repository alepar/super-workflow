package wf_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestCLIExplicitAssetsReplaceMissingDefault(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "prompt.md", "chosen")
	d := wf.New("asset-cli").Assets(filepath.Join(root, "missing"))
	a := wf.NewAgent[pfA, string](d)
	end := wf.NewTerminal[pfEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	edge := a.To(end, "done")
	a.Spec(wf.PromptFile("{assets}/prompt.md"), wf.Retry(wf.Const(1), edge))
	a.Then(edge)
	cfg := wf.MainConfig{Binary: "asset-cli", Defs: []wf.Registration{{Def: d}}}
	state := filepath.Join(t.TempDir(), "state")
	call := func(args ...string) (string, string) {
		var out, errout bytes.Buffer
		args = append(args, "--state-dir", state, "--assets", root)
		if code := wf.CLI(cfg, args, &out, &errout); code != 0 {
			t.Fatalf("%v: %d %s", args, code, &errout)
		}
		return out.String(), errout.String()
	}
	call("run", d.Name(), "--run-id", "asset-run")
	call("wf", "next", "--assets", root, "0", "--json", "--run-id", "asset-run")
	call("next", "--assets", root, "--run-id", "asset-run")
	_, errout := call("wf", "next", "1", "--json", "--run-id", "asset-run")
	_ = errout
	out, _ := call("wf", "prompt", "pfA#1", "--json", "--run-id", "asset-run")
	var fetched wf.PromptFetch
	if err := json.Unmarshal([]byte(out), &fetched); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fetched.Prompt, "--assets "+root) {
		t.Fatalf("child commands dropped override: %s", fetched.Prompt)
	}
}

func TestRunContextCarriesEffectiveAssets(t *testing.T) {
	base, selected := t.TempDir(), t.TempDir()
	d := promptHashDef("answer").Assets(base)
	d.Param("metadata")
	seen := ""
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d, Params: []wf.ParamSpec{{Name: "metadata", Derive: func(c wf.RunContext) (string, error) {
		b, _ := json.Marshal(c)
		var fields struct{ Assets string }
		_ = json.Unmarshal(b, &fields)
		seen = fields.Assets
		return "version", nil
	}}}}}}
	for _, root := range []string{selected, base} {
		args := []string{"run", d.Name(), "--state-dir", filepath.Join(t.TempDir(), "state"), "--run-id", "asset-test"}
		if root == selected {
			args = append(args, "--assets", root)
		}
		var out, errout bytes.Buffer
		if code := wf.CLI(cfg, args, &out, &errout); code != 0 {
			t.Fatalf("run: %d %s", code, &errout)
		}
		if seen != root {
			t.Fatalf("derived metadata used %q, want selected root %q", seen, root)
		}
	}
}

func TestCLIAdoptReportsHashesAndCarriedInstances(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	old, newDef := promptHashDef("old"), promptHashDef("new")
	call := func(d *wf.Def, args ...string) string {
		var out, errout bytes.Buffer
		args = append(args, "--state-dir", state)
		if code := wf.CLI(wf.MainConfig{Defs: []wf.Registration{{Def: d}}}, args, &out, &errout); code != 0 {
			t.Fatalf("%v: %d %s", args, code, &errout)
		}
		return errout.String()
	}
	call(old, "run", old.Name(), "--run-id", "adoption-report")
	call(old, "next", "adoption-report", "0", "--json")
	report := call(newDef, "resume", "adoption-report", "--adopt")
	for _, want := range []string{old.Hash(), newDef.Hash(), "carried root: pfA"} {
		if !strings.Contains(report, want) {
			t.Fatalf("adoption report missing %q: %s", want, report)
		}
	}
}

func TestCLIRemembersAssetRootAcrossCalls(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	for _, dir := range []string{root, other} {
		writeFile(t, dir, "prompt.md", "chosen")
	}
	d := wf.New("remember-assets").Assets(filepath.Join(root, "missing-default"))
	a := wf.NewAgent[pfA, string](d)
	end := wf.NewTerminal[pfEnd](d, wf.Success)
	edge := a.To(end, "done")
	d.OnFailure(wf.Blocked, end)
	a.Spec(wf.PromptFile("{assets}/prompt.md"), wf.Retry(wf.Const(1), edge))
	a.Then(edge)
	cfg := wf.MainConfig{Binary: "asset-cli", Defs: []wf.Registration{{Def: d}}}
	state := filepath.Join(t.TempDir(), "state")
	call := func(want int, args ...string) string {
		t.Helper()
		var out, errout bytes.Buffer
		args = append(args, "--state-dir", state)
		if code := wf.CLI(cfg, args, &out, &errout); code != want {
			t.Fatalf("%v: code %d want %d: %s", args, code, want, &errout)
		}
		return out.String()
	}
	call(0, "run", d.Name(), "--run-id", "remember", "--assets", root)
	call(0, "run", d.Name(), "--run-id", "remember")
	call(2, "run", d.Name(), "--run-id", "remember", "--workspace", t.TempDir())
	call(0, "wf", "status", "--json")
	var instructions []wf.Instruction
	if err := json.Unmarshal([]byte(call(0, "wf", "next", "1", "--json")), &instructions); err != nil {
		t.Fatal(err)
	}
	out := call(0, "wf", "prompt", "pfA#1", "--json")
	if !strings.Contains(out, "chosen") {
		t.Fatalf("wrong prompt: %s", out)
	}
	// Equal content does not authorize switching filesystem roots.
	call(6, "wf", "status", "--assets", other)
	call(0, "wf", "resume", "--adopt", "--assets", other)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	call(0, "wf", "status", "--json")
	call(0, "wf", "prompt", "pfA#1", "--json")
	output := writeAllocated(t, instructions[0].Output, `"done"`)
	call(0, "wf", "done", "pfA#1", "--output", output)
}
