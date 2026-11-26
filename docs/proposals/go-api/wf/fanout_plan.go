package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// FanoutTarget identifies the exact historical offer and its prior checkpoint.
// State is definition-qualified; LocalState is the journal's local state name.
type FanoutTarget struct {
	SnapshotSeq, IssueSeq             int64
	Instance, State, LocalState, Unit string
	Attempt, Visit                    int
	// Substituted, when set, says this target is not its state's latest
	// offer, which could not be forked, and which offer stands in for it:
	// a comparison with the source run must use the source's outcome at
	// IssueSeq, not at its latest attempt.
	Substituted string `json:",omitempty"`
	// Refused, when set, is why this explicitly selected state has no offer
	// that can be forked: the batch reports it as a refused fork.
	Refused string `json:",omitempty"`
	// Gate marks a person gate's offer (gate_offered): the fork's
	// coordinator answers it with wf answer; it has no claim or child.
	Gate bool `json:",omitempty"`
}

// FanoutSkip is a selected state, visit or offer the plan left out because
// no checkpoint of it can be forked (its world had a command in flight, its
// capture failed, it has no checkpoint, or its fork could not bind its
// target), with the reason.
type FanoutSkip struct {
	State    string
	Instance string `json:",omitempty"`
	Visit    int    `json:",omitempty"`
	IssueSeq int64
	Reason   string
}

// FanoutPlan is the forks a fanout makes and what it left out.
type FanoutPlan struct {
	Targets []FanoutTarget
	Skipped []FanoutSkip
}

type fanoutCandidate struct {
	target   FanoutTarget
	snapshot WorldSnapshotData
	problem  string
}

// PlanFanout reads historical offers under the run lock; it never captures
// the current world. An offer made while siblings were live is forkable: the
// fork is a crash-restart that loses them as orphans. For each
// selected state (or visit) the plan takes the latest offer that can be forked and marks it
// Substituted when a later one could not; a state with none is left out
// with its reason, or, named explicitly, planned as a refused fork.
func (e *Engine) PlanFanout(run RunID, states string) (FanoutPlan, error) {
	var result FanoutPlan
	var planErr error
	err := e.withRecords(run, func(records []JournalRecord) {
		if err := legacyRefusal("fanout", run, records); err != nil {
			planErr = err
			return
		}
		def, err := e.defFor(records[0])
		if err != nil {
			planErr = err
			return
		}
		st := &runState{def: def, insts: map[string]*instance{}}
		st.initIndex()
		var candidates []fanoutCandidate
		for i, r := range records {
			if r.Kind == JournalInstructionIssued || r.Kind == JournalGateOffered {
				in := st.insts[r.Instance]
				if in == nil {
					planErr = corrupt(r, "unknown offered instance")
					return
				}
				var unit string
				if r.Kind == JournalGateOffered {
					// A gate has no unit: the fork binds it by the name a
					// unit at its state would take.
					unit = st.unitName(in)
				} else {
					var issued InstructionIssuedData
					if err := json.Unmarshal(r.Data, &issued); err != nil {
						planErr = corrupt(r, "invalid instruction: %v", err)
						return
					}
					unit = fanoutUnitName(st, in, issued.Token)
				}
				c := fanoutCandidate{target: FanoutTarget{IssueSeq: r.Seq, Instance: r.Instance, State: st.qualified(in), LocalState: r.State, Unit: unit, Attempt: r.Attempt, Visit: in.Visits[in.Cur], Gate: r.Kind == JournalGateOffered}, problem: "no immediately preceding offer checkpoint"}
				if i > 0 {
					prev := records[i-1]
					if prev.Kind == JournalWorldSnapshot && prev.Seq+1 == r.Seq && prev.Instance == r.Instance && prev.State == r.State && prev.Attempt == r.Attempt {
						if err = json.Unmarshal(prev.Data, &c.snapshot); err != nil {
							planErr = corrupt(prev, "invalid snapshot: %v", err)
							return
						}
						if c.snapshot.Before == r.Kind {
							c.target.SnapshotSeq = prev.Seq
							c.problem = c.snapshot.Unavailable
							if c.snapshot.Version > 2 {
								c.problem = NewerFormat("world snapshot", c.snapshot.Version, 2).Error()
							} else if c.snapshot.Version != 1 && c.snapshot.Version != 2 {
								c.problem = "unsupported snapshot version"
							}
						}
					}
				}
				candidates = append(candidates, c)
			}
			if err := st.apply(r); err != nil {
				planErr = err
				return
			}
		}
		// An offer is grouped and selected by its state's name as of now:
		// a later migration or adoption renames it (namesNow). Its local
		// state stays the recorded one, which the snapshot checkpoint and the
		// fork's prefix name.
		nowAt := map[int64]string{}
		for _, r := range namesNow(records, st) {
			if r.Kind == JournalInstructionIssued || r.Kind == JournalGateOffered {
				nowAt[r.Seq] = r.State
			}
		}
		for k := range candidates {
			c := &candidates[k]
			if in := st.insts[c.target.Instance]; in != nil && nowAt[c.target.IssueSeq] != "" {
				c.target.State = strings.Join(append(st.defPath(in), nowAt[c.target.IssueSeq]), "/")
			}
		}
		// An offer is eligible when a fork of it would pass every read-only
		// check the fork makes; a check that fails for this offer alone
		// leaves it out, and corruption stops the plan.
		eligible := func(c *fanoutCandidate) (string, error) {
			if c.problem != "" {
				return c.problem, nil
			}
			pre, err := e.forkPreflight(run, records, c.target.SnapshotSeq)
			if err != nil {
				return offerExclusion(err)
			}
			// The fork binds its target before any coordinator starts: a
			// refusal there is this offer's, named by the plan.
			if err := pre.st.untilUnitRefusal(UntilUnitTarget{Instance: c.target.Instance, State: c.target.LocalState}, true); err != nil {
				return err.Error(), nil
			}
			if err := e.checkFanoutSnapshot(run, c.snapshot); err != nil {
				return offerExclusion(err)
			}
			return "", nil
		}
		chosen, skipped, err := selectFanout(candidates, states, eligible)
		if err != nil {
			planErr = err
			return
		}
		result.Skipped = skipped
		for _, c := range chosen {
			result.Targets = append(result.Targets, c.target)
		}
	})
	if err != nil {
		return FanoutPlan{}, err
	}
	if planErr != nil {
		return FanoutPlan{}, planErr
	}
	return result, nil
}

