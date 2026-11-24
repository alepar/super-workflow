package wftest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// GateDeliveryReport distinguishes delivery defects from invalid scenario setup.
type GateDeliveryReport struct {
	Problems, FixtureGaps   []string
	Cases, PositiveControls int
}

func (r *GateDeliveryReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "reserved gate delivery: %d cases, %d positive controls\n", r.Cases, r.PositiveControls)
	for _, p := range r.Problems {
		fmt.Fprintln(&b, p)
	}
	for _, p := range r.FixtureGaps {
		fmt.Fprintln(&b, "fixture gap: "+p)
	}
	return b.String()
}

// CheckReservedGateDelivery reaches a real reserved gate with the supplied
// simulation script, then exercises the real CLI over its journal. It checks
// repeated delivery and answer provenance for agent and sessionless callers.
// Both generic and named-binary next are checked. Adapters remain the Sim's World.
func CheckReservedGateDelivery(factory func() *Sim, gate, answer string, steps ...Step) *GateDeliveryReport {
	report := &GateDeliveryReport{}
	root, err := os.MkdirTemp("", "wf-gate-protocol-")
	if err != nil {
		report.FixtureGaps = append(report.FixtureGaps, err.Error())
		return report
	}
	defer os.RemoveAll(root)
	for _, h := range []string{"claude", "codex", "hermes", "sessionless"} {
		for _, named := range []bool{false, true} {
			site := fmt.Sprintf("%s/%s/named=%t", gate, h, named)
			if err := checkReservedGateCase(report, factory, filepath.Join(root, h, strconv.FormatBool(named)), h, named, gate, answer, steps); err != nil {
				report.FixtureGaps = append(report.FixtureGaps, site+": "+err.Error())
			}
		}
	}
	return report
}

