package wftest

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Unit stages a coordinator walks a handed unit through.
const (
	stageHanded  = iota // handed out, not started
	stageStarted        // started, prompt not fetched
	stageRunning        // started and fetched: the subagent is working
)

// tickStep is how far each loop step moves the fake clock.
const tickStep = time.Second

// workStep is how long a scripted subagent works before its Done, so a unit has
// a duration and the class stall threshold (3x its mean) is not zero.
const workStep = 30 * time.Second

// windowStep moves the clock past the hand-out (2m) and spawn (3m) windows, so a
// step that finds nothing to act on lets a lost unit expire.
const windowStep = 4 * time.Minute

// stallStep moves the clock past any stall threshold a short scenario folds.
const stallStep = time.Hour

// coordUnit is a unit the live coordinator session holds.
type coordUnit struct {
	ins     wf.Instruction
	agent   string
	stage   int
	stalled bool // its scripted subagent has stopped writing its transcript
}

// coordinator plays the paged coordinator of Cap: the session, the fake clock
// and transcripts, the units it holds and the verbs it calls.
//
// Owns: the session ids and the sessions that died, the fake clock and the
// transcript mtimes the engine reads, the units the live session holds and the
// armed AfterDone death.
// Does not: decide anything the engine decides; it hands out, starts, prompts,
// waits and finishes through the Executor verbs, as the coordinator prompt says.
// A held unit it never hears about (withdrawn with its siblings) keeps counting
// against its cap.
// Extend by: a new coordinator behaviour is a method here and a Step in wftest.go.
type coordinator struct {
	r         *runner
	cap       int
	clock     time.Time
	mtimes    map[string]time.Time
	session   string
	sessions  []string
	dead      []string
	down      bool
	agents    int
	since     int64 // the Seq the next Wait reports from
	held      []*coordUnit
	killDone  bool
	stallSeen map[string]bool
	blocked   []string // qualified states the engine reported the run held at (nothing moves it on without a human)
}

func newCoordinator(r *runner, cap int) *coordinator {
	c := &coordinator{
		r: r, cap: cap, clock: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC),
		mtimes: map[string]time.Time{}, stallSeen: map[string]bool{},
	}
	c.begin()
	return c
}

// begin opens the next session.
func (c *coordinator) begin() {
	c.session = fmt.Sprintf("wftest-%d", len(c.sessions)+1)
	c.sessions = append(c.sessions, c.session)
	c.down = false
}

// now is the engine's clock.
func (c *coordinator) now() time.Time { return c.clock }

// mtime is the engine's TranscriptMtime: a transcript the Sim has not written
// yet does not exist.
func (c *coordinator) mtime(path string) (time.Time, error) {
	if t, ok := c.mtimes[path]; ok {
		return t, nil
	}
	return time.Time{}, fs.ErrNotExist
}

// advance moves the clock by d and lets every working subagent write its
// transcript at the new time, so only a Stall step's unit goes quiet.
func (c *coordinator) advance(d time.Duration) {
	c.clock = c.clock.Add(d)
	for _, u := range c.held {
		if u.stage == stageRunning && !u.stalled {
			c.mtimes[transcriptOf(u.ins.Name)] = c.clock
		}
	}
}

func transcriptOf(name string) string { return "/wftest/" + name }

// call runs an engine call, reporting its error. It reports false when the
// engine crashed (an armed CrashAfterIntent) or the call failed.
func (c *coordinator) call(what string, f func() error) bool {
	var err error
	if c.r.guard(func() { err = f() }) {
		return false
	}
	if err != nil {
		c.r.fail("%s: %v", what, err)
		return false
	}
	return true
}

func (c *coordinator) unit(name string) *coordUnit {
	for _, u := range c.held {
		if u.ins.Name == name {
			return u
		}
	}
	return nil
}

func (c *coordinator) drop(name string) {
	c.held = slices.DeleteFunc(c.held, func(u *coordUnit) bool { return u.ins.Name == name })
}

// die ends the session: its subagents vanish and the engine hears nothing.
func (c *coordinator) die() {
	c.dead = append(c.dead, c.session)
	c.held, c.killDone, c.down = nil, false, true
}

// wait is one Wait with a short timeout: the coordinator learns which of its
// units finished or were lost, and answers a stall with Heartbeat(active).
func (c *coordinator) wait() bool {
	var res wf.WaitResult
	if !c.call("wait", func() (err error) {
		res, err = c.r.e.Wait(c.r.id, c.since, time.Millisecond)
		return err
	}) {
		return false
	}
	c.since = res.Seq
	for _, ev := range res.Events {
		switch ev.Kind {
		case wf.EventDone, wf.EventFailed, wf.EventWindow:
			c.drop(ev.Name)
		case wf.EventBlocked:
			c.blocked = append(c.blocked, ev.State) // the engine reported the run held at that state
		case wf.EventStall:
			c.stallSeen[ev.Name] = true
			u := c.unit(ev.Name)
			if u == nil {
				continue
			}
			if !c.call("heartbeat active "+ev.Name, func() error {
				return c.r.e.Heartbeat(c.r.id, wf.HeartbeatTarget{Name: ev.Name}, true)
			}) {
				return false
			}
			u.stalled = false
		}
	}
	return true
}

