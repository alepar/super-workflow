package wftest

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestReservedGateBlockedApplicabilityRequiresTotalStaticAnswers(t *testing.T) {
	for _, mode := range []string{"static", "guarded", "dynamic", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			d := wf.New("gate-applicability")
			answers := wf.NewEnum(d, "answers", "yes", "no")
			gate := wf.NewGate[coverageSource, string](d, "Proceed?", answers, wf.Reserved)
			end := wf.NewTerminal[coverageEnd](d, wf.Success)
			d.OnFailure(wf.Blocked, end)
			yes := gate.To(end, "yes")
			if mode == "guarded" {
				options := wf.Options[struct{ Allowed bool }](d)
				yes = yes.When(wf.Field(options, func(o *struct{ Allowed bool }) *bool { return &o.Allowed }).Is(true))
			}
			row := wf.On("yes", yes)
			if mode == "dynamic" {
				row = wf.OnRoute("yes", func(wf.Ctx) wf.Edge[coverageSource] { return wf.Edge[coverageSource]{} }, yes)
			}
			gate.Answers(row)
			if mode != "incomplete" {
				gate.Answers(row, wf.On("no", gate.To(end, "no")))
			}
			factory := func() *Sim { return New(d, WithTB(t)) }
			coverage := NewFuzzCoverage(factory, nil)
			var blocked string
			for key, edge := range coverage.edgeViews {
				if edge.Class == "blocked" {
					blocked = key
				}
			}
			if blocked == "" {
				t.Fatal("missing inherited blocked route")
			}
			if na := coverage.EdgeNotApplicable[blocked] != ""; na != (mode == "static") {
				t.Errorf("blocked n/a = %v for %s answers", na, mode)
			}
			if mode == "guarded" || mode == "dynamic" {
				trace := factory().Run(Answer(gate, "yes"))
				AssertTerminal(t, trace, end)
				coverage.Add(&FuzzResult{End: "success", Trace: trace})
				if coverage.EdgesReached[blocked] != 1 {
					t.Fatalf("blocked route was not traversed: %v", coverage.EdgesReached)
				}
				for _, problem := range coverage.EdgeProblems() {
					if strings.Contains(problem, blocked) {
						t.Errorf("traversed blocked route rejected: %s", problem)
					}
				}
			}
		})
	}
}

func TestCoverageSeparatesClassInapplicableDefaults(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("applicability")
		a := wf.NewCompute[coverageSource, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
		end := wf.NewTerminal[coverageEnd](d, wf.Success)
		d.OnFailure(wf.Uncertain, end)
		d.OnFailure(wf.Blocked, end)
		a.Then(a.To(end, "done"))
		return New(d)
	}
	c := NewFuzzCoverage(factory, nil)
	c.Add(Fuzz(factory, FuzzConfig{MaxSteps: 4}))
	if len(c.EdgeNotApplicable) != 1 || !strings.Contains(c.String(), "n/a by class") {
		t.Fatalf("missing applicability partition: %s", c)
	}
	problems := strings.Join(c.EdgeProblems(), "\n")
	if strings.Contains(problems, "uncertain") || !strings.Contains(problems, "blocked") {
		t.Fatalf("unsound applicability: %s", problems)
	}
	for key := range c.EdgeNotApplicable {
		c.EdgesReached[key] = 1
		if !strings.Contains(strings.Join(c.EdgeProblems(), "\n"), "n/a by class") {
			t.Fatal("traversed n/a edge hid classifier bug")
		}
	}
}

func TestExplicitFailureEdgeRemainsCoverageObligation(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("explicit-class")
		a := wf.NewCompute[coverageSource, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
		end := wf.NewTerminal[coverageEnd](d, wf.Success)
		a.Retry(wf.Retry(wf.Const(1), a.To(end, "fallback")).OnClass(wf.Uncertain, a.To(end, "explicit uncertain")))
		a.Then(a.To(end, "done"))
		return New(d)
	}
	c := NewFuzzCoverage(factory, nil)
	if len(c.EdgeNotApplicable) != 0 || !strings.Contains(strings.Join(c.EdgeProblems(), "\n"), "explicit uncertain") {
		t.Fatalf("explicit obligation removed: %s", c)
	}
}

