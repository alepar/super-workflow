package wf

import "testing"

// Breaking inputs from the parsing audit, reproduced on the engine and
// skipped until the slice that fixes each one unskips it.

func TestReproKeyOfPartsCollide(t *testing.T) {
	t.Skip("sw-8h7f: effect keys must escape their parts")
	if KeyOf("fix", "a/b") == KeyOf("fix", "a", "b") {
		t.Fatalf("two effects share the key %v", KeyOf("fix", "a/b"))
	}
}

func TestReproBriefClaimInWorkspacePath(t *testing.T) {
	t.Skip("sw-8h7f: the claim travels as a field, not recovered from the brief")
	brief := "cd '/home/u/x --claim y' && '/bin/wf' prompt --claim abc123"
	if got := briefClaim(brief); got != "abc123" {
		t.Fatalf("briefClaim = %q, want abc123", got)
	}
}

func TestReproBriefBinaryWithAndInWorkspace(t *testing.T) {
	t.Skip("sw-8h7f: the binary travels as a field, not recovered from the brief")
	if got := briefBinary("cd '/w/a && b' && /bin/wf prompt"); got != "/bin/wf" {
		t.Fatalf("briefBinary = %q, want /bin/wf", got)
	}
}

func TestReproUnitPathOfGenericState(t *testing.T) {
	t.Skip("sw-8h7f: instance ids need one parser beside their formatter")
	if got := unitPath("G[X][k]"); got != "G[X]/k" {
		t.Fatalf("unitPath = %q, want G[X]/k", got)
	}
}

func TestReproChainBaseOfForkedSuccessor(t *testing.T) {
	t.Skip("sw-8h7f: run ids need one parser beside their formatter")
	if got := chainBase("x.2.rw1"); got == "x.2.rw1" {
		t.Fatalf("chainBase returned the fork of a successor unchanged")
	}
}
