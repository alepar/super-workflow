package wf

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/sink"
)

func TestCoordinatorHandoffRequiresTheWholeFrontier(t *testing.T) {
	co := Coordinator{Harness: "claude", Session: "main"}
	for _, tc := range []struct {
		name        string
		transient   bool
		other       *instance
		instruction InstructionKind
		want        bool
	}{
		{name: "sole outside effect", instruction: InstructionManualVerification, want: true},
		{name: "transient outside effect", instruction: InstructionManualVerification, transient: true},
		{name: "outside effect with ready work", instruction: InstructionManualVerification, other: &instance{ID: "sibling", Phase: phaseReady}},
		{name: "outside effect with handed work", instruction: InstructionManualVerification,
			other: &instance{ID: "sibling", Phase: phaseIssued, Unit: &unit{State: UnitHanded}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := &instance{Cur: "effect", Phase: phaseReady,
				Cmd: &cmdIntent{Seq: 2, Effect: EffectData{Outside: true}}}
			st := &runState{
				started: &RunStartedData{}, order: []string{""}, insts: map[string]*instance{"": in},
				turn: &coordinatorTurnState{generation: 1, attachment: coordinatorAttachment{Harness: co.Harness, Session: co.Session}},
			}
			if tc.other != nil {
				st.order = append(st.order, tc.other.ID)
				st.insts[tc.other.ID] = tc.other
			}
			handoff := coordinatorHandoff{seq: 3, acknowledged: true, data: CoordinatorHandoffData{Admission: 1, Frontiers: []CoordinatorHandoffFrontier{{State: in.Cur, Effect: 2}}}}
			c := &cycle{caller: co, st: st}
			if got := c.turnHandoffCurrent(handoff); got != (tc.other == nil) {
				t.Fatalf("current handoff = %t with sibling %v", got, tc.other)
			}
			if !tc.want {
				// In these cases the handoff must be declined before journaling.
				ins := []Instruction{{Kind: tc.instruction, Effect: 2, Person: true, Transient: tc.transient}}
				if err := c.prepareTurnHandoff(ins); err != nil || ins[0].HandoffReceipt != 0 {
					t.Fatalf("prepared handoff %+v, %v", ins[0], err)
				}
			}
		})
	}
}

func TestCoordinatorTurnDoesNotEndAtALoneChildStall(t *testing.T) {
	co := Coordinator{Harness: "claude", Session: "main"}
	st := &runState{
		started: &RunStartedData{},
		order:   []string{"", "child"},
		insts: map[string]*instance{
			"":      {Phase: phaseReady},
			"child": {ID: "child", Phase: phaseIssued, Unit: &unit{State: UnitStalled}},
		},
		turn: &coordinatorTurnState{generation: 1, attachment: coordinatorAttachment{Harness: co.Harness, Session: co.Session}},
	}
	c := &cycle{caller: co, st: st}
	if d := c.mayEndTurn(co); d.Decision != "continue" {
		t.Fatalf("child stall decision = %+v", d)
	}
}