func TestUncertainApplicabilityDistinguishesRerunAndEdgeEffects(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		idempotent, edge, na bool
	}{
		{"keyed", false, false, false},
		{"idempotent", true, false, true},
		{"idempotent-with-shell-edge", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := wf.New("command-applicability")
			cmd := wf.NewCommand[coverageSource](d, "true").Key("command")
			if tc.idempotent {
				cmd.Idempotent()
			}
			end := wf.NewTerminal[coverageEnd](d, wf.Success)
			d.OnFailure(wf.Uncertain, end)
			edge := cmd.To(end, "done")
			if tc.edge {
				edge = edge.With(wf.ExternalEffect{Adapter: "shell", Op: "run", Key: wf.KeyOf("edge")})
			}
			cmd.Then(edge)
			state := wf.Inspect(d).States[0]
			for i, e := range state.Edges {
				if e.Class == "uncertain" && (state.DefaultEdgeInapplicable(i) != "") != tc.na {
					t.Fatalf("class applicability: %+v", state)
				}
			}
		})
	}
}

func TestStaleApplicabilityRequiresIssueTimeObservation(t *testing.T) {
	for _, observed := range []bool{false, true} {
		d := wf.New("stale-applicability")
		a := wf.NewAgent[coverageSource, bool](d)
		end := wf.NewTerminal[coverageEnd](d, wf.Success)
		d.OnFailure(wf.Stale, end)
		a.Spec(wf.PromptText("work"), wf.Retry[coverageSource](wf.Const(1), a.To(end, "stop")))
		if observed {
			a.Observes(wf.Observe[bool]("world", "read", nil))
		}
		a.Then(a.To(end, "done"))
		state := wf.Inspect(d).States[0]
		for i, e := range state.Edges {
			if e.Class == "stale" && (state.DefaultEdgeInapplicable(i) == "") != observed {
				t.Fatalf("observation=%v: %+v", observed, state)
			}
		}
	}
}

