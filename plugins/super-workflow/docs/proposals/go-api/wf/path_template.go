package wf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

const pathTemplatePrefix = "\x1ewf.path:"
const ownedPathMarker = "$wf.path"

// PathTemplate marks a whole string as a path template, resolved when it is
// bound. Its ownership survives replay; ordinary strings remain opaque. A
// fragment may be appended, and an empty optional path remains empty.
func PathTemplate(template string) string {
	if template == "" {
		return ""
	}
	return pathTemplatePrefix + template
}

type ownedPath struct {
	Base     PathBase `json:"base"`
	Path     string   `json:"path"`
	Fragment string   `json:"fragment,omitempty"`
	// Cite marks a result's citation of a recorded artifact: a
	// reference, not a read. It resolves to its path by joining it to the
	// run directory, whether or not the file is there or still has its
	// recorded type (a replay recreates artifacts its own way).
	Cite bool `json:"cite,omitempty"`
}

func (p ownedPath) validate() error {
	if err := validatePathParam("PathTemplate", p.Path, p.Base); err != nil {
		return err
	}
	if p.Fragment != "" && !strings.HasPrefix(p.Fragment, "#") {
		return fmt.Errorf("PathTemplate fragment must start with #")
	}
	if strings.ContainsRune(p.Fragment, 0) || strings.Contains(p.Path, pathTemplatePrefix) || strings.Contains(p.Fragment, pathTemplatePrefix) {
		return fmt.Errorf("invalid nested PathTemplate or fragment")
	}
	if p.Cite && p.Base != RunRelative {
		return fmt.Errorf("a citation is run-relative")
	}
	return nil
}

// citedPath is a citation's path under the run directory runDir: a join,
// never a read. With no run directory known it is the run-relative path.
func citedPath(runDir string, p ownedPath) string {
	if runDir == "" {
		return p.Path + p.Fragment
	}
	return filepath.Join(runDir, filepath.FromSlash(p.Path)) + p.Fragment
}

// resolveCitations resolves the citation references in raw to path strings
// under runDir (citedPath), leaving every other value as it is.
func resolveCitations(raw json.RawMessage, runDir string) (json.RawMessage, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return raw, nil
	}
	var wire any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&wire); err != nil {
		return nil, err
	}
	changed := false
	wire, err := transformPathJSON(wire, func(value any) (any, bool, error) {
		p, marked, err := markedOwnedPath(value)
		if !marked || err != nil || !p.Cite {
			return nil, false, nil
		}
		changed = true
		return citedPath(runDir, p), true, nil
	})
	if err != nil || !changed {
		return raw, err
	}
	return json.Marshal(wire)
}

func (p ownedPath) value() any {
	inner := map[string]any{"base": string(p.Base), "path": p.Path}
	if p.Fragment != "" {
		inner["fragment"] = p.Fragment
	}
	if p.Cite {
		inner["cite"] = true
	}
	return map[string]any{ownedPathMarker: inner}
}

func markedOwnedPath(value any) (ownedPath, bool, error) {
	if p, ok := value.(ownedPath); ok {
		return p, true, p.validate()
	}
	outer, ok := value.(map[string]any)
	if !ok || len(outer) != 1 {
		return ownedPath{}, false, nil
	}
	raw, ok := outer[ownedPathMarker]
	if !ok {
		return ownedPath{}, false, nil
	}
	invalid := func() (ownedPath, bool, error) { return ownedPath{}, true, fmt.Errorf("invalid owned path marker") }
	inner, ok := raw.(map[string]any)
	if !ok {
		return invalid()
	}
	base, baseOK := inner["base"].(string)
	path, pathOK := inner["path"].(string)
	if !baseOK || !pathOK {
		return invalid()
	}
	p := ownedPath{Base: PathBase(base), Path: path}
	for name, value := range inner {
		switch name {
		case "base", "path":
		case "fragment":
			fragment, ok := value.(string)
			if !ok {
				return invalid()
			}
			p.Fragment = fragment
		case "cite":
			cite, ok := value.(bool)
			if !ok {
				return invalid()
			}
			p.Cite = cite
		default:
			return invalid()
		}
	}
	return p, true, p.validate()
}

