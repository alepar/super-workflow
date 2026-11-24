package wf

import (
	"encoding/json"
	"reflect"
	"testing"
)

type decodeEnum string

func TestEnumValidationIncludesDecodedMapKeys(t *testing.T) {
	d := New("map-enum")
	NewEnum(d, "value", decodeEnum("yes"), decodeEnum("no"))
	out := map[decodeEnum]string{"maybe": "value"}
	if err := validateEnums(reflect.ValueOf(out), d.enums, "output"); err == nil {
		t.Fatal("unknown registered enum map key was accepted")
	}
}

func TestEnumValidationNestedValuesAndPlainStrings(t *testing.T) {
	d := New("nested-enum")
	NewEnum(d, "value", decodeEnum("yes"), decodeEnum("no"))
	NewEnum(d, "plain", "not-a-global-constraint")
	for _, tc := range []struct {
		name  string
		value any
		bad   bool
	}{
		{"valid", struct{ Verdict decodeEnum }{"yes"}, false},
		{"plain", struct{ Text string }{"anything"}, false},
		{"slice", []decodeEnum{"yes", "maybe"}, true},
		{"map-value", map[string]decodeEnum{"key": "maybe"}, true},
		{"pointer", func() *decodeEnum { v := decodeEnum("maybe"); return &v }(), true},
		{"ignored", struct {
			Ignored decodeEnum `json:"-"`
		}{"maybe"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateEnums(reflect.ValueOf(tc.value), d.enums, "output"); (err != nil) != tc.bad {
				t.Fatalf("error=%v wantBad=%v", err, tc.bad)
			}
		})
	}
}

type embeddedEnumFields struct{ Verdict decodeEnum }
type embeddedEnumResult struct{ embeddedEnumFields }

func TestEnumValidationIncludesPromotedFields(t *testing.T) {
	d := New("embedded")
	NewEnum(d, "value", decodeEnum("yes"), decodeEnum("no"))
	var out embeddedEnumResult
	if err := json.Unmarshal([]byte(`{"Verdict":"maybe"}`), &out); err != nil {
		t.Fatal(err)
	}
	if !hasEnum(reflect.TypeFor[embeddedEnumResult](), d.enums, map[reflect.Type]bool{}) {
		t.Fatal("promoted enum field escapes lint")
	}
	if err := validateEnums(reflect.ValueOf(out), d.enums, "output"); err == nil {
		t.Fatal("decoded unknown promoted enum value was accepted")
	}
}

type taggedEmbeddedEnumResult struct {
	embeddedEnumFields `json:"nested"`
}

