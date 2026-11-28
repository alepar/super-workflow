package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Execution of the Sub and Map kinds, and the three rules that cut across
// child instances: Hold locks, write fences and the run budget.
//
// A child is a fresh instance (empty counters, empty OutputOf values, one
// arrival at its first state) spawned by child_spawned on the parent and ended
// by child_settled on the parent. The parent stays at its Sub or Map state
// while its children run; its execute is idempotent, so a call that finds
// nothing to do appends nothing.

// purposeMapSource marks the observed record of a Map source read.
const purposeMapSource = "map_source"

// purposeMapRequery marks a Map source re-read made at hand-out. It updates
// the latest read but is not counted in SourceReads, which pairs one read with
// each settled child.
const purposeMapRequery = "map_requery"

func isMapKind(n *node) bool {
	_, ok := n.kind.(*mapKind)
	return ok
}

func (k *subKind) execute(c *cycle, in *instance, n *node) error {
	return execOps(k.ops, k.base, c, in, n)
}
func (k *mapKind) execute(c *cycle, in *instance, n *node) error {
	return execOps(k.ops, k.base, c, in, n)
}

func (k *subKind) retryPolicy() *retryData { return k.retry }

// ---------------------------------------------------------------- identities

func childPrefix(parent *instance) string {
	if parent.ID == "" {
		return ""
	}
	return parent.ID + "/"
}

// subChildID names the Sub entry of the parent's current arrival at n.
func subChildID(parent *instance, n *node) string {
	return childPrefix(parent) + subSegment(n.name, parent.Visits[n.name])
}

// subSegment is a Sub child's segment of its id: "<state>#<entry>".
func subSegment(state string, entry int) string { return state + "#" + strconv.Itoa(entry) }

// mapSegment is a Map child's segment of its id: "<state>[<idKey(key)>]",
// then "~<gen>" from the second generation and "#<entry>" from the second
// entry on.
func mapSegment(state, key string, gen, entry int) string {
	seg := state + "[" + idKey(key) + "]"
	if gen > 1 {
		seg += "~" + strconv.Itoa(gen)
	}
	if entry > 1 {
		seg += "#" + strconv.Itoa(entry)
	}
	return seg
}

// mapChildID names the Map item key of the parent's current arrival at n; the
// arrival is spelled out only from the second one on.
//
// gen is the key's generation within the arrival: 1 for its first child, 2
// and on for a key a Requery Map re-admitted (readmits), named "[key]~gen".
//
// It is the only place a Map key enters an instance id, and the key enters
// escaped (idKey), so two keys never share an id and no key spells a deeper
// instance's id (sw-ivuk).
func mapChildID(parent *instance, n *node, key string, gen int) string {
	return childPrefix(parent) + mapSegment(n.name, key, gen, parent.Visits[n.name])
}

