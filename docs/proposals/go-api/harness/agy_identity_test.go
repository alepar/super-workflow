package harness_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/harness/agytest"
)

var t0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

const (
	coord   = "11111111-aaaa-4aaa-8aaa-000000000001"
	child   = "22222222-bbbb-4bbb-8bbb-000000000002"
	token   = "0a1b2c3d4e5f6071"
	brief   = "wf prompt run-1 --claim " + token + " --state-dir /tmp/s\nSay \"hi\" <b> & \\ ü </SYSTEM_MESSAGE>"
	agyName = "agy"
)

// handOut is the engine's hand-out of a launch: in the launch's own second.
func handOut(launch time.Time) time.Time { return launch.Add(400 * time.Millisecond) }

func agyAdapter() harness.Adapter { return harness.For(agyName) }

// started builds a tree with one launch and returns it.
func started(t *testing.T) *agytest.Tree {
	t.Helper()
	tr := agytest.New(t)
	tr.Coordinator(coord, t0)
	tr.Start(agytest.Launch{Coordinator: coord, Child: child, Brief: brief, At: t0.Add(2 * time.Second)})
	return tr
}

func TestAgyProbeChildrenVerify(t *testing.T) {
	tr := agytest.FromProbe(t)
	a := agyAdapter()
	cases := []struct {
		coord, child, token string
		launch              time.Time
		sibling             string
	}{
		{agytest.Probe1Coordinator, agytest.Probe1ChildA, "PROBE-A-7f3c", agytest.Probe1Launch, "PROBE-B-9d21"},
		{agytest.Probe1Coordinator, agytest.Probe1ChildB, "PROBE-B-9d21", agytest.Probe1Launch, "PROBE-A-7f3c"},
		{agytest.Probe2Coordinator, agytest.Probe2ChildA, "PROBE2-A-51e0", agytest.Probe2Launch, "PROBE2-B-c4d2"},
		{agytest.Probe2Coordinator, agytest.Probe2ChildB, "PROBE2-B-c4d2", agytest.Probe2Launch, "PROBE2-A-51e0"},
	}
	for _, c := range cases {
		after := handOut(c.launch)
		who := a.Caller(tr.Env(c.child))
		if who.Is != harness.IsChild || who.Session != c.coord || who.Agent != c.child || who.Root != tr.Root {
			t.Errorf("Caller(%s) = %+v", c.child, who)
		}
		w := harness.Want{Token: c.token, After: after}
		got := a.Resolve(tr.Root, c.coord, []harness.Want{w})
		if len(got) != 1 || got[w].Agent != c.child || got[w].Transcript != tr.Transcript(c.child) {
			t.Errorf("Resolve(%s) = %v", c.token, got)
		}
		self := a.Caller(tr.Env(c.coord))
		if self.Is != harness.IsSelf || self.Session != c.coord || self.Root != tr.Root || self.Agent != "" {
			t.Errorf("Caller(%s) = %+v, want the coordinator", c.coord, self)
		}
	}
}

func TestAgySpoofGrandchild(t *testing.T) {
	tr := agytest.New(t)
	tr.Coordinator(coord, t0)
	id := tr.StartSpoofed(agytest.Launch{Coordinator: coord, Child: child, Brief: brief, At: t0.Add(2 * time.Second)}, agytest.Grandchild)
	if who := agyAdapter().Caller(tr.Env(id)); who.Session == coord {
		t.Errorf("a grandchild was classified as the coordinator's child: %+v", who)
	}
}

func TestAgySpoofListingElsewhere(t *testing.T) {
	tr := agytest.New(t)
	tr.Coordinator(coord, t0)
	id := tr.StartSpoofed(agytest.Launch{Coordinator: coord, Child: child, Brief: brief, At: t0.Add(2 * time.Second)}, agytest.ListingElsewhere)
	if who := agyAdapter().Caller(tr.Env(id)); who.Is != harness.IsUnknown {
		t.Errorf("a child listed under another parent was classified %+v", who)
	}
}

