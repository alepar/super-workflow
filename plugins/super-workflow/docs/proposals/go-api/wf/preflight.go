package wf

import "strings"

// PreflightCheck is one named host preflight check: a name plus a closure over
// a PreflightContext that returns a Refusal, or nil when the host fact holds.
// (Named PreflightCheck because Check[O] already names an output validation.)
//
// Owns: one host fact that must hold before a run may exist, and the words
// that explain a refusal and its fix.
// Does not: write the journal, create the state dir, edit any profile or
// sandbox setting, or print; the preflight runner prints and picks the exit
// code. Every probe failure becomes a Refusal whose Cause carries the
// underlying message; there is no separate error channel.
// Extend by: a new host check is one more PreflightCheck value appended to
// Registration.Preflight, built once per binary at registration and stateless.
// PreflightCheck is a struct holding a closure, not an interface: there is
// nothing to implement.
type PreflightCheck struct {
	Name string
	Run  func(PreflightContext) *Refusal
}

// PreflightContext is what a check reads: host facts only. The definition a
// check needs is closed over when the check is built. The CLI fills it from the
// same resolved harness and --model-map values it passes to EngineOptions when
// it creates the run, so a check sees exactly what the run will journal.
type PreflightContext struct {
	Workspace string            // absolute
	Harness   string            // resolved harness
	ModelMap  map[string]string // the run's --model-map
	// Params are the run params resolved before preflight: those given with
	// --param and those derived (e.g. base), as the run will journal them.
	// Checks must not modify it.
	Params map[string]string
}

// Refusal explains why a check failed and how to fix it. A Warn refusal is
// advice, not a refusal: the runner prints it as a warning and goes on, for a
// host fact that may not matter to this run (a cache the run may never write).
type Refusal struct {
	Cause  string
	Remedy string
	Warn   bool
}

// modelTiersCheck is the engine's built-in model-tiers check over the served
// definition (child definitions reachable from it). It refuses only a
// malformed --model-map (checkModelMap: an unknown key or effort; a value is
// checked by resolveTiers, for the harness that starts or binds): a tier with
// no model is a hint the run reports as a degraded record, never a refusal
// (A3c). Resolution is resolveTiers, the same call run creation makes.
func modelTiersCheck(def *Def) PreflightCheck {
	return PreflightCheck{Name: "model-tiers", Run: func(c PreflightContext) *Refusal {
		err := checkModelMap(def, c.ModelMap)
		if err == nil {
			// The start harness is the one that binds the first segment, so
			// resolveTiers (the call run creation makes) checks its values.
			_, _, err = resolveTiers(c.Harness, collectTiers(def), c.ModelMap)
		}
		if err != nil {
			return &Refusal{Cause: strings.TrimPrefix(err.Error(), errBadModelMap.Error()+": "), Remedy: "fix --model-map: keys are a tier (light, standard, strong, top), all, <tier>.effort or all.effort, or a model the refusal lists; a value the harness takes (agy: its tier names)"}
		}
		return nil
	}}
}
