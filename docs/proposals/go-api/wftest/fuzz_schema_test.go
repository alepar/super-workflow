package wftest

import (
	"encoding/json"
	"regexp"
	"testing"
)

// These literal fixtures catch omitted required fields, lost enum alternatives,
// ignored bounds, and sampling that returns values outside its declared shape.
func TestFuzzSchemaValues(t *testing.T) {
	tests := []struct {
		name, schema string
		variant      int
		want         string
	}{
		{"enum null", `{"enum":[null,false,2,"x",{"a":1}]}`, 0, `null`},
		{"enum bool", `{"enum":[null,false,2,"x",{"a":1}]}`, 1, `false`},
		{"enum number", `{"enum":[null,false,2,"x",{"a":1}]}`, 2, `2`},
		{"enum string", `{"enum":[null,false,2,"x",{"a":1}]}`, 3, `"x"`},
		{"enum object", `{"enum":[null,false,2,"x",{"a":1}]}`, 4, `{"a":1}`},
		{"enum wraps", `{"enum":[null,false,2,"x",{"a":1}]}`, 5, `null`},
		{"required only", `{"type":"object","properties":{"z":{"type":"boolean"},"a":{"type":"string"}},"required":["z"]}`, 0, `{"z":false}`},
		{"optional included", `{"type":"object","properties":{"z":{"type":"boolean"},"a":{"type":"string"}},"required":["z"]}`, 1, `{"a":"a","z":true}`},
		{"nested", `{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}}},"required":["rows"]}`, 1, `{"rows":[{"ok":true}]}`},
		{"empty array", `{"type":"array","items":{"type":"integer"}}`, 0, `[]`},
		{"one array", `{"type":"array","items":{"type":"integer"}}`, 1, `[1]`},
		{"min items", `{"type":"array","items":{"type":"null"},"minItems":2,"maxItems":3}`, 0, `[null,null]`},
		{"max items", `{"type":"array","items":{"type":"null"},"minItems":2,"maxItems":3}`, 2, `[null,null,null]`},
		{"empty map", `{"type":"object","additionalProperties":{"type":"integer"}}`, 0, `{}`},
		{"map value", `{"type":"object","additionalProperties":{"type":"integer"}}`, 1, `{"key000":1}`},
		{"integer min", `{"type":"integer","minimum":2.2,"maximum":5.8}`, 0, `3`},
		{"integer max", `{"type":"integer","minimum":2.2,"maximum":5.8}`, 2, `5`},
		{"number min", `{"type":"number","minimum":1.25,"maximum":2.75}`, 0, `1.25`},
		{"number max", `{"type":"number","minimum":1.25,"maximum":2.75}`, 2, `2.75`},
		{"string min", `{"type":"string","minLength":2,"maxLength":4}`, 0, `"aa"`},
		{"string max", `{"type":"string","minLength":2,"maxLength":4}`, 2, `"aaaa"`},
		{"null", `{"type":"null"}`, 2, `null`},
		{"unconstrained", `{}`, 0, `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := drawSchema(json.RawMessage(tt.schema), tt.variant)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %s; want %s", got, tt.want)
			}
		})
	}
}

func TestFuzzSchemaBoundedArray(t *testing.T) {
	got, err := drawSchema(json.RawMessage(`{"type":"array","items":{"type":"boolean"}}`), 2)
	if err != nil {
		t.Fatal(err)
	}
	var values []bool
	if err = json.Unmarshal(got, &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 16 {
		t.Fatalf("array boundary length = %d; want 16", len(values))
	}
}

func TestFuzzSchemaPattern(t *testing.T) {
	for _, pattern := range []string{`^R[0-9]+$`, `^(cat|dog){2,3}$`, `^[A-Z]{2}[0-9]{3}$`, `^\bword\b$`} {
		for v := 0; v < 3; v++ {
			schema, _ := json.Marshal(map[string]any{"type": "string", "pattern": pattern})
			got, err := drawSchema(schema, v)
			if err != nil {
				t.Fatalf("%s: %v", pattern, err)
			}
			var value string
			if err = json.Unmarshal(got, &value); err != nil {
				t.Fatal(err)
			}
			if !regexp.MustCompile(pattern).MatchString(value) {
				t.Fatalf("%q does not match %s", value, pattern)
			}
			again, err := drawSchema(schema, v)
			if err != nil || string(got) != string(again) {
				t.Fatalf("not deterministic: %s, %s, %v", got, again, err)
			}
		}
	}
	got, err := drawSchema(json.RawMessage(`{"type":"string","pattern":"^R[0-9]+$","minLength":5,"maxLength":5}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	var value string
	json.Unmarshal(got, &value)
	if len(value) != 5 || !regexp.MustCompile(`^R[0-9]+$`).MatchString(value) {
		t.Fatalf("invalid bounded pattern sample %q", value)
	}
}

func TestFuzzSchemaErrors(t *testing.T) {
	for _, schema := range []string{``, `null`, `[]`, `{"type":"bogus"}`, `{"enum":[]}`, `{"type":"string","pattern":"["}`, `{"oneOf":[{"type":"string"}]}`, `{"type":"array","minItems":17}`, `{"type":"string","minLength":4,"maxLength":2}`, `{"type":"integer","minimum":1.1,"maximum":1.2}`, `{"type":"object","required":["missing"]}`, `{"type":"array","minItems":-1}`, `{"type":"string","pattern":"^a$","minLength":2}`} {
		t.Run(schema, func(t *testing.T) {
			if got, err := drawSchema(json.RawMessage(schema), 0); err == nil {
				t.Fatalf("got %s; want an invalid/unsupported schema error", got)
			}
		})
	}
}

// A shared variant must not permanently hide enum zero behind empty containers
// or optional omission when the enum and container cycles have the same size.
func TestFuzzSchemaNestedEnumCoverage(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object","properties":{"value":{"enum":["red","green","blue"]}}}`,
		`{"type":"array","items":{"enum":["red","green","blue"]}}`,
		`{"type":"object","additionalProperties":{"enum":["red","green","blue"]}}`,
	} {
		seen := map[string]bool{}
		for v := 0; v < 9; v++ {
			raw, err := drawSchema(json.RawMessage(schema), v)
			if err != nil {
				t.Fatal(err)
			}
			var value any
			if err = json.Unmarshal(raw, &value); err != nil {
				t.Fatal(err)
			}
			switch x := value.(type) {
			case map[string]any:
				for _, item := range x {
					seen[item.(string)] = true
				}
			case []any:
				for _, item := range x {
					seen[item.(string)] = true
				}
			}
		}
		for _, want := range []string{"red", "green", "blue"} {
			if !seen[want] {
				t.Errorf("schema %s never sampled enum alternative %q", schema, want)
			}
		}
	}
}

func TestFuzzSchemaBoundsTotalExpansion(t *testing.T) {
	schema := `{"type":"boolean"}`
	for range 8 {
		schema = `{"type":"array","minItems":16,"items":` + schema + `}`
	}
	if _, err := drawSchema(json.RawMessage(schema), 2); err == nil {
		t.Fatal("exponential sample must hit an explicit total expansion bound")
	}
}
