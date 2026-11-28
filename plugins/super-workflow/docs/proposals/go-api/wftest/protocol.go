package wftest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/harness/agytest"
	"github.com/alepar/super-workflow/docs/proposals/go-api/internal/harnessfixture"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// ChildProtocolReport describes the framework's child protocol, not workflow
// path coverage: a call holding its unit's claim token is accepted whatever
// the harness's records show, and a call without one is refused.
// FixtureGaps mean a positive control could not establish the tested precondition.
type ChildProtocolReport struct {
	Problems    []string
	FixtureGaps []string
	// BlockedEarlier lists attacks refused cleanly at next, before a child verb.
	BlockedEarlier          []string
	Cases, PositiveControls int
	ReservedGateDelivery    *GateDeliveryReport
}

func (r *ChildProtocolReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "child protocol: %d attack cases, %d positive controls\n", r.Cases, r.PositiveControls)
	if r.ReservedGateDelivery != nil {
		fmt.Fprintf(&b, "reserved gate delivery: %d cases, %d positive controls\n", r.ReservedGateDelivery.Cases, r.ReservedGateDelivery.PositiveControls)
	}
	for _, p := range r.BlockedEarlier {
		fmt.Fprintln(&b, "blocked earlier (at next): "+p)
	}
	for _, p := range r.Problems {
		fmt.Fprintln(&b, p)
	}
	for _, p := range r.FixtureGaps {
		fmt.Fprintln(&b, "fixture gap: "+p)
	}
	return b.String()
}

// CheckChildProtocol exercises the real CLI and real harness-record readers with
// isolated files and explicit caller environments. It invokes no host commands,
// changes no process environment and does not substitute the CLI's verdicts.
// It reports current protocol defects; a successfully run assessment is not a pass.
func CheckChildProtocol() *ChildProtocolReport {
	report := &ChildProtocolReport{}
	if err := harnessfixture.CheckRoot(); err != nil {
		report.FixtureGaps = append(report.FixtureGaps, err.Error())
		return report
	}
	root, err := os.MkdirTemp("", "wf-child-protocol-")
	if err != nil {
		report.FixtureGaps = append(report.FixtureGaps, err.Error())
		return report
	}
	defer os.RemoveAll(root)
	for _, h := range harness.Names() {
		checkChildProtocolHarness(report, root, h)
	}
	checkHermesSiblingProtocol(root, report)
	checkInventedStarted(root, report)
	checkHermesBriefProtocol(root, report)
	report.ReservedGateDelivery = CheckReservedGateDelivery(reservedProtocolFactory, "protocolApproval", "yes")
	report.Problems = append(report.Problems, report.ReservedGateDelivery.Problems...)
	report.FixtureGaps = append(report.FixtureGaps, report.ReservedGateDelivery.FixtureGaps...)
	sort.Strings(report.BlockedEarlier)
	sort.Strings(report.Problems)
	sort.Strings(report.FixtureGaps)
	return report
}

