package wf

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

type artifactReferenceValue interface{ artifactReferenceValue() any }

// marshalPathValue retains encoding/json's field selection and representation,
// adding ownership only where the Go type or an explicit template declares it.
func (c *cycle) marshalPathValue(in *instance, value any) (json.RawMessage, error) {
	return c.marshalPathValueMode(in, value, true)
}

func (c *cycle) marshalPathValueMode(in *instance, value any, legacyTemplates bool) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var wire any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&wire); err != nil {
		return nil, err
	}
	if err = c.annotateReferencePaths(in, reflect.ValueOf(value), wire); err != nil {
		return nil, err
	}
	if !legacyTemplates {
		return json.Marshal(wire)
	}
	wire, err = transformPathJSON(wire, func(value any) (any, bool, error) {
		token, ok := value.(string)
		if !ok {
			return nil, false, nil
		}
		return c.scope(in).bindPathTemplate(token)
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(wire)
}

// encoding/json invokes pointer-receiver encoders for values only when the
// value is addressable. Map elements and top-level nonpointer values are not.
func pathValueOwnEncoding(value reflect.Value) bool {
	for _, encoder := range []reflect.Type{jsonMarshalerType, textMarshalerType} {
		if value.Type().Implements(encoder) || value.CanAddr() && value.Addr().Type().Implements(encoder) {
			return true
		}
	}
	return false
}

func (c *cycle) annotateReferencePaths(in *instance, goValue reflect.Value, wire any) error {
	if !goValue.IsValid() {
		return nil
	}
	for goValue.Kind() == reflect.Interface || goValue.Kind() == reflect.Pointer {
		if goValue.IsNil() {
			return nil
		}
		if pathValueOwnEncoding(goValue) {
			return nil
		}
		goValue = goValue.Elem()
	}
	if pathValueOwnEncoding(goValue) {
		return nil
	}
	if goValue.CanInterface() {
		if ref, ok := goValue.Interface().(artifactReferenceValue); ok && reflect.TypeOf(ref.artifactReferenceValue()) == goValue.Type() {
			object, ok := wire.(map[string]any)
			if !ok {
				return nil
			}
			path, _ := object["Path"].(string)
			if path == "" {
				return nil
			}
			// Ref.Path may itself be an explicitly typed template.
			if bound, matched, err := c.scope(in).bindPathTemplate(path); matched || err != nil {
				if err != nil {
					return err
				}
				object["Path"] = bound
				return nil
			}
			path, fragment, hasFragment := strings.Cut(path, "#")
			if !filepath.IsAbs(path) {
				if err := validatePathParam("artifact reference", path, WorkspaceRelative); err != nil {
					return err
				}
				path = filepath.Join(c.workdir(in), path)
			}
			path, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			owned, err := c.ownedPath(path)
			if err != nil {
				return err
			}
			if hasFragment {
				owned.Fragment = "#" + fragment
			}
			if err := owned.validate(); err != nil {
				return err
			}
			object["Path"] = owned.value()
			return nil
		}
	}
	switch goValue.Kind() {
	case reflect.Struct:
		object, ok := wire.(map[string]any)
		if !ok {
			return nil
		}
		for _, field := range jsonFields(goValue.Type()) {
			item, present := object[field.name]
			if !present {
				continue
			}
			// An anonymous Ref flattens its Path into the containing object.
			// Attribute that field to its actual owner, not promoted methods.
			if field.field.Name == "Path" && len(field.index) > 1 {
				owner, err := goValue.FieldByIndexErr(field.index[:len(field.index)-1])
				if err == nil {
					for owner.Kind() == reflect.Pointer && !owner.IsNil() {
						owner = owner.Elem()
					}
					if owner.CanInterface() {
						if ref, ok := owner.Interface().(artifactReferenceValue); ok && reflect.TypeOf(ref.artifactReferenceValue()) == owner.Type() {
							pathObject := map[string]any{"Path": item}
							if err := c.annotateReferencePaths(in, owner, pathObject); err != nil {
								return err
							}
							object[field.name] = pathObject["Path"]
							continue
						}
					}
				}
			}
			value, err := goValue.FieldByIndexErr(field.index)
			if err != nil {
				continue
			}
			if err := c.annotateReferencePaths(in, value, item); err != nil {
				return err
			}
		}
	case reflect.Array, reflect.Slice:
		array, ok := wire.([]any)
		if !ok {
			return nil
		}
		for i, item := range array {
			if err := c.annotateReferencePaths(in, goValue.Index(i), item); err != nil {
				return err
			}
		}
	case reflect.Map:
		object, ok := wire.(map[string]any)
		if !ok {
			return nil
		}
		iter := goValue.MapRange()
		for iter.Next() {
			key, err := pathJSONMapKey(iter.Key())
			if err != nil {
				return err
			}
			if item, present := object[key]; present {
				if err := c.annotateReferencePaths(in, iter.Value(), item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func pathJSONMapKey(key reflect.Value) (string, error) {
	if key.Kind() == reflect.String {
		return key.String(), nil
	}
	if key.CanInterface() {
		if marshaler, ok := key.Interface().(encoding.TextMarshaler); ok {
			data, err := marshaler.MarshalText()
			return string(data), err
		}
	}
	switch key.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(key.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(key.Uint(), 10), nil
	}
	return "", fmt.Errorf("unsupported JSON map key %s", key.Type())
}

// decodeOwnedPathValues resolves only the new owned-path representation. RunPath
// objects remain intact so existing typed Go fields can still decode them.
func (c *cycle) decodeOwnedPathValues(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var wire any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return nil, err
	}
	changed := false
	wire, err := transformPathJSON(wire, func(value any) (any, bool, error) {
		path, marked, err := markedOwnedPath(value)
		if !marked || err != nil {
			return nil, marked, err
		}
		resolved, err := c.resolveOwnedPath(path)
		changed = true
		return resolved, true, err
	})
	if err != nil {
		return nil, err
	}
	if !changed {
		return bytes.Clone(raw), nil // never the folded bytes themselves
	}
	return json.Marshal(wire)
}

func transformPathJSON(value any, transform func(any) (any, bool, error)) (any, error) {
	if transformed, matched, err := transform(value); matched || err != nil {
		return transformed, err
	}
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			transformed, err := transformPathJSON(item, transform)
			if err != nil {
				return nil, err
			}
			value[key] = transformed
		}
	case []any:
		for i, item := range value {
			transformed, err := transformPathJSON(item, transform)
			if err != nil {
				return nil, err
			}
			value[i] = transformed
		}
	}
	return value, nil
}