func TestNativeTurnUsesPinnedWorkspaceAndFencedCommand(t *testing.T) {
	for _, named := range []bool{false, true} {
		workspace := filepath.Join(t.TempDir(), "work with spaces")
		state := filepath.Join(t.TempDir(), "state with spaces")
		registry := t.TempDir()
		def := New("native-turn")
		NewTerminal[struct{}](def, Success)
		cfg := MainConfig{Defs: []Registration{{Def: def}}}
		if named {
			cfg.Binary = "client"
		}
		c := &cli{cfg: cfg, env: func(key string) string {
			if key == "XDG_STATE_HOME" {
				return registry
			}
			return ""
		}}
		x, err := c.engine(state, EngineOptions{Workspace: workspace, RunID: "stop-run", Harness: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		e := x.(*Engine)
		run, err := e.Run(def, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		co := Coordinator{Harness: "codex", Session: "main"}
		if _, err := c.attachTurnBinding(e, state, run, co, turnAttachOptions{initial: true}); err != nil {
			t.Fatal(err)
		}

		c.workspace = "" // a native hook starts independently of the attach invocation
		const event = `{"hook_event_name":"Stop","session_id":"main","turn_id":"turn"}`
		if cmd, warning := c.nativeTurnResult("codex", t.TempDir(), strings.NewReader(event)); cmd != "" || warning != "" {
			t.Fatalf("mismatched workspace = %q, %q", cmd, warning)
		}
		cmd, warning := c.nativeTurnResult("codex", workspace, strings.NewReader(event))
		wantRun := " next stop-run "
		if named {
			wantRun = " wf next "
			if !strings.Contains(cmd, "--run-id stop-run") {
				t.Fatalf("named run flag missing from %q", cmd)
			}
		}
		for _, want := range []string{wantRun, "--expected-admission", sink.Quote(state), sink.Quote(workspace)} {
			if warning != "" || !strings.Contains(cmd, want) {
				t.Fatalf("native continuation = %q, %q, want %q", cmd, warning, want)
			}
		}
	}
}

type turnYieldJournal struct {
	Journal
	onYield func(RunID) error
}

func (j *turnYieldJournal) othersWaiting(RunID) bool { return j.onYield != nil }
func (j *turnYieldJournal) yieldLock(run RunID) error {
	inject := j.onYield
	j.onYield = nil
	return inject(run)
}

func TestCoordinatorFenceRechecksAfterYield(t *testing.T) {
	type work struct{}
	type terminal struct{}
	def := New("turn-yield")
	step := NewCompute[work, int](def, func(Ctx) (int, error) { return 1, nil })
	end := NewTerminal[terminal](def, Success)
	next := step.To(end, "done")
	step.Route(func(int, Ctx) Edge[work] { return next }, next)

	journal := &turnYieldJournal{Journal: MemJournal()}
	e := NewEngine(journal, nil, EngineOptions{Defs: []*Def{def}, RunID: "r", Harness: "claude"}).(*Engine)
	run, err := e.Run(def, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	co := Coordinator{Harness: "claude", Session: "A"}
	co.ExpectedAdmission, err = e.AttachCoordinator(run, co)
	if err != nil {
		t.Fatal(err)
	}
	journal.onYield = func(r RunID) error {
		data, err := json.Marshal(coordinatorAttachment{Harness: "claude", Session: "B", Segment: 1})
		if err != nil {
			return err
		}
		return journal.Append(JournalRecord{Run: r, Kind: JournalCoordinatorAttached, DefHash: def.Hash(), At: e.opts.Clock(), Data: data})
	}
	if _, err := e.HandOutAs(run, 1, co); !errors.Is(err, ErrRejected) {
		t.Fatalf("next after yield = %v, want stale attachment refusal", err)
	}
	unlock, err := journal.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	recs, err := journal.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	if last := recs[len(recs)-1]; last.Kind != JournalCoordinatorAttached {
		t.Fatalf("stale next wrote after the replacement: %+v", last)
	}
}

type turnWaitJournal struct {
	Journal
	watching chan struct{}
}

func (j *turnWaitJournal) Watch(RunID, <-chan struct{}) <-chan struct{} {
	close(j.watching)
	return nil // only the timeout wakes this fixture
}

type turnAgent struct{}
type turnEnd struct{}

func newTurnWaitRun(t *testing.T) (*Engine, *turnWaitJournal, RunID) {
	t.Helper()
	def := New("turn-wait")
	agent := NewAgent[turnAgent, string](def)
	end := NewTerminal[turnEnd](def, Success)
	next := agent.To(end, "done")
	agent.Spec(PromptText("Return a result."), Retry[turnAgent](Const(1), next))
	agent.Route(func(string, Ctx) Edge[turnAgent] { return next }, next)
	j := &turnWaitJournal{Journal: MemJournal(), watching: make(chan struct{})}
	state := t.TempDir()
	e := NewEngine(j, nil, EngineOptions{Defs: []*Def{def}, RunID: "r", Harness: "claude", StateRoot: state,
		RunDir: func(run RunID) string { return filepath.Join(state, "runs", string(run)) }}).(*Engine)
	run, err := e.Run(def, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return e, j, run
}

func waitForTurnWatch(t *testing.T, j *turnWaitJournal) {
	t.Helper()
	select {
	case <-j.watching:
	case <-time.After(2 * time.Second):
		t.Fatal("next never reached its watch")
	}
}

func TestCoordinatorTimeoutRefusesRelistAfterReplacement(t *testing.T) {
	e, j, run := newTurnWaitRun(t)
	a := Coordinator{Harness: "claude", Session: "A"}
	b := Coordinator{Harness: "claude", Session: "B"}
	id, err := e.AttachCoordinator(run, a)
	if err != nil {
		t.Fatal(err)
	}
	a.ExpectedAdmission = id
	if ins, err := e.HandOutAs(run, 1, a); err != nil || len(ins) != 1 || ins[0].Kind != InstructionAgent {
		t.Fatalf("handout = %v, %v", ins, err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := e.NextWait(run, 1, a, 100*time.Millisecond)
		result <- err
	}()
	waitForTurnWatch(t, j)
	if _, err := e.AttachCoordinator(run, b); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrRejected) {
		t.Fatalf("old next after timeout = %v, want stale attachment", err)
	}
	unlock, err := j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	recs, err := j.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	if last := recs[len(recs)-1]; last.Kind != JournalCoordinatorAttached {
		t.Fatalf("old next wrote after replacement: %s", last.Kind)
	}
}

func TestCoordinatorReturningSessionRespectsLiveClaimsAndRetiresOrphans(t *testing.T) {
	e, j, run := newTurnWaitRun(t)
	a := Coordinator{Harness: "claude", Session: "A"}
	b := Coordinator{Harness: "claude", Session: "B"}
	for _, co := range []Coordinator{a, b} {
		if _, err := e.AttachCoordinator(run, co); err != nil {
			t.Fatal(err)
		}
	}
	ins, err := e.HandOutAs(run, 1, b)
	if err != nil || len(ins) != 1 {
		t.Fatalf("hand-out = %v, %v", ins, err)
	}
	if err := e.Started(run, ins[0].Name, Claim{Harness: "claude", Session: "B", Agent: "child"}); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := e.NextWait(run, 1, b, 180*time.Millisecond)
		result <- err
	}()
	waitForTurnWatch(t, j)
	if _, err := e.AttachCoordinator(run, a); !errors.Is(err, ErrRejected) {
		t.Fatalf("A took over a live B: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, err := e.AttachCoordinator(run, a); err != nil {
		t.Fatalf("A could not replace departed B: %v", err)
	}
	fresh, err := e.HandOutAs(run, 1, a)
	if err != nil || len(fresh) != 1 || fresh[0].Kind != InstructionAgent || fresh[0].Name == ins[0].Name {
		t.Fatalf("B's orphan was not replaced: %v, %v", fresh, err)
	}
}

func TestCoordinatorUnavailableEffectRetryDoesNotAdvanceProgress(t *testing.T) {
	in := &instance{Cmd: &cmdIntent{Seq: 9}}
	st := &runState{}
	result := &EffectResultData{Intent: 9, Outcome: "unavailable"}
	check := st.turnProgress(JournalRecord{Kind: JournalEffectResult}, result, in)
	in.Cmd.Result = result
	if check != nil && check() {
		t.Fatal("closing an unavailable effect advanced progress before its retry")
	}
}

func TestCoordinatorLegacyOutsideIntentIsAnAuthoritativeHandoff(t *testing.T) {
	co := Coordinator{Harness: "claude", Session: "main"}
	pending := &pendingEdge{Open: 2} // old intent lacks the Outside bit
	in := &instance{Cur: "effect", Phase: phaseTransition, Pending: pending, def: New("legacy")}
	st := &runState{started: &RunStartedData{}, order: []string{""}, insts: map[string]*instance{"": in},
		turn: &coordinatorTurnState{generation: 1, attachment: coordinatorAttachment{Harness: co.Harness, Session: co.Session}}}
	c := &cycle{caller: co, st: st}
	st.turn.handoff = coordinatorHandoff{seq: 3, acknowledged: true, data: CoordinatorHandoffData{Admission: 1, Frontiers: []CoordinatorHandoffFrontier{{State: in.Cur, Effect: 2}}}}
	if d := c.mayEndTurn(co); d.Reason != "person_handoff" {
		t.Fatalf("old outside intent yielded %+v", d)
	}
}

func TestCoordinatorSwitchRefusesWhenPreviousBindingIsLost(t *testing.T) {
	registry := t.TempDir()
	env := func(string) string { return registry }
	co := Coordinator{Harness: "claude", Session: "main"}
	d := New("turn-registry")
	NewTerminal[turnEnd](d, Success)
	c := &cli{cfg: MainConfig{Defs: []Registration{{Def: d}}}, env: env}
	start := func() (*Engine, string) {
		t.Helper()
		state, workspace := t.TempDir(), t.TempDir()
		x, err := c.engine(state, EngineOptions{Workspace: workspace, RunID: "r", Harness: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		e := x.(*Engine)
		if _, err := e.Run(d, nil, nil); err != nil {
			t.Fatal(err)
		}
		return e, state
	}
	a, first := start()
	b, second := start()
	if _, err := c.attachTurnBinding(a, first, "r", co, turnAttachOptions{initial: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(registry, turnBindingDir, turnBindingName(co))); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attachTurnBinding(b, second, "r", co, turnAttachOptions{}); !errors.Is(err, ErrRejected) {
		t.Fatalf("unproven switch returned %v, want refusal", err)
	}
	_, unrelated := start()
	if _, err := c.attachTurnBinding(b, second, "r", co, turnAttachOptions{previousRun: "r", previousState: unrelated}); !errors.Is(err, ErrRejected) {
		t.Fatalf("accepted an unrelated previous journal: %v", err)
	}
	recovery := turnAttachOptions{previousRun: "r", previousState: first}
	if _, err := c.attachTurnBinding(b, second, "r", co, recovery); err != nil {
		t.Fatal(err)
	}
	if _, err := a.HandOutAs("r", 1, co); !errors.Is(err, ErrRejected) {
		t.Fatalf("explicit recovery left the old attachment active: %v", err)
	}
}

func TestCoordinatorPublicationFailureLeavesRecoverablePendingSelection(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires directory mode enforcement")
	}
	registry, workspace, state := t.TempDir(), t.TempDir(), t.TempDir()
	dir := filepath.Join(registry, turnBindingDir)
	co := Coordinator{Harness: "claude", Session: "main"}
	def := New("turn-publish")
	NewTerminal[turnEnd](def, Success)
	c := &cli{cfg: MainConfig{Defs: []Registration{{Def: def}}}, env: func(string) string { return registry }}
	x, err := c.engine(state, EngineOptions{Workspace: workspace, RunID: "r", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	e := x.(*Engine)
	run, err := e.Run(def, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var committed int64
	e.afterAppend = func(_ *cycle, rec JournalRecord) {
		if rec.Kind != JournalCoordinatorAttached {
			return
		}
		committed = rec.Seq
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	_, attachErr := c.attachTurnBinding(e, state, run, co, turnAttachOptions{initial: true})
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if committed == 0 || attachErr == nil || !strings.Contains(attachErr.Error(), "selection is pending") {
		t.Fatalf("published or failed before journal append: generation %d, error %v", committed, attachErr)
	}
	input := strings.NewReader(`{"hook_event_name":"Stop","session_id":"main"}`)
	if command, warning := c.nativeTurnResult("claude", workspace, input); command != "" || warning != "" {
		t.Fatalf("pending selection became active: %q, %q", command, warning)
	}
	if recovered, err := c.attachTurnBinding(e, state, run, co, turnAttachOptions{}); err != nil || recovered != committed {
		t.Fatalf("recovery returned %d, %v, want the existing generation %d", recovered, err, committed)
	}
}

func TestCoordinatorCapSurvivesRepeatedUnavailableEffectAttempts(t *testing.T) {
	def := New("turn-unavailable")
	NewTerminal[turnEnd](def, Success)
	j := MemJournal()
	e := NewEngine(j, nil, EngineOptions{Defs: []*Def{def}, RunID: "r", Harness: "claude"}).(*Engine)
	run, err := e.Run(def, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	co := Coordinator{Harness: "claude", Session: "main"}
	gen, err := e.AttachCoordinator(run, co)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if err := e.withTelemetry(run, func(c *cycle) error {
			in := c.st.insts[""]
			intent := c.nextSeq()
			if err := c.emit(in, JournalEffectIntent, 0, EffectIntentData{}); err != nil {
				return err
			}
			if err := c.emit(in, JournalEffectResult, 0, EffectResultData{Intent: intent, Outcome: "unavailable"}); err != nil {
				return err
			}
			return c.emit(in, JournalBackoffScheduled, Unavailable, BackoffScheduledData{})
		}); err != nil {
			t.Fatal(err)
		}
		stop, err := e.RecordCoordinatorStop(run, co, strconv.Itoa(i), "")
		if err != nil || stop.Progress != gen {
			t.Fatalf("attempt %d reset progress: %+v, %v", i, stop, err)
		}
		if i == 5 && stop.Reason != "continuation_stalled" {
			t.Fatalf("sixth Stop = %+v, want stall", stop)
		}
	}
}
