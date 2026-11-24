package wf_test

import (
	"path/filepath"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func assetDef(t *testing.T, root string) *wf.Def {
	t.Helper()
	d := promptForDef(byLane, []string{"{assets}/a.md", "{assets}/b.json#/prompt"})
	setter, ok := any(d).(interface{ Assets(string) *wf.Def })
	if !ok {
		t.Fatal("definition cannot declare its asset root")
	}
	setter.Assets(root)
	return d
}

// A prompt edit must invalidate a definition even when its reference is unchanged.
func TestAssetsHashNamedContentsOnly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.md", "first")
	writeFile(t, root, "b.json", `{"prompt":"second"}`)
	d := assetDef(t, root)
	before := d.Hash()
	writeFile(t, root, "unrelated.md", "not declared")
	if d.Hash() != before {
		t.Fatal("unreferenced file changed definition identity")
	}
	writeFile(t, root, "b.json", `{"prompt":"changed"}`)
	if d.Hash() == before {
		t.Fatal("candidate contents absent from definition identity")
	}
}

func TestAssetsHashIndependentOfRoot(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, root := range []string{a, b} {
		writeFile(t, root, "a.md", "first")
		writeFile(t, root, "b.json", `{"prompt":"second"}`)
	}
	if assetDef(t, a).Hash() != assetDef(t, b).Hash() {
		t.Fatal("identical assets differ by installation path")
	}
}

func TestAssetsHashFullDeclaredFilename(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "review", "prefix must not be mistaken for the asset")
	writeFile(t, root, "review notes,(final).md", "first")
	d := promptForDef(byLane, []string{"{assets}/review notes,(final).md"}).Assets(root)
	before := d.Hash()
	writeFile(t, root, "review notes,(final).md", "changed")
	if before == d.Hash() {
		t.Fatal("hash ignored the named file with spaces and punctuation")
	}
}

func TestAssetsMissingOrEscapingFileIsLintError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.md", "first")
	if len(assetDef(t, root).Lint()) == 0 {
		t.Fatal("missing candidate was not linted")
	}
	d := promptForDef(byLane, []string{"{assets}/../" + filepath.Base(root) + "/a.md"})
	setter, ok := any(d).(interface{ Assets(string) *wf.Def })
	if !ok {
		t.Fatal("definition cannot declare its asset root")
	}
	setter.Assets(root)
	if len(d.Lint()) == 0 {
		t.Fatal("asset traversal was not linted")
	}
}

func TestAssetsHashStructuredArguments(t *testing.T) {
	for _, kind := range []string{"effect", "observation"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "script", "prefix")
			writeFile(t, root, "script name.sh", "first")
			d := wf.New("assets").Assets(root)
			a := wf.NewAgent[pfA, string](d)
			end := wf.NewTerminal[pfEnd](d, wf.Success)
			edge := a.To(end, "done")
			invocation := shell.Run("bash", "{assets}/script name.sh")
			if kind == "observation" {
				a.Observes(shell.Observe(invocation))
			} else {
				edge = edge.With(shell.Effect(wf.KeyOf("script"), invocation))
			}
			a.Spec(wf.PromptText("work"), wf.Retry(wf.Const(1), edge))
			a.Then(edge)
			before := d.Hash()
			writeFile(t, root, "script name.sh", "changed")
			if d.Hash() == before {
				t.Fatal("hash ignored full structured asset filename")
			}
		})
	}
}
