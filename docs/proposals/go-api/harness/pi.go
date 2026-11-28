package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/sink"
)

// pi is the pi coding agent (@earendil-works/pi-coding-agent). It has no
// subagent tool: a child is a separate `pi -p` process the coordinator starts
// in the background with its bash tool (SpawnLine), so it outlives its
// coordinator. pi gives every bash command it runs PI_SESSION_ID and
// PI_SESSION_FILE, its own session's, after removing inherited copies; the
// spawn line hands the coordinator's id to the child as SUPER_PI_PARENT,
// which pi passes through. Each session is one JSONL file,
// <agent dir>/sessions/<cwd slug>/<timestamp>_<session id>.jsonl, whose first
// line is the session header and whose first user message is the prompt the
// process was started with, verbatim.
type pi struct{ Generic }

func (pi) Name() string { return "pi" }

// Markers: pi sets PI_SESSION_ID for each bash command it runs.
func (pi) Markers() []string { return []string{"PI_SESSION_ID"} }

// Caller: the session of the pi process whose bash ran the command, and the
// agent dir (PI_CODING_AGENT_DIR, else ~/.pi/agent). A child started by the
// spawn line has SUPER_PI_PARENT, its coordinator's session: the caller is
// that coordinator's child, its own session its agent id. Without it the
// caller is the session's own agent, the coordinator. Identity is
// cooperative: the claim token binds a child's call.
func (pi) Caller(env Env) Caller {
	own, parent := env("PI_SESSION_ID"), env("SUPER_PI_PARENT")
	c := Caller{Harness: "pi", Session: own, FromEnv: true}
	c.Root, _ = piRoot(env)
	switch {
	case own == "":
	case parent == "" || parent == own:
		c.Is = IsSelf
	default:
		c.Session, c.Agent, c.Is = parent, own, IsChild
	}
	return c
}

// piRoot is the agent dir the engine reads pi's records under: the
// PI_CODING_AGENT_DIR value with a leading ~ or ~/ expanded against HOME, then
// cleaned; else <home>/.pi/agent. It never resolves a relative value against
// the working directory of the wf process, which is not pi's: a relative
// value returns an empty root and the variable's name as the second result.
// Caller and piFiles (through the root a Caller recorded) take the root
// from here.
func piRoot(env Env) (root, relative string) {
	dir := env("PI_CODING_AGENT_DIR")
	if dir == "" {
		return home(env, "", filepath.Join(".pi", "agent")), ""
	}
	switch {
	case dir == "~":
		return home(env, "", ""), ""
	case strings.HasPrefix(dir, "~/"):
		return home(env, "", dir[2:]), ""
	case !filepath.IsAbs(dir):
		return "", "PI_CODING_AGENT_DIR"
	}
	return filepath.Clean(dir), ""
}

// SameChild: a pi child is its own session under the same coordinator.
func (pi) SameChild(owner, caller Caller) bool {
	return owner.Agent == caller.Agent && owner.Session == caller.Session
}

// OutputLimit: pi's bash tool keeps the last 2000 lines or 50 KB of a
// command's output, whichever is less (core/tools/truncate.js).
func (pi) OutputLimit(Env) (int, int) { return 50 << 10, 2000 }

// ToolLimit: pi's bash tool has no default timeout; the model may pass one
// per call, in seconds, and deepseek-v4.1-flash passed 30 and 60 in the
// probe, as Hermes models did. The limit is the shortest seen, as Hermes's.
func (pi) ToolLimit(Env) time.Duration { return hermesCallLimit }

// Tiers: OpenRouter models through pi's openrouter provider, at thinking
// levels each one has in pi's catalog (both lack medium and xhigh; pi would
// clamp them). GLM 5.3 Flash always thinks; its tiers never name off.
func (pi) Tiers() map[string]Tier {
	const deepseek, glm = "openrouter/deepseek/deepseek-v4.1-flash-1017", "openrouter/z-ai/glm-5.3-flash"
	return map[string]Tier{
		"light":    {deepseek, "low"},
		"standard": {deepseek, "high"},
		"strong":   {glm, "high"},
		"top":      {glm, "high"},
	}
}

