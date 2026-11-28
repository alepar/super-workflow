package wf

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

type jsonMember struct {
	name  string
	value json.RawMessage
}

// Keep object members in input order: encoding/json merges repeated struct
// fields, while decoding an object into map[string]any loses earlier members.
func jsonMembers(raw json.RawMessage) []jsonMember {
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil
	}
	var members []jsonMember
	for dec.More() {
		token, err = dec.Token()
		if err != nil {
			return nil
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return nil
		}
		members = append(members, jsonMember{token.(string), value})
	}
	return members
}

// validateEnumJSON visits each supplied value rather than collapsing object
// members into a map. This keeps validation independent of decoder reuse of
// structs, slice capacity and fixed arrays.
func validateEnumJSON(t reflect.Type, enums []*enumData, path string, raw json.RawMessage) error {
	if !hasEnum(t, enums, map[reflect.Type]bool{}) {
		return nil
	}
	if enumFor(t, enums) != nil {
		value := reflect.New(t)
		if err := json.Unmarshal(raw, value.Interface()); err != nil {
			return err
		}
		return validateEnumValue(value.Elem(), enums, path, nil, false)
	}
	// A custom codec owns its wire shape. Apply it for each explicit
	// occurrence and enforce enum constraints on the value it returns.
	if ownDecoding(t) {
		value := reflect.New(t)
		if err := json.Unmarshal(raw, value.Interface()); err != nil {
			return err
		}
		return validateEnumValue(value.Elem(), enums, path, nil, false)
	}
	if t.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil
		}
		return validateEnumJSON(t.Elem(), enums, path, raw)
	}
	switch t.Kind() {
	case reflect.Struct:
		fields := jsonFields(t)
		for _, embedded := range embeddedEnums(t, enums) {
			var members []jsonMember
			for _, member := range jsonMembers(raw) {
				if i := jsonMatchingField(fields, member.name); i >= 0 && indexPrefix(fields[i].index, embedded.index) {
					members = append(members, member)
				}
			}
			if len(members) == 0 {
				continue
			}
			var object bytes.Buffer
			object.WriteByte('{')
			for i, member := range members {
				if i > 0 {
					object.WriteByte(',')
				}
				name, _ := json.Marshal(member.name)
				object.Write(name)
				object.WriteByte(':')
				object.Write(member.value)
			}
			object.WriteByte('}')
			if err := validateEnumJSON(embedded.field.Type, enums, path+"."+embedded.field.Name, object.Bytes()); err != nil {
				return err
			}
		}
		for _, member := range jsonMembers(raw) {
			match := jsonMatchingField(fields, member.name)
			if match >= 0 {
				f := fields[match]
				value := member.value
				_, options, _ := strings.Cut(f.field.Tag.Get("json"), ",")
				fieldType := f.field.Type
				for fieldType.Kind() == reflect.Pointer {
					fieldType = fieldType.Elem()
				}
				if strings.Contains(","+options+",", ",string,") && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					switch fieldType.Kind() {
					case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Float32, reflect.Float64, reflect.String:
						var quoted string
						if err := json.Unmarshal(value, &quoted); err != nil {
							return err
						}
						value = json.RawMessage(quoted)
					}
				}
				if err := validateEnumJSON(f.field.Type, enums, path+"."+f.name, value); err != nil {
					return err
				}
			}
		}
	case reflect.Array, reflect.Slice:
		// encoding/json represents byte slices as base64 strings as well as
		// accepting arrays. Decode with the declared type in either form.
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			value := reflect.New(t)
			if err := json.Unmarshal(raw, value.Interface()); err != nil {
				return err
			}
			return validateEnumValue(value.Elem(), enums, path, nil, false)
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		for i, item := range items {
			if t.Kind() == reflect.Array && i >= t.Len() {
				break
			}
			if err := validateEnumJSON(t.Elem(), enums, fmt.Sprintf("%s[%d]", path, i), item); err != nil {
				return err
			}
		}
	case reflect.Map:
		for _, member := range jsonMembers(raw) {
			keyJSON, _ := json.Marshal(member.name)
			// Let encoding/json apply numeric and TextUnmarshaler key semantics.
			object := reflect.New(reflect.MapOf(t.Key(), reflect.TypeFor[json.RawMessage]()))
			if err := json.Unmarshal(append(append([]byte("{"), keyJSON...), []byte(":null}")...), object.Interface()); err != nil {
				return err
			}
			for _, key := range object.Elem().MapKeys() {
				if err := validateEnumValue(key, enums, path+"[key "+member.name+"]", nil, false); err != nil {
					return err
				}
			}
			if err := validateEnumJSON(t.Elem(), enums, path+"["+member.name+"]", member.value); err != nil {
				return err
			}
		}
	}
	return nil
}

func jsonMatchingField(fields []jsonField, name string) int {
	for i, f := range fields {
		if f.name == name {
			return i
		}
	}
	for i, f := range fields {
		if strings.EqualFold(f.name, name) {
			return i
		}
	}
	return -1
}

// Presence here only distinguishes omitted optional containers/fields. Every
// explicit enum has already been checked, so collapsed history cannot hide an
// invalid value in the decoded result.
func jsonFieldValues(fields []jsonField, docs []json.RawMessage) map[string][]json.RawMessage {
	out := map[string][]json.RawMessage{}
	for _, doc := range docs {
		for _, member := range jsonMembers(doc) {
			if i := jsonMatchingField(fields, member.name); i >= 0 {
				out[fields[i].name] = []json.RawMessage{member.value}
			}
		}
	}
	return out
}

func ownDecoding(t reflect.Type) bool {
	for _, candidate := range []reflect.Type{t, reflect.PointerTo(t)} {
		if candidate.Implements(reflect.TypeFor[json.Unmarshaler]()) || candidate.Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) {
			return true
		}
	}
	return false
}
