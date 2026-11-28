package wftest

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"unicode/utf8"
)

// drawSchema samples the schema subset emitted by wf's output-schema walker.
// Variants select enum alternatives and empty, singleton, or bounded-size values.
// Unsupported constraints return errors rather than apparently valid samples.
func drawSchema(schema json.RawMessage, variant int) (json.RawMessage, error) {
	remaining := 4096
	value, err := drawSchemaValue(schema, variant, 0, &remaining)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

type fuzzSchema struct {
	Type       string                     `json:"type"`
	Enum       []json.RawMessage          `json:"enum"`
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
	Additional json.RawMessage            `json:"additionalProperties"`
	Items      json.RawMessage            `json:"items"`
	Pattern    *string                    `json:"pattern"`
	MinItems   *int                       `json:"minItems"`
	MaxItems   *int                       `json:"maxItems"`
	MinLength  *int                       `json:"minLength"`
	MaxLength  *int                       `json:"maxLength"`
	Minimum    *float64                   `json:"minimum"`
	Maximum    *float64                   `json:"maximum"`
}

func fuzzChoice(v, n int) int {
	v %= n
	if v < 0 {
		v += n
	}
	return v
}

// errExpansionBound is the refusal of a sample that would exceed the
// sampler's total node budget.
var errExpansionBound = errors.New("sample exceeds total expansion bound")

func drawSchemaValue(raw json.RawMessage, v, depth int, remaining *int) (any, error) {
	*remaining--
	if *remaining < 0 {
		return nil, errExpansionBound
	}
	if depth > 64 {
		return nil, fmt.Errorf("schema nesting exceeds sampler bound")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil || keys == nil {
		return nil, fmt.Errorf("invalid schema object: %s", raw)
	}
	for k := range keys {
		switch k {
		// examples is an annotation (a form.Of field states its grammar's
		// sample there); the sampler draws from the other keywords.
		case "type", "enum", "properties", "required", "additionalProperties", "items", "pattern", "minItems", "maxItems", "minLength", "maxLength", "minimum", "maximum", "description", "title", "examples":
		default:
			return nil, fmt.Errorf("unsupported schema keyword %q", k)
		}
	}
	var s fuzzSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	if enum, ok := keys["enum"]; ok {
		if len(s.Enum) == 0 {
			return nil, fmt.Errorf("invalid empty enum: %s", enum)
		}
		return s.Enum[fuzzChoice(v, len(s.Enum))], nil
	}
	switch s.Type {
	case "":
		if len(keys) != 0 {
			return nil, fmt.Errorf("unsupported schema without type")
		}
		return []any{nil, false, float64(0), "", []any{}, map[string]any{}}[fuzzChoice(v, 6)], nil
	case "null":
		return nil, nil
	case "boolean":
		return fuzzChoice(v, 2) != 0, nil
	case "integer", "number":
		low, high := math.Inf(-1), math.Inf(1)
		if s.Minimum != nil {
			low = *s.Minimum
		}
		if s.Maximum != nil {
			high = *s.Maximum
		}
		if s.Type == "integer" {
			low = math.Ceil(low)
			high = math.Floor(high)
		}
		if low > high {
			return nil, fmt.Errorf("invalid numeric bounds")
		}
		value := []float64{0, 1, -1}[fuzzChoice(v, 3)]
		if fuzzChoice(v, 3) == 0 && s.Minimum != nil {
			value = low
		}
		if fuzzChoice(v, 3) == 2 && s.Maximum != nil {
			value = high
		}
		value = math.Max(low, math.Min(high, value))
		return value, nil
	case "string":
		n, err := fuzzSize(s.MinLength, s.MaxLength, v, 4096, 16)
		if err != nil {
			return nil, err
		}
		if s.Pattern == nil {
			return strings.Repeat("a", n), nil
		}
		return fuzzPattern(*s.Pattern, s.MinLength, s.MaxLength, n, v)
	case "array":
		n, err := fuzzSize(s.MinItems, s.MaxItems, v, 16, 16)
		if err != nil {
			return nil, err
		}
		item := s.Items
		if len(item) == 0 {
			item = json.RawMessage(`{}`)
		}
		// Past its minimum, an array draws its items from a share of half the
		// budget left, and drops an item that would overrun that share: a
		// legal schema never fails for size, every field drawn after it keeps
		// budget, and the same variant draws the same sample. Its minimum items
		// are drawn first, from the budget itself, and never cut.
		minimum := 0
		if s.MinItems != nil {
			minimum = *s.MinItems
		}
		values := make([]any, 0, n)
		for i := 0; i < n && i < minimum; i++ {
			value, err := drawSchemaValue(item, v+i, depth+1, remaining)
			if err != nil {
				return nil, fmt.Errorf("array item: %w", err)
			}
			values = append(values, value)
		}
		share := *remaining / 2
		left := share
		for i := minimum; i < n && left > 0; i++ {
			before := left
			value, err := drawSchemaValue(item, v+i, depth+1, &left)
			if errors.Is(err, errExpansionBound) {
				left = before
				break
			}
			if err != nil {
				return nil, fmt.Errorf("array item: %w", err)
			}
			values = append(values, value)
		}
		*remaining -= share - left
		return values, nil
	case "object":
		values := map[string]any{}
		required := map[string]bool{}
		for _, key := range s.Required {
			if _, ok := s.Properties[key]; !ok {
				return nil, fmt.Errorf("required property %q has no schema", key)
			}
			required[key] = true
		}
		names := make([]string, 0, len(s.Properties))
		for key := range s.Properties {
			names = append(names, key)
		}
		sort.Strings(names)
		for _, key := range names {
			if !required[key] && v == 0 {
				continue
			}
			value, err := drawSchemaValue(s.Properties[key], v, depth+1, remaining)
			if err != nil {
				return nil, fmt.Errorf("property %q: %w", key, err)
			}
			values[key] = value
		}
		if len(s.Additional) > 0 && string(s.Additional) != "false" {
			additional := s.Additional
			if string(additional) == "true" {
				additional = json.RawMessage(`{}`)
			}
			count := []int{0, 1, 16}[fuzzChoice(v, 3)]
			for i := 0; i < count; i++ {
				key := fmt.Sprintf("key%03d", i)
				if _, exists := s.Properties[key]; exists {
					continue
				}
				value, err := drawSchemaValue(additional, v+i, depth+1, remaining)
				if err != nil {
					return nil, fmt.Errorf("map value: %w", err)
				}
				values[key] = value
			}
		}
		return values, nil
	default:
		return nil, fmt.Errorf("unsupported schema type %q", s.Type)
	}
}

func fuzzSize(min, max *int, v, limit, usualMax int) (int, error) {
	low, high := 0, usualMax
	if min != nil {
		low = *min
	}
	if max != nil {
		high = *max
	}
	if low < 0 || high < 0 || (max != nil && high < low) {
		return 0, fmt.Errorf("invalid size bounds")
	}
	if low > limit {
		return 0, fmt.Errorf("minimum size %d exceeds sampler bound %d", low, limit)
	}
	if max == nil && high < low {
		high = low
	}
	if high > limit {
		high = limit
	}
	switch fuzzChoice(v, 3) {
	case 0:
		return low, nil
	case 1:
		if low < high {
			return low + 1, nil
		}
		return low, nil
	default:
		return high, nil
	}
}

// Patterns use Go's regexp syntax, matching the engine's pattern tags. Search
// deterministic repetition sizes, then verify the candidate against the regexp.
// This is a bounded sampler, not a solver for arbitrary regular expressions.
func fuzzPattern(pattern string, min, max *int, preferred, v int) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("unsupported pattern: %w", err)
	}
	tree, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", fmt.Errorf("unsupported pattern: %w", err)
	}
	low, high := 0, 4096
	if min != nil {
		low = *min
	}
	if max != nil && *max < high {
		high = *max
	}
	targets := []int{preferred, low, 1, 16, high}
	for i := 0; i <= 256; i++ {
		targets = append(targets, i)
	}
	for _, target := range targets {
		value, err := fuzzRegexp(tree, target, v)
		if err != nil {
			continue
		}
		n := utf8.RuneCountInString(value)
		if n >= low && n <= high && re.MatchString(value) {
			return value, nil
		}
	}
	return "", fmt.Errorf("unsupported pattern or unsatisfied bounded string constraints: %q", pattern)
}

