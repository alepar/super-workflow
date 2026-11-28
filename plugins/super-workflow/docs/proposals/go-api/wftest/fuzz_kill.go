package wftest

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// FuzzKillKind is a loss above the journal: the coordinator session or a
// child goes away, or stops answering, while it holds a unit. Each kind is
// recovered with the verbs a coordinator or operator has, never by
// rewriting the journal.
type FuzzKillKind string

const (
	// KillAfterHandOut: the coordinator dies right after next handed the
	// unit out, before it spawned anything. A new session reports the old
	// one died (heartbeat died --session) and drives on.
	KillAfterHandOut FuzzKillKind = "coordinator-died-after-hand-out"
	// KillAfterStarted: the coordinator dies after started, before the
	// child fetched its prompt. Recovered as KillAfterHandOut.
	KillAfterStarted FuzzKillKind = "coordinator-died-after-started"
	// KillWhileRunning: the coordinator session and its child vanish
	// after the prompt fetch. Recovered as KillAfterHandOut.
	KillWhileRunning FuzzKillKind = "coordinator-died-while-running"
	// KillAfterDone: the coordinator dies right after the child's done.
	// Recovered as KillAfterHandOut.
	KillAfterDone FuzzKillKind = "coordinator-died-after-done"
	// KillChildDied: the child dies holding its claim after the prompt
	// fetch; the live coordinator reports it (heartbeat died <name>).
	KillChildDied FuzzKillKind = "child-died-holding-claim"
	// KillSpawnRefused: the harness refuses the spawn. The coordinator
	// holds the hand-out and starts nothing; after the unstarted grace the
	// same session's next offers the unit again.
	KillSpawnRefused FuzzKillKind = "spawn-refused"
	// KillTurnEnded: the coordinator ends its turn while the child runs and
	// stays silent for two hours. The engine reports the stall; the next
	// turn answers it (heartbeat active) and the child finishes.
	KillTurnEnded FuzzKillKind = "coordinator-turn-ended-while-running"
)

// FuzzKillKinds lists every kind, in the order Check sweeps them.
var FuzzKillKinds = []FuzzKillKind{KillAfterHandOut, KillAfterStarted, KillWhileRunning, KillAfterDone, KillChildDied, KillSpawnRefused, KillTurnEnded}

// FuzzKill selects one kill at the first agent instruction of State (a
// qualified state, as FuzzFault.State) whose action is a submit. The run is
// all-good from the kill on: it still owes the success or human gate an
// unkilled run owes, with no retry spent on the killed unit and its losses
// only of the kind's expected causes.
type FuzzKill struct {
	State string
	Kind  FuzzKillKind
}

// killGap is how long the coordinator is away after a death or a refused
// spawn: past the hand-out (2m) and spawn (3m) windows and the unstarted
// grace (5m), so whatever the lost session held has expired.
const killGap = 6 * time.Minute

// turnGap is how long a turn-ended coordinator is away: past the default
// stall threshold (20m) and the implicit lease (30m).
const turnGap = 2 * time.Hour

// killCauses are the attempt-lost causes a kind may journal for the killed
// unit. A cause outside the set is a finding.
var killCauses = map[FuzzKillKind][]string{
	KillAfterHandOut: nil,
	KillAfterStarted: {wf.LostCauseSpawn, wf.LostCauseOrphan},
	KillWhileRunning: {wf.LostCauseOrphan},
	KillAfterDone:    nil,
	KillChildDied:    {wf.LostCauseDied},
	KillSpawnRefused: nil,
	KillTurnEnded:    nil,
}

// killing reports whether the action for in is the configured kill.
func (f *fuzzRun) killing(in wf.Instruction, a FuzzAction) bool {
	k := f.result.config.Kill
	return k != nil && !f.result.KillInjected && a.Kind == FuzzSubmit && f.stateKey(in) == k.State
}

