package wf_test

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runDirState is every file under a run's directory: its size, mtime and
// content hash.
func runDirState(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[path] = "dir " + info.ModTime().String()
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = fmt.Sprintf("%d %s %x", info.Size(), info.ModTime(), sha256.Sum256(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameRunDir(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	for p, b := range before {
		if a, ok := after[p]; !ok {
			t.Errorf("%s removed %s", what, p)
		} else if a != b {
			t.Errorf("%s changed %s:\n  was %s\n  now %s", what, p, b, a)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s created %s", what, p)
		}
	}
}

// A call that only reads a run, or that the engine refuses, leaves the run's
// directory exactly as it was: no lock-file holder record, no repaired
// journal tail, no new file (bl-codex: agents of forks 0007 and 0008
// addressed the ended source run, were refused, and rewrote its lock file).
func TestReadsAndRefusalsLeaveTheRunDirUntouched(t *testing.T) {
	for _, ended := range []bool{true, false} {
		t.Run(fmt.Sprintf("ended=%v", ended), func(t *testing.T) {
			e := newCLIEnv(t)
			run := e.start("agent-cli")
			in := e.next(run, "1")[0]
			if ended {
				e.must("dev", "started", string(run), in.Name, "--session", "s1", "--agent", "a1", "--state-dir", e.state)
				e.must("done", string(run), in.Name, "--output", writeAllocated(t, in.Output, `{"OK":true}`), "--state-dir", e.state)
				if code, _, er := e.cli("next", string(run), "1", "--state-dir", e.state, "--json"); code != 7 {
					t.Fatalf("the run did not end: %d %s", code, er)
				}
			}
			dir := filepath.Join(e.state, "runs", string(run))
			// As a run an older engine made: no turnstile gate. A read or a
			// refusal does not create one.
			if err := os.Remove(filepath.Join(dir, "lock.gate")); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			before := runDirState(t, dir)
			for _, args := range [][]string{
				{"status", string(run), "--state-dir", e.state},
				{"status", string(run), "--state-dir", e.state, "--json"},
				{"journal", string(run), "--state-dir", e.state},
				{"prompt", string(run), "--claim", "0000000000000000", "--state-dir", e.state},
				{"done", string(run), "--claim", "0000000000000000", "--state-dir", e.state},
				{"failed", string(run), "--claim", "0000000000000000", "--reason", "x", "--state-dir", e.state},
				{"validate", string(run), "--claim", "0000000000000000", "--state-dir", e.state},
			} {
				code, _, er := e.cli(args...)
				if args[0] != "status" && args[0] != "journal" && code == 0 {
					t.Errorf("%v was not refused: %s", args, er)
				}
				sameRunDir(t, strings.Join(args[:2], " "), before, runDirState(t, dir))
			}
		})
	}
}
