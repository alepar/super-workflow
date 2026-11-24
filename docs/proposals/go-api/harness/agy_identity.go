package harness

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// --- agyChild, agyCaller: who a conversation is, from the records agy itself
// writes. Identity is
// cooperative: the same OS user can edit these files, so this catches a wrong
// or accidental child, not a forger. A conversation is never matched on its
// content alone: every candidate comes from a launch in the coordinator's own
// log and the result that launch produced.

const agyUser = "USER_INPUT"

// agyFirstStep is the first line of the transcript at path decoded as a step;
// false for no file, an empty file or a first line that is not a step (half
// written).
func agyFirstStep(path string) (agyStep, bool) {
	if path == "" {
		return agyStep{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return agyStep{}, false
	}
	defer f.Close()
	line, _ := bufio.NewReader(f).ReadBytes('\n')
	if len(strings.TrimSpace(string(line))) == 0 {
		return agyStep{}, false
	}
	return agyDecode(line)
}

// agyFirstFrom is a conversation's step 0 read as a delivery from parent: its
// step 0 (step_index present and 0; a record without one is not step 0) is
// source SYSTEM, type SYSTEM_MESSAGE, the recognised wrapper
// (agyWrapper) with sender exactly parent (a "<parent>/task-<n>" sender is a
// task message and never matches). It returns the delivery and the step's
// created_at. The one first-record check agyChild and agyCaller share.
func agyFirstFrom(root, parent, child string) (agyDelivery, time.Time, bool) {
	if !agyValidID(parent) {
		return agyDelivery{}, time.Time{}, false
	}
	s, ok := agyFirstStep(agyTranscript(root, child))
	if !ok || !s.HasIndex || s.Index != 0 || s.Source != agySourceSystem || s.Type != agySystem {
		return agyDelivery{}, time.Time{}, false
	}
	d, ok := agyWrapper(s.Content)
	if !ok || d.Sender != parent {
		return agyDelivery{}, time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, s.CreatedAt)
	if err != nil {
		return agyDelivery{}, time.Time{}, false
	}
	return d, at, true
}

// agyListed reports whether parent's listing names child: the file exists
// and its conversationId is child.
func agyListed(root, parent, child string) bool {
	p := agyListing(root, parent, child)
	if p == "" {
		return false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	var v struct {
		ConversationID string `json:"conversationId"`
	}
	return json.Unmarshal(b, &v) == nil && v.ConversationID == child
}

// agyLaunch is one invoke_subagent call in a coordinator's own log: its
// Prompt, the launching response's time, and the child its result names.
type agyLaunch struct {
	prompt, child string
	at            time.Time
}

// agySourceSystem is the source of unsolicited steps (ERROR_MESSAGE,
// SYSTEM_MESSAGE, a task message, a delivery): they answer no call.
const agySourceSystem = "SYSTEM"

// agyLaunches are session's launches since after (truncated to the second:
// created_at has one-second resolution): each invoke_subagent call of a
// PLANNER_RESPONSE created at or after it, paired per launch over the result
// steps written so far (spec section 4.5, amended). A response's result steps
// are the steps after it, up to the next response, whose source is not SYSTEM:
// ERROR_MESSAGE and SYSTEM_MESSAGE steps, task messages included, answer no
// call and are skipped. Command and launch results are both GENERIC.
// Call k pairs with result k in call order. A launch is written one result
// at a time. The written prefix stands while later calls are pending, so a
// first child verifies
// before its siblings' results exist. More results than calls, and the first
// call whose result is missing or cannot be its own, stop the response (see
// agyPaired). A non-blank line that is not a step (half written, or not a
// step at all) could have been a result or the next response, so the span
// ends there: the results before it pair, nothing at or after it does. Flat
// args only.
func agyLaunches(root, session string, after time.Time) []agyLaunch {
	var steps []agyStep
	var bad []bool // bad[i]: line i was not a step
	if !agySteps(agyTranscript(root, session),
		func(s agyStep) { steps, bad = append(steps, s), append(bad, false) },
		func() { steps, bad = append(steps, agyStep{}), append(bad, true) }) {
		return nil
	}
	floor := after.Truncate(time.Second)
	var out []agyLaunch
	for i, s := range steps {
		if bad[i] || s.Type != agyResponse || len(s.Calls) == 0 {
			continue
		}
		at, err := time.Parse(time.RFC3339, s.CreatedAt)
		if err != nil || at.Before(floor) {
			continue
		}
		var results []agyStep
		for j := i + 1; j < len(steps) && !bad[j] && steps[j].Type != agyResponse; j++ {
			if steps[j].Source != agySourceSystem {
				results = append(results, steps[j])
			}
		}
		if len(results) > len(s.Calls) {
			continue // surplus results: the response cannot be paired in call order
		}
		out = append(out, agyPaired(s.Calls, results, at)...)
	}
	return out
}

// agyPaired pairs calls with the results written so far, in call order, and
// stops at the first call with no result (pending) or whose result cannot be
// its own (an invoke_subagent whose result is not a created-child step, or any
// other call whose result is one): the launches before it stand and nothing
// after it counts. A launch without a flat Prompt is not a launch; pairing
// continues past it.
func agyPaired(calls []agyCall, results []agyStep, at time.Time) []agyLaunch {
	var out []agyLaunch
	for k, c := range calls {
		if k >= len(results) {
			break
		}
		child, created := results[k].createdChild()
		launch := c.Name == "invoke_subagent"
		if launch != created {
			break
		}
		if prompt, ok := c.Args["Prompt"].(string); launch && ok {
			out = append(out, agyLaunch{prompt: prompt, child: child, at: at})
		}
	}
	return out
}

// agyChild is the child session started for token since after: conditions
// 1-5 and 7 of spec §4.5. (1) a launch in session's own log, at or after the
// hand-out, whose Prompt names token as its one claim (onlyClaim); (7) its
// result names C; (2) session's listing names C; (3) C's step 0 is the
// delivery from session with body byte-equal to the Prompt; (4) that step is
// not older than the launch, at one-second resolution; (5) exactly one such
// C. Nothing here matches on content alone: every candidate comes from a
// launch and its result. Identity is cooperative: the same OS user can edit
// these files, so this catches a wrong or accidental child, not a forger.
func agyChild(root, session, token string, after time.Time) (Child, bool) {
	if !agyValidID(session) || token == "" {
		return Child{}, false
	}
	matched := map[string]bool{}
	for _, l := range agyLaunches(root, session, after) {
		if l.child == session || !onlyClaim([]byte(l.prompt), token) || !agyListed(root, session, l.child) {
			continue
		}
		d, at, ok := agyFirstFrom(root, session, l.child)
		if !ok || d.Body != l.prompt || at.Before(l.at) {
			continue
		}
		matched[l.child] = true
	}
	if len(matched) != 1 {
		return Child{}, false
	}
	for c := range matched {
		return Child{Agent: c, Transcript: agyTranscript(root, c)}, true
	}
	return Child{}, false
}

// agyCaller classifies the caller from positive evidence (spec §4.5): a
// child is a conversation exactly one parent's listing names and whose step 0
// is that parent's delivery (Session = parent, Agent = own id, IsChild); a
// coordinator is one whose own log exists, begins with USER_INPUT and is
// named by no listing (Session = own id, IsSelf); anything else is
// unverifiable (IsUnknown, refused). It reads nothing without
// ANTIGRAVITY_CONVERSATION_ID. Under Auto mode the records show only the
// caller's own conversation, so a child is IsUnknown. Identity
// is cooperative: the environment and the records are the same user's.
func agyCaller(env Env) Caller {
	c := Caller{Harness: "agy", FromEnv: true}
	own := env("ANTIGRAVITY_CONVERSATION_ID")
	root := agyRoot(env)
	if !agyValidID(own) || root == "" {
		return c
	}
	first, ok := agyFirstStep(agyTranscript(root, own))
	if !ok {
		return c
	}
	var parents []string
	for _, p := range agyListingParents(root, own) {
		if agyListed(root, p, own) {
			parents = append(parents, p)
		}
	}
	switch {
	case len(parents) == 1:
		if _, _, ok := agyFirstFrom(root, parents[0], own); ok {
			c.Session, c.Agent, c.Root, c.Is = parents[0], own, root, IsChild
		}
	case len(parents) == 0 && first.Type == agyUser:
		c.Session, c.Root, c.Is = own, root, IsSelf
	}
	return c
}

// Caller delegates to agyCaller; the classification is its.
func (agy) Caller(env Env) Caller { return agyCaller(env) }

// Resolve: the child agyChild finds for each wanted token since its
// hand-out; its agent id is its conversation id.
func (agy) Resolve(root, session string, wants []Want) map[Want]Child {
	out := map[Want]Child{}
	for _, w := range wants {
		if c, ok := agyChild(root, session, w.Token, w.After); ok && (w.Agent == "" || w.Agent == c.Agent) {
			out[w] = c
		}
	}
	return out
}

// SameChild: an agy child is its own conversation under the same coordinator.
func (agy) SameChild(owner, caller Caller) bool {
	return owner.Agent != "" && owner.Agent == caller.Agent && owner.Session == caller.Session
}

var _ ChildMatcher = agy{}
