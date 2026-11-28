package wf

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// Breaking inputs from the parsing audit, reproduced on the engine and
// skipped until the slice that fixes each one unskips it.

func TestReproKeyOfPartsCollide(t *testing.T) {
	if KeyOf("fix", "a/b").(idemKey) == KeyOf("fix", "a", "b").(idemKey) {
		t.Fatalf("two effects share the key %v", KeyOf("fix", "a/b").key())
	}
	// Bound parts are escaped too: a placeholder's value holding "/" does
	// not move a part boundary.
	s := scope{}
	a, _ := s.bindEffect(EffectData{Kind: "external", Key: "fix/a%2Fb", KeyParts: []string{"fix", "a/b"}})
	b, _ := s.bindEffect(EffectData{Kind: "external", Key: "fix/a/b", KeyParts: []string{"fix", "a", "b"}})
	if a.Key == b.Key || a.Key != "fix/a%2Fb" || a.LegacyKey != "fix/a/b" || b.LegacyKey != "" {
		t.Fatalf("bound keys %q (legacy %q) and %q (legacy %q)", a.Key, a.LegacyKey, b.Key, b.LegacyKey)
	}
}

// The spawn line carries the instruction's claim, never one read off the
// brief, whose workspace may hold " --claim ".
func TestReproBriefClaimInWorkspacePath(t *testing.T) {
	in := Instruction{Kind: InstructionAgent, Name: "u", Claim: "abc123", Brief: "cd '/home/u/x --claim y' && '/bin/wf' prompt --claim abc123\n"}
	call := renderUnit(harness.For("pi"), in)
	if !strings.Contains(call, "wf-abc123") || strings.Contains(call, "wf-y") {
		t.Fatalf("the spawn line does not carry claim abc123:\n%s", call)
	}
}

// The binary a headless child may run is the engine's own, never read off
// the brief (whose workspace may hold "&& "): a library engine's is the
// default VerbLine's "workflow".
func TestReproBriefBinaryWithAndInWorkspace(t *testing.T) {
	e := NewEngine(MemJournal(), NewAdapters(), EngineOptions{}).(*Engine)
	if e.opts.Binary != "workflow" {
		t.Fatalf("default Binary = %q, want workflow", e.opts.Binary)
	}
}

func TestReproUnitPathOfGenericState(t *testing.T) {
	t.Skip("sw-5l5n: instance ids need one parser beside their formatter")
	if got := unitPath("G[X][k]"); got != "G[X]/k" {
		t.Fatalf("unitPath = %q, want G[X]/k", got)
	}
}

func TestReproChainBaseOfForkedSuccessor(t *testing.T) {
	t.Skip("sw-5l5n: run ids need one parser beside their formatter")
	if got := chainBase("x.2.rw1"); got == "x.2.rw1" {
		t.Fatalf("chainBase returned the fork of a successor unchanged")
	}
}
