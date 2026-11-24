package wf_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	pfA     struct{}
	pfEnd   struct{}
	pfItem  struct{ Lane string }
	pfRes   struct{}
	pfOther struct{ X int }
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParsePromptRefShapes(t *testing.T) {
	for _, s := range []string{"dir/x.md", "/abs/x.md", "{root}/x.md", "dir/args.json#/prompts/scouts/correctness", "{root}/a.json#/p/{lane}", "a.json#/x~1y/z~0"} {
		r, err := wf.ParsePromptRef(s)
		if err != nil {
			t.Errorf("parse %q: %v", s, err)
			continue
		}
		if r.String() != s {
			t.Errorf("String = %q, want %q (unsubstituted)", r.String(), s)
		}
	}
	for _, s := range []string{"", "#/p", "a.json#", "a.json#p", "{unclosed/x.md", "x}.md", "{}/x.md", "a.json#/x~2"} {
		if _, err := wf.ParsePromptRef(s); err == nil {
			t.Errorf("parse %q: want a malformed error", s)
		}
	}
}

func TestPromptRefResolve(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "x.md", "file text")
	writeFile(t, ws, "args.json", `{"prompts":{"scouts":{"correctness":"scout text"},"a/b":"escaped","n":3,"list":["zero"]}}`)
	params := map[string]string{"lane": "correctness", "file": "x.md"}
	for _, tc := range []struct{ ref, want string }{
		{"x.md", "file text"},
		{filepath.Join(ws, "x.md"), "file text"},
		{"{file}", "file text"},
		{"args.json#/prompts/scouts/correctness", "scout text"},
		{"args.json#/prompts/scouts/{lane}", "scout text"},
		{"args.json#/prompts/a~1b", "escaped"},
		{"args.json#/prompts/list/0", "zero"},
	} {
		r, err := wf.ParsePromptRef(tc.ref)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.ref, err)
		}
		got, err := r.Resolve(params, ws)
		if err != nil || got != tc.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", tc.ref, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		ref  string
		want error
	}{
		{"missing.md", wf.ErrPromptNotFound},
		{"missing.json#/p", wf.ErrPromptNotFound},
		{"args.json#/prompts/nope", wf.ErrPromptNotFound},
		{"args.json#/prompts/list/7", wf.ErrPromptNotFound},
		{"{nobody}.md", wf.ErrPromptUnbound},
		{"args.json#/prompts/{nobody}", wf.ErrPromptUnbound},
	} {
		r, err := wf.ParsePromptRef(tc.ref)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.ref, err)
		}
		if _, err := r.Resolve(params, ws); !errors.Is(err, tc.want) {
			t.Errorf("Resolve(%q) error = %v, want %v", tc.ref, err, tc.want)
		}
	}
	r, _ := wf.ParsePromptRef("args.json#/prompts/n")
	if _, err := r.Resolve(params, ws); err == nil || errors.Is(err, wf.ErrPromptNotFound) {
		t.Errorf("non-string value: error = %v, want a non-not-found error", err)
	}
}

