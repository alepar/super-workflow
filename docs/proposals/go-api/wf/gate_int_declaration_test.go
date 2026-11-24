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
