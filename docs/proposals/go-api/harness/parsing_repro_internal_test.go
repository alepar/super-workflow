package harness

import "testing"

// Breaking inputs from the parsing audit, reproduced on the engine and
// skipped until the slice that fixes each one unskips it.

func TestReproOnlyClaimWithClaimInWorkText(t *testing.T) {
	brief := []byte("cd /w && wf prompt --claim T1\nverify that `wf prompt --claim X` rejects stale tokens\n")
	if !firstClaimIs(brief, "T1") {
		t.Fatal("work text naming another --claim stops the child resolving")
	}
}

func TestReproLiteralShellArgvBackslashNewline(t *testing.T) {
	argv, ok := literalShellArgv("wf \\\nprompt --claim T")
	if !ok || len(argv) != 4 || argv[1] != "prompt" {
		t.Fatalf("literalShellArgv = %q, %v; want [wf prompt --claim T]", argv, ok)
	}
}
