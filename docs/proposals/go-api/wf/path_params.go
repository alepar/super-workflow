package wf

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// PathBase declares the root used for a relative path parameter.
type PathBase string

const (
	WorkspaceRelative PathBase = "workspace"
	RunRelative       PathBase = "run"
	externalPathBase  PathBase = "external"
)

// validatePathParam validates durable metadata independently of current declarations.
func validatePathParam(name, value string, base PathBase) error {
	invalid := func() error {
		return fmt.Errorf("%w: parameter %s has invalid %s path %q", ErrRejected, name, base, value)
	}
	if value == "" || strings.ContainsRune(value, 0) {
		return invalid()
	}
	switch base {
	case WorkspaceRelative, RunRelative:
		if filepath.IsAbs(value) || strings.Contains(value, "\\") || path.Clean(value) != value || value == ".." || strings.HasPrefix(value, "../") {
			return invalid()
		}
	case externalPathBase:
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return invalid()
		}
	default:
		return invalid()
	}
	return nil
}

func pathParamRelative(root, value string) (string, bool) {
	if root == "" {
		return "", false
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	relative, err := filepath.Rel(root, value)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", false
	}
	return filepath.ToSlash(relative), true
}

func normalizePathParams(def *Def, params map[string]string, workspace, runDir, stateRoot string) (map[string]string, map[string]PathBase, error) {
	declarations, err := collectPathParamDeclarations(def)
	if err != nil {
		return nil, nil, err
	}
	var values map[string]string
	if params != nil {
		values = make(map[string]string, len(params))
		for name, value := range params {
			values[name] = value
		}
	}
	var bases map[string]PathBase
	for name, declared := range declarations {
		value, present := params[name]
		if !present {
			continue
		}
		base := declared
		candidate := value
		if !filepath.IsAbs(candidate) {
			if err := validatePathParam(name, value, declared); err != nil {
				return nil, nil, err
			}
			root := workspace
			if declared == RunRelative {
				root = runDir
			}
			if root == "" {
				return nil, nil, fmt.Errorf("%w: parameter %s requires its %s root", ErrRejected, name, declared)
			}
			absoluteRoot, err := filepath.Abs(root)
			if err != nil {
				return nil, nil, err
			}
			candidate = filepath.Join(absoluteRoot, filepath.FromSlash(value))
		}
		candidate = filepath.Clean(candidate)
		if relative, inside := pathParamRelative(runDir, candidate); inside {
			value, base = relative, RunRelative
		} else if _, inside := pathParamRelative(stateRoot, candidate); inside {
			return nil, nil, fmt.Errorf("%w: parameter %s points into state outside this run", ErrRejected, name)
		} else if relative, inside := pathParamRelative(workspace, candidate); inside {
			value, base = relative, WorkspaceRelative
		} else {
			value, base = candidate, externalPathBase
		}
		if err := validatePathParam(name, value, base); err != nil {
			return nil, nil, err
		}
		if base == WorkspaceRelative && workspace == "" || base == RunRelative && runDir == "" {
			return nil, nil, fmt.Errorf("%w: parameter %s requires its %s root", ErrRejected, name, base)
		}
		if bases == nil {
			bases = map[string]PathBase{}
		}
		values[name], bases[name] = value, base
	}
	return values, bases, nil
}

// paramValue resolves only explicitly typed parameters against the current roots.
func (c *cycle) paramValue(name string) (string, error) {
	value := c.st.started.Params[name]
	base, typed := c.st.started.ParamPaths[name]
	if !typed {
		return value, nil
	}
	if err := validatePathParam(name, value, base); err != nil {
		return "", err
	}
	if base == externalPathBase {
		return value, nil
	}
	root := c.workspace()
	if base == RunRelative {
		root = c.resultDir()
	}
	if root == "" {
		return "", fmt.Errorf("%w: parameter %s requires its %s root", ErrRejected, name, base)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := confinedRunPath(root, RunPath(value))
	if err != nil {
		return "", fmt.Errorf("%w: parameter %s: %v", ErrRejected, name, err)
	}
	return resolved, nil
}

// normalizeSuccessorPathParams preserves the predecessor's explicit ownership
// when a caller repeats its recorded or original absolute value. New values are
// normalized normally; ordinary parameters are never inferred to be paths.
func normalizeSuccessorPathParams(def *Def, params map[string]string, workspace, runDir, stateRoot, previousRunDir string, pred *RunStartedData) (map[string]string, map[string]PathBase, error) {
	if pred == nil || len(pred.ParamPaths) == 0 {
		return normalizePathParams(def, params, workspace, runDir, stateRoot)
	}
	remaining := make(map[string]string, len(params))
	for name, value := range params {
		remaining[name] = value
	}
	inherited := map[string]string{}
	bases := map[string]PathBase{}
	for name, base := range pred.ParamPaths {
		old := pred.Params[name]
		if err := validatePathParam(name, old, base); err != nil {
			return nil, nil, err
		}
		absolute := old
		switch base {
		case WorkspaceRelative:
			absolute = filepath.Join(pred.Workspace, filepath.FromSlash(old))
		case RunRelative:
			absolute = filepath.Join(previousRunDir, filepath.FromSlash(old))
		}
		supplied, present := params[name]
		if !present || supplied == old || filepath.IsAbs(supplied) && filepath.Clean(supplied) == absolute {
			inherited[name], bases[name] = old, base
			delete(remaining, name)
		}
	}
	values, normalized, err := normalizePathParams(def, remaining, workspace, runDir, stateRoot)
	if err != nil {
		return nil, nil, err
	}
	for name, base := range normalized {
		bases[name] = base
	}
	for name, value := range inherited {
		values[name] = value
	}
	if len(bases) == 0 {
		bases = nil
	}
	return values, bases, nil
}

// Parameters are run-wide, including declarations on reachable Sub/Map definitions.
func collectPathParamDeclarations(def *Def) (map[string]PathBase, error) {
	declarations := map[string]PathBase{}
	seen := map[*Def]bool{}
	var visit func(*Def) error
	visit = func(d *Def) error {
		if d == nil || seen[d] {
			return nil
		}
		seen[d] = true
		names := make([]string, 0, len(d.pathParams))
		for name := range d.pathParams {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			base := d.pathParams[name]
			if old, ok := declarations[name]; ok && old != base {
				return fmt.Errorf("%w: parameter %s has conflicting path bases %q and %q", ErrRejected, name, old, base)
			}
			declarations[name] = base
		}
		for _, child := range d.children {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(def); err != nil {
		return nil, err
	}
	return declarations, nil
}

func rulePathParamDeclarations(c *lintCtx) []Problem {
	if _, err := collectPathParamDeclarations(c.d); err != nil {
		return []Problem{defProblem(c.d, RuleBuild, "%s", err)}
	}
	return nil
}
