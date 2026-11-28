// Package agytest builds agy records trees in temp dirs for harness and wf
// tests. It writes records exactly as agy 1.5.12 does (probe
// in the protocol) and never touches a real
// agy home. This package and harness's agyRecords are the two places the
// brain/<id> layout appears; the identity tests prove they agree.
package agytest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/internal/harnessfixture"
)

// Probe ids and the claim tokens FromProbe gives their briefs.
const (
	Probe1Coordinator = "00000000-0000-4000-8000-000000000002"
	Probe1ChildA      = "00000000-0000-4000-8000-000000000001" // --claim PROBE-A-7f3c
	Probe1ChildB      = "00000000-0000-4000-8000-000000000003" // --claim PROBE-B-9d21
	Probe2Coordinator = "00000000-0000-4000-8000-000000000005"
	Probe2ChildA      = "00000000-0000-4000-8000-000000000004" // --claim PROBE2-A-51e0
	Probe2ChildB      = "00000000-0000-4000-8000-000000000006" // --claim PROBE2-B-c4d2, the long brief
)

// Probe1Launch and Probe2Launch are the probe coordinators' launching
// responses' created_at.
var (
	Probe1Launch = time.Date(2025, 1, 1, 0, 1, 3, 0, time.UTC)
	Probe2Launch = time.Date(2025, 1, 1, 0, 4, 5, 0, time.UTC)
)

// Tree is one records root (what ANTIGRAVITY_APP_DATA_DIR names).
type Tree struct {
	Root string
	t    testing.TB
	msgs int
}

// New is an empty records root in t.TempDir().
func New(t testing.TB) *Tree {
	t.Helper()
	return &Tree{Root: t.TempDir(), t: t}
}

// probeClaims are the probe briefs' claim phrases FromProbe rewrites.
var probeClaims = []string{"PROBE-A-7f3c", "PROBE-B-9d21", "PROBE2-A-51e0", "PROBE2-B-c4d2"}