// checkChildProtocolHarness runs h's protocol matrix into report, through the
// real CLI: for each child verb a positive control, then the cases. The claim
// token is what admits a call, so a caller holding the unit's token is
// accepted whatever its harness's records show (none, another token, the
// token only quoted later) and whatever its environment says, except the
// coordinator's own first fetch; a token never offered is refused.
func checkChildProtocolHarness(report *ChildProtocolReport, root, h string) {
	for _, verb := range []string{"prompt", "validate", "done", "failed"} {
		// Every target verb gets a positive control, including valid output and a
		// whole prompt fetch, so another guard cannot hide missing authorization.
		positive, err := newProtocolCase(filepath.Join(root, h, verb, "positive"), h)
		if err == nil {
			err = positive.authorize(verb)
		}
		if err != nil {
			report.FixtureGaps = append(report.FixtureGaps, h+"/"+verb+" positive setup: "+err.Error())
			continue
		}
		code, out, stderr := positive.invoke(verb, positive.childEnv)
		if code != 0 {
			report.FixtureGaps = append(report.FixtureGaps, fmt.Sprintf("%s/%s positive control exit %d: %s%s", h, verb, code, out, stderr))
			continue
		}
		report.PositiveControls++
		attacks := []string{"missing-record", "forged-marker", "stripped-env", "wrong-token", "quoted-token", "unoffered-token"}
		if h == "codex" {
			attacks = append(attacks, "later-user-token")
		}
		if h == "claude" {
			attacks = append(attacks, "sessionless-first-coordinator")
		}
		for _, attack := range attacks {
			sessionless := attack == "sessionless-first-coordinator"
			c, err := newProtocolCaseWithSession(filepath.Join(root, h, verb, attack), h, sessionless)
			if errors.Is(err, errProtocolBlockedAtNext) {
				report.BlockedEarlier = append(report.BlockedEarlier, h+"/"+verb+"/"+attack)
				continue
			}
			if err == nil && verb != "prompt" {
				if sessionless {
					err = c.prepareSessionlessVerb()
				} else {
					err = c.authorize(verb)
				}
			}
			if err == nil && attack != "unoffered-token" {
				err = os.RemoveAll(c.records)
			}
			if err == nil && (attack == "wrong-token" || attack == "quoted-token" || attack == "later-user-token") {
				wrong := strings.Repeat("f", len(c.token))
				if wrong == c.token {
					wrong = strings.Repeat("0", len(c.token))
				}
				quoted := ""
				if attack == "quoted-token" {
					quoted = c.token
				}
				err = c.writeChildRecordFor(wrong, quoted)
				if err == nil && attack == "later-user-token" {
					err = c.appendLaterUserToken()
				}
			}
			if err != nil {
				report.FixtureGaps = append(report.FixtureGaps, h+"/"+verb+"/"+attack+": "+err.Error())
				continue
			}
			env := c.coordinatorEnv
			if attack == "forged-marker" || attack == "wrong-token" || attack == "quoted-token" || attack == "later-user-token" || attack == "unoffered-token" {
				env = c.childEnv
			}
			// Refused: a token the run never offered, and the coordinator's own
			// session fetching a unit no child has fetched yet.
			refused := attack == "unoffered-token" ||
				(attack == "missing-record" && verb == "prompt" && harness.For(h).Caller(func(k string) string { return env[k] }).Is == harness.IsSelf)
			protected := c.attackProtected("")
			if attack == "unoffered-token" {
				protected = c.presentUnofferedToken()
			}
			if attack == "stripped-env" {
				env = protocolStripped(c.childEnv)
			}
			before, journalErr := os.ReadFile(filepath.Join(c.state, "runs", "protocol-run", "journal.jsonl"))
			if journalErr != nil {
				report.FixtureGaps = append(report.FixtureGaps, h+"/"+verb+"/"+attack+": read journal: "+journalErr.Error())
				continue
			}
			report.Cases++
			code, out, stderr = c.invoke(verb, env)
			site := h + "/" + verb + "/" + attack
			switch {
			case !refused && code != 0:
				report.Problems = append(report.Problems, fmt.Sprintf("claim-refused %s: exit %d for a call holding its unit's claim token: %s%s", site, code, out, stderr))
			case refused && code == 0:
				report.Problems = append(report.Problems, "refusal-required "+site+": the CLI accepted the call")
			case refused && code != 4:
				report.FixtureGaps = append(report.FixtureGaps, fmt.Sprintf("%s: exit %d does not establish a refusal: %s%s", site, code, out, stderr))
			}
			if refused && code != 0 {
				after, err := os.ReadFile(filepath.Join(c.state, "runs", "protocol-run", "journal.jsonl"))
				if err != nil {
					report.FixtureGaps = append(report.FixtureGaps, site+": reread journal: "+err.Error())
				} else if err := refusalWrite(c, verb, stderr, before, after); err != nil {
					report.Problems = append(report.Problems, "unauthorized-write "+site+": "+err.Error())
				}
				for _, leak := range protocolDisclosures(out+stderr, protected...) {
					report.Problems = append(report.Problems, "refusal-disclosure "+site+": "+leak)
				}
			}
		}
	}
}

type protocolWork struct{}
type protocolDone struct{}
type protocolStopped struct{}

func protocolDefinition() *wf.Def {
	d := wf.New("child-protocol")
	a := wf.NewAgent[protocolWork, bool](d)
	done := wf.NewTerminal[protocolDone](d, wf.Success)
	stopped := wf.NewTerminal[protocolStopped](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stopped)
	a.Spec(wf.PromptText("Return true."), wf.Retry[protocolWork](wf.Const(2), a.To(stopped, "exhausted")))
	a.Then(a.To(done, "accepted"))
	return d
}

type protocolCase struct {
	cfg                            wf.MainConfig
	state, records, harness, token string
	instruction                    wf.Instruction
	coordinatorEnv, childEnv       map[string]string
	// agy is the case's agy records tree (c.records is its root); nil for any
	// other harness.
	agy *agytest.Tree
}

var errProtocolBlockedAtNext = errors.New("attack blocked earlier at next")