// SpawnLine starts the child in the background: nohup, its brief on stdin
// as a quoted here-document (print-mode pi takes piped stdin, trimmed, as its
// first message, verbatim: no shell quoting, option or @file reading applies
// to it), its output dropped and its stderr kept in ErrLog (why it did not
// start), -ne to disable extensions, and its session id named for its
// claim, so the coordinator's launch names the child's session file.
// SUPER_PI_CHILD puts the session id in the child's environment, so its
// processes can be found. The line is three lines: the command, the brief
// and the delimiter (piBriefEnd); the coordinator pastes the brief unchanged.
// An empty ErrLog prints 2>/dev/null, which piLaunch refuses: the engine
// always fills ErrLog for pi.
func (pi) SpawnLine(s Spawn) string {
	var b strings.Builder
	if s.Claim != "" {
		b.WriteString(piChildMarkerWord(piSessionID(s.Claim)) + " ")
	}
	b.WriteString(`SUPER_PI_PARENT="$PI_SESSION_ID" nohup pi -p -ne`)
	if s.Claim != "" {
		b.WriteString(" --session-id " + sink.Quote(piSessionID(s.Claim)))
	}
	if s.Model != "" {
		b.WriteString(" --model " + sink.Quote(s.Model))
	}
	if s.Effort != "" && !(s.Effort == "off" && strings.Contains(s.Model, "glm")) {
		b.WriteString(" --thinking " + sink.Quote(s.Effort))
	}
	errLog := s.ErrLog
	if errLog == "" {
		errLog = "/dev/null"
	}
	b.WriteString(piHeredoc + " >/dev/null 2>" + sink.Quote(errLog) + " &\n" + s.Brief + "\n" + piBriefEnd)
	return b.String()
}

// piBriefEnd ends the brief's here-document; piHeredoc opens it, quoted, so
// the shell expands nothing in the brief.
const (
	piBriefEnd = "WF_BRIEF"
	piHeredoc  = " <<'" + piBriefEnd + "'"
)

// piSessionID is the session id a unit's child is started with.
func piSessionID(claim string) string { return "wf-" + claim }

func (pi) SpawnSession(claim string) string { return piSessionID(claim) }

// piChildMarker is the environment entry that names a child's session id
// (the launch's first word). It is the one place the entry is spelled:
// SpawnLine and ResumeLine write it (through piChildMarkerWord, which
// quotes only the value), and the engine's liveness
// check looks for exactly this string among a process's environment entries.
func piChildMarker(id string) string { return "SUPER_PI_CHILD=" + id }

// piChildMarkerWord is the marker as the launch line's assignment word: the
// value quoted, the name bare, so the shell still reads it as an assignment.
func piChildMarkerWord(id string) string { return "SUPER_PI_CHILD=" + sink.Quote(id) }

// piBriefProblem is why pi would not take brief verbatim as the here-document
// of the launch line, or "" when it would: no line that is the delimiter
// (piBriefEnd, which would end the brief early and run the rest as
// commands), and a first word that begins with / must hold another / (an
// absolute path; one slash only is a prompt template such as /review). The
// first word is the text up to the first space, tab or newline.
func piBriefProblem(brief string) string {
	for _, line := range strings.Split(brief, "\n") {
		// Spaces or a CR around it are refused too: a retyped brief can lose
		// them, and the bare delimiter would end the here-document early.
		if strings.TrimSpace(line) == piBriefEnd {
			return "it has a line " + piBriefEnd + ", which ends the launch line's here-document"
		}
	}
	first := strings.TrimLeft(brief, " \t\n")
	if i := strings.IndexAny(first, " \t\n"); i >= 0 {
		first = first[:i]
	}
	if strings.HasPrefix(first, "/") && !strings.Contains(first[1:], "/") {
		return "its first word begins with / and holds no other /, so pi would read it as a prompt template"
	}
	return ""
}

// CheckBrief refuses a brief the launch line cannot carry as its
// here-document: the reason is piBriefProblem's.
func (pi) CheckBrief(brief string) error {
	if why := piBriefProblem(brief); why != "" {
		return fmt.Errorf("pi cannot launch this brief: %s", why)
	}
	return nil
}

// StartLog: where the stderr of a launch that did not start is.
func (pi) StartLog(errLog string) string {
	return "If you ran its launch, why it did not start is in " + errLog + "."
}

