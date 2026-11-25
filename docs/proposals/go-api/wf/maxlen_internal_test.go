package wf

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type (
	mlInner struct {
		Code string `json:"code" maxlen:"3"`
	}
	mlOut struct {
		Summary string    `json:"summary" maxlen:"5"`
		Note    *string   `json:"note,omitempty" maxlen:"4"`
		Tags    []string  `json:"tags,omitempty" maxlen:"2"`
		Inner   mlInner   `json:"inner"`
		Rows    []mlInner `json:"rows,omitempty"`
		Free    string    `json:"free"`
	}
	mlAsk struct{}
)

// TestMaxlenSchema: a maxlen tag is shown to the agent as maxLength, on a
// string, a *string, each element of a []string and nested fields alike.
func TestMaxlenSchema(t *testing.T) {
	var s struct {
		Properties map[string]struct {
			MaxLength int `json:"maxLength"`
			Items     struct {
				MaxLength int `json:"maxLength"`
			}
			Properties map[string]struct {
				MaxLength int `json:"maxLength"`
			}
		}
	}
	if err := json.Unmarshal([]byte(outputSchema(reflect.TypeFor[mlOut](), nil)), &s); err != nil {
		t.Fatal(err)
	}
	p := s.Properties
	if p["summary"].MaxLength != 5 || p["note"].MaxLength != 4 || p["tags"].Items.MaxLength != 2 ||
		p["inner"].Properties["code"].MaxLength != 3 || p["free"].MaxLength != 0 {
		t.Errorf("schema = %+v, want maxLength 5, 4, items 2, nested 3 and none on free", p)
	}
}

// TestMaxlenCheck: the check counts characters (runes, as JSON Schema's
// maxLength does), covers the same fields the schema shows, and refuses
// without echoing the over-long value.
func TestMaxlenCheck(t *testing.T) {
	d := New("ml")
	ok := []string{
		`{"summary":"abcde","inner":{"code":"abc"},"free":"` + strings.Repeat("x", 5000) + `"}`,
		`{"summary":"ééééé","inner":{"code":"日本語"},"tags":["éé"]}`, // 5, 3 and 2 runes, more bytes
		`{"summary":"","inner":{"code":""},"note":"abcd"}`,
	}
	for _, raw := range ok {
		if _, _, err := checkedAgentOutput[mlOut](json.RawMessage(raw), d); err != nil {
			t.Errorf("%s: refused: %v", raw, err)
		}
	}
	long := strings.Repeat("secret-long-value", 3)
	for raw, field := range map[string]string{
		`{"summary":"éééééé","inner":{}}`:                    "output.summary: 6 characters",
		`{"summary":"","note":"abcde","inner":{}}`:           "output.note: 5 characters",
		`{"summary":"","tags":["ab","abc"],"inner":{}}`:      "output.tags[1]: 3 characters",
		`{"summary":"","inner":{"code":"abcd"}}`:             "output.inner.code: 4 characters",
		`{"summary":"","inner":{},"rows":[{"code":"abcd"}]}`: "output.rows[0].code: 4 characters",
		`{"summary":"` + long + `","inner":{}}`:              "output.summary: 51 characters",
	} {
		_, stage, err := checkedAgentOutput[mlOut](json.RawMessage(raw), d)
		if err == nil || stage != "maxlen" || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "limit") {
			t.Errorf("%s: stage %q err %v, want the maxlen stage naming %q and its limit", raw, stage, err, field)
			continue
		}
		if strings.Contains(err.Error(), "secret-long-value") || strings.Contains(err.Error(), "éééééé") {
			t.Errorf("refusal echoes the value: %v", err)
		}
	}
}

type (
	mlBadValue struct {
		S string `json:"s" maxlen:"ten"`
	}
	mlBadZero struct {
		S string `json:"s" maxlen:"0"`
	}
	mlBadKind struct {
		N int `json:"n" maxlen:"3"`
	}
	mlBadNested struct {
		Rows []mlBadValue `json:"rows"`
	}
	mlEncoded   string
	mlBadEncode struct {
		E mlEncoded `json:"e" maxlen:"3"`
	}
	mlBadPtrs struct {
		P []*string `json:"p" maxlen:"3"`
	}
	mlBadMap struct {
		M map[string]string `json:"m" maxlen:"3"`
	}
	mlBadBytes struct {
		B []byte `json:"b" maxlen:"3"`
	}
	mlOKShapes struct {
		P *[]string `json:"p" maxlen:"3"`
		A [2]string `json:"a" maxlen:"3"`
		B string    `json:"b" maxlen:"3" pattern:"[a-z]*"`
	}
)