// FromProbe copies the synthetic probe into t.TempDir() and rewrites
// each probe brief's "claim <TOKEN>." to "--claim <TOKEN> ." in every file:
// the probe briefs name their claim without the flag the engine's briefs
// carry. The replacement holds no byte JSON escapes, so the launch's Prompt,
// the child's step-0 wrapper and the messages record stay byte-equal.
func FromProbe(t testing.TB) *Tree {
	t.Helper()
	tr := New(t)
	src, err := fs.Sub(harnessfixture.Files(), "agy/probe")
	if err != nil {
		t.Fatal(err)
	}
	err = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(tr.Root, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		for _, tok := range probeClaims {
			b = bytes.ReplaceAll(b, []byte("claim "+tok+"."), []byte("--claim "+tok+" ."))
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// Env is the environment of conversation id's shell under this root:
// ANTIGRAVITY_CONVERSATION_ID, ANTIGRAVITY_APP_DATA_DIR = Root, HOME = a temp dir.
func (tr *Tree) Env(id string) harness.Env {
	home := tr.t.TempDir()
	kv := map[string]string{
		"ANTIGRAVITY_CONVERSATION_ID": id,
		"ANTIGRAVITY_APP_DATA_DIR":    tr.Root,
		"HOME":                        home,
	}
	return func(k string) string { return kv[k] }
}

func (tr *Tree) generated(id string) string {
	return filepath.Join(tr.Root, "brain", id, ".system_generated")
}

// Transcript is conversation id's step log path.
func (tr *Tree) Transcript(id string) string {
	return filepath.Join(tr.generated(id), "logs", "transcript_full.jsonl")
}

func (tr *Tree) write(path string, b []byte) {
	tr.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		tr.t.Fatal(err)
	}
}

func stamp(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05Z") }

// nextIndex is the step_index the next line of id's log takes.
func (tr *Tree) nextIndex(id string) int {
	b, err := os.ReadFile(tr.Transcript(id))
	if err != nil {
		return 0
	}
	n := 0
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			n++
		}
	}
	return n
}

// appendStep adds one line to id's log; fields are agy's own names.
func (tr *Tree) appendStep(id string, fields map[string]any) {
	tr.t.Helper()
	fields["step_index"] = tr.nextIndex(id)
	line, err := json.Marshal(fields)
	if err != nil {
		tr.t.Fatal(err)
	}
	p := tr.Transcript(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		tr.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		tr.t.Fatal(err)
	}
}

// Coordinator writes a coordinator conversation whose step 0 is a USER_INPUT at at.
func (tr *Tree) Coordinator(id string, at time.Time) {
	tr.t.Helper()
	tr.appendStep(id, map[string]any{
		"source": "USER_EXPLICIT", "type": "USER_INPUT", "status": "DONE",
		"created_at": stamp(at), "content": "<USER_REQUEST>\nrun the workflow\n</USER_REQUEST>",
	})
}

// Call is one tool call of a PLANNER_RESPONSE.
type Call struct {
	Name string
	Args map[string]any
}

func callsJSON(cs []Call) []map[string]any {
	out := make([]map[string]any, len(cs))
	for i, c := range cs {
		out[i] = map[string]any{"name": c.Name, "args": c.Args}
	}
	return out
}

// Respond appends a PLANNER_RESPONSE to id's log: model, input and output
// tokens, content, and tool calls given as name + args.
func (tr *Tree) Respond(id string, at time.Time, model string, in, out int, content string, calls ...Call) {
	tr.t.Helper()
	f := map[string]any{
		"source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": stamp(at),
		"model": model, "input_tokens": in, "cache_read_tokens": 0, "output_tokens": out,
	}
	if content != "" {
		f["content"] = content
	}
	if len(calls) > 0 {
		f["tool_calls"] = callsJSON(calls)
	}
	tr.appendStep(id, f)
}

// Launch is one child start: Brief is what the engine handed out, At the
// launching response's time (seconds), Model the spawn's tier.
type Launch struct {
	Coordinator, Child, Brief, Model string
	At                               time.Time
}

// Delivery wrapper as agy writes it.
const (
	wrapPrefix = "The following is a <SYSTEM_MESSAGE> not actually sent by the user. It is provided by the system as important information to pay attention to.\n\n<SYSTEM_MESSAGE>\n[Message] "
	wrapSuffix = "\n</SYSTEM_MESSAGE>"
)

// entry is one child of a launching response and the ways a test may bend
// what agy would have written for it.
type entry struct {
	l            Launch
	noLaunch     bool      // no call and no result: a conversation the launch never names
	resultID     string    // the id the result step names (default the child)
	sender       string    // step-0 sender (default the coordinator)
	prefix       string    // wrapper prefix (default agy's)
	body         *string   // step-0 body (default the Prompt)
	listingUnder string    // the conversation holding the listing file (default the coordinator)
	childAt      time.Time // step 0's created_at (default launch + 3s)
}

// spawnCall is the tool call parsed back from agy.SpawnLine's own output, so
// the format the engine emits and the format the identity code reads cannot
// drift: the tool name before the first space, the JSON object after it.
func (tr *Tree) spawnCall(l Launch) (Call, string) {
	tr.t.Helper()
	line := harness.For("agy").SpawnLine(harness.Spawn{Brief: l.Brief, Model: l.Model})
	name, rest, ok := strings.Cut(line, " ")
	if !ok {
		tr.t.Fatalf("spawn line has no arguments: %q", line)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(rest), &args); err != nil {
		tr.t.Fatalf("spawn line arguments are not JSON: %v: %s", err, rest)
	}
	prompt, _ := args["Prompt"].(string)
	return Call{Name: name, Args: args}, prompt
}

// launch records one launching response of coordinator at at for es, as agy
// does, and returns each entry's Prompt.
func (tr *Tree) launch(coordinator string, at time.Time, es []entry) []string {
	tr.t.Helper()
	prompts := make([]string, len(es))
	var calls []Call
	var results []string
	for i, e := range es {
		c, p := tr.spawnCall(e.l)
		prompts[i] = p
		if e.noLaunch {
			continue
		}
		calls = append(calls, c)
		id := e.resultID
		if id == "" {
			id = e.l.Child
		}
		results = append(results, id)
	}
	if len(calls) > 0 {
		tr.Respond(coordinator, at, "gemini-3.8-flash-low", 1000, 50, "", calls...)
		for _, id := range results {
			tr.LaunchResult(coordinator, at, id)
		}
	}
	for i, e := range es {
		tr.records(coordinator, at, e, prompts[i])
	}
	return prompts
}

// LaunchResult appends to coordinator's log the GENERIC step agy writes when
// an invoke_subagent call completes: "Created the following subagent:" and the
// child's conversationId, MODEL source, DONE.
func (tr *Tree) LaunchResult(coordinator string, at time.Time, child string) {
	tr.t.Helper()
	tr.appendStep(coordinator, map[string]any{
		"source": "MODEL", "type": "GENERIC", "status": "DONE", "created_at": stamp(at),
		"content": fmt.Sprintf("Created At: %s\nCreated the following subagent:\n{\n  \"conversationId\":  %q,\n  \"workspaceUris\":  []\n}\nThe subagent will send you a message when it finishes.", stamp(at), child),
	})
}

// CommandResult appends to id's log the step agy writes when a run_command
// call completes: GENERIC, DONE, MODEL source, content as the first child’s step 2 of probe 2
// Created At and Completed At lines, then the command's exit and
// output. A run_command result is GENERIC like a launch's, but names no child.
func (tr *Tree) CommandResult(id string, at time.Time, output string) {
	tr.t.Helper()
	tr.appendStep(id, map[string]any{
		"source": "MODEL", "type": "GENERIC", "status": "DONE", "created_at": stamp(at),
		"content": fmt.Sprintf("Created At: %s\nCompleted At: %s\nCommand exited with code 0.\n\nCommand output:\n%s", stamp(at), stamp(at), output),
	})
}

// OtherResult appends to id's log a MODEL-source, DONE result step of a type
// other than GENERIC (typ, for example CODE_ACTION): a tool result agy keeps
// under its own step type. It is never a created-child result.
func (tr *Tree) OtherResult(id string, at time.Time, typ string) {
	tr.t.Helper()
	tr.appendStep(id, map[string]any{
		"source": "MODEL", "type": typ, "status": "DONE", "created_at": stamp(at), "content": "edited a file",
	})
}

// ErrorMessage appends to id's log the step agy writes when a model call
// fails and is retried: SYSTEM source, ERROR_MESSAGE, DONE, an error field and
// no content (the first child’s step 5 of probe 2: keys created_at, error, source, status,
// step_index, type). It answers no call.
func (tr *Tree) ErrorMessage(id string, at time.Time) {
	tr.t.Helper()
	tr.appendStep(id, map[string]any{
		"source": "SYSTEM", "type": "ERROR_MESSAGE", "status": "DONE", "created_at": stamp(at),
		"error": "API error (attempt 1): RESOURCE_EXHAUSTED (code 429): capacity exhausted. Resets in 0s.",
	})
}

// TaskMessage appends to id's log a SYSTEM_MESSAGE delivered in agy's wrapper
// from "<id>/task-<n>", a background task's message (the first child’s step 7 of probe 2).
// It answers no call.
func (tr *Tree) TaskMessage(id string, at time.Time, n int) {
	tr.t.Helper()
	content := wrapPrefix + "timestamp=" + stamp(at) + " sender=" + fmt.Sprintf("%s/task-%d", id, n) + " priority=MESSAGE_PRIORITY_HIGH content=task finished" + wrapSuffix
	tr.appendStep(id, map[string]any{
		"source": "SYSTEM", "type": "SYSTEM_MESSAGE", "status": "DONE", "created_at": stamp(at), "content": content,
	})
}

// Item is one call of a Response: a launch (its child's records are written)
// or any other call, such as run_command. Exactly one field is set.
type Item struct {
	Launch *Launch
	Call   *Call
}

// Response appends to coordinator's log one PLANNER_RESPONSE at at whose calls
// are items in order, writes the records of every launch's child (the listing,
// step 0, the message: a child's records may exist before its result does) and
// writes no result step. The test appends results and interjections in log
// order with LaunchResult, CommandResult, ErrorMessage and TaskMessage. It
// returns each launch item's Prompt, in order.
func (tr *Tree) Response(coordinator string, at time.Time, items ...Item) []string {
	tr.t.Helper()
	var calls []Call
	var prompts []string
	var es []entry
	var ps []string
	for _, it := range items {
		switch {
		case it.Launch != nil && it.Call == nil:
			c, p := tr.spawnCall(*it.Launch)
			calls = append(calls, c)
			prompts = append(prompts, p)
			es = append(es, entry{l: *it.Launch})
			ps = append(ps, p)
		case it.Call != nil && it.Launch == nil:
			calls = append(calls, *it.Call)
		default:
			tr.t.Fatal("agytest: an Item sets exactly one of Launch and Call")
		}
	}
	tr.Respond(coordinator, at, "gemini-3.8-flash-low", 1000, 50, "", calls...)
	for i, e := range es {
		tr.records(coordinator, at, e, ps[i])
	}
	return prompts
}

// StartPartial records one launching response of coordinator at at with every
// launch's call, only the first written created-child results (agy writes them
// one at a time, a launch taking about 11 s: see fixture coordinator 2 steps 5-7) and every
// child's records, as a pending child's may already exist. It returns the
// prompts in call order.
func (tr *Tree) StartPartial(coordinator string, at time.Time, written int, ls ...Launch) []string {
	tr.t.Helper()
	items := make([]Item, len(ls))
	for i := range ls {
		items[i] = Item{Launch: &ls[i]}
	}
	prompts := tr.Response(coordinator, at, items...)
	for i := 0; i < written && i < len(ls); i++ {
		tr.LaunchResult(coordinator, at, ls[i].Child)
	}
	return prompts
}

// records writes what agy keeps for one child: the listing, its log's step 0
// (the delivery) and the structured message.
func (tr *Tree) records(coordinator string, at time.Time, e entry, prompt string) {
	tr.t.Helper()
	under := e.listingUnder
	if under == "" {
		under = coordinator
	}
	listing, _ := json.Marshal(map[string]any{
		"conversationId":     e.l.Child,
		"subagentDescriptor": map[string]any{"typeName": "self", "role": "wf-unit"},
		"state":              "SUBAGENT_STATE_ALIVE",
	})
	tr.write(filepath.Join(tr.generated(under), "subagents", e.l.Child+".json"), listing)

	body := prompt
	if e.body != nil {
		body = *e.body
	}
	sender := e.sender
	if sender == "" {
		sender = coordinator
	}
	prefix := e.prefix
	if prefix == "" {
		prefix = wrapPrefix
	}
	childAt := e.childAt
	if childAt.IsZero() {
		childAt = at.Add(3 * time.Second)
	}
	content := prefix + "timestamp=" + stamp(childAt) + " sender=" + sender + " priority=MESSAGE_PRIORITY_HIGH content=" + body + wrapSuffix
	tr.appendStep(e.l.Child, map[string]any{
		"source": "SYSTEM", "type": "SYSTEM_MESSAGE", "status": "DONE", "created_at": stamp(childAt), "content": content,
	})

	tr.msgs++
	id := fmt.Sprintf("00000000-0000-4000-8000-%012x", tr.msgs)
	msg, _ := json.Marshal(map[string]any{
		"id": id, "recipient": e.l.Child, "sender": sender, "priority": "MESSAGE_PRIORITY_HIGH",
		"timestamp": childAt.UTC().Format(time.RFC3339Nano), "content": body,
	})
	tr.write(filepath.Join(tr.generated(e.l.Child), "messages", id+".json"), msg)
}

// Start records l as agy does: appends to the coordinator's log a
// PLANNER_RESPONSE at l.At whose one tool call is parsed back from
// harness.For("agy").SpawnLine(harness.Spawn{Brief: l.Brief, Model: l.Model}),
// then its GENERIC result naming the child; writes the coordinator's listing
// file for the child; writes the child's log with step 0 the delivery wrapper
// (sender the coordinator, content the Prompt) three seconds later; and the
// structured messages record. It returns the parsed call's Prompt (the brief
// as the spawn line carries it).
func (tr *Tree) Start(l Launch) string {
	tr.t.Helper()
	return tr.launch(l.Coordinator, l.At, []entry{{l: l}})[0]
}

// StartAll records several launches in ONE launching response (k-th call ↔
// k-th result).
func (tr *Tree) StartAll(coordinator string, at time.Time, ls ...Launch) {
	tr.t.Helper()
	es := make([]entry, len(ls))
	for i, l := range ls {
		es[i] = entry{l: l}
	}
	tr.launch(coordinator, at, es)
}

// Spoof is one forged or wrong variant of a Start, each of which must verify nothing.
type Spoof int

const (
	ResultNamesOther Spoof = iota // (7) the result names another id; the listing and step 0 name the child
	TaskSender                    // step-0 sender is <coordinator>/task-7
	Grandchild                    // the child relaunches the brief to G: G's step 0 is from the child, its launch in the child's log
	AutoMode                      // brain/ holds only the child's own conversation
	SiblingSameBrief              // a sibling (no launch result names it) whose step 0 holds the same brief from the coordinator, plus a listing
	OtherCoordinator              // the launch, result and listing are under another coordinator; the child's step 0 names it
	BeforeHandOut                 // the launch is 2s before l.At, the hand-out
	OneByte                       // step 0's body differs from the Prompt by one byte
	TwoChildren                   // two launches of the same brief, two children
	UnknownWrapper                // step 0's prefix altered by one word
	ChildOlder                    // step 0 one second before the launch
	ListingElsewhere              // the listing file sits under another conversation, not the coordinator
)

// StartSpoofed records l with spoof s applied (l.Coordinator needs its own
// log, from Coordinator) and returns the child id a naive matcher would pick.
func (tr *Tree) StartSpoofed(l Launch, s Spoof) string {
	tr.t.Helper()
	e := entry{l: l}
	switch s {
	case ResultNamesOther:
		e.resultID = "other-" + l.Child
	case TaskSender:
		e.sender = l.Coordinator + "/task-7"
	case Grandchild:
		mid := "mid-" + l.Child
		tr.launch(l.Coordinator, l.At, []entry{{l: Launch{Coordinator: l.Coordinator, Child: mid, Brief: "relaunch what you are told", At: l.At}}})
		g := l
		g.Coordinator = mid
		g.At = l.At.Add(4 * time.Second)
		tr.launch(mid, g.At, []entry{{l: g}})
		return l.Child
	case AutoMode:
		tr.Start(l)
		if err := os.RemoveAll(filepath.Join(tr.Root, "brain", l.Coordinator)); err != nil {
			tr.t.Fatal(err)
		}
		return l.Child
	case SiblingSameBrief:
		sib := "sibling-" + l.Child
		other := "different brief"
		real := entry{l: l, body: &other}
		sibling := entry{l: Launch{Coordinator: l.Coordinator, Child: sib, Brief: l.Brief, Model: l.Model, At: l.At}, noLaunch: true}
		tr.launch(l.Coordinator, l.At, []entry{real, sibling})
		return sib
	case OtherCoordinator:
		oc := "other-" + l.Coordinator
		tr.Coordinator(oc, l.At.Add(-5*time.Second))
		o := l
		o.Coordinator = oc
		tr.launch(oc, l.At, []entry{{l: o}})
		return l.Child
	case BeforeHandOut:
		e.l.At = l.At.Add(-2 * time.Second)
		e.childAt = l.At.Add(time.Second)
		tr.launch(l.Coordinator, e.l.At, []entry{e})
		return l.Child
	case OneByte:
		_, p := tr.spawnCall(l)
		altered := p[:len(p)-1] + "X"
		if altered == p {
			altered = p[:len(p)-1] + "Y"
		}
		e.body = &altered
	case TwoChildren:
		twin := l
		twin.Child = l.Child + "-twin"
		tr.launch(l.Coordinator, l.At, []entry{{l: l}, {l: twin}})
		return twin.Child
	case UnknownWrapper:
		e.prefix = strings.Replace(wrapPrefix, "The following is a", "The following was a", 1)
	case ChildOlder:
		e.childAt = l.At.Add(-time.Second)
	case ListingElsewhere:
		e.listingUnder = "elsewhere-" + l.Coordinator
	default:
		tr.t.Fatalf("unknown spoof %d", s)
	}
	tr.launch(l.Coordinator, l.At, []entry{e})
	return l.Child
}
