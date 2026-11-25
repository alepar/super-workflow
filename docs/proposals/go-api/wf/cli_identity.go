package wf

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// callerEnv is the process environment as the caller started it, before
// the state dir's env sidecar is applied: the sidecar is for the scripts a
// run executes (and may unset the harness variables for them), never for who
// the caller is.
func callerEnv() harness.Env {
	snap := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			snap[k] = v
		}
	}
	return func(k string) string { return snap[k] }
}

// envHarness is the harness the environment env shows.
func envHarness(env harness.Env) string { return harness.Detect(env) }

// envCoordinator is who a next comes from: the harness and the session of
// the caller's environment (A3a), and the harness's own subagent limit as its
// configuration for the caller's working directory sets it (E3). An
// environment that shows no harness leaves the harness empty: the run's own
// then applies.
func envCoordinator(env harness.Env) Coordinator {
	h := envHarness(env)
	if h == "generic" {
		return Coordinator{}
	}
	a := harness.For(h)
	who := a.Caller(env)
	co := Coordinator{Harness: h, Session: who.Session, Root: who.Root}
	if dir, err := os.Getwd(); err == nil {
		co.Limit, co.LimitSource = a.Limit(env, dir)
	}
	co.OutBytes, co.OutLines = a.OutputLimit(env)
	if limit := a.ToolLimit(env); limit > 0 {
		co.Wait = max(limit-nextWaitMargin, limit/2) // under the limit, however short
	}
	return co
}

// promptFromEnv is a child's prompt fetch with the identity its own
// environment gives (A1).
func promptFromEnv(e *Engine, run RunID, name, claim string, env harness.Env, whole bool) (PromptFetch, error) {
	who := harness.For(envHarness(env)).Caller(env)
	if whole {
		return e.PromptFetchWhole(run, name, claim, who)
	}
	return e.PromptFetchAs(run, name, claim, who)
}

// harnessContradiction is why run refuses a --harness value: the environment
// shows another harness (A3c). The message never suggests naming one.
func harnessContradiction(named string) string { return harness.Contradicts(named, os.Getenv) }

// nextWaitMargin is how far under its harness's tool limit a blocking next
// returns: time to print and exit before the harness cuts the call off. A
// limit under twice the margin waits half of it.
const nextWaitMargin = 10 * time.Second

// childUnitVerbs act on the child's own unit. Children can also use the
// read-only views shared with coordinators and people.
var childUnitVerbs = map[string]bool{"prompt": true, "read": true, "validate": true, "done": true, "failed": true}
var childReadVerbs = map[string]bool{"status": true, "explain": true, "journal": true, "lock": true}

// childCaller reports whether the caller's environment says it is a child
// of a coordinator (Codex's thread, Hermes's delegated-child marker): such a
// caller never drives the run, and its next is never taken for a newer
// coordinator.
func childCaller(env harness.Env) bool {
	// Any harness whose own variables say child: a Codex child inside a Hermes
	// terminal, or a Hermes delegate that inherited Codex's variables, is a
	// child whichever harness detection picks.
	for _, h := range harness.Names() {
		if harness.For(h).Caller(env).Is == harness.IsChild {
			return true
		}
	}
	return false
}

// childRefusal is what a child calling a coordinator verb is told.
func childRefusal() string {
	finish := "done"
	if hasValidate {
		finish = "validate and done"
	}
	return "you are a child of this run: finish your unit with " + finish + "; the coordinator handles the rest"
}

// hasValidate: the verb table has validate (set once the table exists; an
// init breaks the table's initialization cycle through drive).
var hasValidate bool

func init() { _, hasValidate = findRow("validate") }

// drivingVerbs are the run verbs an agent of the run's harness runs: its
// coordinator's loop and answers, and its children's verbs. A person's
// verbs (stop, resume, answer, settle, retry) and the read-only views are
// not among them (J1).
// A child's verbs are not among them either: its claim token binds it to
// its unit, so a claimed child of an earlier harness segment finishes under
// its own.
var drivingVerbs = map[string]bool{"next": true, "recover": true, "heartbeat": true, "started": true}

// driverMismatch is why this call is refused on run (J1): the run is driven
// from the harness of its first coordinator call, and this call's
// environment resolves to another, to none, or gives no session. "" when it
// matches or no coordinator has called yet. The records root is not checked
// here: bindSegment and checkSegment do it under the run's lock, where a
// known session under another root is served and an unknown one refused.
// A run whose lock stays held past the invocation's budget is an error, not a
// match.
func (c *cli) driverMismatch(stateDir string, run RunID) (string, error) {
	x, err := c.engine(stateDir, EngineOptions{})
	if err != nil {
		return "", nil
	}
	e, ok := x.(*Engine)
	if !ok {
		return "", nil
	}
	driver, err := e.driver(run)
	if errors.Is(err, ErrRunLocked) {
		return "", err
	}
	h := envHarness(c.env)
	who := harness.For(h).Caller(c.env)
	if who.FromEnv && who.Session == "" && who.Agent != "" && !TrustSessionlessCallers {
		// An agent the environment names with no coordinator session may be
		// a child (opencode's failed parent lookup): it never drives, not
		// even a segment no coordinator has bound yet.
		return mismatchNoSession(h, c.env), nil
	}
	if driver == "" {
		return "", nil
	}
	if h == driver && (who.Session != "" || TrustSessionlessCallers) {
		return "", nil
	}
	if h == driver {
		return mismatchNoSession(h, c.env), nil
	}
	return mismatchOtherHarness(driver, h), nil
}