// handOut picks its own n (its harness cap less the units it holds; the engine
// sets no cap), passes it to HandOut and walks each unit through spawn, Started
// and Prompt. With stop set the coordinator dies at that point on the first
// unit it hands out, and a handed-out unit is required. It returns the
// instructions that need no subagent.
func (c *coordinator) handOut(stop KillPoint) (rest []wf.Instruction, ok bool) {
	var ins []wf.Instruction
	if !c.call("hand out", func() (err error) {
		ins, err = c.r.e.HandOut(c.r.id, max(c.cap-len(c.held), 0))
		return err
	}) {
		return nil, false
	}
	var fresh []*coordUnit
	for _, in := range ins {
		in, err := simulationHumanGate(in)
		if err != nil {
			c.r.fail("%v", err)
			return nil, false
		}
		if in.Kind != wf.InstructionAgent || in.Name == "" {
			rest = append(rest, in)
			continue
		}
		u := &coordUnit{ins: in}
		c.held = append(c.held, u)
		fresh = append(fresh, u)
	}
	if stop != 0 && len(fresh) == 0 {
		c.r.fail("KillCoordinator(%s): the coordinator has no unit to hand out (cap %d, holding %d)", stop, c.cap, len(c.held))
		return nil, false
	}
	if stop == AfterNext {
		c.die()
		return rest, true
	}
	for k, u := range fresh {
		c.agents++
		u.agent = fmt.Sprintf("agent-%d", c.agents)
		if k == 0 && stop == AfterSpawn {
			c.die()
			return rest, true
		}
		claim := wf.Claim{Harness: "wftest", Session: c.session, Agent: u.agent, Transcript: transcriptOf(u.ins.Name)}
		if !c.call("started "+u.ins.Name, func() error { return c.r.e.Started(c.r.id, u.ins.Name, claim) }) {
			return rest, false
		}
		u.stage = stageStarted
		if k == 0 && stop == AfterStarted {
			c.die()
			return rest, true
		}
		if !c.call("prompt "+u.ins.Name, func() error {
			f, err := c.r.e.(*wf.Engine).PromptFetch(c.r.id, u.ins.Name, "")
			u.ins.Prompt, u.ins.Inputs = f.Prompt, f.Inputs
			return err
		}) {
			return rest, false
		}
		u.stage = stageRunning
		c.mtimes[claim.Transcript] = c.clock
		if k == 0 && stop == WhileRunning {
			c.die()
			return rest, true
		}
	}
	return rest, true
}

// tick is one loop step: the clock moves, the coordinator hears what happened
// and hands out up to its own cap.
func (c *coordinator) tick() (rest []wf.Instruction, ok bool) {
	c.advance(tickStep)
	if !c.wait() {
		return nil, false
	}
	return c.handOut(0)
}

// play runs the script's steps.
func (c *coordinator) play() {
	r := c.r
	for i := 0; i < len(r.steps) && !r.stop; i++ {
		st := r.steps[i]
		switch {
		case st.kind == stepResume:
			r.e, r.dead = r.engine(), false
			c.tick() // re-fold the journal and drive what the crash left
		case r.dead:
			r.fail("script step %d (%s): the engine crashed and the script has no Resume() before it", i+1, st.desc)
		case st.kind == stepCrash || st.kind == stepCrashAt:
			r.arm(st)
			c.tick()
		case st.kind == stepKill:
			c.kill(i, st)
		case st.kind == stepNewCoordinator:
			c.newCoordinator()
		case st.kind == stepStall:
			c.stall(i, st)
		case st.kind == stepLoseSession:
			if c.down {
				r.fail("script step %d (%s): the coordinator is already dead", i+1, st.desc)
				continue
			}
			c.die()
		default:
			for n := 0; n < st.times && !r.stop && !r.dead; n++ {
				c.act(i, st)
			}
		}
	}
}

// live fails the script step i unless the coordinator is up.
func (c *coordinator) live(i int, st Step) bool {
	if c.down {
		c.r.fail("script step %d (%s): the coordinator is dead and the script has no NewCoordinator() before it", i+1, st.desc)
		return false
	}
	return true
}

func (c *coordinator) kill(i int, st Step) {
	if !c.live(i, st) {
		return
	}
	if st.at == AfterDone {
		c.killDone = true
		return
	}
	if st.at < AfterNext || st.at > WhileRunning {
		c.r.fail("script step %d (%s): unknown kill point", i+1, st.desc)
		return
	}
	c.advance(tickStep)
	if c.wait() {
		c.handOut(st.at)
	}
}

// newCoordinator opens a fresh session and reports each dead one died, after
// the windows of the units the dead session left have run out.
func (c *coordinator) newCoordinator() {
	if !c.down {
		c.die()
	}
	if len(c.dead) > 0 {
		// The replacement starts only after the dead session is noticed: long
		// enough for the hand-out and spawn windows of what it left to expire.
		c.advance(windowStep)
		if !c.wait() {
			return
		}
	}
	for len(c.dead) > 0 {
		old := c.dead[0]
		if !c.call("heartbeat died --session "+old, func() error {
			return c.r.e.Heartbeat(c.r.id, wf.HeartbeatTarget{Session: old}, false)
		}) {
			return
		}
		c.dead = c.dead[1:]
	}
	c.begin()
	c.tick()
}

