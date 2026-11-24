package wf_test

import (
	"encoding/json"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"testing"
)

// A schema sampler that loses registered enums generates invalid results even
// for its valid-value mode. Read the actual agent schema, not its type name.
func TestStateViewOutputSchemaPreservesRegisteredEnum(t *testing.T) {
	d := outOfEnumDef(false)
	before := d.Hash()
	states := wf.Inspect(d).States
	var schema struct {
		Properties map[string]struct {
			Enum []string
			Type string
		}
	}
	if err := json.Unmarshal(states[0].OutputSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	verdict := schema.Properties["Verdict"]
	if verdict.Type != "string" || len(verdict.Enum) != 2 || verdict.Enum[0] != "pass" || verdict.Enum[1] != "fail" {
		t.Fatalf("enum schema lost: %+v", schema)
	}
	if d.Hash() != before {
		t.Fatal("reading the schema mutated definition identity")
	}
	for _, s := range states {
		if s.Kind == "terminal" && s.OutputSchema() != nil {
			t.Fatalf("terminal %s exposes agent schema", s.Name)
		}
	}
}
