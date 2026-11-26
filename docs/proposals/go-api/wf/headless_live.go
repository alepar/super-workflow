package wf

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// headlessInert: this cycle must neither read nor signal headless child
// processes: a dry cycle or a replay, whose recorded pids are another
// run's. A fork drops the source's records at its rewound record and
// watches only the children it spawns itself.
func (c *cycle) headlessInert() bool { return c.dry || c.e.recordedReplay }

// headlessOf is u's headless child started for its latest offer, nil when
// none was started for it.
func headlessOf(u *unit) *HeadlessStartedData {
	if u == nil || u.Headless == nil || len(u.Offered) == 0 || u.Headless.Claim != u.Offered[len(u.Offered)-1] {
		return nil
	}
	return u.Headless
}

// headlessLive reports whether u's headless child for its latest offer is
// still running.
func (c *cycle) headlessLive(u *unit) bool {
	if c.headlessInert() {
		return false
	}
	hs := headlessOf(u)
	return hs != nil && c.e.headlessProcess().Live(hs.Pid, hs.Start)
}

// errTailText is the last lines of a child's stderr as one quoted line, ""
// when it holds none.
func errTailText(path string) string {
	lines := errLogExcerpt(path)
	if len(lines) == 0 {
		return ""
	}
	return "; its stderr ends: " + strings.Join(lines, " | ")
}

// loseGoneHeadless raises the recovery offer of each claimed unit whose
// headless child's process is gone with no done or failed: at once, since
// a headless child's exit is its end (no transcript needs to settle). A
// headless child cannot be resumed by its coordinator, so the offer names
// only restart and give-up. A unit whose child exited before claiming it is
// left to the never-started re-offer, which quotes the same stderr.
func (c *cycle) loseGoneHeadless() error {
	if c.headlessInert() {
		return nil
	}
	for _, id := range slices.Clone(c.st.order) {
		in := c.st.insts[id]
		if in == nil || in.Settled || c.st.ended || !c.untilUnitAllows(in) {
			continue
		}
		u := in.Unit
		hs := headlessOf(u)
		if hs != nil && u.State == UnitHanded && !u.HeadlessAt.IsZero() && !c.now().Before(u.HeadlessAt.Add(headlessFetchBound)) {
			// Running and never fetched within the bound: a hung launch.
			// Stopped, its unit is re-offered (unstartedCause) quoting its
			// stderr.
			c.killHeadless(u)
			continue
		}
		if hs == nil || !u.State.claimed() || u.Claim == nil || u.Recovery != nil || u.waveClaim() {
			continue
		}
		if c.e.headlessProcess().Live(hs.Pid, hs.Start) {
			continue
		}
		cause := fmt.Sprintf("the headless %s child of %s (pid %d) exited with no done or failed%s", hs.Harness, u.Name, hs.Pid, errTailText(hs.ErrLog))
		if err := c.offerEnded(in, cause); err != nil {
			return err
		}
	}
	return nil
}

// headlessFetchBound is how long a headless child may run without fetching
// its prompt before the engine stops it as hung.
const headlessFetchBound = 120 * time.Second

// killHeadless stops u's headless child, if it still runs: its attempt is
// over (lost, given up, or the run ended).
func (c *cycle) killHeadless(u *unit) {
	if c.headlessInert() {
		return
	}
	if hs := headlessOf(u); hs != nil {
		_ = c.e.headlessProcess().Kill(hs.Pid, hs.Start)
	}
}

// killAllHeadless stops every headless child still running: the run ended.
func (c *cycle) killAllHeadless() {
	for _, in := range c.st.insts {
		if in != nil {
			c.killHeadless(in.Unit)
		}
	}
}