// ResumeLine continues the child's own session (pi resumes a session id it
// finds for the working directory) with message, in the background again.
// Its stderr stays merged into the dropped output: a resume has no log file.
func (pi) ResumeLine(agent, message string) string {
	if agent == "" {
		return ""
	}
	return fmt.Sprintf(`%s SUPER_PI_PARENT="$PI_SESSION_ID" nohup pi -p -ne --session-id %s -- %s </dev/null >/dev/null 2>&1 &`, piChildMarkerWord(agent), sink.Quote(agent), sink.Quote(message))
}

// ResumeNote: pi finds a session id only in the session folder of the
// directory it runs in, so the resume runs where the child ran.
func (pi) ResumeNote() string {
	return "Make that call in the child's own working directory (the one its launch ran in, after any leading cd): pi finds a session id only in that directory's session folder and elsewhere starts a new session without saying so. A resume the engine refuses falls back to a fresh hand-out: run the restart line below and the unit is handed out again."
}

// piFiles are the session files of id under root: those named for it whose
// header names it too (one per working directory pi ran it in). root comes
// from piRoot.
func piFiles(root, id string) []string {
	if root == "" || id == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(root, "sessions", "*", "*_"+id+".jsonl"))
	var match []string
	for _, f := range files {
		if h, ok := piHead(f); ok && h.id == id {
			match = append(match, f)
		}
	}
	return match
}

// piFile is the most recently written session file of id: a coordinator's
// own record (CoordinatorTranscript). A child is found by its launch instead
// (piChild).
func piFile(root, id string) string { return newest(piFiles(root, id)) }

type piHeader struct {
	id, cwd, kickoff string
	created          time.Time // the header's timestamp: when the session began
}

// piHead is a session file's header and its first user message's text, the
// prompt the process was started with.
func piHead(path string) (piHeader, bool) {
	var h piHeader
	first, done := true, false
	ok := eachLine(path, func(line []byte) {
		if done {
			return
		}
		if first {
			first = false
			var s struct{ Type, ID, Cwd, Timestamp string }
			if json.Unmarshal(line, &s) != nil || s.Type != "session" {
				done = true
				return
			}
			h.id, h.cwd = s.ID, s.Cwd
			h.created, _ = time.Parse(time.RFC3339Nano, s.Timestamp)
			return
		}
		var e struct {
			Type    string
			Message struct {
				Role    string
				Content json.RawMessage
			}
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "message" || e.Message.Role != "user" {
			return
		}
		h.kickoff, done = piText(e.Message.Content), true
	})
	return h, ok && h.id != ""
}

// piText is a message content's text: a string, or its text blocks joined.
func piText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var parts []struct{ Type, Text string }
	if json.Unmarshal(content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// piUsage is the token count pi records on an entry. Reasoning and total
// fields are ignored: reasoning is already inside Output.
type piUsage struct{ Input, Output, CacheRead, CacheWrite int }

// piRecord is one session entry as the adapter reads it.
type piRecord struct {
	Type      string
	ID        string
	TargetID  string `json:"targetId"`
	Timestamp string
	// Usage is the token count of usage, compaction and branch_summary
	// entries, which carry it at the top level.
	Usage   *piUsage
	Message struct {
		Role       string
		StopReason string
		// ErrorMessage is why an error stop failed.
		ErrorMessage string
		Content      json.RawMessage
		Usage        *piUsage
	}
}

type piCall struct {
	Name      string
	Arguments map[string]any
}

// calls are the toolCall blocks of an assistant message.
func (r piRecord) calls() []piCall {
	var blocks []struct {
		Type      string
		Name      string
		Arguments map[string]any
	}
	if r.Type != "message" || r.Message.Role != "assistant" || json.Unmarshal(r.Message.Content, &blocks) != nil {
		return nil
	}
	var out []piCall
	for _, b := range blocks {
		if b.Type == "toolCall" {
			out = append(out, piCall{b.Name, b.Arguments})
		}
	}
	return out
}

// piStart is one child launch in a coordinator's session file: the session
// id it names, its brief, the stderr file it writes, and when the call was
// made.
type piStart struct {
	session, brief, errLog string
	dir                    string // the launch's leading cd, if any
	at                     time.Time
}

// piLaunches are the pi children the session file at path started with its
// bash tool since after: each bash call whose command is the adapter's own
// launch line (piLaunch).
func piLaunches(path string, after time.Time) []piStart {
	var out []piStart
	eachLine(path, func(line []byte) {
		if !strings.Contains(string(line), `"toolCall"`) {
			return
		}
		var r piRecord
		if json.Unmarshal(line, &r) != nil {
			return
		}
		at, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil || at.Before(after) {
			return
		}
		for _, c := range r.calls() {
			if c.Name != "bash" {
				continue
			}
			if cmd, _ := c.Arguments["command"].(string); cmd != "" {
				first, body, _ := strings.Cut(strings.TrimLeft(cmd, " \t\n"), "\n")
				if dir, line, ok := piCd(first); ok {
					if session, brief, errLog, ok := piLaunch(line + "\n" + body); ok {
						out = append(out, piStart{session, brief, errLog, dir, at})
					}
				}
			}
		}
	})
	return out
}

// piChildWord is the spawn line's first word: the child's session id as an
// environment entry (piChildMarker), unquoted.
var piChildWord = regexp.MustCompile(`^SUPER_PI_CHILD=([A-Za-z0-9._-]+)$`)

// piParentWord is the spawn line's hand-over of the coordinator's session,
// in the spellings a coordinator may type for it.
var piParentWord = regexp.MustCompile(`^SUPER_PI_PARENT=(?:"\$PI_SESSION_ID"|\$PI_SESSION_ID|"\$\{PI_SESSION_ID\}"|\$\{PI_SESSION_ID\})$`)

// piErrLogWord is the stderr file as one unquoted literal absolute path.
var piErrLogWord = regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`)

// piCd splits one optional leading `cd <dir> && ` off a launch: pi's bash
// tool has no working-directory parameter, so a coordinator told to run in
// the workspace prefixes one. dir must be one literal absolute
// path, and the cd nothing else (no option, second argument, substitution,
// `;` or `||`); a command without a leading cd returns "" and itself.
func piCd(command string) (dir, rest string, ok bool) {
	command = strings.TrimSpace(command)
	if !strings.HasPrefix(command, "cd") {
		return "", command, true
	}
	head, tail, found := cutShellAnd(command)
	if !found {
		return "", "", false
	}
	argv, good := literalShellArgv(head)
	if !good || len(argv) != 2 || argv[0] != "cd" || !filepath.IsAbs(argv[1]) {
		return "", "", false
	}
	return filepath.Clean(argv[1]), strings.TrimSpace(tail), true
}

// cutShellAnd splits command at its first real `&&`: unquoted, unescaped.
// A `#` before it (a comment, or one in a word) refuses the split, so text
// the shell never runs cannot stand in for the separator.
func cutShellAnd(command string) (head, tail string, ok bool) {
	var quote byte
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case c == '\\' && quote != '\'':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#':
			return "", "", false
		case c == '&' && i+1 < len(command) && command[i+1] == '&':
			return command[:i], command[i+2:], true
		}
	}
	return "", "", false
}