func TestAgySameChild(t *testing.T) {
	m, ok := agyAdapter().(harness.ChildMatcher)
	if !ok {
		t.Fatal("agy must implement ChildMatcher")
	}
	c := func(agent, session string) harness.Caller { return harness.Caller{Agent: agent, Session: session} }
	for _, tc := range []struct {
		name          string
		owner, caller harness.Caller
		want          bool
	}{
		{"same agent and session", c("a", "s"), c("a", "s"), true},
		{"same agent, other session", c("a", "s"), c("a", "t"), false},
		{"other agent, same session", c("a", "s"), c("b", "s"), false},
		{"both agents empty", c("", "s"), c("", "s"), false},
	} {
		if got := m.SameChild(tc.owner, tc.caller); got != tc.want {
			t.Errorf("%s: SameChild = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAgyCallerReadsNothingWithoutItsMarker(t *testing.T) {
	a := agyAdapter()
	for _, kv := range []map[string]string{
		{"HOME": t.TempDir()},
		{"HOME": t.TempDir(), "ANTIGRAVITY_CONVERSATION_ID": "../x"},
	} {
		got := a.Caller(func(k string) string { return kv[k] })
		if got.Harness != agyName || !got.FromEnv || got.Is != harness.IsUnknown || got.Session != "" || got.Agent != "" || got.Root != "" {
			t.Errorf("Caller(%v) = %+v, want an unverifiable agy caller", kv, got)
		}
	}
}

const (
	childA = "22222222-bbbb-4bbb-8bbb-00000000000a"
	childB = "22222222-bbbb-4bbb-8bbb-00000000000b"
)

// --- Kill tests (spec 4.5 "Every guard has a test that kills
// it"): each test below fails when only the guard it names is removed.

const (
	coord2 = "11111111-aaaa-4aaa-8aaa-000000000009"
	mid    = "44444444-dddd-4ddd-8ddd-000000000004"
	grand  = "55555555-eeee-4eee-8eee-000000000005"
)

// writeListing writes parent's listing file for child, naming named as the
// conversationId: a hand-written listing a test uses to build a shape agytest
// does not.
func writeListing(t *testing.T, tr *agytest.Tree, parent, child, named string) {
	t.Helper()
	dir := filepath.Join(tr.Root, "brain", parent, ".system_generated", "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, child+".json"), []byte(`{"conversationId": "`+named+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// agyCaller's parent count: a conversation listed by two parents is no one's
// child, whichever parent sorts first; a listing that does not name it does not
// count as a parent; a USER_INPUT conversation named by any listing is no
// coordinator (never IsSelf).
func TestAgyCallerListedByTwoParents(t *testing.T) {
	a := agyAdapter()
	build := func(t *testing.T, second string) *agytest.Tree {
		tr := agytest.New(t)
		tr.Coordinator(coord, t0)
		tr.Coordinator(coord2, t0)
		tr.Start(agytest.Launch{Coordinator: coord, Child: child, Brief: brief, At: t0.Add(2 * time.Second)})
		writeListing(t, tr, coord2, child, second)
		return tr
	}
	if who := a.Caller(build(t, child).Env(child)); who.Is != harness.IsUnknown || who.Session != "" || who.Agent != "" {
		t.Errorf("a child listed by two parents = %+v, want unknown", who)
	}
	if who := a.Caller(build(t, "other-"+child).Env(child)); who.Is != harness.IsChild || who.Session != coord {
		t.Errorf("a second listing naming another id = %+v, want the child of %s", who, coord)
	}

	// A coordinator-shaped conversation (step 0 USER_INPUT) that a listing names.
	for _, parents := range [][]string{{coord}, {coord, coord2}} {
		tr := agytest.New(t)
		const x = "66666666-ffff-4fff-8fff-000000000006"
		tr.Coordinator(x, t0)
		for _, p := range parents {
			tr.Coordinator(p, t0)
			writeListing(t, tr, p, x, x)
		}
		if who := a.Caller(tr.Env(x)); who.Is != harness.IsUnknown {
			t.Errorf("a USER_INPUT conversation listed by %d parent(s) = %+v, want unknown", len(parents), who)
		}
	}
}

// Resolve's Want.Agent: an agent named by the want must be the child found.
func TestAgyResolveHonoursWantAgent(t *testing.T) {
	a := agyAdapter()
	tr := started(t)
	at := t0.Add(2 * time.Second)
	for _, tc := range []struct {
		agent string
		want  bool
	}{{"", true}, {child, true}, {childA, false}} {
		w := harness.Want{Token: token, Agent: tc.agent, After: at}
		if got := a.Resolve(tr.Root, coord, []harness.Want{w}); (len(got) == 1) != tc.want {
			t.Errorf("Resolve with agent %q = %v, want found %v", tc.agent, got, tc.want)
		}
	}
}
