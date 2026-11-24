package wf

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type checkTerminal struct{}

func TestCheckCLIRequiresRegisteredSuite(t *testing.T) {
	d := New("checked")
	NewTerminal[checkTerminal](d, Success)
	var out, errOut bytes.Buffer
	code := CLI(MainConfig{Defs: []Registration{{Def: d}}}, []string{"check", "checked"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "missing checker") {
		t.Fatalf("code=%d out=%s err=%s", code, &out, &errOut)
	}
}

func TestCheckCLIRunsSuiteAndReportsFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		d := New("checked")
		NewTerminal[checkTerminal](d, Success)
		var out, errOut bytes.Buffer
		called := 0
		cfg := MainConfig{Defs: []Registration{{Def: d, Check: func(w io.Writer) error {
			called++
			io.WriteString(w, "calibration and simulation report\n")
			if fail {
				return errors.New("fixture-gap")
			}
			return nil
		}}}}
		code := CLI(cfg, []string{"check", "checked"}, &out, &errOut)
		want := 0
		if fail {
			want = 8
		}
		if called != 1 || !strings.Contains(out.String(), "simulation report") || code != want {
			t.Fatalf("fail=%v called=%v code=%d out=%s err=%s", fail, called, code, &out, &errOut)
		}
	}
}

func TestCheckCLIRejectsInvalidDefinitionBeforeSuite(t *testing.T) {
	d := New("invalid")
	called := false
	var out, errOut bytes.Buffer
	code := CLI(MainConfig{Defs: []Registration{{Def: d, Check: func(io.Writer) error { called = true; return nil }}}}, []string{"check", "invalid"}, &out, &errOut)
	if code != 8 || called || !strings.Contains(errOut.String(), "lint") {
		t.Fatalf("called=%v code=%d err=%s", called, code, &errOut)
	}
}

func TestCheckCLINamedBinary(t *testing.T) {
	d := New("feature")
	NewTerminal[checkTerminal](d, Success)
	var out, errOut bytes.Buffer
	called := false
	cfg := MainConfig{Binary: "wf", Defs: []Registration{{Def: d, Check: func(io.Writer) error { called = true; return nil }}}}
	code := CLI(cfg, []string{"check", "feature"}, &out, &errOut)
	if code != 0 || !called || !strings.Contains(out.String(), "check feature: passed") {
		t.Fatalf("called=%v code=%d out=%s err=%s", called, code, &out, &errOut)
	}
}

func TestCheckCLIUsageRemainsDistinctFromFindings(t *testing.T) {
	for _, args := range [][]string{{"check"}, {"check", "unknown"}, {"check", "one", "two"}} {
		var out, errOut bytes.Buffer
		if code := CLI(MainConfig{}, args, &out, &errOut); code != 2 {
			t.Fatalf("%v exit=%d stderr=%s", args, code, &errOut)
		}
	}
	found := false
	for _, row := range ExitCodes {
		if row.Code == 8 {
			found = row.Name == "check-failed"
		}
	}
	if !found {
		t.Fatal("checker failure missing from exit table")
	}
}
