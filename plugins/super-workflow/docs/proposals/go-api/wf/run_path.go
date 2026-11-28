package wf

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// RunPath identifies a path relative to the current run's directory. Its explicit
// JSON marker survives journal replay and resolves against a fork's RunDir.
// Ordinary strings remain opaque, even when they contain absolute paths.
type RunPath string

const runPathMarker = "$wf.runPath"

func (p RunPath) validate() error {
	s := string(p)
	if s == "" || s == "." || s == ".." || strings.HasPrefix(s, "../") || path.IsAbs(s) || filepath.IsAbs(s) || path.Clean(s) != s || strings.ContainsAny(s, "\\\x00") {
		return fmt.Errorf("invalid RunPath %q: need a clean relative path inside RunDir", s)
	}
	return nil
}

func (p RunPath) MarshalJSON() ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{runPathMarker: string(p)})
}

func (p *RunPath) UnmarshalJSON(raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	value, ok, err := markedRunPath(v)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("RunPath requires exactly one %q marker", runPathMarker)
	}
	*p = value
	return nil
}

// markedRunPath recognizes only the exact typed representation, never a path-like string.
func markedRunPath(v any) (RunPath, bool, error) {
	if p, ok := v.(RunPath); ok {
		return p, true, p.validate()
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return "", false, nil
	}
	raw, ok := m[runPathMarker]
	if !ok {
		return "", false, nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", true, fmt.Errorf("RunPath marker must contain a string")
	}
	p := RunPath(s)
	return p, true, p.validate()
}

// resolveRunPathValue returns a fresh JSON value with only explicit RunPath
// markers resolved. It never modifies journal-owned maps or slices.
func resolveRunPathValue(v any, runDir string) (any, error) {
	if p, ok, err := markedRunPath(v); ok || err != nil {
		if err != nil {
			return nil, err
		}
		if runDir == "" {
			return nil, fmt.Errorf("RunPath needs the engine's RunDir option")
		}
		root, err := filepath.Abs(runDir)
		if err != nil {
			return nil, err
		}
		return confinedRunPath(root, p)
	}
	switch x := v.(type) {
	case map[string]any:
		if x == nil {
			return x, nil
		}
		out := make(map[string]any, len(x))
		for k, value := range x {
			resolved, err := resolveRunPathValue(value, runDir)
			if err != nil {
				return nil, err
			}
			out[k] = resolved
		}
		return out, nil
	case []any:
		if x == nil {
			return x, nil
		}
		out := make([]any, len(x))
		for i, value := range x {
			resolved, err := resolveRunPathValue(value, runDir)
			if err != nil {
				return nil, err
			}
			out[i] = resolved
		}
		return out, nil
	}
	return v, nil
}

// RunPathOf marks an absolute path owned by the current run for replay in a fork.
// Paths outside RunDir cannot be marked; no ordinary history is rewritten.
func RunPathOf(c Ctx, absolute string) (RunPath, error) {
	if !filepath.IsAbs(absolute) {
		return "", fmt.Errorf("RunPathOf needs an absolute path")
	}
	value, err := c.value(ctxKey{kind: "runDir"})
	if err != nil {
		return "", err
	}
	root := decodeValue[string](value)
	if root == "" {
		return "", fmt.Errorf("RunPathOf needs the engine's RunDir option")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil {
		return "", err
	}
	p := RunPath(filepath.ToSlash(relative))
	if err := p.validate(); err != nil {
		return "", err
	}
	if _, err := confinedRunPath(root, p); err != nil {
		return "", err
	}
	return p, nil
}

// confinedRunPath rejects existing escape links before handing a path to an
// adapter or agent. This checks current filesystem state; it does not prevent
// another process replacing an ancestor after the path has been delivered.
func confinedRunPath(root string, p RunPath) (string, error) {
	if info, err := os.Lstat(root); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("RunPath RunDir must not be a symlink: %s", root)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("RunPath RunDir is not a directory: %s", root)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	// Canonicalize parent aliases (for example a platform's /tmp alias) without
	// requiring missing run directories to exist or creating them during binding.
	canonicalRoot, err := runPathExistingPrefix(root)
	if err != nil {
		return "", err
	}
	current := root
	for _, part := range strings.Split(string(p), "/") {
		current = filepath.Join(current, part)
		_, err := os.Lstat(current)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(current)
		if err != nil {
			return "", err
		}
		relative, err := filepath.Rel(canonicalRoot, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return "", fmt.Errorf("RunPath escapes RunDir through existing link: %s", current)
		}
	}
	return filepath.Join(root, filepath.FromSlash(string(p))), nil
}

// realPath is path made absolute with every symbolic link on its existing
// part resolved; a missing tail is kept as written. The engine resolves the
// roots it is handed (workspace, state root, run directory, fork destination)
// once, here, so a platform alias above them (macOS's /tmp and /var lead into
// /private) is not mistaken for a link inside them, which later checks refuse.
func realPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return runPathExistingPrefix(abs)
}

// sameRealPath reports whether a and b name one place once resolved; a run
// recorded under an alias of its workspace has not moved.
func sameRealPath(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := realPath(a)
	rb, errB := realPath(b)
	return errA == nil && errB == nil && ra == rb
}

func runPathExistingPrefix(path string) (string, error) {
	ancestor := path
	var missing []string
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			resolved, err := filepath.EvalSymlinks(ancestor)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		missing = append(missing, filepath.Base(ancestor))
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", fmt.Errorf("path has no existing ancestor: %s", path)
		}
		ancestor = parent
	}
}