func newProtocolCase(root, h string) (*protocolCase, error) {
	return newProtocolCaseWithSession(root, h, false)
}
func newProtocolCaseWithSession(root, h string, sessionless bool) (*protocolCase, error) {
	c := &protocolCase{cfg: wf.MainConfig{Defs: []wf.Registration{{Def: protocolDefinition()}}}, state: filepath.Join(root, "state"), records: filepath.Join(root, "records"), harness: h}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	if h == "agy" {
		// An agy coordinator is IsSelf only from its own USER_INPUT log, which
		// must exist before run.
		c.agy = agytest.New(protocolTB{dir: root})
		c.records = c.agy.Root
		if err := agyBuild(func() { c.agy.Coordinator(protocolParent, time.Now().UTC().Add(-time.Minute)) }); err != nil {
			return nil, err
		}
	}
	c.coordinatorEnv, c.childEnv = protocolEnvironments(h, c.records)
	if c.coordinatorEnv == nil || c.childEnv == nil {
		return nil, fmt.Errorf("no protocol environment for harness %s", h)
	}
	if sessionless {
		c.coordinatorEnv = map[string]string{"CLAUDECODE": "1", "CLAUDE_CONFIG_DIR": c.records}
		c.childEnv = c.coordinatorEnv
	}
	if code, out, err := c.call(c.coordinatorEnv, "run", "child-protocol", "--run-id", "protocol-run", "--workspace", root); code != 0 {
		return nil, fmt.Errorf("run exit%d: %s%s", code, out, err)
	}
	// Only the sessionless attack may be satisfied before hand-out. A failed
	// positive-control setup or any other failure remains a fixture gap.
	var before []byte
	journal := filepath.Join(c.state, "runs", "protocol-run", "journal.jsonl")
	if sessionless {
		var err error
		before, err = os.ReadFile(journal)
		if err != nil {
			return nil, fmt.Errorf("before offer: %w", err)
		}
	}
	code, out, stderr := c.call(c.coordinatorEnv, "next", "protocol-run", "1", "--json")
	if sessionless && code == 4 {
		after, err := os.ReadFile(journal)
		if err != nil {
			return nil, fmt.Errorf("after refused offer: %w", err)
		}
		if !bytes.Equal(before, after) {
			return nil, fmt.Errorf("refused offer mutated the journal")
		}
		if leaks := protocolDisclosures(out+stderr, c.offerProtected()...); len(leaks) != 0 {
			return nil, fmt.Errorf("refused offer disclosure: %v", leaks)
		}
		return nil, errProtocolBlockedAtNext
	}
	if code != 0 {
		return nil, fmt.Errorf("offer exit%d: %s%s", code, out, stderr)
	}
	var instructions []wf.Instruction
	if err := json.Unmarshal([]byte(out), &instructions); err != nil {
		return nil, fmt.Errorf("decode offer: %w: %s", err, out)
	}
	for _, in := range instructions {
		if in.Kind == wf.InstructionAgent {
			c.instruction = in
			break
		}
	}
	claim := regexp.MustCompile(`--claim\s+([a-f0-9]+)`).FindStringSubmatch(c.instruction.Brief)
	if len(claim) != 2 {
		return nil, fmt.Errorf("offer has no claim token: %+v", c.instruction)
	}
	c.token = claim[1]
	if h == "pi" {
		// A pi child's session is named for its claim (the spawn line's
		// --session-id), so its environment is known only now.
		c.childEnv["PI_SESSION_ID"] = "wf-" + c.token
		c.childEnv["PI_SESSION_FILE"] = filepath.Join(c.records, "sessions", "--fixture--", "2026-11-19T00-00-00-000Z_wf-"+c.token+".jsonl")
	}
	return c, nil
}
func (c *protocolCase) call(env map[string]string, args ...string) (int, string, string) {
	var out, err bytes.Buffer
	args = append(args, "--state-dir", c.state)
	code := wf.CLIWithEnv(c.cfg, args, &out, &err, harness.Env(func(k string) string { return env[k] }))
	return code, out.String(), err.String()
}
func (c *protocolCase) invoke(verb string, env map[string]string) (int, string, string) {
	if verb == "prompt" {
		return c.call(env, "prompt", "protocol-run", "--claim", c.token, "--json")
	}
	args := []string{verb, "protocol-run", "--claim", c.token}
	if verb == "failed" {
		args = append(args, "--reason", attackReason, "--class", "blocked")
	}
	return c.call(env, args...)
}
func (c *protocolCase) authorize(verb string) error {
	if err := c.writeChildRecord(); err != nil {
		return err
	}
	want := harness.Want{Token: c.token}
	if c.harness == "codex" {
		want.Agent = "protocol-child-canary"
	}
	if protocolNoRecords[c.harness] {
		// A harness without records: nothing is discovered, and the claim
		// token alone admits the child.
		if got := harness.For(c.harness).Resolve(c.records, "protocol-parent-canary", []harness.Want{want}); len(got) != 0 {
			return fmt.Errorf("a no-records adapter discovered a child: %v", got)
		}
	} else if _, ok := harness.For(c.harness).Resolve(c.records, "protocol-parent-canary", []harness.Want{want})[want]; !ok {
		return fmt.Errorf("adapter did not discover positive child record")
	}
	if verb == "prompt" {
		return nil
	}
	if code, out, err := c.invoke("prompt", c.childEnv); code != 0 {
		return fmt.Errorf("authorized claim exit%d: %s%s", code, out, err)
	}
	if c.instruction.Output == "" {
		return fmt.Errorf("offer omitted allocated output")
	}
	return os.WriteFile(c.instruction.Output, []byte("true"), 0600)
}