// idKey is a Map item key as it stands between the brackets of an instance id:
// each byte that delimits an id ('/' between instances, '[' and ']' around a
// key) and the escape byte '%' itself is written %XX. Every other byte is
// kept, so a key without those four bytes keeps the id it had before the
// escape, and ids journaled the old way still resume, replay and fork. The
// escape is injective, so distinct keys keep distinct ids.
func idKey(key string) string {
	if !strings.ContainsAny(key, "%/[]") {
		return key
	}
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		switch ch := key[i]; ch {
		case '%', '/', '[', ']':
			fmt.Fprintf(&b, "%%%02X", ch)
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

func sourceKey(state string, entry int) string { return fmt.Sprintf("%s#%d", state, entry) }

// entryKids lists the children the parent's current arrival at n spawned, in
// spawn order.
func (c *cycle) entryKids(parent *instance, n *node) []*instance {
	var out []*instance
	for _, id := range c.st.order {
		ch := c.st.insts[id]
		if ch.Parent == parent.ID && id != "" && ch.Spawner == n.name && ch.Entry == parent.Visits[n.name] {
			out = append(out, ch)
		}
	}
	return out
}

func successful(ch *instance) bool {
	return ch.Settled && ch.Class == Accepted && ch.Outcome.Delivered()
}

// ---------------------------------------------------------------- settling

// settleChild journals child_settled for a child that has no terminal result.
func (c *cycle) settleChild(parent, ch *instance, class OutcomeClass) error {
	return c.emitAs(parent.ID, ch.Spawner, ch.Attempt, JournalChildSettled, class, ChildSettledData{
		ID: ch.ID, State: ch.Spawner, Class: class,
	})
}

// checkOpen runs the recovery Check on the child's open effect intent, if it
// has one. A done effect journals its result, so its outcome is captured
// before the child is cancelled; anything else (not done, unknown, not
// idempotent, a failed check) is left as it is, and nothing is applied.
func (c *cycle) checkOpen(ch *instance) (resolved bool, err error) {
	seq, e, ok := openIntent(ch)
	if !ok || e.NonIdempotent {
		return false, nil
	}
	st, cerr := c.e.reg.CheckAt(c.workspace(), EffectContext{Run: c.run, Instance: ch.ID, Sandboxed: c.st.forked, SandboxBin: c.sandboxBin()}, externalOf(e, e.Args))
	if cerr != nil || st != EffectDone {
		return false, nil
	}
	idx := 0
	if ch.Pending != nil {
		idx = ch.Pending.Done
	}
	return true, c.emit(ch, JournalEffectResult, 0, EffectResultData{Intent: seq, Index: idx, OK: true, Outcome: "done"})
}

// cancelTree settles an active child as Rejected with Cancelled set and, first,
// every active descendant of it, so nothing under it stays runnable. An open
// effect intent is checked before the cancel. Its open token is superseded:
// the child is settled, so a submission is Rejected as stale.
func (c *cycle) cancelTree(parent, ch *instance) error {
	for _, id := range c.st.order {
		d := c.st.insts[id]
		if d.Parent == ch.ID && id != "" && !d.Settled {
			if err := c.cancelTree(ch, d); err != nil {
				return err
			}
		}
	}
	if _, err := c.checkOpen(ch); err != nil {
		return err
	}
	return c.emitAs(parent.ID, ch.Spawner, ch.Attempt, JournalChildSettled, Rejected, ChildSettledData{
		ID: ch.ID, State: ch.Spawner, Class: Rejected, Cancelled: true,
	})
}

// ---------------------------------------------------------------- Sub

// subOps is the typed half of a Sub.
type subOps[S, I, R any] struct{ s *Sub[S, I, R] }

// execute starts the entry's child (declared observations read and journaled,
// the input closure run over them), waits while it runs, and once it settles
// routes its terminal through Exits.
func (o subOps[S, I, R]) execute(c *cycle, in *instance, n *node) error {
	id := subChildID(in, n)
	ch := c.st.insts[id]
	if ch == nil {
		return o.start(c, in, n, id)
	}
	switch {
	case ch.Phase == phaseHalted && !ch.Settled && !ch.exhaustionHalt() && !ch.offerOpen():
		// An exhaustion halt or an open recovery offer is not settled: the
		// Sub waits for its answer.
		return c.settleChild(in, ch, Blocked)
	case ch.BudgetStopped && !ch.Settled:
		return c.stopOnBudget(in, ch.Reason)
	case !ch.Settled:
		return nil
	case ch.Terminal == "":
		why := "child " + ch.DefName + " did not reach a terminal"
		if ch.Reason != "" {
			why += ": " + ch.Reason
		}
		return c.fail(in, n, Blocked, []string{why}, "")
	}
	for _, x := range o.s.k.exits {
		if x.terminal != ch.Terminal {
			continue
		}
		plan, err := c.planOf(in, x.e, ch.Result)
		if err != nil {
			return c.halt(in, err.Error())
		}
		return c.emit(in, JournalAccepted, Accepted, AcceptedData{Output: ch.Result, Computed: true, Plan: plan})
	}
	return c.fail(in, n, Blocked, []string{fmt.Sprintf("child %s ended at %s, which has no exit", ch.DefName, ch.Terminal)}, "")
}

func (o subOps[S, I, R]) start(c *cycle, in *instance, n *node, id string) error {
	k := o.s.k
	descs, err := c.bindDescs(in, k.observes)
	if err != nil {
		return c.failErr(in, n, err)
	}
	for _, d := range descs {
		od, err := c.readObs(in, d, "sub", true)
		if err != nil {
			return err
		}
		switch {
		case od.Unavailable:
			return c.fail(in, n, Unavailable, []string{od.Error}, "")
		case od.Error != "":
			return c.fail(in, n, Refused, []string{od.Error}, od.Error)
		}
	}
	ctx, err := c.ctx(in, k.observes...)
	if err != nil {
		return c.failErr(in, n, err)
	}
	fn, _ := k.input.(func(Ctx) I)
	if fn == nil {
		return c.fail(in, n, Blocked, []string{"sub has no input function"}, "")
	}
	val := fn(ctx)
	if ctx.blocked {
		return c.failErr(in, n, ctx.blockedErr())
	}
	if err := validatePatterns(reflect.ValueOf(val), "input"); err != nil {
		return c.fail(in, n, Refused, []string{err.Error()}, err.Error())
	}
	raw, err := c.marshalPathValue(in, val)
	if err != nil {
		return c.fail(in, n, Refused, []string{"sub input is not JSON: " + err.Error()}, "")
	}
	return c.emit(in, JournalChildSpawned, 0, ChildSpawnedData{ID: id, Def: k.child.name, State: n.name, Input: raw})
}

// ---------------------------------------------------------------- Map

// mapOps is the typed half of a Map.
type mapOps[S, I any, K comparable, R any] struct{ m *Map[S, I, K, R] }

// mapItem is one source item: its stable key, its JSON and its write set.
type mapItem struct {
	key     string
	raw     json.RawMessage
	writes  []string
	waiting bool // the item reports WaitingOnDeps: not spawned yet
	gen     int  // the generation a re-admitted key spawns as (0: the first)
}

// waitingOnDeps reports whether the decoded item says it waits on dependencies.
func waitingOnDeps(item any) bool {
	w, ok := item.(interface{ WaitingOnDeps() bool })
	return ok && w.WaitingOnDeps()
}

// waiter is implemented by the ops of a kind that lists items not spawned
// yet; Status reads it through mapKind.waiting.
type waiter interface {
	waiting(c *cycle, in *instance, n *node) []string
}

// waiting is one note per source item with no child yet, in source order:
// "deps" for an item that reports WaitingOnDeps, "held" for one the Map has
// not spawned for another reason (Parallel, a write fence or an entry lock).
func (k *mapKind) waiting(c *cycle, in *instance, n *node) []string {
	if w, ok := k.ops.(waiter); ok {
		return w.waiting(c, in, n)
	}
	return nil
}

// instanceJSON is the wire form of Instance[R] inside a MapResult.
type instanceJSON struct {
	Result             json.RawMessage `json:",omitempty"`
	Class              OutcomeClass
	Terminal           string
	BudgetStopped      bool
	BudgetStoppedBelow bool `json:",omitempty"`
}

// mapResultJSON is the keyed result of the given children, decoded by
// Joined as MapResult[K, R].
// srcErr is the failed source read's error, empty when the read succeeded.
func mapResultJSON(kids []*instance, srcErr string) (json.RawMessage, error) {
	items := make(map[string]instanceJSON, len(kids))
	for _, ch := range kids {
		items[ch.Key] = instanceJSON{Result: ch.Result, Class: ch.Class, Terminal: ch.Terminal, BudgetStopped: ch.BudgetStopped, BudgetStoppedBelow: ch.BudgetStoppedBelow}
	}
	return json.Marshal(struct {
		Items       map[string]instanceJSON
		SourceError string `json:",omitempty"`
	}{items, srcErr})
}

func (o mapOps[S, I, K, R]) execute(c *cycle, in *instance, n *node) error {
	k := o.m.k
	delete(c.turnWaitingMaps, in.ID)
	if k.met == nil || k.partial == nil || k.join == nil {
		return c.fail(in, n, Blocked, []string{"map has no join"}, "") // nothing can have spawned yet
	}
	kids := c.entryKids(in, n)
	for _, ch := range kids {
		switch {
		case ch.Phase == phaseHalted && !ch.Settled && !ch.exhaustionHalt() && !ch.offerOpen():
			// An exhaustion halt or an open recovery offer is not settled:
			// the child stays active and the Map waits for its answer (wf
			// retry --child, wf recover).
			return c.settleChild(in, ch, Blocked)
		case ch.BudgetStopped && !ch.Settled:
			return c.settleChild(in, ch, Unavailable)
		}
	}
	items, srcErr, done, err := o.items(c, in, n, kids)
	if err != nil || done {
		return err
	}
	if k.requery && srcErr == "" {
		if err := o.withdraw(c, in, kids, items); err != nil {
			return err
		}
	}

	// A key has one child per generation; the latest stands for it, so a
	// re-admitted key counts once, by its newest child.
	byKey := map[string]*instance{}
	gens := map[string]int{}
	for _, ch := range kids {
		byKey[ch.Key] = ch
		gens[ch.Key]++
	}
	var active []*instance
	succ, failed := 0, 0
	for _, ch := range kids {
		if byKey[ch.Key] != ch {
			continue
		}
		switch {
		case !ch.Settled:
			active = append(active, ch)
		case ch.Cancelled:
			// neither a success nor a failure, and not still to settle
		case successful(ch):
			succ++
		default:
			failed++
		}
	}
	var todo []mapItem
	var left map[string]bool
	for _, it := range items {
		ch := byKey[it.key]
		if ch != nil && ch != in && k.requery && srcErr == "" && ch.Settled && !successful(ch) {
			if left == nil {
				left = o.leftSince(c, in, n, kids)
			}
			if left[ch.ID] {
				// The key left the source after its child settled and is
				// back: new work, not the settled child's (readmits).
				it.gen = gens[it.key] + 1
				if !ch.Cancelled {
					failed-- // the settled child no longer stands for its key
				}
				ch = nil
			}
		}
		if ch == nil {
			todo = append(todo, it)
			byKey[it.key] = in // a duplicate key is one item
		}
	}

	// A failure under CancelSiblings stops the rest: active siblings are
	// cancelled and no item is spawned any more.
	stop := k.sibling == CancelSiblings && failed > 0
	if stop && len(active) > 0 {
		for _, ch := range active {
			if err := c.cancelTree(in, ch); err != nil {
				return err
			}
		}
		return nil
	}
	remaining := len(active)
	if !stop {
		remaining += len(todo)
	}
	settledAll := len(active) == 0 && (stop || len(todo) == 0)

	var take *edgeUse
	switch {
	case srcErr != "":
		// A failed source read has no Retry to take: the Join's partial edge
		// is the declared route, once the running children have settled.
		if len(active) > 0 {
			return nil
		}
		take = k.partial
	case len(items) == 0 && len(kids) == 0:
		take = k.met // an empty item list meets the join
	default:
		met, partial, err := o.decide(c, k.join, succ, failed, remaining, settledAll, stop, kids)
		if err != nil {
			return o.failParent(c, in, n, Blocked, []string{err.Error()})
		}
		switch {
		case met:
			take = k.met
			if failed > 0 {
				// B3: the join was met with seats missing: say which.
				var missing []string
				for _, ch := range kids {
					if ch.Settled && !ch.Cancelled && ch.Class != Accepted {
						missing = append(missing, ch.ID+" ("+ch.Class.String()+")")
					}
				}
				d := DegradedData{Unit: c.st.qualified(in), Kind: DegradedJoin,
					Cause:  fmt.Sprintf("%d of %d seats; missing %s", succ, len(kids), strings.Join(missing, ", ")),
					Action: "joined: " + k.met.e.label}
				if err := c.emit(in, JournalDegraded, 0, d); err != nil {
					return err
				}
			}
		case partial:
			take = k.partial
		}
	}
	if take != nil {
		// The join is decided: settle what still runs, then journal the keyed
		// result with the edge.
		early := k.join.kind == "quorum" || k.join.kind == "predicate"
		for _, ch := range active {
			if err := o.settleLoser(c, in, ch, early); err != nil {
				return err
			}
		}
		raw, err := mapResultJSON(c.entryKids(in, n), srcErr)
		if err != nil {
			return o.failParent(c, in, n, Refused, []string{"join result is not JSON: " + err.Error()})
		}
		plan, err := c.planOf(in, *take, raw)
		if err != nil {
			return o.haltParent(c, in, err.Error())
		}
		return c.emit(in, JournalAccepted, Accepted, AcceptedData{Output: raw, Computed: true, Plan: plan})
	}

	// Spawn what Parallel and the write fences allow.
	var held [][]string
	for _, ch := range active {
		writes, err := o.writesOf(c, ch.Input)
		if err != nil {
			return o.failParent(c, in, n, Blocked, []string{err.Error()})
		}
		held = append(held, writes)
	}
	for _, it := range todo {
		if k.parallel > 0 && len(active) >= k.parallel {
			break
		}
		if it.waiting || overlapsAny(it.writes, held) || c.entryLocked(k.child) {
			continue
		}
		id := mapChildID(in, n, it.key, max(it.gen, 1))
		if err := c.emit(in, JournalChildSpawned, 0, ChildSpawnedData{ID: id, Def: k.child.name, State: n.name, Key: it.key, Input: it.raw}); err != nil {
			return err
		}
		active = append(active, c.st.insts[id])
		held = append(held, it.writes)
	}
	if c.st.turn != nil && !k.requery && len(todo) == 0 && len(active) > 0 {
		if c.turnWaitingMaps == nil {
			c.turnWaitingMaps = map[string]int64{}
		}
		c.turnWaitingMaps[in.ID] = in.arrivalSeq
	}
	return nil
}

// settleLoser settles an active child once its Map's join is decided. After an
// early decide (Quorum or JoinWhen) only a seat that never spawned is withdrawn:
// a handed unit journals withdrawn first, then the child is cancelled. A seat
// whose agent is already started, running or stalled is settled with Running
// and keeps its unit: it finishes, and its done is journaled as a late_vote
// that never reopens the decision. Any other decide cancels the whole child, so
// its names are retired and a late done is rejected.
func (o mapOps[S, I, K, R]) settleLoser(c *cycle, in, ch *instance, early bool) error {
	u := ch.Unit
	if early && u != nil && u.State.claimed() {
		return c.emitAs(in.ID, ch.Spawner, ch.Attempt, JournalChildSettled, Rejected, ChildSettledData{
			ID: ch.ID, State: ch.Spawner, Class: Rejected, Cancelled: true, Running: true,
		})
	}
	if early && u != nil && u.State == UnitHanded {
		if err := c.emit(ch, JournalWithdrawn, 0, WithdrawnData{Name: u.Name, Reason: "join decided"}); err != nil {
			return err
		}
	}
	return c.cancelTree(in, ch)
}

// failParent fails the Map state after cancelling every child still active, so
// a parent that fails or halts leaves no live child behind.
func (o mapOps[S, I, K, R]) failParent(c *cycle, in *instance, n *node, class OutcomeClass, reasons []string) error {
	if err := o.cancelActive(c, in, n); err != nil {
		return err
	}
	return c.fail(in, n, class, reasons, "")
}

// haltParent is failParent for a halt that consults no route.
func (o mapOps[S, I, K, R]) haltParent(c *cycle, in *instance, why string) error {
	if n := in.node(); n != nil {
		if err := o.cancelActive(c, in, n); err != nil {
			return err
		}
	}
	return c.halt(in, why)
}

func (o mapOps[S, I, K, R]) cancelActive(c *cycle, in *instance, n *node) error {
	for _, ch := range c.entryKids(in, n) {
		if !ch.Settled {
			if err := c.cancelTree(in, ch); err != nil {
				return err
			}
		}
	}
	return nil
}

// decide applies the join policy: met or partial, neither while the outcome is
// still open. remaining counts the children that may still settle.
func (o mapOps[S, I, K, R]) decide(c *cycle, j *JoinPolicy, succ, failed, remaining int, settledAll, stop bool, kids []*instance) (met, partial bool, err error) {
	switch j.kind {
	case "all_settled":
		if settledAll {
			return failed == 0, failed > 0, nil
		}
	case "all_ok":
		switch {
		case failed > 0 && (stop || settledAll):
			return false, true, nil
		case settledAll:
			return true, false, nil
		}
	case "quorum":
		switch {
		case succ >= j.k:
			return true, false, nil
		case succ+remaining < j.k:
			return false, true, nil
		}
	case "predicate":
		pred, _ := j.pred.(func(MapResult[K, R]) bool)
		if pred == nil {
			return false, false, errors.New("join predicate has an unexpected type")
		}
		var settled []*instance
		for _, ch := range kids {
			if ch.Settled && !ch.Cancelled {
				settled = append(settled, ch)
			}
		}
		raw, err := mapResultJSON(settled, "")
		if err != nil {
			return false, false, err
		}
		raw, err = c.decodeOwnedPathValues(raw)
		if err != nil {
			return false, false, err
		}
		var res MapResult[K, R]
		if err := json.Unmarshal(raw, &res); err != nil {
			return false, false, fmt.Errorf("join result does not decode as the map's result type: %w", err)
		}
		switch {
		case pred(res):
			return true, false, nil
		case settledAll:
			return false, true, nil
		}
	default:
		return false, false, fmt.Errorf("unknown join policy %q", j.kind)
	}
	return false, false, nil
}

// items returns the source items with their keys and write sets. done is true
// when the call journaled something (a source read or a failure) and the caller
// should return so the next pass sees it. srcErr is the error of the latest
// source read when it failed (unavailable or errored): no items are decoded
// and the caller routes it.
func (o mapOps[S, I, K, R]) items(c *cycle, in *instance, n *node, kids []*instance) (items []mapItem, srcErr string, done bool, err error) {
	k := o.m.k
	var raws []json.RawMessage
	switch src := k.src; {
	case src.obs != nil:
		d, err := c.scope(in).bindDesc(*src.obs)
		if err != nil {
			return nil, "", true, o.failParent(c, in, n, Blocked, []string{err.Error()})
		}
		want := 1
		if k.requery {
			for _, ch := range kids {
				if ch.Settled {
					want++
				}
			}
		}
		sk := sourceKey(n.name, in.Visits[n.name])
		reads := in.SourceReads[sk]
		rk := in.ID + "/" + sk
		if reads < want {
			_, err := c.readObs(in, d, purposeMapSource, true)
			if err != nil {
				return nil, "", true, err
			}
			c.markRequeried(rk) // a read made this cycle is fresh enough
			return nil, "", true, nil
		}
		if k.requery && c.requery && !c.requeried[rk] {
			// A hand-out reads the source once more, so units that left it are
			// withdrawn before they are handed out. The latest read is used.
			c.markRequeried(rk)
			if _, err := c.readObs(in, d, purposeMapRequery, true); err != nil {
				return nil, "", true, err
			}
			return nil, "", true, nil
		}
		od := in.Observed[canonDesc(d)]
		if od.Unavailable || od.Error != "" {
			return nil, od.Error, false, nil
		}
		if err := c.loadObserved(&od); err != nil {
			return nil, "", true, o.failParent(c, in, n, Refused, []string{"map source: " + err.Error()})
		}
		if err := json.Unmarshal(od.Value, &raws); err != nil {
			return nil, "", true, o.failParent(c, in, n, Refused, []string{"map source is not a JSON array: " + err.Error()})
		}
	case src.static:
		if src.err != nil {
			return nil, "", true, o.failParent(c, in, n, Blocked, []string{"map values are not JSON: " + src.err.Error()})
		}
		raws = src.values
	default:
		from, vals := src.from, in.Outputs
		if src.comp != nil {
			from, vals = src.comp, in.Computed
		}
		if from == nil || src.items == nil {
			return nil, "", true, o.failParent(c, in, n, Blocked, []string{"map has no item source"})
		}
		raw, ok := vals[from.name]
		if !ok {
			return nil, "", true, o.failParent(c, in, n, Blocked, []string{fmt.Sprintf("map source %s has no value", from.name)})
		}
		resolved, resolveErr := c.decodeOwnedPathValues(raw)
		if resolveErr != nil {
			return nil, "", true, o.failParent(c, in, n, Blocked, []string{resolveErr.Error()})
		}
		if raws, err = src.items(resolved); err != nil {
			return nil, "", true, o.failParent(c, in, n, Refused, []string{"map source does not decode: " + err.Error()})
		}
	}
	key, _ := k.key.(func(I) K)
	if key == nil {
		return nil, "", true, o.failParent(c, in, n, Blocked, []string{"map has no item key function"})
	}
	for _, raw := range raws {
		item, raw, err := o.prepareItem(c, in, raw)
		if err != nil {
			return nil, "", true, o.failParent(c, in, n, Refused, []string{"map item: " + err.Error()})
		}
		itemKey := fmt.Sprint(key(item))
		// The key names the child (and its directories and command lines), so
		// a control byte in it is refused here, where it enters from the item.
		if hasControlByte(itemKey) {
			return nil, "", true, o.failParent(c, in, n, Refused, []string{fmt.Sprintf("map item key %q holds a control character", itemKey)})
		}
		items = append(items, mapItem{key: itemKey, raw: raw, writes: o.writes(item), waiting: waitingOnDeps(item) || waitingOnDeps(&item)})
	}
	return items, "", false, nil
}

// prepareItem preserves the projected portable input while returning its live
// value for keys, write fences and readiness. The observation stays untouched.
func (o mapOps[S, I, K, R]) prepareItem(c *cycle, in *instance, raw json.RawMessage) (I, json.RawMessage, error) {
	var item I
	resolved, err := c.decodeOwnedPathValues(raw)
	if err != nil {
		return item, nil, err
	}
	resolved, err = o.m.k.src.projectItem(resolved)
	if err != nil {
		return item, nil, err
	}
	if err = json.Unmarshal(resolved, &item); err != nil {
		return item, nil, err
	}
	if err = validatePatterns(reflect.ValueOf(item), "item"); err != nil {
		return item, nil, err
	}
	portable, err := c.marshalPathValue(in, item)
	if err != nil {
		return item, nil, err
	}
	resolved, err = c.decodeOwnedPathValues(portable)
	if err != nil {
		return item, nil, err
	}
	if err = json.Unmarshal(resolved, &item); err != nil {
		return item, nil, err
	}
	return item, portable, nil
}

// leftSince is the settled, unsuccessful children of kids whose key was
// missing from a read of the Map's source journaled after the child settled.
// Such a key that is in the source again is re-admitted: a Requery Map runs
// it once more as a new child (generation "~2" and on), in the same arrival,
// and the new child stands for the key in the join and the result. A task
// held on a blocker bead leaves bd ready when the blocker edge lands and
// returns when the blocker closes (item 96). A key that never left the
// source keeps its settled child: a failure that is still ready is not new
// work. The journal decides, so a refold re-admits the same keys.
func (o mapOps[S, I, K, R]) leftSince(c *cycle, in *instance, n *node, kids []*instance) map[string]bool {
	src := o.m.k.src.obs
	if src == nil {
		return nil
	}
	d, err := c.scope(in).bindDesc(*src)
	if err != nil {
		return nil
	}
	want := canonDesc(d)
	key, _ := o.m.k.key.(func(I) K)
	settledAt := map[string]int{} // child id -> index of its child_settled record
	byID := map[string]*instance{}
	for _, ch := range kids {
		if ch.Settled && !successful(ch) {
			byID[ch.ID] = ch
		}
	}
	out := map[string]bool{}
	for i, r := range c.recs {
		if r.Instance != in.ID {
			continue
		}
		switch r.Kind {
		case JournalChildSettled:
			var sd ChildSettledData
			if json.Unmarshal(r.Data, &sd) == nil && byID[sd.ID] != nil {
				settledAt[sd.ID] = i
			}
		case JournalObserved:
			var od ObservedData
			if json.Unmarshal(r.Data, &od) != nil || (od.Purpose != purposeMapSource && od.Purpose != purposeMapRequery) ||
				od.Unavailable || od.Error != "" || canonDesc(od.Desc) != want || c.loadObserved(&od) != nil {
				continue
			}
			var raws []json.RawMessage
			if json.Unmarshal(od.Value, &raws) != nil || key == nil {
				continue
			}
			present := map[string]bool{}
			valid := true
			for _, raw := range raws {
				item, _, err := o.prepareItem(c, in, raw)
				if err != nil {
					valid = false
					break
				}
				present[fmt.Sprint(key(item))] = true
			}
			if !valid {
				continue // An unreadable receipt cannot prove a key left.
			}
			for id, at := range settledAt {
				if at < i && !present[byID[id].Key] {
					out[id] = true
				}
			}
		}
	}
	return out
}

func (c *cycle) markRequeried(key string) {
	if !c.requery {
		return
	}
	if c.requeried == nil {
		c.requeried = map[string]bool{}
	}
	c.requeried[key] = true
}

// withdraw cancels the children whose key left a requeried source and that
// never started: an open agent instruction with no unit yet, a pending unit or
// a handed one, in a child that has run no unit before. Each journals
// withdrawn (under the name its unit has, or would have had) and then settles
// Cancelled; a child already withdrawn by a crashed cycle is only settled.
// A started, running or stalled child is left to finish, and so is one that
// ran an earlier unit (a task whose implement finished while its review waits
// for hand-out, or one whose attempt was lost and is pending again): its work
// is under way, whatever the source now says.
func (o mapOps[S, I, K, R]) withdraw(c *cycle, in *instance, kids []*instance, items []mapItem) error {
	live := make(map[string]bool, len(items))
	for _, it := range items {
		live[it.key] = true
	}
	for _, ch := range kids {
		if ch.Settled {
			continue
		}
		if ch.Unit != nil && ch.Unit.State == UnitWithdrawn {
			// A crash after withdrawn and before child_settled: finish the
			// cancel. A withdrawn unit's name is retired and can never run
			// again, even if its key came back to the source.
			if err := c.cancelTree(in, ch); err != nil {
				return err
			}
			continue
		}
		if live[ch.Key] || c.ranAUnit(ch) {
			continue
		}
		var name string
		switch {
		case ch.Unit != nil && ch.Unit.State == UnitHanded:
			name = ch.Unit.Name
		case ch.awaitsHandOut():
			name = c.st.unitName(ch)
		default:
			continue
		}
		if err := c.emit(ch, JournalWithdrawn, 0, WithdrawnData{Name: name, Reason: "item left the source"}); err != nil {
			return err
		}
		if err := c.cancelTree(in, ch); err != nil {
			return err
		}
	}
	return nil
}

// ranAUnit reports whether ch, or a child of it, has run a unit: a unit of it
// was started, fetched its prompt or submitted a result.
func (c *cycle) ranAUnit(ch *instance) bool {
	for _, r := range c.recs {
		if !c.st.within(r.Instance, ch.ID) {
			continue
		}
		switch r.Kind {
		case JournalAttemptStarted, JournalPromptFetched, JournalSubmitted:
			return true
		}
	}
	return false
}

// sourceRaws is the item array of the source as currently read, without side
// effects: an observation source uses its latest journaled read, a static or
// computed one its values. False when there is nothing to list: no read yet,
// an unavailable or failed read, or a source that does not decode. Status uses
// it; items is the execute path and fails the parent instead.
func (o mapOps[S, I, K, R]) sourceRaws(c *cycle, in *instance) ([]json.RawMessage, bool) {
	k := o.m.k
	switch src := k.src; {
	case src.obs != nil:
		d, err := c.scope(in).bindDesc(*src.obs)
		if err != nil {
			return nil, false
		}
		od, ok := in.Observed[canonDesc(d)]
		if !ok || od.Unavailable || od.Error != "" || c.loadObserved(&od) != nil {
			return nil, false
		}
		var raws []json.RawMessage
		if json.Unmarshal(od.Value, &raws) != nil {
			return nil, false
		}
		return raws, true
	case src.static:
		return src.values, src.err == nil
	default:
		from, vals := src.from, in.Outputs
		if src.comp != nil {
			from, vals = src.comp, in.Computed
		}
		if from == nil || src.items == nil {
			return nil, false
		}
		raw, ok := vals[from.name]
		if !ok {
			return nil, false
		}
		resolved, err := c.decodeOwnedPathValues(raw)
		if err != nil {
			return nil, false
		}
		raws, err := src.items(resolved)
		return raws, err == nil
	}
}

// waiting lists the items of the source as currently read that have no child
// yet: "deps" when the item reports WaitingOnDeps, else "held". It is
// recomputed from the source, never stored, and reads and journals nothing.
func (o mapOps[S, I, K, R]) waiting(c *cycle, in *instance, n *node) []string {
	k := o.m.k
	raws, ok := o.sourceRaws(c, in)
	if !ok {
		return nil
	}
	key, _ := k.key.(func(I) K)
	if key == nil {
		return nil
	}
	spawned := map[string]bool{}
	for _, ch := range c.entryKids(in, n) {
		spawned[ch.Key] = true
	}
	var notes []string
	for _, raw := range raws {
		item, _, err := o.prepareItem(c, in, raw)
		if err != nil {
			continue
		}
		kk := fmt.Sprint(key(item))
		if spawned[kk] {
			continue
		}
		spawned[kk] = true // a duplicate key is one item
		if waitingOnDeps(item) || waitingOnDeps(&item) {
			notes = append(notes, "deps")
		} else {
			notes = append(notes, "held")
		}
	}
	return notes
}

// writes is the item's write set: the static Writes and its ItemWrites.
func (o mapOps[S, I, K, R]) writes(item I) []string {
	k := o.m.k
	w := append([]string(nil), k.writes...)
	if fn, _ := k.itemWrites.(func(I) []string); fn != nil {
		w = append(w, fn(item)...)
	}
	return w
}

// writesOf is the write set of the item an active child was spawned with.
func (o mapOps[S, I, K, R]) writesOf(c *cycle, input json.RawMessage) ([]string, error) {
	resolved, err := c.decodeOwnedPathValues(input)
	if err != nil {
		return nil, err
	}
	var item I
	if err := json.Unmarshal(resolved, &item); err != nil {
		return nil, err
	}
	return o.writes(item), nil
}

// overlapsAny reports whether w overlaps any of the held write sets.
func overlapsAny(w []string, held [][]string) bool {
	for _, h := range held {
		for _, a := range w {
			for _, b := range h {
				if globsOverlap(a, b) {
					return true
				}
			}
		}
	}
	return false
}

// globsOverlap reports whether some path can match both write-set globs. The
// dialect is: segments split on "/", "*" and "?" match within one segment, "**"
// as a whole segment matches any number of segments, and a path that is a
// prefix of another overlaps it (a directory covers its files, with or without
// a trailing "/"). A "[" class is not part of the dialect: its characters are
// literal. Conservative on purpose: a false overlap only serialises two
// children.
func globsOverlap(a, b string) bool { return segsOverlap(globSegs(a), globSegs(b)) }

func globSegs(g string) []string { return strings.Split(strings.Trim(path.Clean(g), "/"), "/") }

func segsOverlap(a, b []string) bool {
	switch {
	case len(a) == 0 || len(b) == 0:
		return true // one path is a prefix of the other
	case a[0] == "**":
		return segsOverlap(a[1:], b) || segsOverlap(a, b[1:])
	case b[0] == "**":
		return segsOverlap(a, b[1:]) || segsOverlap(a[1:], b)
	}
	return segOverlap(a[0], b[0]) && segsOverlap(a[1:], b[1:])
}

// segOverlap reports whether some name matches both single-segment patterns.
func segOverlap(a, b string) bool {
	switch {
	case a == "" && b == "":
		return true
	case a != "" && a[0] == '*':
		return segOverlap(a[1:], b) || (b != "" && segOverlap(a, b[1:]))
	case b != "" && b[0] == '*':
		return segOverlap(a, b[1:]) || (a != "" && segOverlap(a[1:], b))
	case a == "" || b == "":
		return false
	case a[0] == '?' || b[0] == '?' || a[0] == b[0]:
		return segOverlap(a[1:], b[1:])
	}
	return false
}

// ---------------------------------------------------------------- Hold

// lockedOut reports whether the instance's pending edge enters a state whose
// locks another instance holds. Such an instance stays in its pending edge,
// not runnable, and applies none of the edge's effects. An edge whose effects
// have started already holds the target's locks.
func (c *cycle) lockedOut(in *instance) bool {
	p := in.Pending
	if p == nil || len(p.Plan.Steps) == 0 || p.Done > 0 || p.Open != 0 {
		return false
	}
	target := in.def.find(p.Plan.Steps[len(p.Plan.Steps)-1].To)
	if target == nil {
		return false
	}
	for _, l := range target.locks {
		for _, other := range c.st.insts {
			if other != in && other.holds(l) {
				return true
			}
		}
	}
	return false
}

// entryLocked reports whether another instance holds a lock the child's start
// state needs, so a new child would enter a held state. The child waits and
// starts on a later drive, once the holder has moved on.
func (c *cycle) entryLocked(child *Def) bool {
	if len(child.nodes) == 0 {
		return false
	}
	for _, l := range child.nodes[0].locks {
		for _, other := range c.st.insts {
			if other.holds(l) {
				return true
			}
		}
	}
	return false
}

func nodeHolds(n *node, l Lock) bool {
	if n == nil {
		return false
	}
	for _, h := range n.locks {
		if h.name == l.name {
			return true
		}
	}
	return false
}

// holds derives the lock table from the fold: an instance holds the locks of
// its current state from arrival until its outgoing edge is chosen, and keeps
// one the edge's target holds as well. LockRun and LockRunKey are one table,
// because a run key maps to one run.
func (in *instance) holds(l Lock) bool {
	cur := in.node()
	switch in.Phase {
	case phaseReady, phaseIssued, phaseBackoff, phaseHalted:
		return nodeHolds(cur, l)
	case phaseTransition:
		p := in.Pending
		if p == nil || len(p.Plan.Steps) == 0 {
			return false
		}
		target := in.def.find(p.Plan.Steps[len(p.Plan.Steps)-1].To)
		started := p.Done > 0 || p.Open != 0
		return nodeHolds(target, l) && (started || nodeHolds(cur, l))
	}
	return false
}

// ---------------------------------------------------------------- run budget

// budgetSpent reports whether the run budget in force is used up, and which
// limit.
func (c *cycle) budgetSpent() (string, bool) { return c.spentUnder(c.st.budget) }

// spentUnder reports whether budget b is used up by what the run has spent,
// and why.
func (c *cycle) spentUnder(b RunBudget) (string, bool) {
	limit, used, ok := c.spentLimit(b)
	if !ok {
		return "", false
	}
	unit := map[string]string{"turns": "turns", "tokens": "tokens", "wall": "wall time"}[limit]
	return fmt.Sprintf("run budget spent: %s %s", used, unit), true
}

// spentLimit is the first limit of b the run has used up (turns, tokens or
// wall) and what it used of it.
func (c *cycle) spentLimit(b RunBudget) (limit, used string, ok bool) {
	switch {
	case b.Turns > 0 && c.st.turns >= b.Turns:
		return "turns", strconv.Itoa(c.st.turns), true
	case b.Tokens > 0 && c.st.tokens >= b.Tokens:
		return "tokens", strconv.Itoa(c.st.tokens), true
	case b.Wall > 0 && !c.st.startedAt.IsZero() && c.e.opts.Clock().Sub(c.st.startedAt) >= b.Wall:
		return "wall", b.Wall.Round(time.Second).String(), true
	}
	return "", "", false
}

// budgetResumeLine is the command the run's operator raises a spent budget
// with: the spent limit named, its new value left to the operator.
func (c *cycle) budgetResumeLine() string {
	limit, used, ok := c.spentLimit(c.st.budget)
	if !ok {
		limit, used = "turns", strconv.Itoa(c.st.turns)
	}
	return c.verbLine("resume", "--operator", "<name>", "--reason", "<why>", "--budget", limit+"=<above "+used+">")
}

// budgetGate stops an instance that is about to issue an agent instruction once
// the run budget is spent; an instruction already open is left to finish.
// stopped is true when it journaled the stop.
func (c *cycle) budgetGate(in *instance, n *node) (stopped bool, err error) {
	if _, ok := n.kind.(*agentKind); !ok {
		return false, nil
	}
	why, spent := c.budgetSpent()
	if !spent {
		return false, nil
	}
	return true, c.stopOnBudget(in, why)
}

// stopOnBudget journals budget_stopped for the instance. A Map child also
// settles: it counts as settled and not successful in the join, with
// Instance.BudgetStopped set and a class that is never Refused. A Sub child
// stops its parent, and the run itself is then stopped (see RunBudget).
func (c *cycle) stopOnBudget(in *instance, why string) error {
	if err := c.emit(in, JournalBudgetStopped, 0, BudgetStoppedData{Reason: why}); err != nil {
		return err
	}
	parent := c.st.insts[in.Parent]
	if in.ID == "" || parent == nil {
		return nil
	}
	if n := parent.def.find(in.Spawner); n != nil && isMapKind(n) {
		return c.settleChild(parent, in, Unavailable)
	}
	return nil
}
