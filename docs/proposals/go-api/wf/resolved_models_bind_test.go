package wf_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// modelsArgvDef is a command that passes the run's {harness} and
// {resolvedModels} as flag values, the shape examplewf's RoastAssembleArgs
// uses for assemble-args.mjs.
func modelsArgvDef() *wf.Def {
	d := wf.New("models-argv")
	exits := wf.NewEnum(d, "d-exit", dExit(0), dExit(1), dExit(124), dExit(125))
	cmd := wf.NewCommand[dRun](d, "assemble", "--harness", "{harness}", "--resolved-models", "{resolvedModels}")
	ok := wf.NewTerminal[dOK](d, wf.Success)
	fail := wf.NewTerminal[dFail](d, wf.Stopped)
	cmd.Key("assemble")
	cmd.Idempotent()
	cmd.RouteCases(wf.Cases[dRun, wf.RunResult](wf.ExitAs[dExit], exits,
		wf.On(dExit(0), cmd.To(ok, "passes")),
		wf.On(dExit(1), cmd.To(fail, "fails")),
		wf.On(dExit(124), cmd.To(fail, "timed out")),
		wf.On(dExit(125), cmd.To(fail, "runner failed"))))
	return d
}

func modelsArgv(t *testing.T, harness string) []string {
	t.Helper()
	d := modelsArgvDef()
	h := newHarnessOpts(t, d, newWorld(), wf.EngineOptions{Harness: harness})
	run := h.run(d, nil)
	h.next(run)
	intents := h.kind(run, wf.JournalEffectIntent)
	if len(intents) != 1 {
		t.Fatalf("effect_intent records = %d, want 1", len(intents))
	}
	var data wf.EffectIntentData
	if err := json.Unmarshal(intents[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	return toStrings(data.Effect.Args["argv"])
}

// A run that resolves no models (a generic harness, or no tiered state) binds
// {resolvedModels} empty, never as "{}": a definition passing it as an
// argument must read "no models resolved" as absent, not as an empty tier
// table that has no model for any tier.
func TestResolvedModelsBindsEmptyWhenNoModelsResolve(t *testing.T) {
	argv := modelsArgv(t, "generic")
	want := []string{"assemble", "--harness", "generic", "--resolved-models", ""}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
}

func TestResolvedModelsBindsEmptyInAPrompt(t *testing.T) {
	d := tierDef("m=[{{resolvedModels}}]")
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "generic"})
	ins := h.next(h.run(d, nil))
	if len(ins) != 1 || !strings.Contains(ins[0].Prompt, "m=[]") {
		t.Fatalf("prompt = %+v, want m=[] for a run with no resolved models", ins)
	}
}
