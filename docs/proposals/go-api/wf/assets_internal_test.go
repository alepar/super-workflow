package wf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAssetsBindFromDefinitionNotRunInput(t *testing.T) {
	root := t.TempDir()
	parent := &instance{ID: "parent", def: New("parent").Assets(root)}
	child := &instance{ID: "child", Parent: "parent", def: New("child")}
	c := &cycle{st: &runState{started: &RunStartedData{Params: map[string]string{"assets": "/wrong"}}, insts: map[string]*instance{"parent": parent}}}
	got, err := c.scope(child).bindString("{assets}/script.sh")
	if err != nil || got != filepath.Join(root, "script.sh") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestAssetsHashBoundInputsAndCommandArguments(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"rules.md", "script.sh"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	d := New("assets").Assets(root)
	a := NewAgent[pfAssetAgent, string](d)
	a.Spec(PromptText("read [RULES]", Bound("RULES", "{assets}/rules.md")), RetryPolicy[pfAssetAgent]{})
	NewCommand[pfAssetCommand](d, "bash", "{assets}/script.sh")
	for _, name := range []string{"rules.md", "script.sh"} {
		before := d.Hash()
		if err := os.WriteFile(filepath.Join(root, name), []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
		if d.Hash() == before {
			t.Fatalf("%s content not hashed", name)
		}
	}
}

func TestAssetsLiteralHashAndFlagValuePaths(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"script", "script#v2.sh", "review", "review notes.md"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("before"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	d := New("argv-assets").Assets(root)
	NewCommand[pfAssetCommand](d, "bash", "{assets}/script#v2.sh", "--config={assets}/review notes.md")
	for _, name := range []string{"script#v2.sh", "review notes.md"} {
		before := d.Hash()
		if err := os.WriteFile(filepath.Join(root, name), []byte("after"), 0600); err != nil {
			t.Fatal(err)
		}
		if before == d.Hash() {
			t.Errorf("argv filename %q was truncated", name)
		}
	}
}

type pfAssetAgent struct{}
type pfAssetCommand struct{}

func TestSavedAssetRootsKeepIndependentChildRoots(t *testing.T) {
	parentRoot, childRoot := t.TempDir(), t.TempDir()
	parent := New("parent-assets").Assets(parentRoot)
	child := New("child-assets").Assets(childRoot)
	parent.children = []*Def{child}
	roots := assetRoots(parent, "")
	parent.Assets(t.TempDir())
	child.Assets(t.TempDir())
	c := &cycle{e: &Engine{}, st: &runState{started: &RunStartedData{Assets: parentRoot, AssetRoots: roots}}}
	got := c.selectedAssetRoots(parent)
	if got[parent.name] != parentRoot || got[parent.name+"/"+child.name] != childRoot {
		t.Fatalf("stored roots replaced: %v", got)
	}
}

func TestSavedAssetRootsDistinguishSharedChildScopes(t *testing.T) {
	leftRoot, rightRoot := t.TempDir(), t.TempDir()
	root, left, right, leaf := New("root"), New("left").Assets(leftRoot), New("right").Assets(rightRoot), New("leaf")
	root.children = []*Def{left, right}
	left.children = []*Def{leaf}
	right.children = []*Def{leaf}
	roots := assetRoots(root, "")
	for _, side := range []*Def{left, right} {
		parent := &instance{ID: side.name, def: side}
		child := &instance{ID: "leaf", Parent: side.name, def: leaf}
		c := &cycle{e: &Engine{}, st: &runState{started: &RunStartedData{AssetRoots: roots}, insts: map[string]*instance{"": {def: root}, side.name: parent, "leaf": child}}}
		got, err := c.scope(child).bindString("{assets}/prompt.md")
		if err != nil || got != filepath.Join(side.assets, "prompt.md") {
			t.Fatalf("%s binding = %q, %v", side.name, got, err)
		}
	}
	if roots["root/left/leaf"] != leftRoot || roots["root/right/leaf"] != rightRoot {
		t.Fatalf("collapsed scopes: %v", roots)
	}
}
