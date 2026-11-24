package wf_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type pathOutputState struct{}
type pathOutputEnd struct{}
type pathOutputNested struct {
	Path *wf.RunPath       `json:"path"`
	Next *pathOutputNested `json:"next"`
}

func outputPathDef[O any](binding string) *wf.Def {
	d := wf.New("output-path")
	a := wf.NewAgent[pathOutputState, O](d)
	end := wf.NewTerminal[pathOutputEnd](d, wf.Success)
	artifact := wf.NewArtifact[string](a, "spec.md")
	var inputs []wf.Input
	if binding != "" {
		inputs = append(inputs, wf.As(binding, wf.Bound("original", wf.ArtifactPath(artifact))))
	}
	a.Spec(wf.PromptText("Write the artifact", inputs...), wf.Retry(wf.Const(1), a.To(end, "failed")))
	a.Then(a.To(end, "done"))
	d.OnFailure(wf.Blocked, end)
	return d
}

func TestAgentOutputPathLint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		def   *wf.Def
		field string
	}{
		{"bound", outputPathDef[struct {
			Spec string `json:"spec,omitempty"`
		}]("spec"), "spec"},
		{"typed root", outputPathDef[wf.RunPath](""), "$"},
		{"nested", outputPathDef[struct {
			Items []pathOutputNested `json:"items"`
		}](""), "items[].path"},
		{"map", outputPathDef[map[string]wf.RunPath](""), "$.*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found := false
			for _, p := range tc.def.Lint() {
				if p.Rule == wf.RuleID("agent-output-path") {
					found = true
					if !strings.Contains(p.Msg, tc.field) {
						t.Errorf("missing field %q: %v", tc.field, p)
					}
				}
			}
			if !found {
				t.Fatal("artifact path output passed lint")
			}
		})
	}
}

func TestAgentOutputPathLintAllowsContentAndIgnoredFields(t *testing.T) {
	type ignored struct {
		Path   wf.RunPath `json:"-"`
		Text   string     `json:"text"`
		hidden wf.RunPath
	}
	for _, d := range []*wf.Def{outputPathDef[ignored]("spec"), outputPathDef[struct {
		Spec string `json:"spec"`
	}]("")} {
		for _, p := range d.Lint() {
			if p.Rule == wf.RuleID("agent-output-path") {
				t.Fatal(p)
			}
		}
	}
}