func TestPromptRefPathTemplateCompatibility(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "review.md", "review text")
	writeFile(t, ws, "args.json", `{"prompts":{"review":"selected review"}}`)
	for _, tc := range []struct{ name, ref, want string }{
		{"dynamic suffix", wf.PathTemplate("{records}/") + "review.md", "review text"},
		{"appended fragment", wf.PathTemplate("{records}/args.json") + "#/prompts/{lane}", "selected review"},
		{"replacement", strings.ReplaceAll(wf.PathTemplate("{worktree}/review.md"), "{worktree}", "{records}"), "review text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := wf.ParsePromptRef(tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ref.Resolve(map[string]string{"records": ws, "lane": "review"}, t.TempDir())
			if err != nil || got != tc.want {
				t.Fatalf("Resolve = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// promptForDef builds a typed child whose only agent uses PromptFor.
func promptForDef(sel func(pfItem) string, candidates []string) *wf.Def {
	d := wf.New("pf")
	a := wf.NewAgent[pfA, string](d)
	end := wf.NewTerminal[pfEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	done := a.To(end, "done")
	a.Spec(wf.PromptFor(sel, candidates, wf.ChildInput()), wf.Retry(wf.Const(1), done))
	a.Route(func(string, wf.Ctx) wf.Edge[pfA] { return done }, done)
	wf.Yield(end, func(wf.Ctx) pfRes { return pfRes{} })
	wf.Typed[pfItem, pfRes](d)
	return d
}

func byLane(i pfItem) string { return i.Lane }

func TestPromptForWalk(t *testing.T) {
	cands := []string{"dir/a.md", "dir/b.json#/p/q"}
	d := promptForDef(byLane, cands)
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint: %v", ps)
	}
	if !strings.Contains(d.Render(), "prompt: one of dir/a.md | dir/b.json#/p/q (by item)\n") {
		t.Errorf("render:\n%s", d.Render())
	}
	if got := wf.Inspect(d).States[0].PromptCandidates; !slices.Equal(got, cands) {
		t.Errorf("Inspect candidates = %v", got)
	}
	if d.Hash() == promptForDef(byLane, []string{"dir/a.md"}).Hash() {
		t.Error("the candidate list does not change the hash")
	}
}

func TestPromptForNeverRunsSel(t *testing.T) {
	d := promptForDef(func(pfItem) string { panic("sel ran") }, []string{"a.md"})
	d.Render()
	d.Hash()
	d.Lint()
	d.Mermaid()
	wf.Inspect(d)
}

func TestRulePromptCandidates(t *testing.T) {
	for name, cands := range map[string][]string{
		"empty":     nil,
		"duplicate": {"a.md", "a.md"},
		"malformed": {"a.json#bad"},
	} {
		t.Run(name, func(t *testing.T) {
			if ps := promptForDef(byLane, cands).Lint(); !hasRule(ps, wf.RulePromptCandidates) {
				t.Fatalf("rule did not fire: %v", ps)
			}
		})
	}
}

func TestPromptForOutsideMatchingChild(t *testing.T) {
	build := func(typed func(*wf.Def)) *wf.Def {
		d := wf.New("pf-bad")
		a := wf.NewAgent[pfA, string](d)
		end := wf.NewTerminal[pfEnd](d, wf.Success)
		done := a.To(end, "done")
		a.Spec(wf.PromptFor(byLane, []string{"a.md"}), wf.Retry(wf.Const(1), done))
		a.Route(func(string, wf.Ctx) wf.Edge[pfA] { return done }, done)
		typed(d)
		return d
	}
	if ps := build(func(*wf.Def) {}).Lint(); !hasRule(ps, wf.RuleBuild) {
		t.Errorf("untyped def: %v", ps)
	}
	other := build(func(d *wf.Def) { wf.Typed[pfOther, pfRes](d) })
	if ps := other.Lint(); !hasRule(ps, wf.RuleBuild) {
		t.Errorf("other input type: %v", ps)
	}
}

func TestPromptForNilSelector(t *testing.T) {
	if ps := promptForDef(nil, []string{"a.md"}).Lint(); !hasRule(ps, wf.RuleBuild) {
		t.Errorf("nil selector: %v", ps)
	}
}

func TestDefinitionsWithoutPromptForOmitCandidates(t *testing.T) {
	d, _, _, _ := okDef("no-prompt-for")
	b, err := json.Marshal(wf.Inspect(d))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "PromptCandidates") {
		t.Error("PromptCandidates reaches the hash input of a definition that does not use it")
	}
}

func TestJournalPromptOffCandidates(t *testing.T) {
	if got := wf.JournalPromptOffCandidates.String(); got != "prompt_off_candidates" {
		t.Errorf("String = %q", got)
	}
}