// offerExclusion is why a fork check failing for one offer leaves that offer
// out ("" never): its world unavailable (a command in flight, a failed live
// capture, a missing or damaged checkpoint, an adapter not here) or its
// prefix not relocatable.
// Any other failure (a corrupt journal) is returned: it stops the plan.
func offerExclusion(err error) (string, error) {
	if errors.Is(err, ErrCorruptJournal) {
		return "", err
	}
	if errors.Is(err, errForkUnrelocatable) || errors.Is(err, ErrUnavailable) {
		return err.Error(), nil
	}
	return "", err
}

// selectFanout picks, for each selected state (each instance and visit in
// "all" mode), the newest offer eligible says can be forked, trying the
// newest first. Every offer passed over is skipped with its reason, and a
// pick that is not its group's newest offer says so (Substituted). A group
// with no eligible offer is skipped whole, or, when the selection names its
// state, picked as Refused. An error from eligible stops the selection.
func selectFanout(candidates []fanoutCandidate, states string, eligible func(*fanoutCandidate) (string, error)) ([]fanoutCandidate, []FanoutSkip, error) {
	mode := strings.TrimSpace(states)
	if mode == "" {
		mode = "distinct"
	}
	available := map[string]bool{}
	for _, c := range candidates {
		available[c.target.State] = true
	}
	if len(available) == 0 {
		return nil, nil, fmt.Errorf("%w: fanout has no historical offered states", ErrUnavailable)
	}
	wanted := map[string]bool{}
	if mode != "distinct" && mode != "all" {
		for _, state := range strings.Split(mode, ",") {
			state = strings.TrimSpace(state)
			if !available[state] || wanted[state] {
				return nil, nil, fmt.Errorf("%w: fanout state %q must name one unique qualified offered state", ErrRejected, state)
			}
			wanted[state] = true
		}
	}
	groups := map[string][]*fanoutCandidate{}
	var keys []string
	for i := range candidates {
		c := &candidates[i]
		if len(wanted) > 0 && !wanted[c.target.State] {
			continue
		}
		key := c.target.State
		if mode == "all" {
			key = fmt.Sprintf("%s\x00%s\x00%d", c.target.Instance, c.target.State, c.target.Visit)
		}
		if groups[key] == nil {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], c)
	}
	var out []fanoutCandidate
	var skipped []FanoutSkip
	skip := func(c *fanoutCandidate, reason string) {
		s := FanoutSkip{State: c.target.State, IssueSeq: c.target.IssueSeq, Reason: reason}
		if mode == "all" {
			s.Instance, s.Visit = c.target.Instance, c.target.Visit
		}
		skipped = append(skipped, s)
	}
	for _, key := range keys {
		group := groups[key]
		sort.Slice(group, func(i, j int) bool { return group[i].target.IssueSeq > group[j].target.IssueSeq })
		var picked *fanoutCandidate
		var newestReason string
		for i, c := range group {
			reason, err := eligible(c)
			if err != nil {
				return nil, nil, fmt.Errorf("fanout %s at offer %d: %w", c.target.State, c.target.IssueSeq, err)
			}
			if reason == "" {
				picked = c
				break
			}
			if i == 0 {
				newestReason = reason
			}
			skip(c, reason)
		}
		switch {
		case picked != nil:
			c := *picked
			if picked != group[0] {
				c.target.Substituted = fmt.Sprintf("latest offer %d: %s; used offer %d", group[0].target.IssueSeq, newestReason, c.target.IssueSeq)
			}
			out = append(out, c)
		case len(wanted) > 0:
			c := *group[0]
			c.target.Refused = fmt.Sprintf("no offer of %s can be forked; latest offer %d: %s", c.target.State, c.target.IssueSeq, newestReason)
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].target.IssueSeq < out[j].target.IssueSeq })
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].IssueSeq < skipped[j].IssueSeq })
	return out, skipped, nil
}

