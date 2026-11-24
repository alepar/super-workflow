package wf_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	ha "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

var trackerFreeAgentRules, _, _ = strings.Cut(wf.RulesForAnyAgent, " The workflow moves this run's tracker items:")

// Every agent prompt the engine issues opens with the rules for the run,
// ahead of the definition's own text, whatever that text is: the template is
// untouched, so the same prompt dispatched raw (outside the engine) carries
// no rules and no placeholder for them.
func TestEveryIssuedAgentPromptStartsWithTheRules(t *testing.T) {
	d, _ := simple("rules-first", wf.PromptText("Do the task.\n", wf.Bound("spec", "the spec")))
	h := newHarness(t, d, nil)
	run, ins := handOneUnit(t, h, d)
	for _, r := range h.kind(run, wf.JournalInstructionIssued) {
		var data wf.InstructionIssuedData
		if err := json.Unmarshal(r.Data, &data); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(data.Prompt, trackerFreeAgentRules+"\n\n") || !strings.Contains(data.Prompt, "\n\nDo the task.") {
			t.Fatalf("issued prompt = %q, want the rules first, then the task", data.Prompt)
		}
	}
	f, err := h.e.(*wf.Engine).PromptFetch(run, ins.Name, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.Prompt, trackerFreeAgentRules+"\n\n") {
		t.Fatalf("fetched prompt = %q, want the rules first", f.Prompt)
	}
	if n := strings.Count(f.Prompt, trackerFreeAgentRules); n != 1 {
		t.Fatalf("the rules appear %d times in the fetched prompt", n)
	}
}

// A paged prompt carries the rules on its first body page, wrapped to the
// width like any other text, and its contract's goal stays the task's first
// line, not the rules.
func TestAPagedPromptStartsWithTheRulesAndKeepsItsGoal(t *testing.T) {
	d, _ := simple("rules-paged", wf.PromptText("First constraint.\n"+strings.Repeat("long instruction. ", 1400)+"\nLast constraint."))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true})
	run, ins := handOneUnit(t, h, d)
	if err := h.e.Started(run, ins.Name, claim("child")); err != nil {
		t.Fatal(err)
	}
	e := h.e.(*wf.Engine)
	page, err := e.PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.Delivered, "Goal: First constraint.") {
		t.Fatalf("page 1 = %q, want the goal to be the task's first line", page.Delivered)
	}
	all := page.Delivered
	for n := 2; n <= page.Pages; n++ {
		h.now = h.now.Add(time.Second)
		if page, err = e.PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Page: n, Code: page.NextCode}); err != nil {
			t.Fatal(err)
		}
		all += page.Delivered
	}
	// Paged lines are cut at the width, mid-word, so the text is compared
	// without its whitespace.
	flat := strings.Join(strings.Fields(all), "")
	rules := strings.Join(strings.Fields(trackerFreeAgentRules), "")
	r, task := strings.Index(flat, rules), strings.Index(flat, "Firstconstraint.longinstruction.")
	if r < 0 || task < 0 || r > task {
		t.Fatalf("paged body: rules at %d, task at %d; want the rules ahead of the task", r, task)
	}
	if n := strings.Count(flat, rules); n != 1 {
		t.Fatalf("the paged body carries the rules %d times, want once", n)
	}
}

// The rules say who moves this run's tracker items, the workflow, and leave
// a write the task's prompt directs to the agent (Decompose files beads, a
// design fix and Implement update them): a prompt-directed bd create or
// update must not read as a violation, or the child refuses its own task.
func TestTheRulesLetATasksPromptDirectTrackerWrites(t *testing.T) {
	const clause = "The workflow moves this run's tracker items: change one only if your task's prompt says so."
	// The spawn brief holds no single quote (a pi launch quotes it whole).
	const briefClause = "The workflow moves the tracker items of this run: change one only if the prompt of your task says so."
	for name, rules := range map[string]string{"RulesForAnyAgent": wf.RulesForAnyAgent, "RulesForSpawnBrief": wf.RulesForSpawnBrief, "RulesForThisRun": wf.RulesForThisRun} {
		clause := clause
		if name == "RulesForSpawnBrief" {
			clause = briefClause
		}
		if !strings.Contains(rules, clause) {
			t.Errorf("%s does not let the task's prompt direct tracker writes: %q", name, rules)
		}
		if strings.Count(rules, "tracker items") != strings.Count(rules, clause) {
			t.Errorf("%s names tracker items without the prompt exception: %q", name, rules)
		}
	}
}

// The rules say what to do, not what counts as cheating (guidance, not
// containment): the containment lines are gone, the agent CLI rule excepts
// the engine's own child dispatch, and the hard rules that remain are stated.
func TestTheRulesGuideAndDoNotPolice(t *testing.T) {
	all := wf.RulesForThisRun + "\n" + wf.RulesForSpawnBrief
	for _, gone := range []string{"counts as", "workaround", "work around", "engine's source", "copy run state", "scripts that drive", "use only the commands it prints", "complete result"} {
		if strings.Contains(all, gone) {
			t.Errorf("the rules still hold %q:\n%s", gone, all)
		}
	}
	for _, want := range []string{
		"Move this run only with the workflow engine's verbs", "ordinary shell work (mkdir, reading files) is your own",
		"Never edit or delete lock, journal or state files", "flags that bypass a sandbox or approvals",
		"reach a refused permission or approval another way", "address another run's state",
		"except through the engine's child dispatch",
	} {
		if !strings.Contains(wf.RulesForThisRun, want) {
			t.Errorf("the coordinator's rules lack %q:\n%s", want, wf.RulesForThisRun)
		}
	}
	for _, want := range []string{"start another agent CLI except through the child dispatch of the engine", "run your failed command with --reason saying what blocked you and what you need"} {
		if !strings.Contains(wf.RulesForSpawnBrief, want) {
			t.Errorf("the spawn brief's rules lack %q:\n%s", want, wf.RulesForSpawnBrief)
		}
	}
	// A child that cannot fix a refusal fails its unit with what it needs,
	// so the run routes it: it is not told to report and stop.
	if !strings.HasSuffix(wf.FixableRefusalTail, "run your failed command with --reason saying what blocked you and what you need, so the run can route it.") {
		t.Errorf("the fixable refusal's tail does not end in the failed verb: %s", wf.FixableRefusalTail)
	}
}
