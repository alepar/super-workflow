package wf

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// segmentModels is what segment i spawns with when driven from harness h
// : the models its binding record journaled, else what binding it
// from h would journal.
//
// Owns: the order a segment's map is chosen in: a map given at its boundary;
// else, for the first segment, run_started's (its journaled Models on the
// start harness, its ModelMap re-resolved on another); else the previous
// segment's on the same harness; else h's own table with no map. A segment
// bound by a record from before per-segment maps keeps the rule of its day:
// the start's Models, re-resolved for another harness with the start's map.
// Returns resolveTiers' refusal (errBadModelMap) when the map it must
// resolve holds a value h does not take: a value is checked here, against the
// harness that binds, and nowhere at a verb entry.
// Does not: journal (bindSegment does), or touch a unit's already recorded
// hand-out.
func (c *cycle) segmentModels(i int, h string) (SegmentModels, error) {
	st := c.st
	s := &st.segments[i]
	if s.models != nil {
		return *s.models, nil
	}
	h = normalizeHarness(h)
	if h == "" {
		h = c.segmentHarness(i)
	}
	if s.mapGiven != nil {
		return c.resolveSegment(h, s.mapGiven, ModelsFromMap)
	}
	rec, _ := recordedHarness(*st.started)
	start := SegmentModels{ModelMap: st.started.ModelMap, Models: st.started.Models, Efforts: st.started.Efforts, From: ModelsFromStart, Harness: rec}
	if s.Harness != "" {
		// A binding from before per-segment maps recorded no resolution: the
		// only recorded models are the start's, whatever harness bound it
		// (history never re-resolves from the live table).
		return start, nil
	}
	if i == 0 {
		if h == rec {
			return start, nil
		}
		// Not bound yet, on another harness: a fresh resolution of the start
		// map, which binding will record.
		return c.resolveSegment(h, st.started.ModelMap, ModelsFromStart)
	}
	prevH := c.segmentHarness(i - 1)
	if h == prevH {
		prev, err := c.segmentModels(i-1, prevH)
		prev.From = ModelsFromCarried
		return prev, err
	}
	return c.resolveSegment(h, nil, ModelsFromDefaults)
}

// segmentHarness is the harness segment i is driven from: its binding, else
// the previous segment's, else the harness the run was started under.
func (c *cycle) segmentHarness(i int) string {
	for ; i >= 0; i-- {
		s := c.st.segments[i]
		if s.Harness != "" {
			return s.Harness
		}
		if s.models != nil && s.models.Harness != "" {
			return s.models.Harness // a fork note's: the history's harness
		}
	}
	h, _ := recordedHarness(*c.st.started)
	return h
}

// resolveSegment resolves modelMap for harness h over the definition's tiers.
// A map h refuses (a value it does not take: errBadModelMap) yields the zero
// SegmentModels and the error, never a models-less value that a binding would
// journal: keys and efforts were refused where the map was given.
func (c *cycle) resolveSegment(h string, modelMap map[string]string, from string) (SegmentModels, error) {
	sm := SegmentModels{ModelMap: maps.Clone(modelMap), From: from, Harness: h}
	resolved, _, err := resolveTiers(h, collectTiers(c.st.def), modelMap)
	if err != nil {
		return SegmentModels{}, err
	}
	for t, tm := range resolved {
		if sm.Models == nil {
			sm.Models, sm.Efforts = map[string]string{}, map[string]string{}
		}
		sm.Models[t], sm.Efforts[t] = tm.Model, tm.Effort
		if tm.Effort == "" {
			sm.Efforts[t] = EffortHarnessDefault
		}
	}
	return sm, nil
}

// shown is a models answer for a display or a render: a refusal is reported
// where the segment binds (bindSegment), so a reader of an unbound segment
// whose map its harness refuses shows no models rather than failing.
func shown(sm SegmentModels, _ error) SegmentModels { return sm }

// modelsIfAny is shown for a status: nil when the segment's map is refused.
func modelsIfAny(sm SegmentModels, err error) *SegmentModels {
	if err != nil {
		return nil
	}
	return &sm
}

// unitModels is the one rule for which models apply to a unit's recorded
// event: an issue (kind JournalInstructionIssued) renders and
// dispatches with the models of the segment it is issued in; a prompt fetch
// renders with the models of the segment the attempt's latest hand-out was
// made in, else its issue's. A bound segment answers from its journaled
// models only. In a replay the same question is answered from the source
// journal, event by event (replayModels, built by recordedEventModels).
func (c *cycle) unitModels(in *instance, kind JournalKind) SegmentModels {
	if rm := c.e.replayModels; rm != nil {
		// The issue under way is the next one; a fetch follows the latest.
		n := c.st.issuedAt[replayKey(in.ID, in.Cur)]
		if kind != JournalInstructionIssued {
			n--
		}
		if sm, ok := rm.models(kind, in.ID, in.Cur, n); ok {
			return sm
		}
	}
	if kind == JournalInstructionIssued {
		return shown(c.currentModels(""))
	}
	seg := in.issuedSegment
	if u := in.Unit; u != nil && u.Attempt == in.Attempt && u.HandSegment != 0 {
		seg = u.HandSegment
	}
	for i := range c.st.segments {
		if c.st.segments[i].Start == seg {
			return shown(c.segmentModels(i, ""))
		}
	}
	return shown(c.currentModels(""))
}

