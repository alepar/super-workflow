package wf

import (
	"fmt"
	"slices"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// resolveSearchEvery is how often one drive loop searches the same transcript
// root and session again for children it has not found. A done, a failed or a
// new process searches at once.
const resolveSearchEvery = 5 * time.Second

// resolveChildren attaches the agent id and transcript of each claimed unit
// whose claim lacks them, found by its harness adapter from the claim token
// its child fetched the prompt with (A1). It runs lazily, at the top of every
// drive: at the first liveness check, at done and on resume (A2). A child not
// found yet (its transcript not flushed) is searched again later; a unit that
// never resolves lives by the heartbeat and its implicit lease, as on a
// harness without an adapter.
func (c *cycle) resolveChildren() error {
	type key struct{ harness, root, session string }
	type pending struct {
		in   *instance
		want harness.Want
	}
	groups := map[key][]pending{}
	var order []key
	for _, id := range c.st.order {
		in := c.st.insts[id]
		u := in.Unit
		if !c.untilUnitAllows(in) || u == nil || in.Settled || !u.State.claimed() || u.Claim == nil || u.waveClaim() || u.Claim.Root == "" {
			continue
		}
		if u.Claim.Agent != "" && u.Claim.Transcript != "" {
			continue
		}
		w := harness.Want{Token: u.Bound, Agent: u.Claim.Agent, After: u.HandedAt.Add(-mtimeSlack), ErrLog: u.errLogOf(u.Bound)}
		if w.Token == "" && w.Agent == "" {
			continue
		}
		k := key{u.Claim.Harness, u.Claim.Root, u.Claim.Session}
		if groups[k] == nil {
			order = append(order, k)
		}
		groups[k] = append(groups[k], pending{in, w})
	}
	now := c.now()
	for _, k := range order {
		sk := k.harness + "\x00" + k.root + "\x00" + k.session
		c.e.mu.Lock()
		last, searched := c.e.searchedAt[sk]
		if searched && !c.searchNow && now.Sub(last) < resolveSearchEvery {
			c.e.mu.Unlock()
			continue
		}
		if c.e.searchedAt == nil {
			c.e.searchedAt = map[string]time.Time{}
		}
		c.e.searchedAt[sk] = now
		c.e.mu.Unlock()
		ps := groups[k]
		wants := make([]harness.Want, len(ps))
		for i, p := range ps {
			wants[i] = p.want
		}
		found := harness.For(k.harness).Resolve(k.root, k.session, wants)
		for _, p := range ps {
			ch, ok := found[p.want]
			if !ok {
				continue
			}
			u := p.in.Unit
			add := MetadataAttachedData{Name: u.Name, State: u.State.String(), Resolved: true}
			if u.Claim.Agent == "" {
				add.Agent = ch.Agent
			} else if u.Claim.Agent != ch.Agent {
				continue // the environment's id wins; a contradicting find is not attached
			}
			if u.Claim.Transcript == "" {
				add.Transcript = ch.Transcript
			}
			if add.Agent == "" && add.Transcript == "" {
				continue
			}
			if err := c.emit(p.in, JournalMetadataAttached, 0, add); err != nil {
				return err
			}
		}
	}
	return nil
}

// endSettle is how long a transcript that reads EndIfSettled must stay
// unwritten before the engine asks whether a process of the child is live. It
// covers the in-process gap after a turn's last write, never a retry backoff
// (pi writes the context_edit first) or a compaction call (the live marker
// covers that).
const endSettle = 15 * time.Second

// endWatched reports whether loseEndedChildren looks at in's unit. The
// deadline listing shares it: a unit the reconcile skips must list no end
// settle deadline, or a blocking wait would wake for it again and again.
func (c *cycle) endWatched(in *instance) bool {
	u := in.Unit
	return c.untilUnitAllows(in) && u != nil && !in.Settled && u.State.claimed() && u.Claim != nil && !u.waveClaim() && u.Claim.Transcript != "" && u.Recovery == nil
}

// loseEndedChildren raises the recovery offer (F1) of each claimed unit
// whose child's transcript ended on its harness's end marker with no done or
// failed (E2): at once, not at the stall threshold, and not as a blind
// re-offer, since the child's work so far may be most of the unit. A
// transcript is read again only when its mtime moved since it was last read.
// A harness whose children can write after a final-looking turn
// (harness.SettlingEnd) answers EndIfSettled: the transcript is then ended
// once it has stayed unwritten for endSettle on the engine's clock and no
// live process carries the child's marker (EngineOptions.ChildLive); a live
// marker or a failed scan re-arms the settle. The pending settle is a
// deadlineEndSettle, so a blocking wait wakes for it; it journals nothing.
func (c *cycle) loseEndedChildren() error {
	for _, id := range slices.Clone(c.st.order) {
		in := c.st.insts[id]
		if in == nil || c.st.ended || !c.endWatched(in) {
			continue
		}
		u := in.Unit
		path := u.Claim.Transcript
		mt, err := c.e.opts.TranscriptMtime(path)
		if err != nil {
			c.dropEndPending(path) // an unstatable transcript has no settle to wait for
			continue
		}
		c.e.mu.Lock()
		last, read := c.e.endChecked[path]
		c.e.mu.Unlock()
		if read && last.Equal(mt) {
			continue
		}
		if u.ResumedAt != "" && mt.UTC().Format(time.RFC3339Nano) == u.ResumedAt {
			c.dropEndPending(path)
			continue // resumed: the child has not written since the answer
		}
		cause := fmt.Sprintf("the child of %s ended (its transcript's last turn is over) with no done or failed", u.Name)
		ended := false
		if se, ok := harness.For(u.Claim.Harness).(harness.SettlingEnd); ok {
			switch se.EndVerdict(path) {
			case harness.EndYes:
				ended = true
			case harness.EndIfSettled:
				settled, err := c.endSettled(se, u, path, mt)
				if err != nil {
					return err
				}
				if !settled {
					continue // pending: looked at again when due, or when the mtime moves
				}
				ended = true
			}
		} else {
			ended = harness.For(u.Claim.Harness).Ended(path)
		}
		c.dropEndPending(path)
		if ended {
			if err := c.offerEnded(in, cause); err != nil {
				return err
			}
		}
		c.e.mu.Lock()
		if c.e.endChecked == nil {
			c.e.endChecked = map[string]time.Time{}
		}
		c.e.endChecked[path] = mt
		c.e.mu.Unlock()
	}
	return nil
}

// dropEndPending forgets path's pending settle.
func (c *cycle) dropEndPending(path string) {
	c.e.mu.Lock()
	delete(c.e.endPending, path)
	c.e.mu.Unlock()
}

// endSettled decides an EndIfSettled transcript: arms its settle from mt when
// it has none (or one for an older mtime), and when due asks whether a
// process of the child is live. It returns true only when the settle is due
// and none is; a live marker or a failed scan re-arms the settle.
func (c *cycle) endSettled(se harness.SettlingEnd, u *unit, path string, mt time.Time) (bool, error) {
	c.e.mu.Lock()
	p, ok := c.e.endPending[path]
	if !ok || !p.mt.Equal(mt) {
		p = endPend{mt: mt, due: mt.Add(endSettle)}
	}
	if c.e.endPending == nil {
		c.e.endPending = map[string]endPend{}
	}
	c.e.endPending[path] = p
	c.e.mu.Unlock()
	if c.now().Before(p.due) {
		return false, nil
	}
	live := c.e.opts.ChildLive
	if live == nil {
		live = func(_, child string) (bool, error) { return se.ChildLive(child) }
	}
	if alive, err := live(u.Claim.Harness, u.Claim.Agent); err != nil || alive {
		c.e.mu.Lock()
		c.e.endPending[path] = endPend{mt: mt, due: c.now().Add(endSettle)}
		c.e.mu.Unlock()
		return false, nil
	}
	return true, nil
}