func (mlEncoded) MarshalText() ([]byte, error) { return []byte("x"), nil }

type (
	mlEncodedList    []string
	mlBadEncodedList struct {
		L mlEncodedList `json:"l" maxlen:"3"`
	}
)

func (mlEncodedList) MarshalJSON() ([]byte, error) { return []byte(`[]`), nil }

func lintMaxlen[O any](t *testing.T) []Problem {
	t.Helper()
	d := New("ml")
	a := NewAgent[mlAsk, O](d)
	done := NewTerminal[struct{}](d, Success)
	a.Spec(PromptText("x"), Retry[mlAsk](Const(1), a.To(done, "gave up")))
	e := a.To(done, "ok")
	a.Route(func(O, Ctx) Edge[mlAsk] { return e }, e)
	var out []Problem
	for _, p := range d.Lint() {
		if p.Rule == RuleMaxlenTag {
			out = append(out, p)
		}
	}
	return out
}

// TestMaxlenTagLint: a malformed tag is refused by lint, before any run.
func TestMaxlenTagLint(t *testing.T) {
	if ps := lintMaxlen[mlOut](t); len(ps) != 0 {
		t.Errorf("well-formed tags: %v", ps)
	}
	for name, ps := range map[string][]Problem{
		"not a number": lintMaxlen[mlBadValue](t),
		"zero":         lintMaxlen[mlBadZero](t),
		"not a string": lintMaxlen[mlBadKind](t),
		"nested":       lintMaxlen[mlBadNested](t),
		"own encoding": lintMaxlen[mlBadEncode](t),
		"[]*string":    lintMaxlen[mlBadPtrs](t),
		"map":          lintMaxlen[mlBadMap](t),
		"[]byte":       lintMaxlen[mlBadBytes](t),
		"encoded list": lintMaxlen[mlBadEncodedList](t),
	} {
		if len(ps) != 1 {
			t.Errorf("%s: problems %v, want one %s", name, ps, RuleMaxlenTag)
		}
	}
}

// TestMaxlenOtherShapes: *[]string and [N]string are capped too, and a field
// with both tags is refused for its length without echoing the value.
func TestMaxlenOtherShapes(t *testing.T) {
	if ps := lintMaxlen[mlOKShapes](t); len(ps) != 0 {
		t.Errorf("lint: %v", ps)
	}
	d := New("ml")
	for raw, want := range map[string]string{
		`{"p":["abcd"],"a":["",""],"b":""}`: "output.p[0]: 4 characters",
		`{"a":["","abcd"],"b":""}`:          "output.a[1]: 4 characters",
		`{"a":["",""],"b":"ABCDE"}`:         "output.b: 5 characters",
	} {
		_, stage, err := checkedAgentOutput[mlOKShapes](json.RawMessage(raw), d)
		if err == nil || stage != "maxlen" || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "ABCDE") {
			t.Errorf("%s: stage %q err %v, want %q", raw, stage, err, want)
		}
	}
}

type mlIn struct {
	Topic string `json:"topic" maxlen:"x"`
}

// TestMaxlenTagLintInputAndResult: lint covers the definition's input and a
// yield's result, which the engine checks too.
func TestMaxlenTagLintInputAndResult(t *testing.T) {
	child := New("child")
	Typed[mlIn, mlBadZero](child)
	a := NewAgent[mlAsk, mlOut](child)
	done := NewTerminal[struct{}](child, Success)
	a.Spec(PromptText("x"), Retry[mlAsk](Const(1), a.To(done, "gave up")))
	e := a.To(done, "ok")
	a.Route(func(mlOut, Ctx) Edge[mlAsk] { return e }, e)
	Yield(done, func(Ctx) mlBadZero { return mlBadZero{} })
	var got []string
	for _, p := range child.Lint() {
		if p.Rule == RuleMaxlenTag {
			got = append(got, p.Msg)
		}
	}
	if len(got) != 2 || !strings.Contains(strings.Join(got, "|"), "input field") || !strings.Contains(strings.Join(got, "|"), "result field") {
		t.Errorf("problems %q, want one on the input and one on the result", got)
	}
}