// A sessionless first hand-out must never become a library-call exemption.
// If prompt already refuses, still exercise each later verb on the offered unit;
// if it accepts, provide valid output so unrelated validation cannot hide the bug.
func (c *protocolCase) prepareSessionlessVerb() error {
	code, out, stderr := c.invoke("prompt", c.coordinatorEnv)
	if code != 0 && code != 4 {
		return fmt.Errorf("sessionless prompt setup exit%d: %s%s", code, out, stderr)
	}
	return os.WriteFile(c.instruction.Output, []byte("true"), 0600)
}

// Codex user messages after kickoff may quote another unit's token. That is
// different from a tool result and must not establish this child's identity.
func (c *protocolCase) appendLaterUserToken() error {
	record, err := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "An unrelated unit was offered: --claim " + c.token}}}})
	if err != nil {
		return err
	}
	path := filepath.Join(c.records, "sessions", "2026", "11", "15", "rollout-2026-11-15T00-00-00-protocol-child-canary.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(record, '\n'))
	return err
}

// protocolNoRecords are the harnesses whose adapter reads no records: the
// engine learns of a child only through its verbs, so the record writers
// write nothing and the record attacks exercise the token alone.
var protocolNoRecords = map[string]bool{"opencode": true}

func protocolEnvironments(h, root string) (map[string]string, map[string]string) {
	const parent, child = protocolParent, protocolChild
	var coordinator, kid map[string]string
	switch h {
	case "claude":
		coordinator = map[string]string{"CLAUDE_CODE_SESSION_ID": parent, "CLAUDE_CONFIG_DIR": root}
		kid = map[string]string{"CLAUDE_CODE_SESSION_ID": parent, "CLAUDE_CONFIG_DIR": root}
	case "codex":
		coordinator = map[string]string{"CODEX_THREAD_ID": parent, "CODEX_SESSION_ID": parent, "CODEX_HOME": root}
		kid = map[string]string{"CODEX_THREAD_ID": child, "CODEX_SESSION_ID": parent, "CODEX_HOME": root}
	case "hermes":
		coordinator = map[string]string{"HERMES_SESSION_ID": parent, "HERMES_HOME": root}
		kid = map[string]string{"HERMES_SESSION_ID": child, "HERMES_HOME": root, "HERMES_DELEGATED_CHILD_CONTEXT": root}
	case "pi":
		dir := filepath.Join(root, "sessions", "--fixture--")
		coordinator = map[string]string{"PI_SESSION_ID": parent, "PI_CODING_AGENT_DIR": root, "PI_SESSION_FILE": filepath.Join(dir, "2026-11-19T00-00-00-000Z_"+parent+".jsonl")}
		kid = map[string]string{"PI_SESSION_ID": child, "SUPER_PI_PARENT": parent, "PI_CODING_AGENT_DIR": root, "PI_SESSION_FILE": filepath.Join(dir, "2026-11-19T00-00-00-000Z_"+child+".jsonl")}
	case "agy":
		// No HOME: the engine reads only this map, and the records root is
		// ANTIGRAVITY_APP_DATA_DIR before anything under a home.
		coordinator = map[string]string{"ANTIGRAVITY_CONVERSATION_ID": parent, "ANTIGRAVITY_APP_DATA_DIR": root}
		kid = map[string]string{"ANTIGRAVITY_CONVERSATION_ID": child, "ANTIGRAVITY_APP_DATA_DIR": root}
	case "opencode":
		// The plugin's variables; root names the data directory, never read.
		coordinator = map[string]string{"OPENCODE": "1", "OPENCODE_SESSION_ID": parent, "OPENCODE_PARENT_SESSION_ID": "", "XDG_DATA_HOME": root}
		kid = map[string]string{"OPENCODE": "1", "OPENCODE_SESSION_ID": child, "OPENCODE_PARENT_SESSION_ID": parent, "XDG_DATA_HOME": root}
	}
	return coordinator, kid
}
func (c *protocolCase) writeChildRecord() error {
	// Preserve record order and rebind identities, claim, paths and start time.
	file, destination, oldClaim := "", "", ""
	replacements := []string{}
	switch c.harness {
	case "codex":
		file, oldClaim = "codex-v2-child-1.jsonl", "0123456789abcdef"
		destination = "sessions/2026/11/15/rollout-2026-11-15T00-00-00-protocol-child-canary.jsonl"
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		// The recorded input is JavaScript containing a JSON string containing a
		// shell command. Quote each layer independently, including unusual paths.
		shellQuote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
		command := shellQuote(executable) + " prompt protocol-run --claim " + c.token + " --state-dir " + shellQuote(c.state)
		encodedCommand, err := json.Marshal(command)
		if err != nil {
			return err
		}
		input := "text(await tools.exec_command({cmd:" + string(encodedCommand) + ",max_output_tokens:6000}));\n"
		encodedInput, err := json.Marshal(input)
		if err != nil {
			return err
		}
		raw, err := harnessfixture.ReadFile("started/" + file)
		if err != nil {
			return err
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		var call struct {
			Type    string                     `json:"type"`
			Payload map[string]json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &call); err != nil {
			return err
		}
		call.Payload["input"] = encodedInput
		encodedCall, err := json.Marshal(call)
		if err != nil {
			return err
		}
		replacements = append(replacements, lines[len(lines)-1], string(encodedCall),
			"00000000-0000-7000-8000-000000000002", "protocol-child-canary",
			"00000000-0000-7000-8000-000000000001", "protocol-parent-canary",
			"2000-01-01T00:00:00.000Z", time.Now().UTC().Format(time.RFC3339Nano))
	case "claude":
		file, oldClaim = "claude-child.jsonl", "claude-fixture-claim"
		destination = "projects/-fixture/protocol-parent-canary/subagents/agent-protocol-child-canary.jsonl"
		replacements = append(replacements, "fixture-child", "protocol-child-canary")
		parent := filepath.Join(c.records, "projects", "-fixture", "protocol-parent-canary.jsonl")
		if err := os.MkdirAll(filepath.Dir(parent), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(parent, []byte("{}\n"), 0600); err != nil {
			return err
		}
	case "hermes":
		file, oldClaim = "hermes-child.log", "1010101010101010"
		destination = "cache/delegation/live/deleg_protocol/task-0.log"
	case "pi":
		return c.writePiRecords()
	case "agy":
		return c.writeAgyChild(c.instruction.Brief, "")
	case "opencode":
		return nil
	default:
		return fmt.Errorf("unsupported harness %s", c.harness)
	}
	raw, err := harnessfixture.ReadFile("started/" + file)
	if err != nil {
		return err
	}
	replacements = append(replacements, oldClaim, c.token)
	body := strings.NewReplacer(replacements...).Replace(string(raw))
	path := filepath.Join(c.records, destination)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0600)
}

// A quoted token belongs only to a later tool result, never to this child's
// kickoff. Rewriting the same record after a legitimate claim also checks that
// post-claim verbs cannot rely on caller metadata instead of current evidence.
func (c *protocolCase) writeChildRecordFor(kickoffToken, quotedToken string) error {
	write := func(path, body string) error {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(body), 0600)
	}
	brief := "Run: wf prompt --claim " + kickoffToken + ", then do what it prints."
	switch c.harness {
	case "agy":
		return c.writeAgyChild(brief, quotedToken)
	case "opencode":
		return nil
	case "claude":
		project := filepath.Join(c.records, "projects", "-fixture")
		if err := write(filepath.Join(project, "protocol-parent-canary.jsonl"), "{}\n"); err != nil {
			return err
		}
		record, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": brief}})
		body := string(record) + "\n"
		if quotedToken != "" {
			result, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "other-command", "content": "An unrelated unit was offered: --claim " + quotedToken}}}})
			body += string(result) + "\n"
		}
		return write(filepath.Join(project, "protocol-parent-canary", "subagents", "agent-protocol-child-canary.jsonl"), body)
	case "codex":
		meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": "protocol-child-canary", "source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "protocol-parent-canary"}}}}})
		kickoff, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": brief}}}})
		body := string(meta) + "\n" + string(kickoff) + "\n"
		if quotedToken != "" {
			result, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "other-command", "output": "An unrelated unit was offered: --claim " + quotedToken}})
			body += string(result) + "\n"
		}
		return write(filepath.Join(c.records, "sessions", "2026", "11", "15", "rollout-2026-11-15T00-00-00-protocol-child-canary.jsonl"), body)
	case "pi":
		now := time.Now().UTC().Format(time.RFC3339Nano)
		dir := filepath.Join(c.records, "sessions", "--fixture--")
		kid := "wf-" + kickoffToken
		line := harness.For("pi").SpawnLine(harness.Spawn{Brief: brief, Claim: kickoffToken, ErrLog: c.piErrLog(kickoffToken)})
		launch, _ := json.Marshal(map[string]any{"type": "message", "timestamp": now, "message": map[string]any{"role": "assistant", "stopReason": "toolUse",
			"content": []any{map[string]any{"type": "toolCall", "name": "bash", "arguments": map[string]any{"command": line}}}}})
		coord := `{"type":"session","version":3,"id":"protocol-parent-canary","timestamp":"` + now + `","cwd":"/fixture"}` + "\n" + string(launch) + "\n"
		if err := write(filepath.Join(dir, "2026-11-19T00-00-00-000Z_protocol-parent-canary.jsonl"), coord); err != nil {
			return err
		}
		kick, _ := json.Marshal(map[string]any{"type": "message", "timestamp": now, "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": brief}}}})
		body := `{"type":"session","version":3,"id":"` + kid + `","timestamp":"` + now + `","cwd":"/fixture"}` + "\n" + string(kick) + "\n"
		if quotedToken != "" {
			result, _ := json.Marshal(map[string]any{"type": "message", "timestamp": now, "message": map[string]any{"role": "toolResult", "toolName": "bash", "content": []any{map[string]any{"type": "text", "text": "An unrelated unit was offered: --claim " + quotedToken}}}})
			body += string(result) + "\n"
		}
		return write(filepath.Join(dir, "2026-11-19T00-00-01-000Z_"+kid+".jsonl"), body)
	case "hermes":
		body := "=== Hermes subagent live transcript ===\ndelegation: deleg_protocol   task: 0\ngoal: " + brief + "\n"
		if quotedToken != "" {
			body += "00:00:01 result | terminal ok: An unrelated unit was offered: --claim " + quotedToken + "\n"
		}
		return write(filepath.Join(c.records, "cache", "delegation", "live", "deleg_protocol", "task-0.log"), body)
	}
	return fmt.Errorf("unsupported harness %s", c.harness)
}

