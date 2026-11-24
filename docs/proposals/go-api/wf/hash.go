package wf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Hash returns the definition hash: "sha256:" and the first 16 hex characters
// of the SHA-256 of the canonical JSON of Inspect(d) (encoding/json sorts map
// keys, so the encoding is canonical). It is a function of the same walk
// Render and Mermaid use, so anything shown is pinned.
//
// Closures are not part of the hash: route, check, predicate and compute
// bodies are pinned by build identity, not by this value (see the LANGUAGES §1
// packaging note). Effect and observation args are serialised with an
// injective, quoting encoding (strings quoted, slice elements and struct
// fields delimited), so two definitions that differ in any arg differ in
// render and hash. Edge targets, labels, effects (adapter, op, key and args),
// observations (adapter, op and args), predicates, budgets, case tables,
// route candidates (Route and OnRoute: labels, targets and effects, pinned
// like Cases edges), retry policies, prompts and every other shown field are.
func (d *Def) Hash() string {
	return hashView(Inspect(d))
}

func hashView(view DefView) string {
	b, err := json.Marshal(view)
	if err != nil {
		panic("wf: hashing definition view: " + err.Error()) // plain strings and ints cannot fail to marshal
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// definitionHash permits a per-invocation asset choice without mutating the shared definition.
func (e *Engine) definitionHash(d *Def) string {
	return definitionHashAssets(d, assetRoots(d, e.opts.Assets))
}

func definitionHashAssets(d *Def, roots map[string]string) string {
	return definitionHashAssetsIn(d, roots, func(root string) assetFS { return hostAssets(root) })
}

// definitionHashAssetsIn is definitionHashAssets reading each root's assets
// through open(root).
func definitionHashAssetsIn(d *Def, roots map[string]string, open func(root string) assetFS) string {
	// Structural inspection reads no asset: every read below goes through
	// open. A definition without a selected root reads its declared (or
	// inherited) one, as Inspect would.
	v := inspectStructure(d)
	var walk func(*DefView, string)
	walk = func(v *DefView, prefix string) {
		key := prefix + v.Name
		root, ok := roots[key]
		if !ok {
			root = v.assetRoot
		}
		v.Assets, _ = assetContentsIn(root, v.States, open(root))
		for i := range v.Children {
			walk(&v.Children[i], key+"/")
		}
	}
	walk(&v, "")
	return hashView(v)
}