// stall silences the named running unit and passes the clock beyond its
// threshold until the engine reports the stall; the coordinator's wait answers
// it with Heartbeat(active).
func (c *coordinator) stall(i int, st Step) {
	if !c.live(i, st) {
		return
	}
	var u *coordUnit
	for _, h := range c.held {
		if h.stage == stageRunning && (h.ins.Name == st.path || pathMatches(st.path, h.ins.State)) {
			u = h
			break
		}
	}
	if u == nil {
		c.r.fail("script step %d (%s) matches no running unit\n  holding: %s", i+1, st.desc, c.describeHeld(nil))
		return
	}
	name := u.ins.Name
	u.stalled, c.stallSeen[name] = true, false
	for try := 0; try < 3 && !c.stallSeen[name]; try++ {
		c.advance(stallStep)
		if !c.wait() {
			return
		}
	}
	if !c.stallSeen[name] {
		c.r.fail("script step %d (%s): the engine did not report %s stalled", i+1, st.desc, name)
	}
}

// act performs one repetition of a Submit, Answer or Settle step. A step that
// matches nothing lets the clock pass the windows, waits and retries once.
func (c *coordinator) act(i int, st Step) {
	if st.kind == stepValidate {
		c.r.fail("script step %d (%s): ValidateAt runs without Cap(n)", i+1, st.desc)
		return
	}
	if !c.live(i, st) {
		return
	}
	var u *coordUnit
	var in wf.Instruction
	for try := 0; ; try++ {
		rest, ok := c.tick()
		if !ok {
			return
		}
		found := false
		if st.kind == stepSubmit || st.kind == stepFail {
			u = c.running(st.path)
			found = u != nil
		} else {
			in, found = pick(st, rest)
		}
		if found {
			break
		}
		if try == 1 {
			c.r.fail("script step %d does not match the issued instruction\n  expected: %s\n  issued:   %s%s", i+1, st.desc, c.describeHeld(rest), c.blockedNote())
			return
		}
		c.advance(windowStep)
		if !c.wait() {
			return
		}
	}
	for _, cr := range st.crashes {
		c.r.arm(cr)
	}
	switch st.kind {
	case stepFail:
		if !c.fetchPages(i, st, u.ins.Name) {
			return
		}
		if !c.call(st.desc, func() error { _, err := c.r.e.FailedAs(c.r.id, u.ins.Name, st.reason, st.failure); return err }) {
			return
		}
		c.drop(u.ins.Name)
	case stepSubmit:
		if err := c.r.writeArtifacts(st, u.ins); err != nil {
			c.r.fail("%s: %v", st.desc, err)
			return
		}
		out, err := json.Marshal(st.value)
		if err != nil {
			c.r.fail("script step %d (%s): %v", i+1, st.desc, err)
			return
		}
		c.advance(workStep)
		if !c.fetchPages(i, st, u.ins.Name) {
			return
		}
		if !c.call(fmt.Sprintf("script step %d (%s)", i+1, st.desc), func() error {
			_, err := c.r.e.Done(c.r.id, u.ins.Name, wf.Submission{Output: out})
			return err
		}) {
			return
		}
		c.drop(u.ins.Name)
		if c.killDone {
			c.die()
		}
	case stepAnswer:
		c.call(fmt.Sprintf("script step %d (%s)", i+1, st.desc), func() error { return c.r.e.Answer(c.r.id, in.State, st.value.(string)) })
	case stepSettle:
		c.call(fmt.Sprintf("script step %d (%s)", i+1, st.desc), func() error {
			if st.result != nil {
				return simSettle(c.r.e, c.r.id, in.Effect, true, st.result)
			}
			return simSettle(c.r.e, c.r.id, in.Effect, st.applied, nil)
		})
	}
}

// blockedNote says which states the run halted at, for a script that ran into
// a halt.
func (c *coordinator) blockedNote() string {
	if len(c.blocked) == 0 {
		return ""
	}
	return "\n  (the run blocked at " + strings.Join(c.blocked, ", ") + ")"
}

// running is the first working unit whose state matches path.
func (c *coordinator) running(path string) *coordUnit {
	for _, u := range c.held {
		if u.stage == stageRunning && pathMatches(path, u.ins.State) {
			return u
		}
	}
	return nil
}

// describeHeld lists what the coordinator holds and the other open instructions.
func (c *coordinator) describeHeld(rest []wf.Instruction) string {
	var parts []string
	for _, u := range c.held {
		parts = append(parts, fmt.Sprintf("unit %q (%s, attempt %d, stage %d)", u.ins.Name, u.ins.State, u.ins.Attempt, u.stage))
	}
	if len(rest) > 0 {
		parts = append(parts, describe(rest))
	}
	if len(parts) == 0 {
		return "nothing (no open instruction)"
	}
	return strings.Join(parts, "; ")
}
