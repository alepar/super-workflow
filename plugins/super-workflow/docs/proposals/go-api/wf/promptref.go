package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// PromptRef is a prompt candidate reference: a file path, or file#/json/pointer
// (RFC 6901) into a JSON file whose value is a string. Either part may carry
// {param} placeholders.
//
// Owns: parsing a candidate, rendering it unsubstituted for render and hash,
// and resolving it to prompt text.
// Does not: decide what a reference outside the candidates or a missing one
// means: the agent kind's issue step classes those Blocked and Unavailable.
// Agents never read prompt files themselves.
// Extend by: a new reference shape is one case here.
type PromptRef struct{ file, pointer string }

var (
	// ErrPromptNotFound marks a resolve whose file or JSON pointer is missing;
	// callers class it Unavailable.
	ErrPromptNotFound = errors.New("prompt reference not found")
	// ErrPromptUnbound marks a {param} placeholder with no value; callers class
	// it Blocked.
	ErrPromptUnbound = errors.New("prompt reference has an unbound placeholder")
)

var promptPlaceholder = regexp.MustCompile(`\{[A-Za-z][A-Za-z0-9_-]*\}`)

// ParsePromptRef parses a candidate; a malformed one is an error, which
// RulePromptCandidates reports at lint. A PathTemplate marker is consumed here;
// the reference resolves its placeholders from the supplied parameter values.
func ParsePromptRef(s string) (PromptRef, error) {
	s = strings.TrimPrefix(s, pathTemplatePrefix)
	file, pointer, hasPointer := strings.Cut(s, "#")
	switch {
	case file == "":
		return PromptRef{}, fmt.Errorf("prompt reference %q has no file", s)
	case hasPointer && !strings.HasPrefix(pointer, "/"):
		return PromptRef{}, fmt.Errorf("prompt reference %q: a JSON pointer starts with /", s)
	case strings.ContainsAny(promptPlaceholder.ReplaceAllString(s, ""), "{}"):
		return PromptRef{}, fmt.Errorf("prompt reference %q has a malformed placeholder", s)
	}
	for i := 0; i < len(pointer); i++ {
		if pointer[i] == '~' && (i+1 == len(pointer) || (pointer[i+1] != '0' && pointer[i+1] != '1')) {
			return PromptRef{}, fmt.Errorf("prompt reference %q has a bad ~ escape", s)
		}
	}
	return PromptRef{file: file, pointer: pointer}, nil
}

// String is the unsubstituted reference, as declared.
func (r PromptRef) String() string {
	if r.pointer == "" {
		return r.file
	}
	return r.file + "#" + r.pointer
}

// Path is the file r names once params are bound, absolute under workspace
// when relative: what Resolve reads.
func (r PromptRef) Path(params map[string]string, workspace string) (string, error) {
	path, err := bindPlaceholders(r.file, params)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	return path, nil
}

// Resolve substitutes params and returns the prompt text. A relative file is
// read from workspace. A missing file or pointer wraps ErrPromptNotFound and an
// unbound placeholder wraps ErrPromptUnbound; any other failure (unreadable
// file, invalid JSON, a non-string value) is a plain error.
func (r PromptRef) Resolve(params map[string]string, workspace string) (string, error) {
	path, err := r.Path(params, workspace)
	if err != nil {
		return "", err
	}
	pointer, err := bindPlaceholders(r.pointer, params)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: %s", ErrPromptNotFound, path)
	}
	if err != nil {
		return "", err
	}
	if pointer == "" {
		return string(data), nil
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("prompt %s: %w", path, err)
	}
	v, ok := jsonPointer(doc, pointer)
	if !ok {
		return "", fmt.Errorf("%w: %s#%s", ErrPromptNotFound, path, pointer)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("prompt %s#%s: value is not a string", path, pointer)
	}
	return s, nil
}

func bindPlaceholders(s string, params map[string]string) (string, error) {
	missing := ""
	out := promptPlaceholder.ReplaceAllStringFunc(s, func(m string) string {
		v, ok := params[m[1:len(m)-1]]
		if !ok && missing == "" {
			missing = m
		}
		return v
	})
	if missing != "" {
		return "", fmt.Errorf("%w: %s in %s", ErrPromptUnbound, missing, s)
	}
	return out, nil
}

var pointerUnescape = strings.NewReplacer("~1", "/", "~0", "~")

func jsonPointer(doc any, pointer string) (any, bool) {
	for _, tok := range strings.Split(pointer, "/")[1:] {
		switch v := doc.(type) {
		case map[string]any:
			x, ok := v[pointerUnescape.Replace(tok)]
			if !ok {
				return nil, false
			}
			doc = x
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			doc = v[i]
		default:
			return nil, false
		}
	}
	return doc, true
}

// lintPromptFor checks a PromptFor prompt: the candidate list
// (RulePromptCandidates), and that its item type is the input of the agent's
// Typed child (RuleBuild). It runs at lint because Typed is called after Spec.
func lintPromptFor(n *node, p Prompt) []Problem {
	if p.item == nil {
		return nil
	}
	var out []Problem
	if p.sel == nil {
		out = append(out, n.problem(RuleBuild, "PromptFor with a nil selector"))
	}
	if n.def.input != p.item {
		out = append(out, n.problem(RuleBuild, "PromptFor selects on %s but definition %s is not a Typed child with that input", p.item, n.def.name))
	}
	if len(p.candidates) == 0 {
		out = append(out, n.problem(RulePromptCandidates, "PromptFor has no candidates"))
	}
	seen := map[string]bool{}
	for _, c := range p.candidates {
		if seen[c] {
			out = append(out, n.problem(RulePromptCandidates, "candidate %q is listed twice", c))
		}
		seen[c] = true
		if _, err := ParsePromptRef(c); err != nil {
			out = append(out, n.problem(RulePromptCandidates, "%v", err))
		}
	}
	return out
}