func TestAgentUnavailableApplicabilityRequiresPureInlinePrompt(t *testing.T) {
	for _, mode := range []string{"inline", "check", "check-observation", "dynamic-check", "input-observation", "prompt-input", "param", "child-input", "output", "computed", "aliased-output", "artifact", "bound-path", "pure-and-observed", "prompt-file", "dynamic-prompt", "done-when", "edge-effect", "explicit"} {
		t.Run(mode, func(t *testing.T) {
			d := wf.New("agent-unavailable")
			a := wf.NewAgent[coverageSource, bool](d)
			end := wf.NewTerminal[coverageEnd](d, wf.Success)
			d.OnFailure(wf.Unavailable, end)
			prompt := wf.PromptText("work")
			obs := wf.Observe[bool]("world", "read", nil)
			var checks []wf.Check[bool]
			check := wf.Checkf("availability", func(bool, wf.Ctx) error { return wf.ErrUnavailable })
			switch mode {
			case "check":
				checks = append(checks, check)
			case "check-observation":
				checks = append(checks, check.Observes(obs))
			case "dynamic-check":
				checks = append(checks, check.ObservesFrom(func(bool) []wf.Reader { return []wf.Reader{obs} }))
			case "input-observation":
				a.Observes(obs)
			case "prompt-input":
				prompt = wf.PromptText("work", wf.Bound("input", "value"))
			case "param":
				prompt = wf.PromptText("work", d.Param("goal"))
			case "child-input":
				prompt = wf.PromptText("work", wf.ChildInput())
			case "output", "aliased-output", "pure-and-observed":
				prior := wf.NewAgent[struct{ Prior bool }, string](d)
				input := wf.Out(prior)
				if mode == "aliased-output" {
					input = wf.As("previous", input)
				}
				prompt = wf.PromptText("work", input)
				if mode == "pure-and-observed" {
					a.Observes(obs)
				}
			case "computed":
				prior := wf.NewCompute[struct{ Prior bool }, string](d, func(wf.Ctx) (string, error) { return "value", nil })
				prompt = wf.PromptText("work", wf.Out(prior))
			case "artifact":
				prompt = wf.PromptText("work", wf.NewArtifact[string](a, "input"))
			case "bound-path":
				prompt = wf.PromptText("work", wf.Bound("input", "{records}/file"))
			case "prompt-file":
				prompt = wf.PromptFile("work.md")
			case "dynamic-prompt":
				prompt = wf.PromptFor(func(int) string { return "work.md" }, []string{"work.md"})
			case "done-when":
				wf.DoneWhen(a, obs, "true")
			}
			retry := wf.Retry[coverageSource](wf.Const(1), a.To(end, "stop"))
			if mode == "explicit" {
				retry = retry.OnClass(wf.Unavailable, a.To(end, "explicit unavailable"))
			}
			a.Spec(prompt, retry, checks...)
			edge := a.To(end, "done")
			if mode == "edge-effect" {
				edge = edge.With(wf.ExternalEffect{Adapter: "world", Op: "write", Key: wf.KeyOf("write")})
			}
			a.Then(edge)
			pure := mode == "inline" || mode == "prompt-input" || mode == "param" || mode == "child-input" || mode == "output" || mode == "computed" || mode == "aliased-output"
			// Only the subject agent has an inherited Unavailable route under test.
			state := wf.Inspect(d).States[0]
			for i, edge := range state.Edges {
				if edge.Class == "unavailable" && (state.DefaultEdgeInapplicable(i) != "") != pure {
					t.Errorf("Unavailable n/a = %v, want %v", state.DefaultEdgeInapplicable(i) != "", pure)
				}
			}
		})
	}
}

func TestMapUnavailableApplicabilityExcludesExternalEdges(t *testing.T) {
	for _, mode := range []string{"values", "observation", "edge-effect", "explicit"} {
		t.Run(mode, func(t *testing.T) {
			child := wf.New("child")
			result := wf.NewTerminal[coverageEnd](child, wf.Success)
			wf.Yield(result, func(wf.Ctx) bool { return true })
			d := wf.New("map-unavailable")
			source := wf.FromValues(1)
			if mode == "observation" {
				source = wf.FromObservation(wf.Observe[[]int]("world", "items", nil))
			}
			m := wf.NewMap[coverageSource, int, int, bool](d, source, func(i int) int { return i }, wf.Typed[int, bool](child))
			end := wf.NewTerminal[coverageEnd](d, wf.Success)
			d.OnFailure(wf.Unavailable, end)
			d.OnFailure(wf.Blocked, end)
			met := m.To(end, "met")
			if mode == "edge-effect" {
				met = met.With(wf.ExternalEffect{Adapter: "world", Op: "write", Key: wf.KeyOf("write")})
			}
			if mode == "explicit" {
				m.OnClass(wf.Unavailable, m.To(end, "explicit unavailable").Unreachable("no unavailable operation"))
			}
			m.Join(wf.AllOK(), met, m.To(end, "partial"))
			assertClassApplicability(t, d, "unavailable", mode == "values" || mode == "observation")
			assertClassApplicability(t, d, "blocked", false)
		})
	}
}

func assertClassApplicability(t *testing.T, d *wf.Def, class string, wantNA bool) {
	t.Helper()
	c := NewFuzzCoverage(func() *Sim { return New(d) }, nil)
	found := false
	for key, edge := range c.edgeViews {
		if edge.Class != class {
			continue
		}
		found = true
		if got := c.EdgeNotApplicable[key] != ""; got != wantNA {
			t.Errorf("%s n/a = %v, want %v", key, got, wantNA)
		}
	}
	if !found {
		t.Fatalf("missing %s route", class)
	}
}
