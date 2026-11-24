package wf

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// A document a cycle's bindings read is decoded once for the cycle, and a
// lookup through the decoded value answers as decoding it again would:
// nested paths, keys matched without case, numbers as written, misses.
func TestDocLookupsDecodeEachDocumentOncePerCycle(t *testing.T) {
	raw := json.RawMessage(`{"spec":{"Path":"docs/x.md","n":9007199254740993},"list":[1,2],"flag":true}`)
	c := &cycle{}
	decodes := 0
	defer func(old func()) { docDecoded = old }(docDecoded)
	docDecoded = func() { decodes++ }
	cases := []struct {
		path string
		want any
		ok   bool
	}{
		{"spec.path", "docs/x.md", true},
		{"spec.N", json.Number("9007199254740993"), true},
		{"flag", true, true},
		{"spec.missing", nil, false},
		{"list.0", nil, false},
	}
	uncached := map[string]any{}
	for _, tt := range cases {
		v, ok := lookupDocValue(raw, tt.path)
		if ok {
			uncached[tt.path] = v
		}
	}
	decodes = 0
	for _, tt := range cases {
		got, ok := c.docValue(raw, tt.path)
		want, wantOK := uncached[tt.path]
		if ok != tt.ok || ok != wantOK || (ok && (got != tt.want || got != want)) {
			t.Errorf("%s: %v %v, want %v %v (uncached %v %v)", tt.path, got, ok, tt.want, tt.ok, want, wantOK)
		}
	}
	if decodes != 1 {
		t.Errorf("decoded %d times in one cycle, want 1", decodes)
	}
	if v, ok := c.docValue(json.RawMessage(`not json`), "a"); ok || v != "" {
		t.Errorf("a malformed document: %v %v", v, ok)
	}
}

// Concurrent placeholder reads in one compute (cold: nothing decoded yet)
// share the cycle's document cache safely.
func TestDocLookupsConcurrentColdReads(t *testing.T) {
	s := pathTemplateScope(t)
	s.in.Input = json.RawMessage(`{"file":"X.txt"}`)
	ctx := &runCtx{c: s.c, in: s.in}
	for round := 0; round < 200; round++ {
		s.c.docs = nil // cold: a fresh cycle
		start := make(chan struct{})
		errs := make(chan error, 12)
		got := make(chan string, 12)
		for i := 0; i < 12; i++ {
			go func() {
				<-start
				p, err := ResolvePath(ctx, PathTemplate("{file}"))
				errs <- err
				got <- p
			}()
		}
		close(start)
		for i := 0; i < 12; i++ {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			if p := <-got; !strings.HasSuffix(p, "X.txt") {
				t.Fatalf("resolved %q", p)
			}
		}
	}
}

// rewriting is a workflow type whose decoder writes into the bytes it is
// given, as an in-place decoder may.
type rewriting struct{ File string }

func (r *rewriting) UnmarshalJSON(b []byte) error {
	if i := bytes.Index(b, []byte("X.txt")); i >= 0 {
		b[i] = 'Y'
	}
	var v struct{ File string }
	err := json.Unmarshal(b, &v)
	r.File = v.File
	return err
}

// A typed read through a workflow decoder never changes the journal bytes
// the cycle's placeholder lookups and later decodes read.
func TestTypedReadsDoNotChangeFoldedBytes(t *testing.T) {
	s := pathTemplateScope(t)
	s.in.Input = json.RawMessage(`{"file":"X.txt"}`)
	ctx := &runCtx{c: s.c, in: s.in}
	before, err := ResolvePath(ctx, PathTemplate("{file}"))
	if err != nil {
		t.Fatal(err)
	}
	if typed := InputOf[rewriting](ctx); typed.File != "Y.txt" {
		t.Fatalf("the decoder did not run as written: %+v", typed)
	}
	after, err := ResolvePath(ctx, PathTemplate("{file}"))
	if err != nil || after != before {
		t.Fatalf("lookup after a typed read: %q, was %q (%v)", after, before, err)
	}
	if v, _ := lookupDocValue(s.in.Input, "file"); v != "X.txt" {
		t.Fatalf("the folded bytes changed: %v (%s)", v, s.in.Input)
	}
}