func (c *cycle) resolveOwnedPath(p ownedPath) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	if p.Base == externalPathBase {
		return p.Path + p.Fragment, nil
	}
	root := c.workspace()
	if p.Base == RunRelative {
		root = c.resultDir()
	}
	if p.Cite && root != "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", err
		}
		return citedPath(abs, p), nil
	}
	if root == "" {
		return "", fmt.Errorf("PathTemplate requires its %s root", p.Base)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := confinedRunPath(root, RunPath(p.Path))
	if err != nil {
		return "", err
	}
	return resolved + p.Fragment, nil
}

func (c *cycle) ownedPath(absolute string) (ownedPath, error) {
	if !filepath.IsAbs(absolute) || filepath.Clean(absolute) != absolute || strings.ContainsRune(absolute, 0) {
		return ownedPath{}, fmt.Errorf("PathTemplate needs a clean absolute path: %q", absolute)
	}
	p := ownedPath{Base: externalPathBase, Path: absolute}
	state := c.e.opts.StateRoot
	if state == "" && len(c.recs) > 0 && c.recs[0].Roots != nil {
		state = c.recs[0].Roots.State
	}
	if relative, inside := pathParamRelative(c.resultDir(), absolute); inside {
		p.Base, p.Path = RunRelative, relative
	} else if _, inside := pathParamRelative(state, absolute); inside {
		return ownedPath{}, fmt.Errorf("PathTemplate points into state outside this run")
	} else if relative, inside := pathParamRelative(c.workspace(), absolute); inside {
		p.Base, p.Path = WorkspaceRelative, relative
	}
	return p, p.validate()
}

func (s scope) bindPathTemplate(token string) (any, bool, error) {
	if !strings.HasPrefix(token, pathTemplatePrefix) {
		if strings.Contains(token, pathTemplatePrefix) {
			return nil, true, fmt.Errorf("PathTemplate must occupy the whole value")
		}
		return token, false, nil
	}
	value, err := s.bindPathSource(strings.TrimPrefix(token, pathTemplatePrefix))
	return value, true, err
}

func (s scope) bindPathSource(source string) (any, error) {
	if strings.Contains(source, pathTemplatePrefix) {
		return nil, fmt.Errorf("nested PathTemplate is invalid")
	}
	bound, err := s.bindPlaceholders(source)
	if err != nil {
		return nil, err
	}
	if strings.Contains(bound, pathTemplatePrefix) {
		return nil, fmt.Errorf("nested PathTemplate is invalid")
	}
	path, fragment, hasFragment := strings.Cut(bound, "#")
	// A directory template may append an empty filename. Preserve the root,
	// but canonicalize trailing separators before validating interior components.
	if path != "" {
		trimmed := strings.TrimRight(path, string(filepath.Separator))
		if trimmed == "" {
			path = string(filepath.Separator)
		} else {
			path = trimmed
		}
	}
	if path == "" {
		return nil, fmt.Errorf("PathTemplate path is empty")
	}
	if !filepath.IsAbs(path) {
		// A workspace-root field commonly contains ".". Composing that
		// root with a suffix is equivalent to the relative suffix itself.
		for strings.HasPrefix(path, "./") {
			path = strings.TrimPrefix(path, "./")
		}
		if err := validatePathParam("PathTemplate", path, WorkspaceRelative); err != nil {
			return nil, err
		}
		root := s.c.workspace()
		if root == "" {
			return nil, fmt.Errorf("PathTemplate requires a workspace")
		}
		root, err = filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		path = filepath.Join(root, filepath.FromSlash(path))
	}
	p, err := s.c.ownedPath(path)
	if err != nil {
		return nil, err
	}
	if hasFragment {
		p.Fragment = "#" + fragment
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return p.value(), nil
}