// sameDir: a launch's cd is where its child's session says it ran, through
// symlinks (pi records the physical working directory).
func sameDir(dir, cwd string) bool {
	if dir == filepath.Clean(cwd) {
		return true
	}
	d, err1 := filepath.EvalSymlinks(dir)
	c, err2 := filepath.EvalSymlinks(cwd)
	return err1 == nil && err2 == nil && d == c
}

// piShellToken is one token of a launch line: a word, or one of the operators
// <, >, 2>, & and &&. raw is the word as typed; value is what the shell
// passes for it, and literal says the shell passes it unexpanded (no $ or `
// outside single quotes, no unquoted glob or tilde character).
type piShellToken struct {
	op, raw, value string
	literal        bool
}

// piShellTokens splits a whole launch line into words and the operators the
// launch form uses, quote-aware, in one pass. A # that begins a word outside
// quotes (a comment), a newline outside quotes, any other operator (; | ( )
// >> >& <& &> and the like) or an unclosed quote refuses the line, so no text
// the shell would not run can stand in for part of the form.
func piShellTokens(command string) ([]piShellToken, bool) {
	if strings.ContainsRune(command, 0) {
		return nil, false
	}
	var toks []piShellToken
	var raw, value strings.Builder
	var quote byte
	started, literal := false, true
	flush := func() {
		if started {
			toks = append(toks, piShellToken{raw: raw.String(), value: value.String(), literal: literal})
		}
		raw.Reset()
		value.Reset()
		started, literal = false, true
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		if c == '\\' && quote != '\'' {
			if i+1 == len(command) || command[i+1] == '\n' || command[i+1] == '\r' {
				return nil, false
			}
			i++
			if quote == '"' && !strings.ContainsRune("$`\"\\", rune(command[i])) {
				value.WriteByte('\\')
			}
			raw.WriteString(command[i-1 : i+1])
			value.WriteByte(command[i])
			started = true
			continue
		}
		if quote != 0 {
			raw.WriteByte(c)
			if c == quote {
				quote = 0
				continue
			}
			if quote == '"' && (c == '$' || c == '`') {
				literal = false
			}
			value.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			raw.WriteByte(c)
			started = true
		case ' ', '\t':
			flush()
		case '#':
			if !started {
				return nil, false // a comment
			}
			raw.WriteByte(c)
			value.WriteByte(c)
		case '<':
			if started || i+1 < len(command) && strings.IndexByte("<>&", command[i+1]) >= 0 {
				return nil, false
			}
			toks = append(toks, piShellToken{op: "<"})
		case '>':
			op := ">"
			if started {
				if raw.String() != "2" {
					return nil, false
				}
				raw.Reset()
				value.Reset()
				started, literal = false, true
				op = "2>"
			}
			if i+1 < len(command) && strings.IndexByte(">&|", command[i+1]) >= 0 {
				return nil, false
			}
			toks = append(toks, piShellToken{op: op})
		case '&':
			flush()
			if i+1 < len(command) && command[i+1] == '&' {
				toks = append(toks, piShellToken{op: "&&"})
				i++
				continue
			}
			if i+1 < len(command) && command[i+1] == '>' {
				return nil, false
			}
			toks = append(toks, piShellToken{op: "&"})
		case '\n', '\r', ';', '|', '(', ')':
			return nil, false
		default:
			if c == '$' || c == '`' || strings.IndexByte("*?[]{}", c) >= 0 || c == '~' && !started {
				literal = false
			}
			raw.WriteByte(c)
			value.WriteByte(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	flush()
	return toks, true
}

// piLaunch reads a bash command as the adapter's launch line, literally and
// in exactly the shape SpawnLine emits. Its first line, tokenized whole by
// piShellTokens, is the SUPER_PI_CHILD marker, then the SUPER_PI_PARENT
// hand-over, nohup, pi with -p, -ne and --session-id, optionally --model and
// --thinking (each at most once, no other option or word: no -e or other
// extension option, no message argument), then exactly <<'WF_BRIEF'
// >/dev/null 2><path> & with nothing after: the brief on stdin, output
// redirected, stderr written to <session id>.stderr at an unquoted absolute
// path, and the background operator. The lines after it are the brief (one
// acceptable to piBriefProblem) and then the delimiter as the last line. The
// marker's value must be the session id. It returns the session id, the
// prompt and the stderr path. Anything else (a foreground launch, a missing
// -ne, an unquoted delimiter, 2>/dev/null or 2>&1, a comment, pipes,
// substitutions, a second command, text after the delimiter) is no launch.
func piLaunch(command string) (session, brief, errLog string, ok bool) {
	command = strings.TrimSuffix(command, "\n")
	first, rest, found := strings.Cut(command, "\n")
	if !found {
		return "", "", "", false
	}
	if rest == piBriefEnd {
		brief = ""
	} else if b, cut := strings.CutSuffix(rest, "\n"+piBriefEnd); cut {
		brief = b
	} else {
		return "", "", "", false
	}
	cmd, redirs, found := strings.Cut(first, piHeredoc+" ")
	if !found {
		return "", "", "", false
	}
	toks, good := piShellTokens(cmd)
	tail, good2 := piShellTokens(redirs)
	if !good || !good2 || len(toks) < 2+2 || len(tail) != 5 {
		return "", "", "", false
	}
	m := piChildWord.FindStringSubmatch(toks[0].raw)
	if toks[0].op != "" || m == nil || toks[1].op != "" || !piParentWord.MatchString(toks[1].raw) {
		return "", "", "", false
	}
	marker := m[1]
	if tail[0].op != ">" || tail[1].op != "" || tail[1].raw != "/dev/null" ||
		tail[2].op != "2>" || tail[3].op != "" || !piErrLogWord.MatchString(tail[3].raw) ||
		tail[4].op != "&" {
		return "", "", "", false
	}
	errLog = tail[3].raw
	var argv []string
	for _, t := range toks[2:] {
		if t.op != "" || !t.literal {
			return "", "", "", false
		}
		argv = append(argv, t.value)
	}
	if len(argv) < 2 || argv[0] != "nohup" || filepath.Base(argv[1]) != "pi" {
		return "", "", "", false
	}
	seen := map[string]bool{}
	for i := 2; i < len(argv); i++ {
		name := argv[i]
		switch name {
		case "--print":
			name = "-p"
		case "--no-extensions":
			name = "-ne"
		}
		switch name {
		case "-p", "-ne":
		case "--session-id", "--model", "--thinking":
			if i+1 >= len(argv) {
				return "", "", "", false
			}
			i++
			if name == "--session-id" {
				session = argv[i]
			}
		default:
			return "", "", "", false // an option the adapter never emits, or a message argument
		}
		if seen[name] {
			return "", "", "", false
		}
		seen[name] = true
	}
	if !seen["-p"] || !seen["-ne"] || session == "" || marker != session ||
		filepath.Base(errLog) != session+".stderr" || piBriefProblem(brief) != "" {
		return "", "", "", false
	}
	return session, brief, errLog, true
}

// sameBrief: a child's kickoff is the brief it was launched with.
func sameBrief(kickoff, brief string) bool {
	return strings.TrimSpace(kickoff) == strings.TrimSpace(brief)
}

// piChild is the child the coordinator session started for token since
// after: a launch in the coordinator's own session file whose brief names
// token as its one claim and whose session id is the claim's (piSessionID),
// and that session's own file (named and headed by that id, begun at or
// after the launch, in the launch's cd when it has one) whose first user
// message is that brief. Every file of
// that id is checked against the launch, not the newest alone, so an older
// session of the same id elsewhere (copied or touched since) neither hides
// the child nor stands in for it; more than one match is ambiguous and
// finds nothing. A launch in any other session's file, a file begun before
// its launch, or a file whose name and header disagree is not it. When
// errLog (the claim's recorded stderr path) is not empty, a launch whose 2>
// word differs is not it either.
func piChild(root, session, token, errLog string, after time.Time) (Child, bool) {
	coord := piFile(root, session)
	if coord == "" || token == "" {
		return Child{}, false
	}
	want := piSessionID(token)
	if want == session {
		return Child{}, false
	}
	matched := map[string]bool{}
	for _, l := range piLaunches(coord, after) {
		if l.session != want || !firstClaimIs([]byte(l.brief), token) || errLog != "" && l.errLog != errLog {
			continue
		}
		for _, f := range piFiles(root, want) {
			if h, ok := piHead(f); ok && !h.created.Before(l.at.Truncate(time.Millisecond)) && sameBrief(h.kickoff, l.brief) && (l.dir == "" || sameDir(l.dir, h.cwd)) {
				matched[f] = true
			}
		}
	}
	if len(matched) != 1 {
		return Child{}, false
	}
	for f := range matched {
		return Child{Agent: want, Transcript: f}, true
	}
	return Child{}, false
}

// Resolve: the child piChild finds for each wanted token since its
// hand-out; its agent id is its session id.
func (pi) Resolve(root, session string, wants []Want) map[Want]Child {
	out := map[Want]Child{}
	for _, w := range wants {
		if c, ok := piChild(root, session, w.Token, w.ErrLog, w.After); ok && (w.Agent == "" || w.Agent == c.Agent) {
			out[w] = c
		}
	}
	return out
}

// CoordinatorTranscript: the session's own file.
func (pi) CoordinatorTranscript(root, session string) string { return piFile(root, session) }

// Usage sums input, cache writes and output (pi counts reasoning in output;
// cache reads are not counted) over everything the session recorded:
// assistant and tool-result messages, usage entries, and compaction and
// branch_summary entries. ok is false only when none of them records usage.
func (pi) Usage(transcript string) (int, bool) {
	n, seen := 0, false
	if !eachLine(transcript, func(line []byte) {
		var r piRecord
		if json.Unmarshal(line, &r) != nil {
			return
		}
		var u *piUsage
		switch r.Type {
		case "message":
			if r.Message.Role == "assistant" || r.Message.Role == "toolResult" {
				u = r.Message.Usage
			}
		case "usage", "compaction", "branch_summary":
			u = r.Usage
		}
		if u != nil {
			n, seen = n+u.Input+u.CacheWrite+u.Output, true
		}
	}) || !seen {
		return 0, false
	}
	return n, true
}

// piEnd is the end rule over the raw lines of a session's tail, in order. It
// reads no clock, stats no file and looks at no process; the engine settles
// what it answers EndIfSettled (SettlingEnd). The last message entry decides:
//   - not an assistant's, or one with a tool call: EndNo;
//   - aborted: EndYes (the process was interrupted and writes no more);
//   - stop: EndIfSettled (pi may still run a threshold compaction after it);
//   - error or length: EndNo when a later context_edit targets it (pi retries
//     or recovers: it writes the context_edit before its retry backoff sleep
//     and before the overflow summarisation call, so the edit is on disk
//     before the pause), else EndIfSettled (a later compaction or usage entry
//     changes nothing). A context_edit after an error does not prove a retry
//     follows: when recovery compaction fails or is cancelled, pi keeps the
//     omission, appends no compaction and schedules no retry. The reader
//     reads that as not ended (EndNo) and the stall clock covers it;
//   - anything else (toolUse, deferred, pending, no stop): EndNo.
//
// A line after that message that is not JSON is a half-written entry: EndNo.
// A recovery whose context_edit has not been written yet reads EndIfSettled
// at the error; the engine's settle, and the child's live marker, cover it.
func piEnd(lines [][]byte) EndVerdict {
	last := -1
	var recs []piRecord
	for i, l := range lines {
		var r piRecord
		if json.Unmarshal(l, &r) != nil {
			recs = append(recs, piRecord{Type: "\x00unreadable"})
			continue
		}
		recs = append(recs, r)
		if r.Type == "message" {
			last = i
		}
	}
	if last < 0 {
		return EndNo
	}
	for _, r := range recs[last+1:] {
		if r.Type == "\x00unreadable" {
			return EndNo
		}
	}
	m := recs[last]
	if m.Message.Role != "assistant" || len(m.calls()) > 0 {
		return EndNo
	}
	switch m.Message.StopReason {
	case "aborted":
		return EndYes
	case "stop":
		return EndIfSettled
	case "error", "length":
		if m.ID != "" {
			for _, r := range recs[last+1:] {
				if r.Type == "context_edit" && r.TargetID == m.ID {
					return EndNo
				}
			}
		}
		return EndIfSettled
	}
	return EndNo
}

// EndVerdict reads the tail of the session file with piEnd; an unreadable
// file is EndNo.
func (pi) EndVerdict(transcript string) EndVerdict {
	var lines [][]byte
	if !tailLines(transcript, func(l []byte) { lines = append(lines, slices.Clone(l)) }) {
		return EndNo
	}
	return piEnd(lines)
}

// Ended: pi writes no end record, and only an aborted turn is final at once;
// every other stop may be followed by a retry or a compaction (EndVerdict).
func (pi) Ended(transcript string) bool { return pi{}.EndVerdict(transcript) == EndYes }

// ChildLive: whether the child's launched pi process is still running on
// this host: a process carrying the child's marker (piChildMarker) in its
// environment that is that pi itself (piChildLive). A process the child
// started inherits the marker but is not counted, so a surviving descendant
// does not hold the child live. An error means unknown: the engine then
// re-arms its settle and never reports the child ended on it.
func (pi) ChildLive(child string) (bool, error) { return piChildLive("/proc", child) }

// piChildLive scans <proc>/<pid>/environ for the exact NUL-delimited entry
// piChildMarker(child) and counts a holder only when its command line is the
// launched pi's (piLaunchedCmdline). A pid that vanished mid-scan is skipped,
// and so is one that cannot be the launched pi: a process whose environ is
// owned by another uid (another user's, or a non-dumpable one, which /proc
// shows as root's) or that holds permitted capabilities (pi, started by this
// user, holds none). A pid that could be it but whose environ or command line
// cannot be read makes the scan incomplete: an error, unknown and not dead.
// A failure to list proc is an error too.
func piChildLive(proc, child string) (bool, error) {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return false, err
	}
	want := piChildMarker(child)
	uid := os.Getuid()
	for _, e := range entries {
		name := e.Name()
		if name == "" || strings.Trim(name, "0123456789") != "" {
			continue
		}
		dir := filepath.Join(proc, name)
		env, err := os.ReadFile(filepath.Join(dir, "environ"))
		if err != nil {
			if piGone(err) || !piCouldBeOurs(dir, uid) {
				continue
			}
			return false, fmt.Errorf("the environment of pid %s cannot be read: %w", name, err)
		}
		if !slices.Contains(strings.Split(string(env), "\x00"), want) {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			if piGone(err) {
				continue
			}
			return false, fmt.Errorf("the command line of pid %s cannot be read: %w", name, err)
		}
		if piLaunchedCmdline(cmdline, child) {
			return true, nil
		}
	}
	return false, nil
}

// piLaunchedCmdline: a command line that is the launched pi of child. Before
// node retitles it, the process (nohup, env, node, through the launch's exec
// chain) shows its arguments, among them --session-id <child>. pi then
// retitles its process "pi", and node's retitle (libuv) overwrites the whole
// argument area with the title and NULs, so /proc/<pid>/cmdline reads "pi"
// alone (checked with node 22: process.title = "pi" leaves "pi" and NULs).
// That form is matched as it is: the marker in the same process's environment
// names the child, and a process the child started has its own command line
// (one more pi, retitled, is the one case this cannot tell apart, and it
// reads live, never dead). The open session file was not used instead: pi
// appends to its session file and does not keep it open.
func piLaunchedCmdline(cmdline []byte, child string) bool {
	var args []string
	for _, a := range strings.Split(string(cmdline), "\x00") {
		if a != "" {
			args = append(args, a)
		}
	}
	if len(args) == 1 && args[0] == "pi" {
		return true
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--session-id" && args[i+1] == child {
			return true
		}
	}
	return false
}

// piGone: a read failed because the process exited during the scan.
func piGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// piCouldBeOurs: whether the process at dir could be a pi this user started:
// its environ is owned by uid and its status shows no permitted capability.
// A process whose status cannot be read for another reason than having
// exited is counted as could-be.
func piCouldBeOurs(dir string, uid int) bool {
	st, err := os.Stat(filepath.Join(dir, "environ"))
	if err != nil {
		return !piGone(err)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != uid {
		return false
	}
	status, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return !piGone(err)
	}
	for _, l := range strings.Split(string(status), "\n") {
		if v, ok := strings.CutPrefix(l, "CapPrm:"); ok {
			return strings.Trim(strings.TrimSpace(v), "0") == ""
		}
	}
	return true
}

// Tail: the last tool call (name and arguments) and the last assistant text;
// when the last assistant message stopped with error or length, the text is
// that stop instead (a later assistant message with any other stop clears it).
func (pi) Tail(transcript string) (string, string) {
	var tool, text, stop string
	eachLine(transcript, func(line []byte) {
		var r piRecord
		if json.Unmarshal(line, &r) != nil || r.Type != "message" || r.Message.Role != "assistant" {
			return
		}
		if cs := r.calls(); len(cs) > 0 {
			c := cs[len(cs)-1]
			args, _ := json.Marshal(c.Arguments)
			tool = oneLineCut(c.Name + " " + string(args))
		}
		var blocks []struct{ Type, Text string }
		if json.Unmarshal(r.Message.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					text = oneLineCut(b.Text)
				}
			}
		}
		switch r.Message.StopReason {
		case "error":
			first, _, _ := strings.Cut(strings.TrimSpace(r.Message.ErrorMessage), "\n")
			stop = oneLineCut("error: " + first)
		case "length":
			stop = "length: output cut at the token limit"
		default:
			stop = ""
		}
	})
	if stop != "" {
		return tool, stop
	}
	return tool, text
}

// ReadAny: a read call whose path (absolute, or relative to the session's
// working directory) is one of paths, or any other call (bash cat, sed)
// whose arguments name one.
func (pi) ReadAny(transcript string, paths []string) (bool, bool) {
	h, _ := piHead(transcript)
	read, calls := false, 0
	ok := eachLine(transcript, func(line []byte) {
		if read || !strings.Contains(string(line), `"toolCall"`) {
			return
		}
		var r piRecord
		if json.Unmarshal(line, &r) != nil {
			return
		}
		for _, c := range r.calls() {
			calls++
			for _, v := range c.Arguments {
				if s, ok := v.(string); ok && namesIn(s, h.cwd, paths) {
					read = true
				}
			}
		}
	})
	return read, ok && (read || calls > 0)
}
