package wf

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/form"
)

type scFindingID struct{}

func (scFindingID) Name() string           { return "wf-test-finding-id" }
func (g scFindingID) Check(s string) error { return form.CheckMinted(g.Prefix(), s) }
func (scFindingID) Samples() []string      { return []string{"F1", "F23"} }
func (scFindingID) Prefix() byte           { return 'F' }
func (g scFindingID) Pattern() string      { return form.MintedPattern(g.Prefix()) }

var scGrades = []string{"keep", "drop"}

type scGrade struct{}

func (scGrade) Name() string           { return "wf-test-grade" }
func (g scGrade) Check(s string) error { return form.CheckWord(s, g.Values()...) }
func (scGrade) Samples() []string      { return scGrades }
func (scGrade) Values() []string       { return scGrades }

func init() {
	form.Register[scFindingID]()
	form.Register[scGrade]()
}

type scFormOut struct {
	ID     form.Of[scFindingID]                      `json:"id"`
	Grades map[form.Of[scFindingID]]form.Of[scGrade] `json:"grades"`
	Covers []form.Of[scFindingID]                    `json:"covers,omitempty"`
	Note   string                                    `json:"note"`
	Parent *form.Of[scFindingID]                     `json:"parent,omitempty"`
}

// A form.Of field reaches the agent as a string with its grammar's pattern,
// values and example, never as {} (any value).
func TestOutputSchemaRendersFormValues(t *testing.T) {
	var s struct {
		Properties map[string]map[string]any
	}
	if err := json.Unmarshal([]byte(outputSchema(reflect.TypeFor[scFormOut](), nil)), &s); err != nil {
		t.Fatal(err)
	}
	id := s.Properties["id"]
	if id["type"] != "string" || id["pattern"] != "^F[1-9][0-9]*$" || !reflect.DeepEqual(id["examples"], []any{"F1"}) {
		t.Fatalf("id schema %v", id)
	}
	grade := s.Properties["grades"]["additionalProperties"].(map[string]any)
	if grade["type"] != "string" || !reflect.DeepEqual(grade["enum"], []any{"keep", "drop"}) {
		t.Fatalf("grade schema %v", grade)
	}
	if p := s.Properties["parent"]; p["pattern"] != "^F[1-9][0-9]*$" {
		t.Fatalf("parent schema %v", p)
	}
	items := s.Properties["covers"]["items"].(map[string]any)
	if items["pattern"] != "^F[1-9][0-9]*$" {
		t.Fatalf("covers items %v", items)
	}
}

func TestOutputExampleIsAValidOutput(t *testing.T) {
	ex := outputExample(reflect.TypeFor[scFormOut](), nil)
	want := `{
  "covers": [
    "F1"
  ],
  "grades": {
    "F1": "keep"
  },
  "id": "F1",
  "note": "text",
  "parent": "F1"
}`
	if ex != want {
		t.Fatalf("example:\n%s\nwant:\n%s", ex, want)
	}
	if _, stage, err := checkedAgentOutput[scFormOut](json.RawMessage(ex), &Def{}); err != nil {
		t.Fatalf("the example is refused at %s: %v", stage, err)
	}
}

// Submission decodes into the output type, so a form field outside its
// grammar is refused at decode.
func TestSubmissionRefusesMalformedFormValue(t *testing.T) {
	for _, bad := range []string{`{"id":"F0","note":""}`, `{"id":"F1","grades":{"F2":"maybe"}}`, `{"id":"finding about x"}`} {
		_, stage, err := checkedAgentOutput[scFormOut](json.RawMessage(bad), &Def{})
		if err == nil || stage != "decode" || !strings.Contains(err.Error(), "wf-test-") {
			t.Errorf("%s: stage %q, err %v", bad, stage, err)
		}
	}
}