// protocolTB is the testing.TB agytest's builders report to inside
// CheckChildProtocol, which runs outside any test: TempDir makes a directory
// under the case's root, and a builder failure panics with its message, which
// agyBuild recovers as a fixture gap. Only the methods agytest calls are
// implemented; the embedded nil TB is never called.
type protocolTB struct {
	testing.TB
	dir string
}

func (protocolTB) Helper() {}
func (p protocolTB) TempDir() string {
	d, err := os.MkdirTemp(p.dir, "agy-")
	if err != nil {
		panic(err)
	}
	return d
}
func (protocolTB) Fatal(args ...any)                 { panic(fmt.Sprint(args...)) }
func (protocolTB) Fatalf(format string, args ...any) { panic(fmt.Sprintf(format, args...)) }

// agyBuild runs an agytest builder call and returns its failure, if any, as an
// error: a builder failure is a fixture gap, never a crash of the checker.
func agyBuild(f func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("agy records fixture: %v", r)
		}
	}()
	f()
	return nil
}

// writeAgyChild records the child in the case's agy tree: the coordinator's
// launching response (the coordinator's log is rebuilt when an attack removed
// the records), the child's log started with brief, and, for a quoted token,
// a later run_command result that mentions it. The launch is after the
// hand-out, as a real one is.
func (c *protocolCase) writeAgyChild(brief, quotedToken string) error {
	now := time.Now().UTC()
	return agyBuild(func() {
		if _, err := os.Stat(c.agy.Transcript(protocolParent)); err != nil {
			c.agy.Coordinator(protocolParent, now.Add(-time.Minute))
		}
		c.agy.Start(agytest.Launch{Coordinator: protocolParent, Child: protocolChild, Brief: brief, At: now})
		if quotedToken != "" {
			c.agy.Respond(protocolChild, now, "m", 1, 1, "", agytest.Call{Name: "run_command", Args: map[string]any{"CommandLine": "echo"}})
			c.agy.CommandResult(protocolChild, now, "An unrelated unit was offered: --claim "+quotedToken)
		}
	})
}

