package wf

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
)

// Output schema: the JSON schema of an agent's output type, derived by
// reflection, and the Submission block the Engine appends to an agent
// instruction.
//
// Owns: the schema text an agent follows (object, properties, required, the
// JSON primitive types, arrays, maps, enums of registered Enum-typed fields,
// json tags, omitempty and the pattern tag of string fields) and the block's
// wording.
// Does not: validate submissions (Submit decodes into O and runs the checks;
// a pattern tag is shown to the agent here and checked by validateAgentTags),
// share a layer with fsx's schema-subset validator (a different direction), or
// choose the submit command line (the caller passes it verbatim).
// Extend by: a new supported Go type shape is one case in schemaWalk.shape.

const submissionFormat = `## Submission

Work in %[1]s. Use absolute paths under it for every file you read, edit or test.

When the work is done, write your result as JSON to a file outside %[1]s and outside any other git worktree, for example in a directory made by mktemp -d: a result file left in a worktree stops its removal. It must match this JSON schema:

%[2]s

Then run this exact command, with <file> replaced by that file's absolute path:

%[3]s
`

// submissionBlock renders the Submission block for output type t. enums are the
// definition's registered Enums; submit is the exact command line, with its
// <file> placeholder.
func submissionBlock(t reflect.Type, enums []*enumData, workspace, submit string, output ...string) string {
	if len(output) != 0 && output[0] != "" {
		return fmt.Sprintf("## Submission\n\nWork in %s. Use absolute paths under it for every file you read, edit or test.\n\nWrite your result as JSON to this allocated output file: %s\nIt must match this JSON schema:\n\n%s\n\nThen run this exact command:\n\n%s\n", workspace, output[0], outputSchema(t, enums), submit)
	}
	return fmt.Sprintf(submissionFormat, workspace, outputSchema(t, enums), submit)
}

// outputSchema returns the indented JSON schema of t. Keys are sorted, so the
// text is deterministic for a fixed type and Enum set.
func outputSchema(t reflect.Type, enums []*enumData) string {
	w := &schemaWalk{enums: enums, active: map[reflect.Type]bool{}}
	b, err := json.MarshalIndent(w.of(t), "", "  ")
	if err != nil {
		panic("wf: marshalling an output schema: " + err.Error()) // maps of strings and Enum values
	}
	return string(b)
}

type schemaWalk struct {
	enums  []*enumData
	active map[reflect.Type]bool // structs being walked, to stop on recursion
}

func (w *schemaWalk) of(t reflect.Type) map[string]any {
	s := w.shape(t)
	if vals := w.enumValues(t); vals != nil {
		s["enum"] = vals
	}
	return s
}

// enumValues returns the registered Enum values of t. Only a named type can
// identify an Enum: NewEnum over plain strings must not constrain every string.
func (w *schemaWalk) enumValues(t reflect.Type) []any {
	if t.PkgPath() == "" {
		return nil
	}
	for _, e := range w.enums {
		vals := reflect.ValueOf(e.vals)
		if vals.Kind() != reflect.Slice || vals.Type().Elem() != t {
			continue
		}
		out := make([]any, vals.Len())
		for i := range out {
			out[i] = vals.Index(i).Interface()
		}
		return out
	}
	return nil
}

var (
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// ownEncoding reports a type whose Go shape says nothing about its wire shape.
func ownEncoding(t reflect.Type) bool {
	for _, x := range []reflect.Type{t, reflect.PointerTo(t)} {
		if x.Implements(jsonMarshalerType) || x.Implements(textMarshalerType) {
			return true
		}
	}
	return false
}

func (w *schemaWalk) shape(t reflect.Type) map[string]any {
	if t == reflect.TypeFor[RunPath]() {
		return map[string]any{"type": "object", "properties": map[string]any{runPathMarker: map[string]any{"type": "string", "minLength": 1, "description": "Clean relative path inside RunDir; no absolute path or traversal."}}, "required": []string{runPathMarker}, "additionalProperties": false}
	}
	if ownEncoding(t) {
		return map[string]any{}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return w.of(t.Elem())
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string"} // encoding/json: base64
		}
		return map[string]any{"type": "array", "items": w.of(t.Elem())}
	case reflect.Array:
		return map[string]any{"type": "array", "items": w.of(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": w.of(t.Elem())}
	case reflect.Struct:
		return w.object(t)
	}
	return map[string]any{} // interface: any JSON value
}

func (w *schemaWalk) object(t reflect.Type) map[string]any {
	if w.active[t] {
		return map[string]any{"type": "object"}
	}
	w.active[t] = true
	defer delete(w.active, t)
	props := map[string]any{}
	var required []string
	w.fields(t, props, &required)
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// fields follows encoding/json: "-" skips a field, an untagged embedded struct
// flattens, unexported fields are skipped, and omitempty or omitzero means not
// required. A pattern:"..." tag on a string field becomes its "pattern", and
// a maxlen:"N" tag its "maxLength" (on a list of strings, its items').
func (w *schemaWalk) fields(t reflect.Type, props map[string]any, required *[]string) {
	for _, f := range jsonFields(t) {
		fs := w.of(f.field.Type)
		if p, ok := f.field.Tag.Lookup("pattern"); ok && fs["type"] == "string" {
			fs["pattern"] = p
		}
		if n, ok, err := maxlenOf(f.field.Tag); ok && err == nil {
			if fs["type"] == "string" {
				fs["maxLength"] = n
			} else if items, _ := fs["items"].(map[string]any); fs["type"] == "array" && items["type"] == "string" {
				items["maxLength"] = n
			}
		}
		props[f.name] = fs
		if !f.optional {
			*required = append(*required, f.name)
		}
	}
}