func (e *Engine) checkFanoutSnapshot(run RunID, data WorldSnapshotData) error {
	if filepath.IsAbs(data.Directory) || filepath.Clean(data.Directory) != data.Directory || !strings.HasPrefix(data.Directory, "snapshots"+string(filepath.Separator)) {
		return fmt.Errorf("%w: invalid checkpoint directory", ErrCorruptJournal)
	}
	dir, err := e.snapshotRunDir(run)
	if err != nil {
		return err
	}
	root := filepath.Join(dir, data.Directory)
	if data.Version == 2 {
		return e.checkStoreSnapshot(root, data)
	}
	bytes, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return fmt.Errorf("%w: checkpoint manifest: %v", ErrUnavailable, err)
	}
	var saved WorldSnapshotData
	err = json.Unmarshal(bytes, &saved)
	// The manifest records the step its capture was taken before; a later
	// checkpoint of the same cycle names the capture again before its own step.
	saved.Before = data.Before
	// Adapter metadata is compared as JSON values (numbers exactly): a
	// relocated journal re-encodes its records' data.
	// A missing or null adapters map and {} are the same: no adapter.
	same := err == nil && sameAdapters(saved.Adapters, data.Adapters) && len(saved.Adapters) == len(data.Adapters)
	bare, bareData := saved, data
	bare.Adapters, bareData.Adapters = nil, nil
	if !same || !reflect.DeepEqual(bare, bareData) {
		return fmt.Errorf("%w: checkpoint manifest differs from journal", ErrUnavailable)
	}
	available := map[string]bool{}
	for _, a := range e.reg.snapshotAdapters() {
		available[a.name] = true
	}
	for name := range data.Adapters {
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name || !available[name] {
			return fmt.Errorf("%w: checkpoint adapter %q unavailable", ErrUnavailable, name)
		}
		if fi, err := os.Stat(filepath.Join(root, name)); err != nil || !fi.IsDir() {
			return fmt.Errorf("%w: checkpoint adapter %q data missing", ErrUnavailable, name)
		}
	}
	return nil
}