const (
	protocolParent = "protocol-parent-canary"
	protocolChild  = "protocol-child-canary"
)

// protocolMarkers are every registered adapter's markers (harness.Names,
// harness.Detector): the variables that make a shell a harness's. Derived
// from the registry as the wftest TestMain helper is, so a new adapter's
// stripped-env attack strips its own marker without editing a list.
func protocolMarkers() []string {
	var out []string
	for _, n := range harness.Names() {
		if d, ok := harness.For(n).(harness.Detector); ok {
			out = append(out, d.Markers()...)
		}
	}
	return out
}

// protocolStripped is env without any registered marker: the caller's own
// shell with the harness's identity removed and everything else (its records
// root among them) kept.
func protocolStripped(env map[string]string) map[string]string {
	out := maps.Clone(env)
	for _, m := range protocolMarkers() {
		delete(out, m)
	}
	return out
}

// protectedValue is a value a refusal must not print, and the label a
// disclosure of it is reported under: the report names the label, never the
// value, so it does not leak what it guards.
type protectedValue struct{ label, value string }

// protocolDisclosures is what a refusal's text gives away that it must not,
// as labels: the fixture's identities (the coordinator's and the child's
// session ids, which stand for any value read from a caller's environment or
// a harness's records) and each of protected, the values of the case the
// refused caller must not be shown.
//
// It does not report what a refusal says is missing. The word "session", a
// harness's name and the names of the environment variables an adapter
// reads are guidance: a caller told nothing cannot fix its call. Only
// values are protected.
func protocolDisclosures(text string, protected ...protectedValue) []string {
	var leaks []string
	fixture := []protectedValue{{"the coordinator's session id", protocolParent}, {"the child's session id", protocolChild}}
	for _, p := range append(fixture, protected...) {
		if p.value != "" && strings.Contains(text, p.value) && !slices.Contains(leaks, "exposes "+p.label) {
			leaks = append(leaks, "exposes "+p.label)
		}
	}
	return leaks
}