func fuzzRegexp(re *syntax.Regexp, target, v int) (string, error) {
	switch re.Op {
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText, syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return "", nil
	case syntax.OpLiteral:
		return string(re.Rune), nil
	case syntax.OpCharClass:
		if len(re.Rune) > 0 {
			return string(re.Rune[0]), nil
		}
	case syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return "a", nil
	case syntax.OpCapture:
		return fuzzRegexp(re.Sub[0], target, v)
	case syntax.OpAlternate:
		return fuzzRegexp(re.Sub[fuzzChoice(v, len(re.Sub))], target, v)
	case syntax.OpConcat:
		var b strings.Builder
		for _, sub := range re.Sub {
			s, err := fuzzRegexp(sub, target, v)
			if err != nil {
				return "", err
			}
			if b.Len()+len(s) > 4096 {
				return "", fmt.Errorf("pattern exceeds sampler bound")
			}
			b.WriteString(s)
		}
		return b.String(), nil
	case syntax.OpQuest, syntax.OpStar, syntax.OpPlus, syntax.OpRepeat:
		low, high := 0, 4096
		switch re.Op {
		case syntax.OpQuest:
			high = 1
		case syntax.OpPlus:
			low = 1
		case syntax.OpRepeat:
			low = re.Min
			if re.Max >= 0 {
				high = re.Max
			}
		}
		n := target
		if n < low {
			n = low
		}
		if n > high {
			n = high
		}
		s, err := fuzzRegexp(re.Sub[0], target, v)
		if err != nil {
			return "", err
		}
		if n > 4096 || (len(s) > 0 && n > 4096/len(s)) {
			return "", fmt.Errorf("pattern exceeds sampler bound")
		}
		return strings.Repeat(s, n), nil
	}
	return "", fmt.Errorf("unsupported regexp operation %s", re.Op)
}
