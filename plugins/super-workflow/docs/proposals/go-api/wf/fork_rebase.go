package wf

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// ForkSource is where a fork's journal prefix was written: the source run's
// workspace, state root and run directory. History the fork carries (a
// command's printed output, an observation's value) may name them.
type ForkSource struct {
	Workspace, State, RunDir string
}

// forkSources are the source roots of every fork in this run's history,
// oldest first: a fork of a fork carries its ancestor's rewound record too.
func (c *cycle) forkSources() []ForkSource {
	if c.forkRoots != nil {
		return *c.forkRoots
	}
	var out []ForkSource
	for _, r := range c.recs {
		if r.Kind != JournalRewound {
			continue
		}
		var d RewoundData
		if json.Unmarshal(r.Data, &d) == nil && d.Source != nil {
			out = append(out, *d.Source)
		}
	}
	c.forkRoots = &out
	return out
}

// sourceRootIn is the first source root that s contains, as a substring,
// once this run's own workspace and run directory are set aside (a fork
// may live below a source root), or "". Outside a fork it is always "".
func (c *cycle) sourceRootIn(s string) string {
	sources := c.forkSources()
	if len(sources) == 0 || s == "" {
		return ""
	}
	var own []string
	if dir := c.resultDir(); dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			own = append(own, withRealPath(abs)...)
		}
	}
	if ws, err := filepath.Abs(c.workspace()); err == nil {
		own = append(own, withRealPath(ws)...)
	}
	sort.Slice(own, func(i, j int) bool { return len(own[i]) > len(own[j]) })
	for _, o := range own {
		s = strings.ReplaceAll(s, o, "\x00")
	}
	for _, src := range sources {
		for _, root := range []string{src.RunDir, src.State, src.Workspace} {
			if root == "" {
				continue
			}
			for _, spelling := range withRealPath(root) {
				if strings.Contains(s, spelling) {
					return root
				}
			}
		}
	}
	return ""
}

// withRealPath is p and, when it differs and resolves, its real path: a
// file's final target is spelled by real paths.
func withRealPath(p string) []string {
	if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
		return []string{p, real}
	}
	return []string{p}
}

// checkForkFile is checkForkValue for a file the engine is about to open: the
// path as bound, then its final target, links followed, so a link inside the
// fork cannot reach the source.
func (c *cycle) checkForkFile(path, field string) error {
	if err := c.checkForkValue(path, field); err != nil {
		return err
	}
	if len(c.forkSources()) == 0 {
		return nil
	}
	if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
		return c.checkForkValue(real, field)
	}
	return nil
}

// markedSegments are the path templates marked inside prose t: each runs
// from its marker to the next whitespace, trailing punctuation left out (a
// shared path helper embedded in a sentence).
func markedSegments(t string) []string {
	var out []string
	for {
		i := strings.Index(t, pathTemplatePrefix)
		if i < 0 {
			return out
		}
		t = t[i+len(pathTemplatePrefix):]
		end := strings.IndexFunc(t, unicode.IsSpace)
		if end < 0 {
			end = len(t)
		}
		if seg := strings.TrimRight(t[:end], ",.;:)"); seg != "" {
			out = append(out, seg)
		}
		t = t[end:]
	}
}

// checkForkValue refuses, in a fork, a value bound into an operational field
// (a command's argv, dir or stdin, an effect's args, an observation's or
// DoneWhen's descriptor, an agent's workdir) that names a source root
// anywhere: it would act on the source run's world. The step is then stuck
// with that reason; nothing is rewritten. A typed path marker is checked at
// its resolved target: this run's own roots pass, an external target under a
// source root does not. This is the guard for untyped history
// (command output, agent results and what a definition derives from them),
// which the fork-time relocation check does not read (CheckJournalRelocation).
func (c *cycle) checkForkValue(v any, field string) error {
	if len(c.forkSources()) == 0 {
		return nil
	}
	if isPathMarker(v) {
		// A typed path is checked at its resolved target: one bound after
		// the fork to a source path is external, and its marker alone
		// proves nothing.
		resolved, err := c.resolvePathValues(v)
		if err != nil {
			return nil // the use reports its own resolution failure
		}
		if s, ok := resolved.(string); ok {
			return c.checkForkValue(s, field)
		}
		return nil
	}
	switch x := v.(type) {
	case string:
		if root := c.sourceRootIn(x); root != "" {
			return fmt.Errorf("%w: %s names %s, a root of the run this one was forked from: a fork never acts on its source's world", ErrUnavailable, field, root)
		}
	case map[string]any:
		for _, k := range sortedPathFields(x) {
			if err := c.checkForkValue(x[k], field+"."+k); err != nil {
				return err
			}
		}
	case []any:
		for i, e := range x {
			if err := c.checkForkValue(e, fmt.Sprintf("%s[%d]", field, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// isPathMarker reports whether v is one of the typed path representations
// (an owned path, a path parameter, a run path).
func isPathMarker(v any) bool {
	if _, marked, _ := markedOwnedPath(v); marked {
		return true
	}
	if _, marked, _ := markedPathParam(v); marked {
		return true
	}
	_, marked, _ := markedRunPath(v)
	return marked
}
