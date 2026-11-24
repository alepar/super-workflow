package wf_test

import (
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// stallText is the text next's stall question for the run, as the coordinator
// reads it.
func stallText(t *testing.T, h *harness, run wf.RunID) string {
	t.Helper()
	res, err := h.e.(*wf.Engine).NextWait(run, 0, wf.Coordinator{}, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return wf.RenderNext(res, "", func(v string, a ...string) string { return strings.Join(append([]string{v}, a...), " ") })
}

// coordinatorActives are the coordinator's active answers, in journal order.
func coordinatorActives(t *testing.T, h *harness, run wf.RunID) []wf.HeartbeatData {
	t.Helper()
	var out []wf.HeartbeatData
	for _, hb := range heartbeats(t, h, run) {
		if hb.Status == "active" && hb.By == "coordinator" {
			out = append(out, hb)
		}
	}
	return out
}

// the regression case (journal Kind 32 from seq 8886): a child stuck on an approval
// prompt its coordinator could not see. At each lease expiry the coordinator
// answered active and the lease renewed with no progress, forever. An active
// answer with no progress since the previous one counts once; on the second
// in a row the question tells the coordinator to look in the child's own
// thread for a pending prompt or an error, and to report it died (lost, so
// the unit is offered again) if it sees neither. The count is journaled.
func TestRepeatedActiveWithoutProgressAsksForEvidence(t *testing.T) {
	d := laDef("la-attest", 5*time.Minute)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, ins := fetchedUnit(t, h, d)
	answer := func() {
		t.Helper()
		if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: ins.Name}, true); err != nil {
			t.Fatal(err)
		}
	}
	expire := func() string {
		t.Helper()
		h.now = h.now.Add(5 * time.Minute)
		h.handOut(run, 0)
		return stallText(t, h, run)
	}
	const evidence = "look in the child's own thread"

	if q := expire(); !strings.Contains(q, "stalled "+ins.Name) || strings.Contains(q, evidence) {
		t.Fatalf("first question:\n%s\nwant the plain stall question", q)
	}
	answer() // after the child's fetch: progress since, counts nothing
	if q := expire(); strings.Contains(q, evidence) {
		t.Fatalf("second question, after one active answer:\n%s\nwant the plain stall question", q)
	}
	answer() // nothing from the child since the last answer: the second in a row
	q := expire()
	if !strings.Contains(q, evidence) || !strings.Contains(q, "pending prompt") || !strings.Contains(q, "heartbeat died "+ins.Name) ||
		!strings.Contains(q, "heartbeat active "+ins.Name+" --note") || !strings.Contains(q, "asks again") {
		t.Fatalf("third question, after two active answers with no progress:\n%s\nwant it to ask for evidence with its three outcomes: observed work (--note), a prompt or error, inconclusive (asks again)", q)
	}
	if !strings.Contains(q, "2 times") {
		t.Errorf("the question does not say how many answers saw no progress:\n%s", q)
	}
	act := coordinatorActives(t, h, run)
	if len(act) != 2 || act[0].Unprogressed != 1 || act[1].Unprogressed != 2 {
		t.Fatalf("coordinator actives = %+v, want Unprogressed 1 then 2", act)
	}

	// The coordinator finds nothing and reports it died: the unit is lost and
	// offered again.
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: ins.Name}, false); err != nil {
		t.Fatal(err)
	}
	if lost := lostRecords(t, h, run); len(lost) != 1 || lost[0].Cause != wf.LostCauseDied {
		t.Fatalf("losses = %+v, want one died", lost)
	}
	if again := h.handOut(run, 1); len(again) != 1 {
		t.Fatalf("the lost unit was not offered again: %+v", again)
	}
}

