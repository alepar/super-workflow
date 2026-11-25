package wf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
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
	return definitionHashAssets(d, nil)
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
//
// For a definition handed to an engine, the definition's part of the hash
// is computed once per *Def per process (inspectStructure and hashPlanOf).
// Every call still reads the assets it references, so a changed asset
// changes the hash within one process, and the view is marshalled again only
// for asset contents not seen before.
func definitionHashAssetsIn(d *Def, roots map[string]string, open func(root string) assetFS) string {
	// Structural inspection reads no asset: every read below goes through
	// open. A definition without a selected root reads its declared (or
	// inherited) one, as Inspect would.
	v := inspectStructure(d)
	p := hashPlanOf(d, v)
	var assets []map[string]string
	var walk func(*DefView, string)
	walk = func(v *DefView, prefix string) {
		key := prefix + v.Name
		root, ok := roots[key]
		if !ok {
			root = v.assetRoot
		}
		v.Assets, _ = readAssetRefs(root, p.refs[len(assets)], open(root))
		assets = append(assets, v.Assets)
		for i := range v.Children {
			walk(&v.Children[i], key+"/")
		}
	}
	walk(&v, "")
	if p.hashes == nil {
		return hashView(v) // not frozen: nothing is kept
	}
	b, err := json.Marshal(assets)
	if err != nil {
		panic("wf: keying definition assets: " + err.Error()) // string maps cannot fail to marshal
	}
	key := string(b)
	p.mu.Lock()
	h, ok := p.hashes[key]
	p.mu.Unlock()
	if ok {
		return h
	}
	if onHashCompute != nil {
		onHashCompute(d)
	}
	h = hashView(v)
	p.mu.Lock()
	p.hashes[key] = h
	p.mu.Unlock()
	return h
}

// hashPlan is what a definition's hash reuses across calls: each view's
// asset references, collected once, depth first in the order the hash walks
// the views, and the hashes computed so far by the asset contents read.
type hashPlan struct {
	refs [][]stateAssetRefs

	mu     sync.Mutex
	hashes map[string]string // JSON of the views' asset contents to the hash
}

var hashPlans sync.Map // *Def to *hashPlan

// onHashCompute, when set (tests), sees each definition hash computed
// rather than reused.
var onHashCompute func(*Def)

// hashPlanOf is d's plan, made from v (inspectStructure(d)) when none is
// kept: kept for a frozen definition, made afresh with no hashes kept for
// one that may still be under construction.
func hashPlanOf(d *Def, v DefView) *hashPlan {
	frozen := d.frozen.Load()
	if p, ok := hashPlans.Load(d); ok && frozen {
		return p.(*hashPlan)
	}
	p := &hashPlan{}
	var walk func(DefView)
	walk = func(v DefView) {
		p.refs = append(p.refs, assetRefs(v.States))
		for _, c := range v.Children {
			walk(c)
		}
	}
	walk(v)
	if !frozen {
		return p
	}
	p.hashes = map[string]string{}
	actual, _ := hashPlans.LoadOrStore(d, p)
	return actual.(*hashPlan)
}