// renderModels is unitModels for the render under way: the issue of the
// instance being issued, else a fetch (or a non-agent state's binding, which
// has neither and gets the current segment's).
func (c *cycle) renderModels(in *instance) SegmentModels {
	switch {
	case in == nil:
		return shown(c.currentModels(""))
	case c.issuing == in:
		return c.unitModels(in, JournalInstructionIssued)
	}
	return c.unitModels(in, JournalPromptFetched)
}

// recordedModels answers unitModels' question from a source journal alone,
// for a replay. Events are keyed by the issue they follow: the n-th
// issue of an instance in a state (an instance|state key with the issue's
// index), so a state visited again, whose attempt numbering restarts, is
// another key. An issue gets the models of the segment it was recorded in;
// the fetches after it the segment of its latest hand-out before the first
// fetch (else the issue's); its hand-out the model and effort the recorded
// hand-out that a child claimed (the one before the first fetch, else the
// latest) was made with. A segment's models are its binding record's (or a
// fork note's), even for an event before the binding, else the start's for
// the first segment, else the previous segment's: never a tier table.
type recordedModels struct {
	segs    []*SegmentModels
	issues  map[string][]int         // instance|state -> segment of each issue
	fetches map[string]int           // issue key -> segment its first fetch rendered with
	hands   map[string]HandedOutData // issue key -> the hand-out a child claimed
}

func issueKey(instance, state string, n int) string {
	return fmt.Sprintf("%s|%d", replayKey(instance, state), n)
}

func recordedEventModels(recs []JournalRecord) *recordedModels {
	rm := &recordedModels{issues: map[string][]int{}, fetches: map[string]int{}, hands: map[string]HandedOutData{}}
	var bound []bool
	set := func(sm *SegmentModels) {
		if n := len(rm.segs); n > 0 && !bound[n-1] {
			rm.segs[n-1], bound[n-1] = sm, true
		}
	}
	latest := map[string]string{} // instance -> key of its latest issue
	handSeg := map[string]int{}   // instance -> segment of its latest hand-out since that issue
	fetched := map[string]bool{}  // issue key -> a fetch followed it
	for _, r := range recs {
		cur := len(rm.segs) - 1
		switch r.Kind {
		case JournalRunStarted:
			var d RunStartedData
			if json.Unmarshal(r.Data, &d) == nil {
				rec, _ := recordedHarness(d)
				rm.segs = append(rm.segs, &SegmentModels{ModelMap: d.ModelMap, Models: d.Models, Efforts: d.Efforts, From: ModelsFromStart, Harness: rec})
				bound = append(bound, false)
			}
		case JournalResumed, JournalRewound:
			if len(rm.segs) > 0 {
				rm.segs, bound = append(rm.segs, nil), append(bound, false)
			}
		case JournalCoordinator:
			var d CoordinatorData
			if json.Unmarshal(r.Data, &d) == nil && d.SegmentStart != 0 && d.Models != nil {
				set(d.Models)
			}
		case JournalNote:
			var d NoteData
			if json.Unmarshal(r.Data, &d) == nil && d.Models != nil {
				set(d.Models)
			}
		case JournalInstructionIssued:
			k := replayKey(r.Instance, r.State)
			latest[r.Instance] = issueKey(r.Instance, r.State, len(rm.issues[k]))
			rm.issues[k] = append(rm.issues[k], cur)
			delete(handSeg, r.Instance)
		case JournalHandedOut:
			handSeg[r.Instance] = cur
			if k, ok := latest[r.Instance]; ok && !fetched[k] {
				var d HandedOutData
				if json.Unmarshal(r.Data, &d) == nil {
					rm.hands[k] = d
				}
			}
		case JournalPromptFetched:
			k, ok := latest[r.Instance]
			if !ok || fetched[k] {
				continue
			}
			fetched[k] = true
			seg, handed := handSeg[r.Instance]
			if !handed {
				l := rm.issues[replayKey(r.Instance, r.State)]
				seg = l[len(l)-1]
			}
			rm.fetches[k] = seg
		}
	}
	for i := 1; i < len(rm.segs); i++ {
		if rm.segs[i] == nil {
			rm.segs[i] = rm.segs[i-1]
		}
	}
	return rm
}