// A sign of life from the child between answers is progress: the count starts
// over, and the plain question comes back.
func TestChildProgressResetsTheAttestationCount(t *testing.T) {
	d := laDef("la-attest-reset", 8*time.Minute)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, ins := fetchedUnit(t, h, d)
	answer := func() {
		t.Helper()
		if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: ins.Name}, true); err != nil {
			t.Fatal(err)
		}
	}
	expire := func() string {
		t.Helper()
		h.now = h.now.Add(8 * time.Minute)
		h.handOut(run, 0)
		return stallText(t, h, run)
	}
	expire()
	answer()
	expire()
	answer()
	// The child validates past a quarter of its lease: a heartbeat by child.
	h.now = h.now.Add(3 * time.Minute)
	if _, err := h.e.Validate(run, ins.Name, wf.Submission{Output: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if q := expire(); strings.Contains(q, "look in the child's own thread") {
		t.Fatalf("question after the child's own sign of life:\n%s\nwant the plain stall question", q)
	}
	answer()
	if act := coordinatorActives(t, h, run); act[len(act)-1].Unprogressed != 1 {
		t.Fatalf("the answer after progress counted %d, want 1", act[len(act)-1].Unprogressed)
	}
}

// laAttested is a fetched unit of laDef under a 5m lease, with the
// coordinator's active answer, the lease expiry and the question as helpers.
func laAttested(t *testing.T, name string) (h *harness, run wf.RunID, ins wf.Instruction, answer func(note string), expire func() string) {
	t.Helper()
	d := laDef(name, 5*time.Minute)
	h = newHarnessOpts(t, d, nil, lsOpts())
	run, ins = fetchedUnit(t, h, d)
	answer = func(note string) {
		t.Helper()
		if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: ins.Name, Note: note}, true); err != nil {
			t.Fatal(err)
		}
	}
	expire = func() string {
		t.Helper()
		h.now = h.now.Add(5 * time.Minute)
		h.handOut(run, 0)
		return stallText(t, h, run)
	}
	return h, run, ins, answer, expire
}

// Round 2 (1): a transcript write is the harness's file, not an engine event
// from the child (rule 5): it renews liveness but is not progress, so the
// count runs on through it.
func TestTranscriptWriteIsNotProgress(t *testing.T) {
	d := lsDef("la-attest-transcript", true)
	m := newTranscripts()
	h := newHarnessOpts(t, d, nil, lvOpts(m, 0))
	run, ins := handOneUnit(t, h, d)
	lvRunning(t, h, m, run, ins)
	stall := func() string {
		t.Helper()
		h.now = h.now.Add(21 * time.Minute)
		h.handOut(run, 0)
		return stallText(t, h, run)
	}
	answer := func() {
		t.Helper()
		if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: ins.Name}, true); err != nil {
			t.Fatal(err)
		}
	}
	stall()
	answer()
	m.set("/t/a.jsonl", h.now.Add(time.Minute)) // only the transcript moved
	h.now = h.now.Add(2 * time.Minute)
	h.handOut(run, 0)
	stall()
	answer()
	if act := coordinatorActives(t, h, run); len(act) != 2 || act[1].Unprogressed != 2 {
		t.Fatalf("coordinator actives = %+v, want the second at Unprogressed 2 despite the transcript write", act)
	}
	if q := stall(); !strings.Contains(q, "look in the child's own thread") {
		t.Fatalf("question after two answers with only a transcript write between:\n%s\nwant the evidence question", q)
	}
}

