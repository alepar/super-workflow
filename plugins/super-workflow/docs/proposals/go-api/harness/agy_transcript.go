package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// agy records live under brain/<id>. This file assembles the records paths.

// agyRoot is the records root: ANTIGRAVITY_APP_DATA_DIR, which agy sets in
// every command's shell, else
// $HOME/.gemini/antigravity-cli from the environment's HOME. Never the
// process's own home directory: an environment with neither has no records
// root ("").
func agyRoot(env Env) string {
	if d := env("ANTIGRAVITY_APP_DATA_DIR"); d != "" {
		return d
	}
	if h := env("HOME"); h != "" {
		return filepath.Join(h, ".gemini", "antigravity-cli")
	}
	return ""
}

// agyValidID reports whether id can name a conversation directory: a
// conversation id never holds a path separator, so one that does (or is . or
// ..) is refused before any path is built from it.
func agyValidID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00")
}

// agyGenerated is <root>/brain/<id>/.system_generated, "" for no root or a
// bad id.
func agyGenerated(root, id string) string {
	if root == "" || !agyValidID(id) {
		return ""
	}
	return filepath.Join(root, "brain", id, ".system_generated")
}

// agyTranscript is conversation id's own step log.
func agyTranscript(root, id string) string {
	g := agyGenerated(root, id)
	if g == "" {
		return ""
	}
	return filepath.Join(g, "logs", "transcript_full.jsonl")
}

// agyListing is the file parent holds for its child.
func agyListing(root, parent, child string) string {
	g := agyGenerated(root, parent)
	if g == "" || !agyValidID(child) {
		return ""
	}
	return filepath.Join(g, "subagents", child+".json")
}

// agyMessagesDir holds the structured deliveries to id.
func agyMessagesDir(root, id string) string {
	g := agyGenerated(root, id)
	if g == "" {
		return ""
	}
	return filepath.Join(g, "messages")
}