func (rm *recordedModels) segment(i int) (SegmentModels, bool) {
	if i < 0 || i >= len(rm.segs) || rm.segs[i] == nil {
		return SegmentModels{}, false
	}
	return *rm.segs[i], true
}

// models is the recorded models of instance's n-th issue in state (kind
// JournalInstructionIssued) or of the fetches that followed it.
func (rm *recordedModels) models(kind JournalKind, instance, state string, n int) (SegmentModels, bool) {
	if kind == JournalInstructionIssued {
		if l := rm.issues[replayKey(instance, state)]; n >= 0 && n < len(l) {
			return rm.segment(l[n])
		}
		return SegmentModels{}, false
	}
	if seg, ok := rm.fetches[issueKey(instance, state, n)]; ok {
		return rm.segment(seg)
	}
	return SegmentModels{}, false
}

// handOut is the model and effort the recorded hand-out of instance's n-th
// issue in state spawned its child with.
func (rm *recordedModels) handOut(instance, state string, n int) (model, effort string, ok bool) {
	// A hand-out from before an earlier revision recorded no spawn: the issued dispatch
	// stands, as it did then.
	d, ok := rm.hands[issueKey(instance, state, n)]
	if !ok || d.Spawn == nil {
		return "", "", false
	}
	return d.Spawn.Model, d.Spawn.Effort, true
}

// currentModels is what the current segment spawns with from harness h ("":
// the segment's own harness), or segmentModels' refusal.
func (c *cycle) currentModels(h string) (SegmentModels, error) {
	return c.segmentModels(len(c.st.segments)-1, h)
}

// spawnDispatch is the dispatch a state's own hints spawn with from harness
// h under the segment models sm. A tiered hint takes sm's model (none when sm
// lacks the tier: a hint, A3c) and, for an unset effort, sm's effort; an
// untiered one goes once through sm's model -> model map. A state's own
// wf.AgentType wins, else h's type.
func spawnDispatch(own DispatchHints, h string, sm SegmentModels) DispatchHints {
	d := own
	if d.AgentType == "" {
		d.AgentType = harnessAgentType(h)
	}
	if own.Tier != "" {
		d.Model = sm.Models[string(own.Tier)]
		if e := sm.Efforts[string(own.Tier)]; d.Model != "" && d.Effort == "" && e != EffortHarnessDefault {
			d.Effort = e
		}
		return d
	}
	if to, ok := sm.ModelMap[own.Model]; ok && own.Model != "" {
		d.Model = to
	}
	return d
}

// modelsLine is the models line of status and the bootstrap:
// "models: strong=gpt-6.2-astra top=gpt-6.2-astra (codex defaults)", tiers in
// dispatch order; "" when the definition uses no tier and no map is in force.
func modelsLine(sm *SegmentModels, h string) string {
	if sm == nil || len(sm.Models) == 0 && len(sm.ModelMap) == 0 {
		return ""
	}
	tiers := tiersOf(sm.Models)
	if tiers == "" {
		tiers = "no tiers"
	}
	return "models: " + tiers + " (" + modelsSource(sm, h) + ")"
}

// segmentHarnessOf is the harness rs's current segment is driven from, for
// its models line: the segment's binding, else rs.Harness.
func segmentHarnessOf(rs RunStatus) string {
	if n := len(rs.Segments); n > 0 && rs.Segments[n-1].Harness != "" {
		return rs.Segments[n-1].Harness
	}
	return rs.Harness
}

// ModelsFor is what run's current segment spawns with when driven from
// harness h: its bound models, or what binding it from h would choose
// . A read: it never migrates, drives or binds.
func (e *Engine) ModelsFor(run RunID, h string) (*SegmentModels, error) {
	var sm SegmentModels
	err := e.withDryRun(run, func(c *cycle) error {
		var err error
		sm, err = c.currentModels(h)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &sm, nil
}

// tiersOf renders a tier -> model map in dispatch order: "light=a strong=b".
func tiersOf(m map[string]string) string {
	var parts []string
	for _, t := range dispatchTiers {
		if v, ok := m[string(t)]; ok {
			parts = append(parts, string(t)+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

// modelsSource says where a segment's models came from, for a person.
func modelsSource(sm *SegmentModels, h string) string {
	if len(sm.ModelMap) > 0 {
		s := "--model-map " + mapText(sm.ModelMap)
		if sm.From == ModelsFromCarried {
			s += ", carried"
		}
		return s
	}
	if h == "" {
		h = "harness"
	}
	return fmt.Sprintf("%s defaults", h)
}

// mapText renders a --model-map as it is typed, keys sorted.
func mapText(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}