// Round 2 (2): any engine event from the child is progress, even one the
// lease debounce would not journal: a validate one second after an answer
// resets the count.
func TestValidateRightAfterAnAnswerIsProgress(t *testing.T) {
	h, run, ins, answer, expire := laAttested(t, "la-attest-validate")
	expire()
	answer("")
	expire()
	answer("")
	h.now = h.now.Add(time.Second)
	if _, err := h.e.Validate(run, ins.Name, wf.Submission{Output: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if q := expire(); strings.Contains(q, "look in the child's own thread") {
		t.Fatalf("question after a validate:\n%s\nwant the plain stall question", q)
	}
	answer("")
	if act := coordinatorActives(t, h, run); act[len(act)-1].Unprogressed != 1 {
		t.Fatalf("the answer after the validate counted %d, want 1", act[len(act)-1].Unprogressed)
	}
}

// Round 2 (2): an active answer counts at most once per stall episode: the
// same question answered twice counts one.
func TestOneCountPerStallEpisode(t *testing.T) {
	h, run, _, answer, expire := laAttested(t, "la-attest-episode")
	expire()
	answer("")
	answer("")
	if act := coordinatorActives(t, h, run); len(act) != 2 || act[0].Unprogressed != 1 || act[1].Unprogressed != 1 {
		t.Fatalf("coordinator actives = %+v, want both at Unprogressed 1", act)
	}
	if q := expire(); strings.Contains(q, "look in the child's own thread") {
		t.Fatalf("question after one episode answered twice:\n%s\nwant the plain stall question", q)
	}
}

// Round 2 (3a): an active answer with a note of the work the coordinator saw
// is evidence: the note is journaled and the count starts over.
func TestActiveWithObservedWorkResetsTheCount(t *testing.T) {
	h, run, ins, answer, expire := laAttested(t, "la-attest-note")
	expire()
	answer("")
	expire()
	answer("")
	if q := expire(); !strings.Contains(q, "look in the child's own thread") {
		t.Fatalf("want the evidence question first:\n%s", q)
	}
	answer("go test ./... still running, new output at 12:31")
	act := coordinatorActives(t, h, run)
	if last := act[len(act)-1]; last.Unprogressed != 0 || last.Note != "go test ./... still running, new output at 12:31" {
		t.Fatalf("the noted answer = %+v, want Unprogressed 0 and its note", last)
	}
	if q := expire(); strings.Contains(q, "look in the child's own thread") {
		t.Fatalf("question after observed work:\n%s\nwant the plain stall question", q)
	}
	if lost := lostRecords(t, h, run); len(lost) != 0 {
		t.Fatalf("a live worker was lost: %+v (unit %s)", lost, ins.Name)
	}
}

// Round 2 (3c, 4): an inconclusive round keeps an earlier revision's bounded recheck;
// when the recheck asks again with the count still at 2, the question says
// that a second inconclusive round is answered died.
func TestSecondInconclusiveRoundIsDied(t *testing.T) {
	h, run, ins, answer, expire := laAttested(t, "la-attest-inconclusive")
	expire()
	answer("")
	expire()
	answer("")
	first := expire()
	if !strings.Contains(first, "look in the child's own thread") || strings.Contains(first, "still cannot tell") || !strings.Contains(first, "run next again") {
		t.Fatalf("first evidence question:\n%s\nwant the recheck instruction for cannot-tell, without the second-round line", first)
	}
	// The coordinator cannot tell and answers neither; the recheck asks again.
	h.now = h.now.Add(5 * time.Minute)
	second := stallText(t, h, run)
	if !strings.Contains(second, "look in the child's own thread") || !strings.Contains(second, "still cannot tell") || !strings.Contains(second, "heartbeat died "+ins.Name) {
		t.Fatalf("the recheck's question:\n%s\nwant the evidence question telling a second inconclusive round to run died", second)
	}
	// One instruction per round: the second round's cannot-tell line is died
	// alone, with no run-neither or run-next-again line beside it.
	if strings.Contains(second, "run neither") || strings.Contains(second, "run next again") {
		t.Fatalf("the recheck's question:\n%s\nholds both the recheck and the died instruction for cannot-tell", second)
	}
}

// Final round (1): the coordinator's latest observation of a unit's work is
// in the serialized status and in its text rendering.
func TestObservedWorkShowsInStatus(t *testing.T) {
	h, run, ins, answer, expire := laAttested(t, "la-attest-observed")
	expire()
	const seen = "go test ./... still running, new output at 12:31"
	answer(seen)
	st := h.status(run)
	found := false
	for _, u := range st.Units {
		if u.Name == ins.Name {
			found = true
			if u.Observed != seen {
				t.Errorf("status unit %s Observed = %q, want %q", u.Name, u.Observed, seen)
			}
		}
	}
	if !found {
		t.Fatalf("status has no unit %s: %+v", ins.Name, st.Units)
	}
	if text := wf.RenderStatus(st); !strings.Contains(text, "observed: "+seen) {
		t.Errorf("rendered status lacks the observation:\n%s", text)
	}
}
