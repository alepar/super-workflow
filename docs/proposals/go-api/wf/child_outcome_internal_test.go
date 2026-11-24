package wf

import (
	"encoding/json"
	"testing"
)

// an earlier revision post-cap (2): a Migration renaming a Sub re-keys its child's
// outcome with its result, so ChildOutcome under the new name still reads it.
func TestRenameKeepsTheChildOutcomeWithItsSub(t *testing.T) {
	in := &instance{
		Outputs: map[string]json.RawMessage{}, Computed: map[string]json.RawMessage{}, Visits: map[string]int{},
		Children:      map[string]json.RawMessage{"Old": json.RawMessage(`{}`)},
		ChildOutcomes: map[string]Outcome{"Old": Partial},
	}
	in.rename(map[string]string{"Old": "New"}, "New")
	if got := in.ChildOutcomes["New"]; got != Partial {
		t.Errorf("ChildOutcomes after renaming Old to New = %v, want New: partial", in.ChildOutcomes)
	}
	if _, ok := in.ChildOutcomes["Old"]; ok {
		t.Errorf("ChildOutcomes still keys Old: %v", in.ChildOutcomes)
	}
}