// kill injects the configured kill on in, whose action is a, and recovers
// from it with the coordinator's or operator's verbs. Driving on afterwards
// is the ordinary loop's: it hands the unit out again.
func (f *fuzzRun) kill(in wf.Instruction, a FuzzAction) error {
	k := f.result.config.Kill
	f.result.KillInjected = true
	records := f.runner.trace().Journal()
	f.killName, f.killFrom = in.Name, len(records)
	// The unit's own hand-out names its instance and state as the journal
	// records them.
	for i := len(records) - 1; i >= 0; i-- {
		var h wf.HandedOutData
		if records[i].Kind == wf.JournalHandedOut && json.Unmarshal(records[i].Data, &h) == nil && h.Name == in.Name {
			f.killInstance, f.killState = records[i].Instance, records[i].State
			break
		}
	}
	if !f.good {
		// A prefix of other outcomes reached the state: all-good from here.
		f.good = true
		f.sim.world.setFuzzGood(true)
		f.result.LivenessStart = f.step
	}
	transcript := f.transcriptPath(in.Name)
	switch k.Kind {
	case KillAfterHandOut:
		return f.newSession()
	case KillAfterStarted:
		if err := f.engine.Started(f.runner.id, in.Name, f.claimOf(in, transcript)); err != nil {
			return err
		}
		return f.newSession()
	case KillWhileRunning:
		if err := f.claim(&in, transcript); err != nil {
			return err
		}
		return f.newSession()
	case KillAfterDone:
		if err := f.act(in, a); err != nil {
			return err
		}
		return f.newSession()
	case KillChildDied:
		if err := f.claim(&in, transcript); err != nil {
			return err
		}
		f.now = f.now.Add(time.Minute)
		return f.engine.HeartbeatAs(f.runner.id, wf.HeartbeatTarget{Name: in.Name}, false, f.coordinator())
	case KillSpawnRefused:
		f.now = f.now.Add(killGap)
		return nil
	case KillTurnEnded:
		if err := f.claim(&in, transcript); err != nil {
			return err
		}
		f.now = f.now.Add(turnGap)
		// The next turn hears what happened while it was away.
		res, err := f.engine.Wait(f.runner.id, 0, time.Millisecond)
		if err != nil {
			return err
		}
		stalled := false
		for _, ev := range res.Events {
			if ev.Kind == wf.EventStall && ev.Name == in.Name {
				stalled = true
			}
		}
		if !stalled {
			f.find("kill-stall-unreported", fmt.Sprintf("a child silent for %s after its coordinator's turn ended was not reported stalled", turnGap))
			return nil
		}
		if err := f.engine.HeartbeatAs(f.runner.id, wf.HeartbeatTarget{Name: in.Name}, true, f.coordinator()); err != nil {
			return err
		}
		return f.finish(in, a, transcript)
	}
	return fmt.Errorf("unknown kill kind %q", k.Kind)
}

// newSession replaces a dead coordinator session the way a supervisor does:
// after the dead session's windows ran out, the new session reports it died
// and drives on.
func (f *fuzzRun) newSession() error {
	old := f.coordinator().Session
	f.sessions++
	f.now = f.now.Add(killGap)
	return f.engine.HeartbeatAs(f.runner.id, wf.HeartbeatTarget{Session: old}, false, f.coordinator())
}

// coordinator is the live coordinator session.
func (f *fuzzRun) coordinator() wf.Coordinator {
	session := "fuzz-session"
	if f.sessions > 0 {
		session = fmt.Sprintf("fuzz-session-%d", f.sessions+1)
	}
	return wf.Coordinator{Harness: "claude", Session: session}
}

// claimOf is the claim the live session's child starts in with.
func (f *fuzzRun) claimOf(in wf.Instruction, transcript string) wf.Claim {
	return wf.Claim{Harness: "claude", Session: f.coordinator().Session, Agent: in.Name, Transcript: transcript, Context: wf.ContextFresh}
}

// killProperties judges a run with an injected kill: no retry was spent on
// the killed unit, and it lost attempts only for the kind's causes.
func (f *fuzzRun) killProperties() {
	k := f.result.config.Kill
	if k == nil || !f.result.KillInjected || f.result.Finding != nil {
		return
	}
	records := f.result.Trace.Journal()
	if f.killFrom > len(records) {
		return
	}
	for _, r := range records[f.killFrom:] {
		switch r.Kind {
		case wf.JournalRefused:
			var d wf.RefusedData
			if json.Unmarshal(r.Data, &d) == nil && d.Spent && r.Instance == f.killInstance && r.State == f.killState {
				f.findAt(k.State, "kill-retry-spent", fmt.Sprintf("%s spent a retry of %s; a loss above the journal must not", k.Kind, f.killName))
				return
			}
		case wf.JournalAttemptLost:
			var d wf.AttemptLostData
			if json.Unmarshal(r.Data, &d) == nil && d.Name == f.killName && !slices.Contains(killCauses[k.Kind], d.Cause) {
				f.findAt(k.State, "kill-lost-cause", fmt.Sprintf("%s lost %s as %q, want one of %q", k.Kind, f.killName, d.Cause, killCauses[k.Kind]))
				return
			}
		}
	}
}
