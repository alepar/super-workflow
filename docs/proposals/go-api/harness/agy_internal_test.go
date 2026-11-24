package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

const agyBrief = "workflow prompt run-1 --claim 0a1b2c3d4e5f6071 --state-dir /tmp/s\n" +
	"You are a child agent. Say \"hi\", keep C:\\path\\x, tab\there, <tag attr=\"x\"> &amp; & ünïcödé — \u2028 end."

func TestAgyDetection(t *testing.T) {
	for _, kv := range []map[string]string{
		{"ANTIGRAVITY_CONVERSATION_ID": "c"},
		{"ANTIGRAVITY_CONVERSATION_ID": "c", "CLAUDECODE": "1", "CLAUDE_CODE_ENTRYPOINT": "cli", "CLAUDE_CODE_SESSION_ID": "s"},
		{"ANTIGRAVITY_CONVERSATION_ID": "c", "CODEX_THREAD_ID": "t"},
		{"ANTIGRAVITY_CONVERSATION_ID": "c", "CODEX_THREAD_ID": "t", "CLAUDECODE": "1"},
	} {
		if got := Detect(envMap(kv)); got != "agy" {
			t.Errorf("Detect(%v) = %q, want agy", kv, got)
		}
	}
	if got := Detect(envMap(map[string]string{"ANTIGRAVITY_CONVERSATION_ID": "c", "HERMES_SESSION_ID": "h"})); got != "hermes" {
		t.Errorf("Hermes stays first; Detect = %q", got)
	}
	if got := Detect(envMap(map[string]string{"ANTIGRAVITY_CONVERSATION_ID": "c", "PI_SESSION_ID": "p"})); got != "pi" {
		t.Errorf("pi stays before agy; Detect = %q", got)
	}
	if !Known("agy") || For("agy").Name() != "agy" || Canonical("agy") != "agy" {
		t.Fatal("agy must be a registered adapter under its canonical name")
	}
}

// agyArgs splits a line into the tool name and its decoded JSON arguments,
// failing on anything but one line of exactly one JSON object after the name.
func agyArgs(t *testing.T, line, tool string) map[string]string {
	t.Helper()
	if strings.ContainsAny(line, "\n\r") {
		t.Fatalf("not one line: %q", line)
	}
	rest, ok := strings.CutPrefix(line, tool+" ")
	if !ok {
		t.Fatalf("line does not start with %q: %s", tool+" ", line)
	}
	var args map[string]string
	dec := json.NewDecoder(strings.NewReader(rest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		t.Fatalf("not JSON after the tool name: %v: %s", err, rest)
	}
	if dec.More() {
		t.Fatalf("trailing data after the JSON object: %s", rest)
	}
	return args
}

func TestAgySpawnLineIsTheFlatCall(t *testing.T) {
	a := For("agy")
	for _, model := range []string{"", "inherit"} {
		line := a.SpawnLine(Spawn{Brief: agyBrief, Claim: "0a1b2c3d4e5f6071", Model: model, Effort: "high", AgentType: "Explore"})
		if !strings.HasPrefix(line, `invoke_subagent {"TypeName":"self","Role":"wf-unit","Prompt":"`) {
			t.Fatalf("flat form, key order TypeName, Role, Prompt: %s", line)
		}
		args := agyArgs(t, line, "invoke_subagent")
		if args["Prompt"] != agyBrief {
			t.Fatalf("Prompt not byte-equal to the brief:\n got %q\nwant %q", args["Prompt"], agyBrief)
		}
		if _, has := args["Model"]; has || len(args) != 3 {
			t.Fatalf("model %q: no Model, exactly three keys: %v", model, args)
		}
	}
	for _, model := range []string{"flash_lite", "flash", "pro"} {
		args := agyArgs(t, a.SpawnLine(Spawn{Brief: agyBrief, Model: model}), "invoke_subagent")
		if args["Model"] != model || args["TypeName"] != "self" || args["Role"] != "wf-unit" || args["Prompt"] != agyBrief {
			t.Fatalf("model %q: %v", model, args)
		}
	}
	if line := a.SpawnLine(Spawn{Brief: "<a> & <b>"}); !strings.Contains(line, `"Prompt":"<a> & <b>"`) {
		t.Fatalf("HTML characters stay literal: %s", line)
	}
	if a.AgentType() != "self" {
		t.Fatalf("AgentType = %q", a.AgentType())
	}
}

func TestAgyResumeAndChildren(t *testing.T) {
	a := For("agy")
	msg := "fix \"this\"\nthen C:\\x <ok> &"
	line := a.ResumeLine("00000000-0000-4000-8000-000000000001", msg)
	if !strings.HasPrefix(line, `send_message {"Recipient":"00000000-0000-4000-8000-000000000001","Message":"`) {
		t.Fatalf("resume line: %s", line)
	}
	if args := agyArgs(t, line, "send_message"); args["Message"] != msg || len(args) != 2 {
		t.Fatalf("resume args: %v", args)
	}
	if a.ResumeLine("", msg) != "" {
		t.Fatal("no agent, no resume line")
	}
	if !a.ChildrenDieWithCoordinator() {
		t.Error("agy children run inside the coordinator's process")
	}
	if n, src := a.Limit(envMap(nil), t.TempDir()); n != 0 || src != "" {
		t.Errorf("Limit = %d, %q", n, src)
	}
}

func TestAgyTiersAreInherit(t *testing.T) {
	tiers := For("agy").Tiers()
	if len(tiers) != 4 {
		t.Fatalf("Tiers = %v", tiers)
	}
	for _, tier := range []string{"light", "standard", "strong", "top"} {
		if got := tiers[tier]; got != (Tier{Model: "inherit"}) {
			t.Errorf("tier %s = %+v, want inherit", tier, got)
		}
	}
	if len(agyTiers) != 4 || agyTiers[0] != agyInherit {
		t.Fatalf("agyTiers = %v", agyTiers)
	}
}

func TestAgyLimits(t *testing.T) {
	a := For("agy")
	if b, l := a.OutputLimit(envMap(nil)); b != 9<<10 || l != 0 {
		t.Errorf("OutputLimit = %d, %d", b, l)
	}
	if d := a.ToolLimit(envMap(nil)); d != 0 {
		t.Errorf("ToolLimit = %v", d)
	}
}