func checkReservedGateCase(report *GateDeliveryReport, factory func() *Sim, root, h string, named bool, gate, answer string, steps []Step) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("scenario setup: %v", p)
		}
	}()
	s := factory()
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	reg := wf.NewAdapters()
	if err = reg.Register(s.world); err != nil {
		return err
	}
	journal := wf.FileJournal(filepath.Join(root, "state"))
	cj := NewCrashJournal(journal)
	r := &runner{s: s, steps: foldCrashes(steps), j: cj, cj: cj, reg: reg, dir: root}
	recordedHarness := h
	if h == "sessionless" {
		recordedHarness = "claude"
	}
	r.e = wf.NewEngine(cj, reg, wf.EngineOptions{Defs: []*wf.Def{s.def}, Assets: s.assets, Workspace: s.workspace, Harness: recordedHarness, RunID: "gate-protocol", RunDir: func(wf.RunID) string { return root }, Clock: s.clock, EffectRunner: s.runner})
	if r.id, err = r.e.Run(s.def, s.params, s.options); err != nil {
		return err
	}
	r.play()
	ins, err := r.e.Next(r.id, "")
	if err != nil {
		return err
	}
	var target *wf.Instruction
	for i := range ins {
		if ins[i].Kind == wf.InstructionHumanGate && ins[i].State == gate {
			target = &ins[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("script did not reach gate %q: %+v; trace: %s", gate, ins, r.trace())
	}
	reserved := false
	var inspect func(wf.DefView)
	inspect = func(v wf.DefView) {
		for _, st := range v.States {
			if strings.TrimPrefix(st.Path, s.def.Name()+"/") == gate {
				reserved = st.Kind == "gate" && st.Reservation == "reserved"
			}
		}
		for _, child := range v.Children {
			inspect(child)
		}
	}
	inspect(wf.Inspect(s.def))
	if !reserved {
		return fmt.Errorf("%s is not declared Reserved", gate)
	}
	env, _ := protocolEnvironments(h, filepath.Join(root, "records"))
	if h == "sessionless" {
		env = map[string]string{}
	}
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: s.def}}, Adapters: reg, EffectRunner: s.runner}
	if named {
		cfg.Binary = s.def.Name()
	}
	call := func(verb string, extra ...string) (int, string, string) {
		actualVerb := verb
		if verb == "poll" {
			actualVerb = "next"
		}
		args := []string{actualVerb, string(r.id)}
		if named {
			args = []string{"wf", actualVerb, "--run-id", string(r.id)}
			if verb == "next" {
				args = []string{"next", "--run-id", string(r.id)}
			}
		}
		args = append(args, extra...)
		args = append(args, "--state-dir", filepath.Join(root, "state"))
		if s.assets != "" {
			args = append(args, "--assets", s.assets)
		}
		var out, stderr bytes.Buffer
		code := wf.CLIWithEnv(cfg, args, &out, &stderr, harness.Env(func(k string) string { return env[k] }))
		return code, out.String(), stderr.String()
	}
	site := fmt.Sprintf("%s/%s/named=%t", gate, h, named)
	report.Cases++

	records := func() ([]wf.JournalRecord, error) {
		unlock, err := journal.Lock(r.id)
		if err != nil {
			return nil, err
		}
		defer unlock()
		return journal.Records(r.id)
	}
	beforeNext, err := records()
	if err != nil {
		return err
	}
	code, raw, stderr := call("poll", "1", "--json")
	if code != 0 && code != 7 {
		return fmt.Errorf("JSON next exit%d: %s%s", code, raw, stderr)
	}
	var delivered []wf.Instruction
	if err = json.Unmarshal([]byte(raw), &delivered); err != nil {
		return fmt.Errorf("JSON next: %w: %s", err, raw)
	}
	if len(delivered) != 1 || delivered[0].State != gate {
		return fmt.Errorf("JSON next did not deliver the reached gate: %+v", delivered)
	}
	in := delivered[0]
	if code != 7 || in.Kind != wf.InstructionStopped || in.Question != target.Question || !reflect.DeepEqual(in.Answers, target.Answers) || !reflect.DeepEqual(in.IntValues, target.IntValues) || in.Child != target.Child || in.Visit != target.Visit || !in.Deadline.Equal(target.Deadline) {
		report.Problems = append(report.Problems, fmt.Sprintf("reserved-gate-instruction %s: exit%d kind=%s question=%q choices=%v", site, code, in.Kind, in.Question, in.Answers))
	}
	if !strings.Contains(in.Submit, " answer ") || !strings.Contains(in.Submit, "--gate ") || !strings.Contains(in.Submit, gate) || !strings.Contains(in.Submit, "--value <value>") {
		report.Problems = append(report.Problems, "reserved-gate-command "+site+": "+in.Submit)
	}
	if !strings.Contains(in.Submit, "--child ") || !strings.Contains(in.Submit, target.Child) || !strings.Contains(in.Submit, fmt.Sprintf("--visit %d", target.Visit)) {
		report.Problems = append(report.Problems, "reserved-gate-binding "+site+": "+in.Submit)
	}
	expected := *target
	expected.Submit = in.Submit
	want := reservedGateText(expected)
	if in.Prompt != want {
		report.Problems = append(report.Problems, "reserved-gate-prompt "+site+": "+in.Prompt)
	}
	verbs := []string{"next", "next"}
	if named {
		verbs = append(verbs, "poll")
	}
	for i, verb := range verbs {
		code, out, stderr := call(verb)
		if code != 0 && code != 7 {
			return fmt.Errorf("next exit%d: %s%s", code, out, stderr)
		}
		if code != 7 || out != want+"\n" {
			report.Problems = append(report.Problems, fmt.Sprintf("reserved-gate-delivery %s next%d exit%d: %s", site, i+1, code, out))
		}
	}
	before, err := records()
	if err != nil {
		return err
	}
	for _, rec := range before[len(beforeNext):] {
		if rec.Kind == wf.JournalGateAnswered {
			report.Problems = append(report.Problems, "reserved-gate-autoanswered "+site)
		}
	}

	// Change the answering session after polling: provenance must describe the
	// actual answer caller, not the last coordinator which asked the question.
	for key, value := range env {
		if value == "protocol-parent-canary" {
			env[key] = "protocol-answer-canary"
		}
	}
	argv, err := protocolCommandArgs(in.Submit, answer)
	if err != nil {
		return fmt.Errorf("delivered answer command: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if len(argv) == 0 || argv[0] != executable {
		return fmt.Errorf("delivered answer names wrong executable: %q", argv)
	}
	var out, answerErr bytes.Buffer
	code = wf.CLIWithEnv(cfg, argv[1:], &out, &answerErr, harness.Env(func(k string) string { return env[k] }))
	if code != 0 {
		return fmt.Errorf("delivered answer exit%d: %s%s", code, out.String(), answerErr.String())
	}
	after, err := records()
	if err != nil {
		return err
	}
	found := false
	for _, rec := range after[len(before):] {
		if rec.Kind != wf.JournalGateAnswered {
			continue
		}
		var data struct{ Answer, Session string }
		if err = json.Unmarshal(rec.Data, &data); err != nil {
			return err
		}
		if data.Answer != answer {
			continue
		}
		found = true
		want := "protocol-answer-canary"
		if h == "sessionless" {
			want = ""
		}
		if data.Session != want {
			report.Problems = append(report.Problems, fmt.Sprintf("gate-answer-provenance %s: session=%q want%q", site, data.Session, want))
		}
	}
	if !found {
		return fmt.Errorf("accepted answer lacks gate_answered event")
	}
	report.PositiveControls++
	return nil
}

