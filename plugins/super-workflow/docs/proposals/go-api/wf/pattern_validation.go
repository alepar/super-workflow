package wf

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"sync"
	"unicode/utf8"
)

// A string field with a pattern:"..." tag is a validated type (reference
// workflow S3a): the engine checks it when an agent's result is decoded and
// when a compute returns its value, and refuses a value that does not match.
// The empty string is allowed (an unset optional field). A value must match
// the whole pattern: it is checked as ^(?:pattern)$.
//
// A maxlen:"N" tag caps a string field (a *string's string, each element of
// a []string) at N characters, counted as runes like JSON Schema's maxLength.
// It is shown to the agent as maxLength and checked in the same places as a
// pattern; a refusal names the field, its length and the limit, never the
// value. N is a positive decimal integer (ruleMaxlenTag lints it).

// maxlenOf parses a field's maxlen tag: ok false when there is none, an error
// when its value is not a positive decimal integer.
func maxlenOf(tag reflect.StructTag) (n int, ok bool, err error) {
	v, ok := tag.Lookup("maxlen")
	if !ok {
		return 0, false, nil
	}
	n, err = strconv.Atoi(v)
	if err != nil || n <= 0 || strconv.Itoa(n) != v {
		return 0, true, fmt.Errorf("maxlen %q is not a positive whole number", v)
	}
	return n, true, nil
}

// checkMaxlen refuses s when it is longer than the field's maxlen tag.
func checkMaxlen(tag reflect.StructTag, where, s string) error {
	limit, ok, err := maxlenOf(tag)
	if !ok {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: the definition's %v", where, err)
	}
	if n := utf8.RuneCountInString(s); n > limit {
		return &maxlenError{fmt.Sprintf("%s: %d characters, over the field's limit of %d", where, n, limit)}
	}
	return nil
}

// maxlenError is a value over its maxlen cap: its own refusal stage.
type maxlenError struct{ msg string }

func (e *maxlenError) Error() string { return e.msg }

var patternCache sync.Map // pattern -> *regexp.Regexp or error

func compiledPattern(p string) (*regexp.Regexp, error) {
	if v, ok := patternCache.Load(p); ok {
		if re, ok := v.(*regexp.Regexp); ok {
			return re, nil
		}
		return nil, v.(error)
	}
	// Always a full-string match: ^a|b$ anchors only its ends, so each
	// alternative is anchored by wrapping the whole.
	re, err := regexp.Compile(`^(?:` + p + `)$`)
	if err != nil {
		patternCache.Store(p, err)
		return nil, err
	}
	patternCache.Store(p, re)
	return re, nil
}

// validatePatterns checks every pattern-tagged string field reachable in v.
func validatePatterns(v reflect.Value, path string) error {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Struct:
		if ownEncoding(v.Type()) {
			return nil
		}
		for _, f := range jsonFields(v.Type()) {
			fv := v.FieldByIndex(f.index)
			p, tagged := f.field.Tag.Lookup("pattern")
			// A *string field's tag applies to the string it points at.
			for tagged && fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					break
				}
				fv = fv.Elem()
			}
			// A maxlen tag caps the string (each element of a list of strings).
			if _, capped := f.field.Tag.Lookup("maxlen"); capped {
				sv := fv
				for sv.Kind() == reflect.Pointer && !sv.IsNil() {
					sv = sv.Elem()
				}
				switch {
				case sv.Kind() == reflect.String:
					if err := checkMaxlen(f.field.Tag, path+"."+f.name, sv.String()); err != nil {
						return err
					}
				case (sv.Kind() == reflect.Slice || sv.Kind() == reflect.Array) && sv.Type().Elem().Kind() == reflect.String:
					for i := range sv.Len() {
						if err := checkMaxlen(f.field.Tag, fmt.Sprintf("%s.%s[%d]", path, f.name, i), sv.Index(i).String()); err != nil {
							return err
						}
					}
				}
			}
			// On a list of strings, the tag applies to each element.
			if tagged && (fv.Kind() == reflect.Slice || fv.Kind() == reflect.Array) && fv.Type().Elem().Kind() == reflect.String {
				re, err := compiledPattern(p)
				if err != nil {
					return fmt.Errorf("%s.%s: the definition's pattern %q does not compile: %v", path, f.name, p, err)
				}
				for i := range fv.Len() {
					if s := fv.Index(i).String(); s != "" && !re.MatchString(s) {
						return fmt.Errorf("%s.%s[%d]: %q does not match the pattern %s", path, f.name, i, s, p)
					}
				}
				continue
			}
			if tagged && fv.Kind() == reflect.String {
				if s := fv.String(); s != "" {
					re, err := compiledPattern(p)
					if err != nil {
						return fmt.Errorf("%s.%s: the definition's pattern %q does not compile: %v", path, f.name, p, err)
					}
					if !re.MatchString(s) {
						return fmt.Errorf("%s.%s: %q does not match the pattern %s", path, f.name, s, p)
					}
				}
				continue
			}
			if err := validatePatterns(fv, path+"."+f.name); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := range v.Len() {
			if err := validatePatterns(v.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if err := validatePatterns(iter.Value(), fmt.Sprintf("%s[%v]", path, iter.Key())); err != nil {
				return err
			}
		}
	}
	return nil
}