// The protected values of each place the probe reads a refusal. Every one
// includes the case's records root: where the harness keeps its transcripts
// is the caller's own, and no refusal has a reason to print it.

// root is the case's records root as a protected value.
func (c *protocolCase) root() protectedValue {
	return protectedValue{"the records root", c.records}
}

// attackProtected is what the refusal of one attack on the case must not
// print. real is the unit's claim token when the caller presented another:
// then both are protected, the unit's own because the caller was not given
// it, and the one it presented (c.token) because a refusal has no call to
// repeat what a caller passed. real is "" when the caller presented the
// unit's own token, which it holds already and is not held to. The reason a
// failed supplies (attackReason) is a caller-passed value too.
func (c *protocolCase) attackProtected(real string) []protectedValue {
	out := []protectedValue{c.root(), {"the supplied reason", attackReason}}
	if real != "" && real != c.token {
		out = append(out, protectedValue{"the unit's claim token", real}, protectedValue{"the presented claim token", c.token})
	}
	return out
}

// attackReason is the --reason an attack's failed supplies (invoke).
const attackReason = "fixture attempt failed"

// presentUnofferedToken makes the case's caller present a claim token the
// run never offered, in place of the unit's own, and returns what the
// refusal of that call must not print.
func (c *protocolCase) presentUnofferedToken() []protectedValue {
	real := c.token
	c.token = strings.Repeat("f", len(c.token))
	return c.attackProtected(real)
}

// offerProtected is what a refused offer (next) must not print.
func (c *protocolCase) offerProtected() []protectedValue {
	return []protectedValue{c.root()}
}

// startedProtected is what the refusal of a started with invented metadata
// must not print: session and agent are the ids the caller supplied, which
// a refusal has no call to repeat.
func (c *protocolCase) startedProtected(session, agent string) []protectedValue {
	return []protectedValue{c.root(), {"the supplied session id", session}, {"the supplied agent id", agent}}
}

// siblingProtected is what the refusal of a tokenless sibling must not
// print: the unit's claim token, which it was not given, session, the
// session id its environment supplied, and the reason its failed supplied.
func (c *protocolCase) siblingProtected(session string) []protectedValue {
	return []protectedValue{c.root(), {"the unit's claim token", c.token}, {"the supplied session id", session}, {"the supplied reason", siblingReason}}
}