func gateQuestionSentence(question string) string {
	question = strings.TrimSpace(question)
	if !strings.HasSuffix(question, ".") && !strings.HasSuffix(question, "?") && !strings.HasSuffix(question, "!") {
		question += "."
	}
	return question
}

type protocolApproval struct{}
type protocolApprovalDone struct{}

func reservedProtocolFactory() *Sim {
	d := wf.New("reserved-protocol")
	values := wf.NewEnum(d, "choice", "yes", "no")
	gate := wf.NewGate[protocolApproval, string](d, "Ship the result?", values, wf.Reserved)
	end := wf.NewTerminal[protocolApprovalDone](d, wf.Success)
	gate.Answers(wf.On("yes", gate.To(end, "approved")), wf.On("no", gate.To(end, "declined")))
	return New(d)
}

// The simulator supplies explicit scripted human replies. The coordinator-facing
// stop instruction remains a gate for that purpose, after its delivery is checked.
func simulationHumanGate(in wf.Instruction) (wf.Instruction, error) {
	if in.Kind != wf.InstructionStopped || in.Question == "" {
		return in, nil
	}
	want := reservedGateText(in)
	if in.Prompt != want {
		return in, fmt.Errorf("reserved gate %s did not deliver the human-decision instruction: %s", in.State, in.Prompt)
	}
	in.Kind = wf.InstructionHumanGate
	return in, nil
}

func reservedGateText(in wf.Instruction) string {
	text := fmt.Sprintf("This run is waiting for a person at %s: %s Choices: %s. Ask the person who started the run and wait for their reply. Answer only with what they tell you; do not choose for them. Run this with their choice as <value>:\n%s", in.State, gateQuestionSentence(in.Question), strings.Join(in.Answers, ", "), in.Submit)
	for _, value := range in.IntValues {
		if value.Proposal == nil {
			continue
		}
		command := strings.Replace(in.Submit, "<value>", protocolShellQuote(value.Answer), 1)
		text += fmt.Sprintf("\nFor %s, the proposed number is %d (allowed %d..%d). Omit --number to accept the proposal; to use the person's number, run:\n%s --number <integer>", value.Answer, *value.Proposal, value.Min, value.Max, command)
	}
	return text
}

func protocolShellQuote(s string) string {
	if s != "" && s[0] != '#' && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-./:=@%+,#", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// Parse only the literal shell quoting emitted by the CLI; never run a shell.
// Accept the engine's leading cd <literal> && directive, then substitute the
// answer placeholder while preserving every delivered executable argument.
func protocolCommandArgs(command, answer string) ([]string, error) {
	if strings.Count(command, "<value>") != 1 {
		return nil, fmt.Errorf("want one answer placeholder: %s", command)
	}
	quoted := "'" + strings.ReplaceAll(answer, "'", "'\\''") + "'"
	command = strings.Replace(command, "<value>", quoted, 1)
	var args []string
	var word strings.Builder
	single, escaped, started := false, false, false
	anchored, skipAnd := false, false
	for i, c := range command {
		if skipAnd {
			skipAnd = false
			continue
		}
		if escaped {
			word.WriteRune(c)
			escaped = false
			started = true
			continue
		}
		if single {
			if c == '\'' {
				single = false
			} else {
				word.WriteRune(c)
			}
			continue
		}
		switch {
		case c == '\'':
			single = true
			started = true
		case c == '\\':
			escaped = true
			started = true
		case strings.ContainsRune(" \t\r\n", c):
			if started {
				args = append(args, word.String())
				word.Reset()
				started = false
			}
		case c == '&' && !anchored && !started && len(args) == 2 && args[0] == "cd" && args[1] != "" && strings.HasPrefix(command[i:], "&& "):
			args = nil
			anchored, skipAnd = true, true
		case strings.ContainsRune(";|&<>$`()\"", c):
			return nil, fmt.Errorf("nonliteral shell syntax in delivered command")
		default:
			word.WriteRune(c)
			started = true
		}
	}
	if single || escaped {
		return nil, fmt.Errorf("unterminated quoting in delivered command")
	}
	if started {
		args = append(args, word.String())
	}
	return args, nil
}
