package wf

import (
	"reflect"
	"strings"
	"unicode"
)

// jsonField is a field encoding/json can actually decode: embedded-field
// dominance is resolved before schema, lint or enum validation sees it.
type jsonField struct {
	name     string
	index    []int
	field    reflect.StructField
	optional bool
	tagged   bool
}

func jsonFields(t reflect.Type) []jsonField {
	var fields []jsonField
	var collect func(reflect.Type, []int, map[reflect.Type]bool)
	collect = func(t reflect.Type, index []int, active map[reflect.Type]bool) {
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
			if !f.IsExported() && (!f.Anonymous || ft.Kind() != reflect.Struct) {
				continue
			}
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			if !validJSONFieldName(name) {
				name = ""
			}
			path := append(append([]int{}, index...), i)
			if f.Anonymous && name == "" && ft.Kind() == reflect.Struct {
				collect(ft, path, active)
				continue
			}
			tagged := name != ""
			if name == "" {
				name = f.Name
			}
			fields = append(fields, jsonField{name: name, index: path, field: f, tagged: tagged, optional: strings.Contains(","+opts+",", ",omitempty,") || strings.Contains(","+opts+",", ",omitzero,")})
		}
	}
	collect(t, nil, map[reflect.Type]bool{})
	// At the shallowest depth, exactly one tagged field wins; otherwise there
	// must be exactly one field. Ties are ignored, as encoding/json specifies.
	byName := map[string][]int{}
	for i, f := range fields {
		byName[f.name] = append(byName[f.name], i)
	}
	selected := map[int]bool{}
	for _, indexes := range byName {
		depth := len(fields[indexes[0]].index)
		for _, i := range indexes {
			if n := len(fields[i].index); n < depth {
				depth = n
			}
		}
		var nearest, tagged []int
		for _, i := range indexes {
			if len(fields[i].index) == depth {
				nearest = append(nearest, i)
				if fields[i].tagged {
					tagged = append(tagged, i)
				}
			}
		}
		switch {
		case len(tagged) == 1:
			selected[tagged[0]] = true
		case len(tagged) == 0 && len(nearest) == 1:
			selected[nearest[0]] = true
		}
	}
	var out []jsonField
	for i, f := range fields {
		if selected[i] {
			out = append(out, f)
		}
	}
	return out
}

func validJSONFieldName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", r) && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
