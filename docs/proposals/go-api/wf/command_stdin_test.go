package wf_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// stdinDef is detDef with a Command that takes the given Stdin template.
func stdinDef(stdin string) *wf.Def {
	return detDef(func(c *wf.Command[dRun]) {
		c.Idempotent()
		if stdin != "" {
			c.Stdin(stdin)
		}
	})
}

func intentArgs(t *testing.T, d *wf.Def) map[string]any {
	t.Helper()
	h := newHarness(t, d, newWorld())
	run := h.run(d, map[string]string{"topic": "build"})
	h.next(run)
	intents := h.kind(run, wf.JournalEffectIntent)
	if len(intents) != 1 {
		t.Fatalf("effect_intent records = %d, want 1", len(intents))
	}
	var data wf.EffectIntentData
	if err := json.Unmarshal(intents[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	return data.Effect.Args
}

func TestCommandStdinBoundIntoArgs(t *testing.T) {
	args := intentArgs(t, stdinDef("{topic}"))
	if args["stdin"] != "build" {
		t.Fatalf("effect_intent args = %v, want stdin \"build\"", args)
	}
	if got := strings.Join(toStrings(args["argv"]), " "); got != "make build" {
		t.Fatalf("argv = %q, want stdin kept out of argv", got)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestCommandWithoutStdinArgsUnchanged(t *testing.T) {
	args := intentArgs(t, stdinDef(""))
	if _, has := args["stdin"]; has {
		t.Fatalf("effect_intent args = %v, want no stdin key", args)
	}
}

func TestCommandStdinRendersAndHashes(t *testing.T) {
	plain, with := stdinDef(""), stdinDef("{topic}")
	if !strings.Contains(with.Render(), "stdin={topic}") {
		t.Fatalf("render = %q, want stdin={topic}", with.Render())
	}
	if strings.Contains(plain.Render(), "stdin=") {
		t.Fatalf("render = %q, want no stdin= without Stdin", plain.Render())
	}
	if plain.Hash() == with.Hash() {
		t.Fatal("Stdin did not change the definition hash")
	}
	// The omitempty field leaves a definition without Stdin at its hash from
	// before the field existed; the view JSON proves the key is absent.
	b, err := json.Marshal(wf.Inspect(plain))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"Stdin"`) {
		t.Fatalf("inspect JSON of a definition without Stdin names Stdin: %s", b)
	}
}
