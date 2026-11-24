package wf

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var assetReference = regexp.MustCompile(`\{assets\}/([^\s"'` + "`" + `#{}<>|,;\)\]]+)`)

// assetFS reads assets by their root-relative name.
type assetFS interface {
	Stat(name string) (os.FileInfo, error)
	ReadFile(name string) ([]byte, error)
}

// hostAssets reads assets under root as the host's paths, links followed:
// the engine's own reads (lint, the definition hash).
type hostAssets string

func (r hostAssets) Stat(name string) (os.FileInfo, error) {
	p := filepath.Join(string(r), name)
	if h := hostAssetReadHook; h != nil {
		h(p)
	}
	return os.Stat(p)
}
func (r hostAssets) ReadFile(name string) ([]byte, error) {
	p := filepath.Join(string(r), name)
	if h := hostAssetReadHook; h != nil {
		h(p)
	}
	return os.ReadFile(p)
}

// hostAssetReadHook, when set (tests), sees each host asset read's path.
var hostAssetReadHook func(path string)

// assetContents walks declarations, never directory entries or user closures.
// Directory references bind normally but do not include their children.
func assetContents(root string, states []StateView) (map[string]string, []Problem) {
	return assetContentsIn(root, states, hostAssets(root))
}

// assetContentsIn is assetContents reading through fsys.
func assetContentsIn(root string, states []StateView, fsys assetFS) (map[string]string, []Problem) {
	var contents map[string]string
	var problems []Problem
	seen := map[string]bool{}
	for _, s := range states {
		var refs, texts []string
		add := func(value string, prompt bool) {
			if _, tail, ok := strings.Cut(value, "="); ok && strings.HasPrefix(tail, "{assets}/") {
				value = tail
			}
			if strings.HasPrefix(value, "{assets}/") {
				path := strings.TrimPrefix(value, "{assets}/")
				if prompt {
					path, _, _ = strings.Cut(path, "#")
				}
				refs = append(refs, path)
			} else {
				texts = append(texts, value)
			}
		}
		if kind, ok := s.node.kind.(*agentKind); ok {
			if kind.prompt.path != "" {
				add(kind.prompt.path, true)
			}
			for _, ref := range kind.prompt.candidates {
				add(ref, true)
			}
			texts = append(texts, kind.prompt.text)
			for _, input := range kind.prompt.inputs {
				desc := input.inputRef()
				if desc.kind == "bound" {
					add(desc.tmpl, false)
				}
			}
		}
		for _, arg := range s.Argv {
			add(arg, false)
		}
		addArgs := func(args map[string]any) {
			values, err := genericArgs(args)
			if err != nil {
				problems = append(problems, s.node.problem(RuleBuild, "asset arguments: %v", err))
				return
			}
			var walk func(any)
			walk = func(value any) {
				switch v := value.(type) {
				case string:
					add(v, false)
				case []any:
					for _, item := range v {
						walk(item)
					}
				case map[string]any:
					for _, item := range v {
						walk(item)
					}
				}
			}
			walk(values)
		}
		for _, read := range nodeReads(s.node) {
			addArgs(read.Args)
		}
		addEffects := func(effects []Effect) {
			for _, effect := range effects {
				if external, ok := effect.(ExternalEffect); ok {
					addArgs(external.Args)
				}
			}
		}
		for _, use := range s.uses {
			addEffects(use.effects)
		}
		for _, edge := range s.node.edges {
			if edge.when != nil && !s.node.routable(edge) {
				addEffects(edge.effects)
			}
		}
		for _, text := range texts {
			for _, match := range assetReference.FindAllStringSubmatch(text, -1) {
				refs = append(refs, match[1])
			}
		}
		for _, rel := range refs {
			if seen[rel] {
				continue
			}
			seen[rel] = true
			if contents == nil {
				contents = map[string]string{}
			}
			contents[rel] = "unavailable"
			var err error
			switch {
			case root == "":
				err = fmt.Errorf("declare Assets(root)")
			case !filepath.IsLocal(rel):
				err = fmt.Errorf("path must stay within the asset root")
			default:
				var info os.FileInfo
				info, err = fsys.Stat(rel)
				if err == nil {
					if info.IsDir() {
						contents[rel] = "directory"
					} else {
						var data []byte
						data, err = fsys.ReadFile(rel)
						if err == nil {
							contents[rel] = "sha256:" + hashBytes(data)
						}
					}
				}
			}
			if err != nil {
				problems = append(problems, s.node.problem(RuleBuild, "asset %q: %v", rel, err))
			}
		}
	}
	return contents, problems
}

