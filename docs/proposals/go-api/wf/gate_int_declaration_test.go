package wf

import (
	"encoding/json"
	"strings"
	"testing"
)

type intDeclGate struct{}
type intDeclDone struct{}

func intDeclaration(res Reservation) (*Def, *Gate[intDeclGate, string]) {
	d := New("integer-declaration")
	g := NewGate[intDeclGate](d, "Extend?", NewEnum(d, "choice", "extend", "stop"), res)
	end := NewTerminal[intDeclDone](d, Success)
	g.Answers(On("extend", g.To(end, "extend")), On("stop", g.To(end, "stop")))
	return d, g
}
func TestGateIntDeclarationHashAndLint(t *testing.T) {
	plain, _ := intDeclaration(Reserved)
	raw, _ := json.Marshal(Inspect(plain))
	if strings.Contains(string(raw), "IntValues") {
		t.Fatal("untyped gate changed hash shape")
	}
	d, g := intDeclaration(Reserved)
	g.IntValue("extend", 1, 100, func(Ctx) int { return 25 }).IntMinimum("extend", "progress-v1", func(Ctx) int { return 2 })
	if len(d.Lint()) != 0 {
		t.Fatal(d.Lint())
	}
	if d.Hash() == plain.Hash() || !strings.Contains(d.Render(), "progress-v1") {
		t.Fatal("integer metadata missing from identity/render")
	}
	renamed, rg := intDeclaration(Reserved)
	rg.IntValue("extend", 1, 100, func(Ctx) int { return 25 }).IntMinimum("extend", "progress-v2", func(Ctx) int { return 2 })
	if renamed.Hash() == d.Hash() {
		t.Fatal("dynamic minimum identity not hashed")
	}
	for name, configure := range map[string]func(*Gate[intDeclGate, string]){
		"inverted": func(g *Gate[intDeclGate, string]) { g.IntValue("extend", 2, 1, func(Ctx) int { return 1 }) },
		"unknown":  func(g *Gate[intDeclGate, string]) { g.IntValue("missing", 1, 2, func(Ctx) int { return 1 }) },
		"nil":      func(g *Gate[intDeclGate, string]) { g.IntValue("extend", 1, 2, nil) },
		"duplicate": func(g *Gate[intDeclGate, string]) {
			g.IntValue("extend", 1, 2, func(Ctx) int { return 1 })
			g.IntValue("extend", 1, 2, func(Ctx) int { return 1 })
		},
		"minimum-before-value": func(g *Gate[intDeclGate, string]) { g.IntMinimum("extend", "bound", func(Ctx) int { return 1 }) },
		"unnamed-minimum": func(g *Gate[intDeclGate, string]) {
			g.IntValue("extend", 1, 2, func(Ctx) int { return 1 }).IntMinimum("extend", "", func(Ctx) int { return 1 })
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, g := intDeclaration(Reserved)
			configure(g)
			if len(d.Lint()) == 0 {
				t.Fatal("invalid integer declaration accepted")
			}
		})
	}
	policy, pg := intDeclaration(PolicyAnswerable)
	pg.IntValue("extend", 1, 2, func(Ctx) int { return 1 })
	if len(policy.Lint()) == 0 {
		t.Fatal("policy integer declaration accepted")
	}
}

// An integer answer is named in a prose line of the gate's instructions,
// which escapes it to keep that line whole; lint refuses an answer the
// escape would change, so the person never reads an answer other than the
// one the command takes. A plain answer, and one with ordinary Unicode, is
// accepted.
func TestGateIntLintRefusesAnswersTheEscapeChanges(t *testing.T) {
	for name, tc := range map[string]struct {
		answer string
		bad    bool
	}{
		"plain":        {"extend", false},
		"unicode":      {"verlängern 🙂", false},
		"line-break":   {"ext\nend", true},
		"carriage":     {"extend\r", true},
		"tab":          {"ext\tend", true},
		"nul":          {"ext\x00end", true},
		"c1-control":   {"ext\u0085end", true},
		"invalid-utf8": {"ext\xffend", true},
	} {
		t.Run(name, func(t *testing.T) {
			d := New("integer-escape")
			g := NewGate[intDeclGate](d, "Extend?", NewEnum(d, "choice", tc.answer, "stop"), Reserved)
			end := NewTerminal[intDeclDone](d, Success)
			g.Answers(On(tc.answer, g.To(end, "extend")), On("stop", g.To(end, "stop")))
			g.IntValue(tc.answer, 1, 100, func(Ctx) int { return 25 })
			var hit bool
			for _, p := range d.Lint() {
				hit = hit || p.Rule == RuleGateAnswers && strings.Contains(p.Msg, "holds a control character or invalid UTF-8")
			}
			if hit != tc.bad {
				t.Errorf("answer %q: lint flags it %v, want %v: %v", tc.answer, hit, tc.bad, d.Lint())
			}
		})
	}
}
