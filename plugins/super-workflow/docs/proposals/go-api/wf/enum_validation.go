package wf

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

func (o agentOps[S, O]) outputType() reflect.Type { return reflect.TypeFor[O]() }

// enumFor constrains named registered types only, as outputSchema does. An enum
// over plain strings must not constrain unrelated string fields.
func enumFor(t reflect.Type, enums []*enumData) *enumData {
	if t.PkgPath() == "" {
		return nil
	}
	for _, e := range enums {
		if reflect.TypeOf(e.vals).Elem() == t {
			return e
		}
	}
	return nil
}

func hasEnum(t reflect.Type, enums []*enumData, seen map[reflect.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	if enumFor(t, enums) != nil {
		return true
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return hasEnum(t.Elem(), enums, seen)
	case reflect.Map:
		return hasEnum(t.Key(), enums, seen) || hasEnum(t.Elem(), enums, seen)
	case reflect.Struct:
		if len(embeddedEnums(t, enums)) > 0 {
			return true
		}
		for _, f := range jsonFields(t) {
			if hasEnum(f.field.Type, enums, seen) {
				return true
			}
		}
	}
	return false
}

// validateEnums checks every explicit enum value, including values in repeated
// JSON members, then checks the decoded result. An overwritten invalid value
// is still a refusal; omitted optional zero values remain allowed.
func validateEnums(v reflect.Value, enums []*enumData, path string, raw ...json.RawMessage) error {
	if !v.IsValid() {
		return nil
	}
	for _, doc := range raw {
		if err := validateEnumJSON(v.Type(), enums, path, doc); err != nil {
			return err
		}
	}
	return validateEnumValue(v, enums, path, raw, len(raw) > 0)
}

func validateEnumValue(v reflect.Value, enums []*enumData, path string, docs []json.RawMessage, known bool) error {
	if !v.IsValid() {
		return nil
	}
	if e := enumFor(v.Type(), enums); e != nil {
		vals := reflect.ValueOf(e.vals)
		for i := 0; i < vals.Len(); i++ {
			if v.Comparable() && vals.Index(i).Comparable() && v.Equal(vals.Index(i)) {
				return nil
			}
		}
		return fmt.Errorf("%s: value %v is outside enum %s (%s)", path, v, e.name, strings.Join(e.values, ", "))
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return validateEnumValue(v.Elem(), enums, path, docs, known)
		}
	case reflect.Struct:
		for _, f := range embeddedEnums(v.Type(), enums) {
			value, err := v.FieldByIndexErr(f.index)
			if err != nil {
				continue
			}
			if err := validateEnumValue(value, enums, path+"."+f.field.Name, nil, false); err != nil {
				return err
			}
		}
		fields := jsonFields(v.Type())
		values := jsonFieldValues(fields, docs)
		for _, f := range fields {
			value, err := v.FieldByIndexErr(f.index)
			if err != nil {
				continue
			} // nil anonymous pointer: no promoted values
			child, present := values[f.name]
			if known && !present && f.optional && value.IsZero() {
				continue
			}
			if err := validateEnumValue(value, enums, path+"."+f.name, child, known); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var items []json.RawMessage
		if len(docs) > 0 {
			_ = json.Unmarshal(docs[len(docs)-1], &items)
		}
		for i := 0; i < v.Len(); i++ {
			var child []json.RawMessage
			if i < len(items) {
				child = []json.RawMessage{items[i]}
			}
			if err := validateEnumValue(v.Index(i), enums, fmt.Sprintf("%s[%d]", path, i), child, known); err != nil {
				return err
			}
		}
	case reflect.Map:
		object := reflect.New(reflect.MapOf(v.Type().Key(), reflect.TypeFor[json.RawMessage]()))
		for _, doc := range docs {
			if err := json.Unmarshal(doc, object.Interface()); err != nil {
				return err
			}
		}
		iter := v.MapRange()
		for iter.Next() {
			key := iter.Key()
			if err := validateEnumValue(key, enums, fmt.Sprintf("%s[key %v]", path, key), nil, false); err != nil {
				return err
			}

			var child []json.RawMessage
			if raw := object.Elem().MapIndex(key); raw.IsValid() {
				child = []json.RawMessage{raw.Interface().(json.RawMessage)}
			}
			if err := validateEnumValue(iter.Value(), enums, fmt.Sprintf("%s[%v]", path, key), child, known); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o computeOps[S, O]) outputType() reflect.Type   { return reflect.TypeFor[O]() }
func (o subOps[S, I, R]) outputType() reflect.Type    { return reflect.TypeFor[R]() }
func (o mapOps[S, I, K, R]) outputType() reflect.Type { return reflect.TypeFor[MapResult[K, R]]() }

func (o commandOps[S]) outputType() reflect.Type { return reflect.TypeFor[RunResult]() }

// JSON promotion must not discard a registered enum on the enclosing embedded
// struct. Only embeddings with a visible promoted field constrain this output.
func embeddedEnums(t reflect.Type, enums []*enumData) []jsonField {
	visible := jsonFields(t)
	var out []jsonField
	active := map[reflect.Type]bool{}
	var visit func(reflect.Type, []int)
	visit = func(t reflect.Type, index []int) {
		if active[t] {
			return
		}
		active[t] = true
		defer delete(active, t)
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !f.Anonymous || ft.Kind() != reflect.Struct || name == "-" || validJSONFieldName(name) {
				continue
			}
			path := append(append([]int{}, index...), i)
			if enumFor(ft, enums) != nil {
				for _, field := range visible {
					if indexPrefix(field.index, path) {
						out = append(out, jsonField{index: path, field: f})
						break
					}
				}
			} else {
				visit(ft, path)
			}
		}
	}
	visit(t, nil)
	return out
}
func indexPrefix(index, prefix []int) bool {
	if len(index) < len(prefix) {
		return false
	}
	for i, n := range prefix {
		if index[i] != n {
			return false
		}
	}
	return true
}