// The ways a call is refused as a harness mismatch. Each is HarnessMismatch
// followed by what was expected and what this call shows, in plain words:
// the harness names, that a session is missing, and the names of the
// environment variables involved. Saying what is missing is guidance: a
// caller cannot supply what it is not told about. They print names only,
// never a variable's value, a session id or a records root, and only names
// this binary knows: a harness name reaches these from a caller (a
// library's Coordinator, a --harness flag, a journal another binary wrote),
// so one that no registered adapter answers to is never echoed
// (knownHarness).

// knownHarness is name as a refusal may print it: the name of an adapter
// this binary registers, else "" (no harness at all, or a string a caller
// supplied that the text must not repeat).
func knownHarness(name string) string {
	name = normalizeHarness(name)
	if name == "" || name == "generic" || !slices.Contains(harness.Names(), name) {
		return ""
	}
	return name
}

// unknownDriver stands for a segment's harness this binary has no adapter
// for; unknownCaller for a caller's.
const (
	unknownDriver = "a harness this binary does not know"
	unknownCaller = "another harness"
)

// mismatchOtherHarness: the segment is driven from driver and the call
// comes from got ("" or "generic": no harness at all).
func mismatchOtherHarness(driver, got string) string {
	from := knownHarness(driver)
	if from == "" {
		from = unknownDriver
	}
	got = normalizeHarness(got)
	switch caller := knownHarness(got); {
	case got == "" || got == "generic":
		detail := "this command's environment shows no agent harness"
		if vars := harnessVars(driver); len(vars) > 0 {
			detail += fmt.Sprintf(" (%s is recognised by %s, and none of them is set)", from, strings.Join(vars, ", "))
		}
		return fmt.Sprintf("%s. This part of the run is driven from %s; %s", HarnessMismatch, from, detail)
	case caller == "":
		return fmt.Sprintf("%s. This part of the run is driven from %s; this command comes from %s", HarnessMismatch, from, unknownCaller)
	default:
		return fmt.Sprintf("%s. This part of the run is driven from %s; this command's environment is %s's", HarnessMismatch, from, caller)
	}
}

// mismatchNoSession: the call's environment is the driving harness's but
// names no session. The adapter says which variables it is recognised by,
// not which of them carries the session, so the text lists the ones that
// are not set.
func mismatchNoSession(h string, env harness.Env) string {
	name := knownHarness(h)
	if name == "" {
		return fmt.Sprintf("%s. A coordinator session of the harness this part of the run is driven from is expected; this command's environment names no session", HarnessMismatch)
	}
	detail := fmt.Sprintf("this command's environment is %s's but names no session", name)
	var unset []string
	for _, v := range harnessVars(name) {
		if env == nil || env(v) == "" {
			unset = append(unset, v)
		}
	}
	if len(unset) > 0 {
		detail += fmt.Sprintf(": of the variables %s is recognised by, not set: %s", name, strings.Join(unset, ", "))
	}
	if sh, ok := harness.For(name).(harness.SessionHinter); ok && env != nil {
		if hint := sh.SessionHint(env); hint != "" {
			detail += ": " + hint
		}
	}
	return fmt.Sprintf("%s. A coordinator session of %s is expected; %s", HarnessMismatch, name, detail)
}

// mismatchUnknownSession: a session of the driving harness that the run has
// not seen, under another records root than its coordinator's.
func mismatchUnknownSession(h string) string {
	what := "a session"
	if name := knownHarness(h); name != "" {
		what = "a " + name + " session"
	}
	return fmt.Sprintf("%s. This is %s the run has not seen, under another records root (another profile or home) than the run's coordinator: admitted, it would replace that coordinator and retire its children. A session the run already knows may move between profiles", HarnessMismatch, what)
}

// harnessVars are the environment variables the adapter of harness name is
// recognised by (its Markers), nil for a name no registered adapter answers
// to and for an adapter with none.
func harnessVars(name string) []string {
	name = knownHarness(name)
	if name == "" {
		return nil
	}
	if d, ok := harness.For(name).(harness.Detector); ok {
		return d.Markers()
	}
	return nil
}

// TrustSessionlessCallers lets a CLI call whose harness environment names no
// session drive a run, and run a child verb with no claim token.
// It is for test suites that drive the CLI from a bare harness marker
// (CLAUDECODE alone); it is a Go variable, so no caller's environment can
// set it. Off, a session-less call is refused (J1).
var TrustSessionlessCallers bool

// HarnessMismatch begins J1's refusal of a coordinator verb. An
// agent told which harness drives the run once started that harness's CLI
// itself (fanout sweep bl-codex), so it says not to, and how a person moves
// a run to another harness. What follows it says what was expected and what
// the call shows (mismatchOtherHarness, mismatchNoSession,
// mismatchUnknownSession).
const HarnessMismatch = "this command is not running in the agent harness this part of the run is driven from: " + coordinatorStopReport + "; do not start another agent CLI to do this run's units, except through the engine's child dispatch. A person moves a run to another harness by stopping it and resuming it from there"