func ruleAssets(c *lintCtx) []Problem {
	root := c.d.assets
	for i := len(c.ancestors) - 1; root == "" && i >= 0; i-- {
		root = c.ancestors[i].assets
	}
	_, problems := assetContents(root, c.states)
	for _, p := range c.d.params {
		if strings.EqualFold(p.name, "assets") {
			problems = append(problems, Problem{Rule: RuleBuild, Msg: "assets is an engine binding, not a run parameter"})
		}
		if strings.EqualFold(p.name, scratchName) {
			problems = append(problems, Problem{Rule: RuleBuild, Msg: "scratch is an engine binding, not a run parameter"})
		}
	}
	return problems
}

func effectiveAssets(d *Def, override string) string {
	root := override
	if root == "" {
		root = d.assets
	}
	if root == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return root
	}
	return abs
}

// lintServedAssets checks the selected root without mutating a definition shared by callers.
func (d *Def) lintServedAssets(root string) []Problem {
	return d.lintServedAssetRoots(assetRoots(d, root))
}

func (d *Def) lintServedAssetRoots(roots map[string]string) []Problem {
	selected := append([]rule(nil), rules...)
	selected[0] = func(c *lintCtx) []Problem {
		names := make([]string, 0, len(c.ancestors)+1)
		for _, ancestor := range c.ancestors {
			names = append(names, ancestor.name)
		}
		names = append(names, c.d.name)
		_, ps := assetContents(roots[strings.Join(names, "/")], c.states)
		return ps
	}
	return append(lintDef(d, "", map[*Def]bool{}, selected, true), lintDef(d, "", map[*Def]bool{}, servedRules, false)...)
}

// assetRoots captures each definition's effective root, including inherited
// roots. It preserves child definitions with independent asset declarations.
func assetRoots(d *Def, override string) map[string]string {
	roots := map[string]string{}
	var walk func(DefView, string)
	walk = func(v DefView, prefix string) {
		key := prefix + v.Name
		root := v.assetRoot
		if override != "" {
			root = override
		}
		if root != "" {
			roots[key] = effectiveAssets(nil, root)
		}
		for _, child := range v.Children {
			walk(child, key+"/")
		}
	}
	walk(inspectStructure(d), "")
	return nilIfEmpty(roots)
}

func (c *cycle) selectedAssetRoots(d *Def) map[string]string {
	roots := assetRoots(d, c.e.opts.Assets)
	if c.e.opts.Assets != "" || c.st.started == nil {
		return roots
	}
	if roots == nil {
		roots = map[string]string{}
	}
	for name, root := range c.st.started.AssetRoots {
		roots[name] = root
	}
	// Legacy migrated records carry only the explicitly selected root.
	if len(c.st.started.AssetRoots) == 0 && c.st.started.Assets != "" {
		return assetRoots(d, c.st.started.Assets)
	}
	return nilIfEmpty(roots)
}

// assetProblems is every asset d's states read that roots do not hold, as
// definitionHashAssets walks them. Every definition is checked, one with no
// root against the empty root, so a state reading {assets}/ there is a
// problem too.
func assetProblems(d *Def, roots map[string]string) []Problem {
	var out []Problem
	var walk func(DefView, string)
	walk = func(v DefView, prefix string) {
		key := prefix + v.Name
		_, problems := assetContents(roots[key], v.States)
		out = append(out, problems...)
		for _, child := range v.Children {
			walk(child, key+"/")
		}
	}
	walk(Inspect(d), "")
	return out
}