func TestEnumValidationIncludesTaggedAnonymousStruct(t *testing.T) {
	d := New("tagged")
	NewEnum(d, "value", decodeEnum("yes"), decodeEnum("no"))
	var out taggedEmbeddedEnumResult
	if err := json.Unmarshal([]byte(`{"nested":{"Verdict":"maybe"}}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Verdict != "maybe" {
		t.Fatal("fixture did not decode the embedded field")
	}
	if !hasEnum(reflect.TypeFor[taggedEmbeddedEnumResult](), d.enums, map[reflect.Type]bool{}) {
		t.Fatal("tagged anonymous enum field escapes lint")
	}
	if err := validateEnums(reflect.ValueOf(out), d.enums, "output"); err == nil {
		t.Fatal("decoded unknown tagged enum value was accepted")
	}
}

type shadowedEnumResult struct {
	embeddedEnumFields
	Verdict string
}

func TestEnumValidationIgnoresShadowedEmbeddedFields(t *testing.T) {
	d := New("shadowed")
	NewEnum(d, "value", decodeEnum("yes"), decodeEnum("no"))
	var out shadowedEnumResult
	if err := json.Unmarshal([]byte(`{"Verdict":"ordinary string"}`), &out); err != nil {
		t.Fatal(err)
	}
	if hasEnum(reflect.TypeFor[shadowedEnumResult](), d.enums, map[reflect.Type]bool{}) {
		t.Fatal("hidden enum field affected lint")
	}
	if err := validateEnums(reflect.ValueOf(out), d.enums, "output"); err != nil {
		t.Fatalf("valid decoded JSON was refused: %v", err)
	}
}

type optionalEnumResult struct {
	Verdict decodeEnum `json:"verdict,omitempty"`
}

func TestEnumDecodeDistinguishesMissingOptionalFromInvalidValue(t *testing.T) {
	d := New("optional")
	NewEnum(d, "value", decodeEnum("yes"), decodeEnum("no"))
	for _, tc := range []struct {
		raw string
		bad bool
	}{{`{}`, false}, {`{"verdict":null}`, true}, {`{"verdict":""}`, true}, {`{"verdict":"maybe"}`, true}, {`{"verdict":"yes"}`, false}} {
		t.Run(tc.raw, func(t *testing.T) {
			var out optionalEnumResult
			if err := json.Unmarshal([]byte(tc.raw), &out); err != nil {
				t.Fatal(err)
			}
			err := validateEnums(reflect.ValueOf(out), d.enums, "output", json.RawMessage(tc.raw))
			if (err != nil) != tc.bad {
				t.Fatalf("error=%v wantBad=%v", err, tc.bad)
			}
		})
	}
}

type caseSplitEnumResult struct {
	Verdict decodeEnum `json:"Verdict,omitempty"`
	Lower   string     `json:"verdict"`
}

func TestEnumDecodePresenceRespectsExactJSONFieldMatch(t *testing.T) {
	d := New("case-split")
	NewEnum(d, "value", decodeEnum("yes"), decodeEnum("no"))
	raw := json.RawMessage(`{"verdict":"ordinary"}`)
	var out caseSplitEnumResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if err := validateEnums(reflect.ValueOf(out), d.enums, "output", raw); err != nil {
		t.Fatalf("lowercase field is not the omitted enum: %v", err)
	}
}

func TestRegisteredComparableStructInAnonymousFieldDoesNotPanic(t *testing.T) {
	d := New("struct-enum")
	NewEnum(d, "object", embeddedEnumFields{Verdict: "yes"}, embeddedEnumFields{Verdict: "no"})
	for _, tc := range []struct {
		raw string
		bad bool
	}{{`{"nested":{"Verdict":"yes"}}`, false}, {`{"nested":{"Verdict":"maybe"}}`, true}} {
		t.Run(tc.raw, func(t *testing.T) {
			var out taggedEmbeddedEnumResult
			if err := json.Unmarshal([]byte(tc.raw), &out); err != nil {
				t.Fatal(err)
			}
			err := validateEnums(reflect.ValueOf(out), d.enums, "output", json.RawMessage(tc.raw))
			if (err != nil) != tc.bad {
				t.Fatalf("err=%v wantBad=%v", err, tc.bad)
			}
		})
	}
}

func TestEnumJSONRejectsEveryExplicitInvalidValue(t *testing.T) {
	type nested struct {
		Verdict decodeEnum `json:"verdict,omitempty"`
	}
	type result struct {
		Nested  nested            `json:"nested"`
		Pointer *nested           `json:"pointer"`
		Values  map[string]nested `json:"values"`
	}
	d := New("duplicates")
	NewEnum(d, "value", decodeEnum("yes"), decodeEnum("no"))
	for _, tc := range []struct {
		raw string
		bad bool
	}{
		{`{"nested":{"verdict":""},"nested":{}}`, true},
		{`{"nested":{"verdict":""},"NESTED":{}}`, true},
		{`{"nested":{"verdict":""},"nested":null}`, true},
		{`{"pointer":{"verdict":""},"pointer":{},"pointer":null,"pointer":{}}`, true},
		{`{"values":{"x":{"verdict":""}},"values":{"x":{}}}`, true},
		{`{"values":{"x":{"verdict":""}},"values":{"y":{}}}`, true},
		{`{"values":{"x":{"verdict":""}},"values":null,"values":{"y":{}}}`, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			var out result
			raw := json.RawMessage(tc.raw)
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			err := validateEnums(reflect.ValueOf(out), d.enums, "output", raw)
			if (err != nil) != tc.bad {
				t.Fatalf("error=%v wantBad=%v", err, tc.bad)
			}
		})
	}
}

func TestEnumPresenceUsesDecodedMapKeys(t *testing.T) {
	type item struct {
		Verdict decodeEnum `json:"verdict,omitempty"`
	}
	d := New("numeric-keys")
	NewEnum(d, "value", decodeEnum("yes"), decodeEnum("no"))
	for _, tc := range []struct {
		raw string
		bad bool
	}{
		{`{"01":{"verdict":""}}`, true},
		{`{"01":{"verdict":""},"1":{}}`, true},
		{`{"1":{},"01":{"verdict":""}}`, true},
		{`{"01":{"verdict":"yes"}}`, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			var out map[int]item
			raw := json.RawMessage(tc.raw)
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			err := validateEnums(reflect.ValueOf(out), d.enums, "output", raw)
			if (err != nil) != tc.bad {
				t.Fatalf("error=%v wantBad=%v", err, tc.bad)
			}
		})
	}
}

func TestEnumJSONRejectsRetainedArrayAndSliceValues(t *testing.T) {
	type item struct {
		Verdict decodeEnum `json:"verdict,omitempty"`
	}
	d := New("retained")
	NewEnum(d, "value", decodeEnum("yes"), decodeEnum("no"))
	for _, raw := range []json.RawMessage{json.RawMessage(`{"items":[{"verdict":""}],"items":null}`), json.RawMessage(`{"items":[{"verdict":""}],"items":[{}]}`)} {
		var array struct {
			Items [1]item `json:"items"`
		}
		var slice struct {
			Items []item `json:"items"`
		}
		for _, out := range []any{&array, &slice} {
			if err := json.Unmarshal(raw, out); err != nil {
				t.Fatal(err)
			}
			if err := validateEnums(reflect.ValueOf(out).Elem(), d.enums, "output", raw); err == nil {
				t.Fatalf("invalid explicit value accepted: %s in %T", raw, out)
			}
		}
	}
}

func TestEnumValidationAllowsNilInterfaceOutput(t *testing.T) {
	if err := validateEnums(reflect.ValueOf(nil), nil, "output", json.RawMessage(`null`)); err != nil {
		t.Fatal(err)
	}
}

func TestOptionalContainerStillChecksRequiredEnumWhenPresent(t *testing.T) {
	type child struct{ Verdict decodeEnum }
	type result struct {
		Child child `json:"child,omitempty"`
	}
	d := New("optional-container")
	NewEnum(d, "value", decodeEnum("yes"), decodeEnum("no"))
	for _, tc := range []struct {
		raw string
		bad bool
	}{{`{}`, false}, {`{"child":{}}`, true}, {`{"child":{"Verdict":"yes"}}`, false}} {
		var out result
		raw := json.RawMessage(tc.raw)
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		err := validateEnums(reflect.ValueOf(out), d.enums, "output", raw)
		if (err != nil) != tc.bad {
			t.Fatalf("%s: error=%v wantBad=%v", raw, err, tc.bad)
		}
	}
}

func TestEnumJSONHonorsStringFieldEncoding(t *testing.T) {
	type result struct {
		Verdict decodeEnum `json:"verdict,string"`
	}
	d := New("quoted-enum")
	NewEnum(d, "value", decodeEnum("yes"), decodeEnum("no"))
	for _, tc := range []struct {
		raw string
		bad bool
	}{{`{"verdict":"\"yes\""}`, false}, {`{"verdict":"\"maybe\""}`, true}} {
		var out result
		raw := json.RawMessage(tc.raw)
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		err := validateEnums(reflect.ValueOf(out), d.enums, "output", raw)
		if (err != nil) != tc.bad {
			t.Fatalf("%s: error=%v wantBad=%v", raw, err, tc.bad)
		}
	}
}

type decodeByteEnum uint8

func TestEnumJSONUsesByteSliceWireEncoding(t *testing.T) {
	d := New("bytes")
	NewEnum(d, "byte", decodeByteEnum(1), decodeByteEnum(2))
	type result struct{ Bytes []decodeByteEnum }
	for _, tc := range []struct {
		raw string
		bad bool
	}{{`{"Bytes":"AQI="}`, false}, {`{"Bytes":"AQM="}`, true}, {`{"Bytes":[1,2]}`, false}, {`{"Bytes":[1,3]}`, true}, {`{"Bytes":"AQM=","Bytes":"AQI="}`, true}} {
		var out result
		raw := json.RawMessage(tc.raw)
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		err := validateEnums(reflect.ValueOf(out), d.enums, "output", raw)
		if (err != nil) != tc.bad {
			t.Fatalf("%s: error=%v wantBad=%v", raw, err, tc.bad)
		}
	}
}

type promotedStructEnum struct{ Verdict string }
type promotedStructEnumOutput struct{ promotedStructEnum }

func TestPromotedRegisteredStructEnumIsValidated(t *testing.T) {
	d := New("promoted-enum")
	NewEnum(d, "verdict", promotedStructEnum{"yes"}, promotedStructEnum{"no"})
	if !hasEnum(reflect.TypeFor[promotedStructEnumOutput](), d.enums, map[reflect.Type]bool{}) {
		t.Error("embedded enum hidden from lint")
	}
	for _, tc := range []struct {
		raw string
		bad bool
	}{{`{"Verdict":"yes"}`, false}, {`{"Verdict":"maybe"}`, true}} {
		var out promotedStructEnumOutput
		raw := json.RawMessage(tc.raw)
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		err := validateEnums(reflect.ValueOf(out), d.enums, "output", raw)
		if (err != nil) != tc.bad {
			t.Fatalf("%s: error=%v wantBad=%v", raw, err, tc.bad)
		}
	}
}

func TestFullyShadowedRegisteredStructEnumDoesNotConstrainOutput(t *testing.T) {
	type result struct {
		promotedStructEnum
		Verdict string
	}
	d := New("shadowed-struct-enum")
	NewEnum(d, "verdict", promotedStructEnum{"yes"}, promotedStructEnum{"no"})
	if hasEnum(reflect.TypeFor[result](), d.enums, map[reflect.Type]bool{}) {
		t.Fatal("hidden embedding requires an enum check")
	}
	raw := json.RawMessage(`{"Verdict":"ordinary"}`)
	var out result
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if err := validateEnums(reflect.ValueOf(out), d.enums, "output", raw); err != nil {
		t.Fatalf("shadowed embedding refused: %v", err)
	}
}

type customEnumList []decodeEnum

func (v *customEnumList) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	*v = customEnumList{decodeEnum(text)}
	return nil
}
func (v customEnumList) MarshalJSON() ([]byte, error) { return json.Marshal(string(v[0])) }
func TestEnumJSONRespectsCustomContainerCodec(t *testing.T) {
	d := New("custom-codec")
	NewEnum(d, "value", decodeEnum("yes"), decodeEnum("no"))
	type result struct{ Values customEnumList }
	for _, tc := range []struct {
		raw string
		bad bool
	}{{`{"Values":"yes"}`, false}, {`{"Values":"maybe"}`, true}, {`{"Values":"maybe","Values":"yes"}`, true}} {
		var out result
		raw := json.RawMessage(tc.raw)
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		err := validateEnums(reflect.ValueOf(out), d.enums, "output", raw)
		if (err != nil) != tc.bad {
			t.Fatalf("%s: error=%v wantBad=%v", raw, err, tc.bad)
		}
	}
}