// FanoutOutcome is journal evidence only. Nil counts mean unknown; transcript
// usage and elapsed wall time belong to the caller's independent measurements.
type FanoutOutcome struct {
	Status              string
	Refused             []string
	Tokens              *int
	CoordinatorCalls    *int
	ChildVerbsAttempted *int
}

// ClassifyFanout ignores all source-prefix records and other state instances.
func ClassifyFanout(records []JournalRecord, target FanoutTarget) (FanoutOutcome, error) {
	out := FanoutOutcome{Status: "stalled"}
	done, ended := false, false
	issuedToken := ""
	issuedAttempt := 0
	submitted := false
	local := target.LocalState
	if local == "" {
		local = target.State
	}
	for _, r := range records {
		if r.Seq <= target.SnapshotSeq {
			continue
		}
		if r.Kind == JournalRunEnded {
			ended = true
		}
		// A later loop visit must not turn this visit's failed result into done.
		if r.Kind == JournalTransition && r.Instance == target.Instance && r.State == local {
			ended = true
			break
		}

		if r.Instance != target.Instance || r.State != local || r.Attempt < target.Attempt {
			continue
		}
		switch r.Kind {
		case JournalInstructionIssued:
			var d InstructionIssuedData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				return out, err
			}
			issuedToken, issuedAttempt, submitted = d.Token, r.Attempt, false
		case JournalAccepted:
			var d AcceptedData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				return out, err
			}
			if !d.Computed && submitted && issuedAttempt == r.Attempt {
				done = true
			} else {
				ended = true
			}
			submitted = false
		case JournalSubmitted:
			var d SubmittedData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				return out, err
			}
			submitted = d.Token != "" && d.Token == issuedToken && r.Attempt == issuedAttempt
			if d.Tokens > 0 {
				if out.Tokens == nil {
					out.Tokens = new(int)
				}
				*out.Tokens += d.Tokens
			}
		case JournalSubmissionRefused, JournalRefused:
			submitted = false
			var d RefusedData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				return out, err
			}
			out.Refused = append(out.Refused, d.Reasons...)
		case JournalRecoveryOffered:
			var d RecoveryOfferedData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				return out, err
			}
			ended = true
		case JournalAttemptLost:
			var d AttemptLostData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				return out, err
			}
			if d.Cause == LostCauseDied || d.Cause == LostCauseOrphan {
				ended = true
			}
		}
	}
	switch {
	case done:
		out.Status = "done"
	case len(out.Refused) > 0:
		out.Status = "refused"
	case ended:
		out.Status = "ended-without-done"
	}
	return out, nil
}

// A retained prior-attempt unit must not lend its name to a reissued offer.
func fanoutUnitName(st *runState, in *instance, token string) string {
	if in.Unit != nil && in.Unit.Name != "" && in.Unit.Token == token && token != "" {
		return in.Unit.Name
	}
	return st.unitName(in)
}

// checkStoreSnapshot checks a version-2 checkpoint: its tree is in the run's
// store and holds a directory for each adapter it names, each adapter
// available here.
func (e *Engine) checkStoreSnapshot(store string, data WorldSnapshotData) error {
	available := map[string]bool{}
	for _, a := range e.reg.snapshotAdapters() {
		available[a.name] = true
	}
	for name := range data.Adapters {
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name || !available[name] {
			return fmt.Errorf("%w: checkpoint adapter %q unavailable", ErrUnavailable, name)
		}
	}
	// The tree is read and checked once per process; each record's metadata
	// is compared with it every time.
	_, err := e.snapshotStore(store).validate(data.Tree, data.Adapters)
	return err
}

// snapshotStore is the engine's reader of the store at dir, which keeps the
// checkpoints it validated.
func (e *Engine) snapshotStore(dir string) *snapshotStore {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stores == nil {
		e.stores = map[string]*snapshotStore{}
	}
	if e.stores[dir] == nil {
		e.stores[dir] = openSnapshotStore(dir)
	}
	return e.stores[dir]
}