// agyListingParents are the conversations whose listing directory holds a
// file for child: its parents as the records show them, unverified (an
// identity check decides what they mean). Sorted.
func agyListingParents(root, child string) []string {
	if agyGenerated(root, child) == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(root, "brain", "*", ".system_generated", "subagents", child+".json"))
	var out []string
	for _, f := range files {
		// <root>/brain/<parent>/.system_generated/subagents/<child>.json
		p := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(f))))
		if p != child && agyValidID(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// --- agyStep: the single decoder of transcript_full.jsonl steps. Readers and the identity code never
// unmarshal agy JSON themselves; a new field is added here. It decides no
// meaning.

type agyStep struct {
	Index     int       `json:"-"` // step_index; 0 when absent (see HasIndex)
	HasIndex  bool      `json:"-"` // the record carries a step_index
	RawIndex  *int      `json:"step_index"`
	Source    string    `json:"source"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	CreatedAt string    `json:"created_at"`
	Content   string    `json:"content"`
	Input     *int      `json:"input_tokens"`
	Output    *int      `json:"output_tokens"`
	CacheRead *int      `json:"cache_read_tokens"`
	Calls     []agyCall `json:"tool_calls"`
}

// agyCall is one tool call of a PLANNER_RESPONSE: {name, args}.
type agyCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

const (
	agyResponse = "PLANNER_RESPONSE"
	agySystem   = "SYSTEM_MESSAGE"
	agyGeneric  = "GENERIC"
)

// agyDecode is one line as a step; false for a line that is not one (half
// written, or no type).
func agyDecode(line []byte) (agyStep, bool) {
	var s agyStep
	if json.Unmarshal(line, &s) != nil || s.Type == "" {
		return agyStep{}, false
	}
	if s.RawIndex != nil {
		s.Index, s.HasIndex = *s.RawIndex, true
	}
	return s, true
}

// agySteps calls f with each decodable step of the transcript at path, in
// order, and bad with each non-blank line that is not a step; false when the
// file cannot be read. bad may be nil.
func agySteps(path string, f func(agyStep), bad func()) bool {
	if path == "" {
		return false
	}
	return eachLine(path, func(line []byte) {
		if len(strings.TrimSpace(string(line))) == 0 {
			return
		}
		if s, ok := agyDecode(line); ok {
			f(s)
		} else if bad != nil {
			bad()
		}
	})
}

// usage reports the step's token counts: input plus output, cache reads not
// counted; false for a step that carries none.
func (s agyStep) usage() (int, bool) {
	if s.Type != agyResponse || (s.Input == nil && s.Output == nil) {
		return 0, false
	}
	n := 0
	if s.Input != nil {
		n += *s.Input
	}
	if s.Output != nil {
		n += *s.Output
	}
	return n, true
}

// agyTaskRef is the background task a RUNNING run_command result names
// ("task id: <conversation>/task-<n>").
var agyTaskRef = regexp.MustCompile(`task id: (\S+/task-\d+)`)

// taskID is the background task this RUNNING result step started; "" when it
// names none. Only a tool result (GENERIC) counts: a model's own text that
// quotes a task id starts nothing.
func (s agyStep) taskID() string {
	if s.Type != agyGeneric {
		return ""
	}
	if m := agyTaskRef.FindStringSubmatch(s.Content); m != nil {
		return m[1]
	}
	return ""
}

// agyCreated is the subagent object a launch's GENERIC result prints after
// "Created the following subagent:\n".
const agyCreated = "Created the following subagent:\n"

// createdChild is the conversationId a launch's result step names: the JSON
// object that follows agyCreated, decoded with a json.Decoder (it is
// followed by more text); false when the step is not such a result.
func (s agyStep) createdChild() (string, bool) {
	if s.Type != agyGeneric {
		return "", false
	}
	i := strings.Index(s.Content, agyCreated)
	if i < 0 {
		return "", false
	}
	var v struct {
		ConversationID string `json:"conversationId"`
	}
	if json.NewDecoder(strings.NewReader(s.Content[i+len(agyCreated):])).Decode(&v) != nil || !agyValidID(v.ConversationID) {
		return "", false
	}
	return v.ConversationID, true
}

// --- agyWrapper: agy's message delivery. The one recogniser of the wrapper; an unrecognised wrapper is
// no delivery and never matches.

const (
	agyWrapPrefix = "The following is a <SYSTEM_MESSAGE> not actually sent by the user. It is provided by the system as important information to pay attention to.\n\n<SYSTEM_MESSAGE>\n[Message] "
	agyWrapSuffix = "\n</SYSTEM_MESSAGE>"
)

// agyWrapHeader is the header right after the prefix, anchored: a brief that
// itself holds a [Message] … sender= line is body, never header.
var agyWrapHeader = regexp.MustCompile(`^timestamp=(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z) sender=(\S+) priority=(\S+) content=`)

// agyDelivery is one unwrapped message.
type agyDelivery struct {
	At               time.Time
	Sender, Priority string
	Body             string
}

// agyWrapper unwraps a SYSTEM_MESSAGE step's content: the fixed prefix, the
// anchored header, and the FINAL suffix, so a body holding
// "</SYSTEM_MESSAGE>" comes back intact. A sender
// "<id>/task-<n>" is a background task's message, never the conversation id.
func agyWrapper(content string) (agyDelivery, bool) {
	rest, ok := strings.CutPrefix(content, agyWrapPrefix)
	if !ok || !strings.HasSuffix(rest, agyWrapSuffix) {
		return agyDelivery{}, false
	}
	rest = rest[:len(rest)-len(agyWrapSuffix)]
	m := agyWrapHeader.FindStringSubmatchIndex(rest)
	if m == nil {
		return agyDelivery{}, false
	}
	at, err := time.Parse(time.RFC3339, rest[m[2]:m[3]])
	if err != nil {
		return agyDelivery{}, false
	}
	return agyDelivery{At: at, Sender: rest[m[4]:m[5]], Priority: rest[m[6]:m[7]], Body: rest[m[1]:]}, true
}

// --- readers on agy (spec §4.3). They take a transcript path, never an env.

// Usage: the sum of input and output tokens over the transcript's
// PLANNER_RESPONSE steps; cache reads are not counted. ok is false when the
// file is unreadable or no step carries a count.
func (agy) Usage(transcript string) (int, bool) {
	total, seen := 0, false
	if !agySteps(transcript, func(s agyStep) {
		if n, ok := s.usage(); ok {
			total += n
			seen = true
		}
	}, nil) {
		return 0, false
	}
	return total, seen
}

// agyEnd folds steps, in log order, into Ended's verdict: one fold serves the
// tail window and the full read, so the two cannot weigh a step differently.
type agyEnd struct {
	last         agyStep
	have, broken bool // broken: the last non-blank line was not a step
	pending      map[string]bool
}

func newAgyEnd() *agyEnd { return &agyEnd{pending: map[string]bool{}} }

// step folds one decoded step.
func (e *agyEnd) step(s agyStep) {
	e.last, e.have, e.broken = s, true, false
	if s.Status == "RUNNING" {
		id := s.taskID()
		if id == "" {
			id = "step-" + strconv.Itoa(s.Index)
		}
		e.pending[id] = true
	}
	if s.Type == agySystem {
		if d, ok := agyWrapper(s.Content); ok {
			delete(e.pending, d.Sender)
		}
	}
}

// bad folds one non-blank line that is not a step.
func (e *agyEnd) bad() { e.broken = true }

// contentOnly: the last step is a response with text and no tool call.
func (e *agyEnd) contentOnly() bool {
	return e.have && e.last.Type == agyResponse && strings.TrimSpace(e.last.Content) != "" && len(e.last.Calls) == 0
}

func (e *agyEnd) ended() bool { return !e.broken && len(e.pending) == 0 && e.contentOnly() }

// agyEndedFull is Ended over the whole transcript.
func agyEndedFull(transcript string) bool {
	e := newAgyEnd()
	return agySteps(transcript, e.step, e.bad) && e.ended()
}

// agyEndedTail is Ended over the tailLines window; decided is false when
// only a full read can answer. The window always holds the file's last
// lines, so a broken last line, or a last step that is not a content-only
// response, makes a full read false too. A window whose first line is step 0
// (agy writes step 0 as its log's first line) is the whole file. Any other
// window with a content-only last step, or with no complete step, may hide a
// RUNNING step further back: undecided.
func agyEndedTail(transcript string) (ended, decided bool) {
	e := newAgyEnd()
	first, whole := true, false
	if !tailLines(transcript, func(line []byte) {
		s, ok := agyDecode(line)
		if first {
			first, whole = false, ok && s.HasIndex && s.Index == 0
		}
		if ok {
			e.step(s)
		} else {
			e.bad()
		}
	}) {
		return false, true // unreadable: a full read cannot read it either
	}
	switch {
	case whole:
		return e.ended(), true
	case e.broken || (e.have && !e.contentOnly()):
		return false, true
	default:
		return false, false
	}
}

// Ended: the last step is a PLANNER_RESPONSE with content and no tool calls,
// no background task is still pending, and no line after it is half written.
// A content-only response ends a turn, not the conversation, while a task it
// started still runs: a RUNNING result names <id>/task-<n> and is pending
// until a SYSTEM_MESSAGE from that sender. A RUNNING step that names no task is pending for good. Never
// exit codes, never status alone. It reads the tailLines window and falls
// back to a full read whenever the window does not start at step 0 (or holds
// no complete step) and its last step is a content-only response: a RUNNING
// step may be further back than the window (spec §4.3, amended), so every
// window that could answer "ended" is confirmed by a full read and the
// result always equals a full read's.
func (agy) Ended(transcript string) bool {
	if ended, decided := agyEndedTail(transcript); decided {
		return ended
	}
	return agyEndedFull(transcript)
}

// Tail: the last tool call (name and JSON arguments) and the last response
// text, each one line (oneLineCut).
func (agy) Tail(transcript string) (string, string) {
	var tool, msg string
	agySteps(transcript, func(s agyStep) {
		if s.Type != agyResponse {
			return
		}
		if len(s.Calls) > 0 {
			c := s.Calls[len(s.Calls)-1]
			args, _ := json.Marshal(c.Args)
			tool = oneLineCut(c.Name + " " + string(args))
		}
		if strings.TrimSpace(s.Content) != "" {
			msg = oneLineCut(s.Content)
		}
	}, nil)
	return tool, msg
}

// agyReadTools are agy's file-read tools and the one argument that names the
// file read: read_file's Path is recorded (agy-probe transcripts 0cb4f0e2,
// 4eadc29f, 5c903001); view_file is agy's documented read tool (its hooks reference), its
// AbsolutePath argument unrecorded.
var agyReadTools = map[string]string{
	"read_file": "Path",
	"view_file": "AbsolutePath",
}

// read is the absolute path this call reads; false for a call that is not a
// file read or whose path argument is missing or relative (agy records no
// working directory per call, so a relative path resolves to nothing).
func (c agyCall) read() (string, bool) {
	key, ok := agyReadTools[c.Name]
	if !ok {
		return "", false
	}
	p, ok := c.Args[key].(string)
	if !ok || !filepath.IsAbs(p) {
		return "", false
	}
	return filepath.Clean(p), true
}

// ReadAny: a file-read call (agyReadTools) whose path argument is one of
// paths. Only a read tool's own path argument counts: a path named in any
// other argument, or in another tool's call (run_command's CommandLine,
// write_file's Content, send_message's Message), is no read. known is false
// when the transcript is unreadable or holds no tool call.
func (agy) ReadAny(transcript string, paths []string) (bool, bool) {
	want := map[string]bool{}
	for _, p := range paths {
		if filepath.IsAbs(p) {
			want[filepath.Clean(p)] = true
		}
	}
	read, calls := false, 0
	if !agySteps(transcript, func(s agyStep) {
		if s.Type != agyResponse {
			return
		}
		for _, c := range s.Calls {
			calls++
			if p, ok := c.read(); ok && want[p] {
				read = true
			}
		}
	}, nil) {
		return false, false
	}
	return read, calls > 0
}

// CoordinatorTranscript: the session's own step log, when it exists.
func (agy) CoordinatorTranscript(root, session string) string {
	if p := agyTranscript(root, session); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