// refusalWrite checks what a refused child verb wrote: nothing at
// all, unless the refusal is the coordinator's own first prompt fetch (its
// text says so; started refuses unrecorded), which appends exactly one
// record: degraded, kind child refused, for this run and unit, its cause
// naming the verb.
func refusalWrite(c *protocolCase, verb, stderr string, before, after []byte) error {
	audited := verb == "prompt" && strings.Contains(stderr, "this is a child's command")
	if !audited {
		if !bytes.Equal(before, after) {
			return fmt.Errorf("the journal changed: %q", bytes.TrimPrefix(after, before))
		}
		return nil
	}
	added, ok := bytes.CutPrefix(after, before)
	if !ok {
		return fmt.Errorf("the journal was rewritten, not appended to")
	}
	lines := bytes.Split(bytes.TrimSuffix(added, []byte("\n")), []byte("\n"))
	if len(added) == 0 || added[len(added)-1] != '\n' || len(lines) != 1 {
		return fmt.Errorf("want one refusal record appended, got %q", added)
	}
	var rec wf.JournalRecord
	var d wf.DegradedData
	if json.Unmarshal(lines[0], &rec) != nil || json.Unmarshal(rec.Data, &d) != nil {
		return fmt.Errorf("the appended record is not a journal record: %q", lines[0])
	}
	if rec.Kind != wf.JournalDegraded || rec.Run != "protocol-run" || d.Kind != wf.DegradedChildRefused || d.Unit != c.instruction.Name || !strings.HasPrefix(d.Cause, verb+": ") {
		return fmt.Errorf("the appended record is not this %s refusal of %s: %q", verb, c.instruction.Name, lines[0])
	}
	// Its envelope: the next sequence number, and the coordinates of the
	// unit's hand-out (the probe journals no migration).
	var last, handOut wf.JournalRecord
	for _, line := range bytes.Split(bytes.TrimSpace(before), []byte("\n")) {
		var r wf.JournalRecord
		if json.Unmarshal(line, &r) != nil {
			return fmt.Errorf("the journal before the call does not decode: %q", line)
		}
		last = r
		var h wf.HandedOutData
		if r.Kind == wf.JournalHandedOut && json.Unmarshal(r.Data, &h) == nil && h.Name == c.instruction.Name {
			handOut = r
		}
	}
	if handOut.Kind != wf.JournalHandedOut {
		return fmt.Errorf("no hand-out of %s before the call", c.instruction.Name)
	}
	if rec.Seq != last.Seq+1 || rec.Instance != handOut.Instance || rec.State != handOut.State || rec.Attempt != handOut.Attempt {
		return fmt.Errorf("the refusal record's envelope (seq %d, %q %q %d) is not seq %d at the hand-out's (%q %q %d)", rec.Seq, rec.Instance, rec.State, rec.Attempt, last.Seq+1, handOut.Instance, handOut.State, handOut.Attempt)
	}
	return nil
}

// piErrLog is the stderr file the engine records for a pi child's claim.
func (c *protocolCase) piErrLog(token string) string {
	return filepath.Join(c.state, "child-logs", "protocol-run", "wf-"+token+".stderr")
}

// writePiRecords installs a synthetic pi launch and child session, rebound
// to the case's identities, claim, stderr file and time.
func (c *protocolCase) writePiRecords() error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	stamp := regexp.MustCompile(`"timestamp":"\d{4}-\d\d-\d\dT[0-9:.]+Z"`)
	kid := "wf-" + c.token
	rebind := strings.NewReplacer("/nonexistent/child-logs/example/wf-1011121314151617.stderr", c.piErrLog(c.token), "fixture-pi-parent", "protocol-parent-canary", "wf-1011121314151617", kid, "1011121314151617", c.token)
	dir := filepath.Join(c.records, "sessions", "--fixture--")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for file, dest := range map[string]string{
		"pi-coordinator.jsonl": "2026-11-19T06-45-48-625Z_protocol-parent-canary.jsonl",
		"pi-child.jsonl":       "2026-11-19T06-45-50-124Z_" + kid + ".jsonl",
	} {
		raw, err := harnessfixture.ReadFile("started/" + file)
		if err != nil {
			return err
		}
		body := stamp.ReplaceAllString(rebind.Replace(string(raw)), `"timestamp":"`+now+`"`)
		if err := os.WriteFile(filepath.Join(dir, dest), []byte(body), 0600); err != nil {
			return err
		}
	}
	return nil
}
