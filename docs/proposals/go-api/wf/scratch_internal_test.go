package wf

import (
	"strings"
	"testing"
)

// A scratch directory's name is the engine's: a slug of the qualified state
// and a hash of the instance id, so two instances in one state never share
// one, and the same instance and state always get the same.
func TestScratchLeafIdentity(t *testing.T) {
	a, b := scratchLeaf("c-models/chWork", "kid-1"), scratchLeaf("c-models/chWork", "kid-2")
	if a == b {
		t.Fatalf("two instances share %s", a)
	}
	if scratchLeaf("c-models/chWork", "kid-1") != a {
		t.Fatal("the same instance and state named two directories")
	}
	if scratchLeaf("c-models/chOther", "kid-1") == a {
		t.Fatal("two states of one instance share a directory")
	}
	for _, leaf := range []string{a, scratchLeaf("x/../y z", "")} {
		for _, ch := range leaf {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-') {
				t.Fatalf("leaf %q holds %q", leaf, ch)
			}
		}
	}
}

// Two states whose qualified names share a long prefix never meet: the hash
// covers the full qualified state and the instance id, unambiguously (an earlier revision
// review round 1, P2).
func TestScratchLeafHashesTheWholeIdentity(t *testing.T) {
	long := strings.Repeat("s", slugMax)
	if scratchLeaf(long+"A", "i") == scratchLeaf(long+"B", "i") {
		t.Fatal("two states sharing their first 200 bytes share a directory")
	}
	if scratchLeaf("a/b", "c") == scratchLeaf("a", "b/c") || scratchLeaf("ab", "") == scratchLeaf("a", "b") {
		t.Fatal("the identity's encoding is ambiguous")
	}
	leaf := scratchLeaf("x", "y")
	if i := strings.LastIndexByte(leaf, '-'); i < 0 || len(leaf)-i-1 < 16 {
		t.Fatalf("digest of %q is shorter than 16 hex", leaf)
	}
}
