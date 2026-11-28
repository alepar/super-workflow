package wf

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Text is a sequence of literal data and explicitly authored template spans.
type Text struct{ spans []TextSpan }

// TextSpan is the durable representation of one span in a versioned plan.
type TextSpan struct {
	Text   string
	Expand bool `json:",omitempty"`
	Path   bool `json:",omitempty"`
}

// Template marks a definition-authored text span.
func Template(source string) Text { return Text{spans: []TextSpan{{Text: source, Expand: true}}} }

// PathText marks a whole effect argument as an authored, owned path template.
// It also accepts the spelling returned by PathTemplate for paths shared with
// a command or observation.
func PathText(source string) Text {
	return Text{spans: []TextSpan{{Text: strings.TrimPrefix(source, pathTemplatePrefix), Path: true}}}
}

// TextOf joins strings as literal data and Text values with their provenance.
func TextOf(parts ...any) Text {
	var out Text
	for _, part := range parts {
		switch v := part.(type) {
		case string:
			out.spans = append(out.spans, TextSpan{Text: v})
		case Text:
			out.spans = append(out.spans, v.spans...)
		default:
			panic(fmt.Sprintf("wf.TextOf: expected string or wf.Text, got %T", part))
		}
	}
	return out
}

// Raw returns the source spelling for inspection and definition-time adapters.
func (t Text) Raw() string {
	var b strings.Builder
	for _, span := range t.spans {
		b.WriteString(span.Text)
	}
	return b.String()
}

type textKey struct{ text Text }

func (k textKey) key() string { return k.text.Raw() }

// KeyWith joins literal and template key parts, separated by slash.
func KeyWith(parts ...any) Key {
	joined := make([]any, 0, len(parts)*2)
	for i, part := range parts {
		if i > 0 {
			joined = append(joined, "/")
		}
		joined = append(joined, part)
	}
	return textKey{TextOf(joined...)}
}

// EffectArgTemplate replaces one field's source string with these typed spans.
// A path segment is a JSON object key or a zero-based array index.
type EffectArgTemplate struct {
	Path  []string
	Spans []TextSpan
}

// WithTemplateArg attaches authored spans for an argument's path. The
// descriptor's ordinary Args retain their source spelling for inspection.
func (e ExternalEffect) WithTemplateArg(text Text, path ...string) ExternalEffect {
	entry := EffectArgTemplate{Path: slices.Clone(path), Spans: slices.Clone(text.spans)}
	e.argTemplates = append(slices.Clone(e.argTemplates), entry)
	return e
}

const effectBindingVersion = 1

func (s scope) bindText(spans []TextSpan) (string, error) {
	var b strings.Builder
	for _, span := range spans {
		text := span.Text
		if span.Path {
			return "", fmt.Errorf("PathText must occupy a whole effect argument")
		}
		if span.Expand {
			var err error
			text, err = s.bindPlaceholders(text)
			if err != nil {
				return "", err
			}
		}
		b.WriteString(text)
	}
	return b.String(), nil
}

func (s scope) bindEffectArg(text EffectArgTemplate) (any, error) {
	if len(text.Spans) == 1 {
		span := text.Spans[0]
		if span.Path {
			if span.Text == "" {
				return "", nil
			}
			return s.bindPathSource(span.Text)
		}
		if span.Expand {
			return s.bindValueMode(span.Text, true, false, false)
		}
	}
	return s.bindText(text.Spans)
}

// setEffectArg updates a declared leaf in an already cloned generic document.
func setEffectArg(v any, path []string, value any) bool {
	if len(path) == 0 {
		return false
	}
	key := path[0]
	switch x := v.(type) {
	case map[string]any:
		child, ok := x[key]
		if !ok {
			return false
		}
		if len(path) == 1 {
			x[key] = value
			return true
		}
		return setEffectArg(child, path[1:], value)
	case []any:
		i, err := strconv.Atoi(key)
		if err != nil || i < 0 || i >= len(x) {
			return false
		}
		if len(path) == 1 {
			x[i] = value
			return true
		}
		return setEffectArg(x[i], path[1:], value)
	default:
		return false
	}
}
