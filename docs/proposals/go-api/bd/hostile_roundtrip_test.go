package bd_test

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/sink"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest/hostile"
)

// Every corpus input has a stated outcome on every path, never "whatever
// happened": the adapter refuses it before bd runs (hasNUL: no process can
// receive a NUL byte; an empty label, which bd drops), bd refuses it (a
// blank title), or it round-trips exactly. Expected labels come from the
// input, not from the encoder: what bd stores is percent-decoded and
// compared with the input.

func hasNUL(v string) bool { return strings.ContainsRune(v, 0) }

// hostileJSON is v after a JSON round trip: effect args are journaled as
// JSON before any adapter sees them, so invalid UTF-8 already arrives as
// U+FFFD. Every other input must arrive intact.
func hostileJSON(v string) string {
	if utf8.ValidString(v) {
		return v
	}
	b, _ := json.Marshal(v)
	var out string
	_ = json.Unmarshal(b, &out)
	return out
}

// hostileApply is rt.Apply with a panic turned into a test failure, so one
// input that crashes the adapter does not stop the corpus.
func hostileApply(t *testing.T, rt wf.AdapterRuntime, dir string, e wf.ExternalEffect) (raw json.RawMessage, err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("%s panicked: %v", e.Op, p)
			err = errors.New("panicked")
		}
	}()
	return rt.Apply(dir, e)
}

// argvBd is a fake bd that records the argv of every call, one file per
// call, and answers like bd with an empty result.
type argvBd struct{ bin, calls string }

func newArgvBd(t *testing.T) argvBd {
	t.Helper()
	dir := t.TempDir()
	f := argvBd{bin: filepath.Join(dir, "bd"), calls: filepath.Join(dir, "calls")}
	if err := os.Mkdir(f.calls, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nn=$(ls " + f.calls + " | wc -l)\nprintf '%s\\0' \"$@\" > " + f.calls + "/$n\n" +
		"if [ \"$1\" = create ]; then echo '{\"id\":\"tc-1\"}'; else echo '[]'; fi\n"
	if err := os.WriteFile(f.bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f argvBd) reset(t *testing.T) {
	t.Helper()
	ents, err := os.ReadDir(f.calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if err := os.Remove(filepath.Join(f.calls, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

// argv is every call's argv since the last reset, in order.
func (f argvBd) argv(t *testing.T) [][]string {
	t.Helper()
	ents, err := os.ReadDir(f.calls)
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]string, len(ents))
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil || n >= len(out) {
			t.Fatalf("call file %q", e.Name())
		}
		b, err := os.ReadFile(filepath.Join(f.calls, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[n] = strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	}
	return out
}

// flagValue is the value after the first --name in argv.
func flagValue(argv []string, name string) (string, bool) {
	for i, a := range argv {
		if a == name && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// bdSlice parses a value as bd (pflag's StringSlice) does: "" is no labels,
// anything else is one CSV record.
func bdSlice(v string) ([]string, error) {
	if v == "" {
		return []string{}, nil
	}
	return csv.NewReader(strings.NewReader(v)).Read()
}

// checkStoredLabel fails unless stored is a label bd keeps as it is (no CSV
// special character, no whitespace of any kind at an edge, which bd would
// trim) and decodes, by URL percent-decoding, to want.
func checkStoredLabel(t *testing.T, what, stored, want string) {
	t.Helper()
	if strings.ContainsAny(stored, ",\"\r\n") {
		t.Errorf("%s: label %q holds a CSV special character", what, stored)
	}
	if strings.TrimFunc(stored, unicode.IsSpace) != stored {
		t.Errorf("%s: label %q has whitespace at an edge, which bd trims", what, stored)
	}
	if got, err := url.PathUnescape(stored); err != nil || got != want {
		t.Errorf("%s: label %q decodes to %q (err %v), want %q", what, stored, got, err, want)
	}
}

// distinct records that label came from input under what, failing when an
// earlier, different input gave the same label.
type distinct map[string]string

func (d distinct) add(t *testing.T, what, label, input string) {
	t.Helper()
	k := what + "\x00" + label
	if prev, ok := d[k]; ok && prev != input {
		t.Errorf("%s: inputs %q and %q both give label %q", what, prev, input, label)
	}
	d[k] = input
}

// A label written through Create, ReadyIn and ListByLabel reaches bd as
// exactly that one label after bd's CSV parse and trim; distinct inputs give
// distinct labels. An empty label is refused, except an empty root label or
// ReadyIn scope, which means none.
func TestHostileLabelArgvRoundTrip(t *testing.T) {
	fake := newArgvBd(t)
	rt := bd.Runtime(bd.Config{Bin: fake.bin})
	ws := t.TempDir()
	seen := distinct{}
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			nokey, want := wf.NonIdempotent("hostile"), hostileJSON(in.Value)
			apply := func(is bd.Issue) error { _, err := rt.Apply(ws, bd.Create(nokey, is)); return err }
			read := func(d wf.ObservationDesc) error { _, err := rt.Read(ws, d); return err }
			for _, op := range []struct {
				name, flag string
				want       string // the label bd must store, decoded
				emptyNone  bool   // an empty value means no label, not a refusal
				run        func() error
			}{
				// Every arg is taken as the journal delivers it (invalid
				// UTF-8 as a JSON round trip leaves it). No key: it would ride
				// on --labels.
				{"Create Labels", "--labels", want, false, func() error { return apply(bd.Issue{Title: "t", Labels: []string{in.Value}}) }},
				{"Create RootLabel", "--labels", want, true, func() error { return apply(bd.Issue{Title: "t", RootLabel: in.Value}) }},
				{"ReadyIn label", "--label", want, true, func() error { return read(bd.ReadyIn(in.Value).Desc()) }},
				{"ReadyIn exclude", "--exclude-label", want, false, func() error { return read(bd.ReadyIn("x", in.Value).Desc()) }},
				{"ListByLabel", "--label", want, false, func() error { return read(bd.ListByLabel(in.Value).Desc()) }},
			} {
				fake.reset(t)
				err := op.run()
				calls := fake.argv(t)
				switch {
				case hasNUL(in.Value):
					if !errors.Is(err, sink.ErrNUL) || len(calls) != 0 {
						t.Errorf("%s: err %v after %d bd calls; want sink.ErrNUL before any", op.name, err, len(calls))
					}
				case in.Value == "" && op.emptyNone:
					if err != nil || len(calls) != 1 {
						t.Errorf("%s: err %v after %d bd calls; want one call", op.name, err, len(calls))
					} else if v, ok := flagValue(calls[0], op.flag); ok {
						t.Errorf("%s: empty value passed %s %q; want no label", op.name, op.flag, v)
					}
				case in.Value == "":
					if !errors.Is(err, sink.ErrEmptyLabel) || len(calls) != 0 {
						t.Errorf("%s: err %v after %d bd calls; want sink.ErrEmptyLabel before any", op.name, err, len(calls))
					}
				case err != nil || len(calls) != 1:
					t.Errorf("%s: err %v after %d bd calls; want one call", op.name, err, len(calls))
				default:
					v, ok := flagValue(calls[0], op.flag)
					got, perr := bdSlice(v)
					if !ok || perr != nil || len(got) != 1 {
						t.Errorf("%s: %s %q (present %v) parses as %q (err %v); want one label", op.name, op.flag, v, ok, got, perr)
						continue
					}
					checkStoredLabel(t, op.name, got[0], op.want)
					seen.add(t, op.name, got[0], op.want)
				}
			}
		})
	}
}

// The key label Check looks a create up by reaches bd as one label that
// decodes to "key:" and the key (or is the SHA-256 form, for a key too long
// to stamp); distinct keys give distinct labels.
func TestHostileKeyLabelArgvRoundTrip(t *testing.T) {
	fake := newArgvBd(t)
	rt := bd.Runtime(bd.Config{Bin: fake.bin})
	ws := t.TempDir()
	seen := distinct{}
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			e := bd.Create(bd.IssueKey("sp:x", in.Value), bd.Issue{Title: "t"})
			fake.reset(t)
			_, err := rt.Check(ws, e)
			calls := fake.argv(t)
			if hasNUL(in.Value) {
				if !errors.Is(err, sink.ErrNUL) || len(calls) != 0 {
					t.Errorf("err %v after %d bd calls; want sink.ErrNUL before any", err, len(calls))
				}
				return
			}
			if err != nil || len(calls) == 0 || calls[0][0] != "list" {
				t.Fatalf("err %v, calls %q; want a list call first", err, calls)
			}
			v, _ := flagValue(calls[0], "--label")
			got, perr := bdSlice(v)
			if perr != nil || len(got) != 1 || len(got[0]) > 255 {
				t.Fatalf("--label %q parses as %q (err %v); want one label of at most 255 bytes", v, got, perr)
			}
			key := hostileJSON(e.KeyID())
			sum := sha256.Sum256([]byte(key))
			if hashed := "key:sha256-" + hex.EncodeToString(sum[:]); got[0] != hashed {
				checkStoredLabel(t, "key label", got[0], "key:"+key)
			}
			seen.add(t, "key label", got[0], key)
		})
	}
}

// A value in a positional slot (a title, an id) must not be read as an
// option: the title goes as one --title= element, the id after "--".
func TestHostilePositionalArgv(t *testing.T) {
	fake := newArgvBd(t)
	rt := bd.Runtime(bd.Config{Bin: fake.bin})
	ws := t.TempDir()
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			fake.reset(t)
			_, err := rt.Apply(ws, bd.Create(wf.KeyOf("k"), bd.Issue{Title: in.Value}))
			calls := fake.argv(t)
			switch {
			case hasNUL(in.Value):
				if !errors.Is(err, sink.ErrNUL) || len(calls) != 0 {
					t.Errorf("Create: err %v after %d bd calls; want sink.ErrNUL before any", err, len(calls))
				}
			case err != nil || len(calls) != 1:
				t.Errorf("Create: err %v after %d bd calls; want one call", err, len(calls))
			case !slices.Equal(calls[0][:2], []string{"create", "--title=" + hostileJSON(in.Value)}):
				t.Errorf("Create title %q: argv %q; want it as the one element --title=<title>", in.Value, calls[0])
			}
			fake.reset(t)
			_, err = rt.Read(ws, bd.Show(in.Value).Desc())
			calls = fake.argv(t)
			switch {
			case hasNUL(in.Value):
				if !errors.Is(err, sink.ErrNUL) || len(calls) != 0 {
					t.Errorf("Show: err %v after %d bd calls; want sink.ErrNUL before any", err, len(calls))
				}
			case len(calls) != 1 || !slices.Equal(calls[0], []string{"show", "--json", "--", hostileJSON(in.Value)}):
				// The fake finds no bead, so Show itself fails; its argv is
				// what is under test.
				t.Errorf("Show id %q: calls %q; want show --json -- <id>", in.Value, calls)
			}
		})
	}
}

// showRaw is bd's own JSON for bead id, read without the adapter.
func showRaw(t *testing.T, dir, id string) map[string]any {
	t.Helper()
	cmd := exec.Command("bd", "show", id, "--json")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bd show %s: %v", id, err)
	}
	var v []map[string]any
	if err := json.Unmarshal(out, &v); err != nil || len(v) != 1 {
		t.Fatalf("bd show %s: %v: %s", id, err, out)
	}
	return v[0]
}

// Free text written through the real bd reads back exactly: a title, a
// description, a label, a metadata value and a close reason, each on its
// own bead so a refusal on one path never skips another. The stated
// exceptions: a value holding NUL is refused before bd runs on every path
// but metadata, where it is a JSON escape;
// bd refuses a blank title ("title required", "whitespace-only"); an empty
// label is refused (bd would drop it); an empty close reason makes bd store
// its default "Closed".
func TestHostileTextRealBdRoundTrip(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	labels := distinct{}
	labelled := map[string][]string{} // decoded label -> the beads created with it
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			want := hostileJSON(in.Value)
			refusedNUL := func(what string, err error) bool {
				t.Helper()
				if !hasNUL(in.Value) {
					return false
				}
				if !errors.Is(err, sink.ErrNUL) {
					t.Errorf("%s: err %v, want sink.ErrNUL", what, err)
				}
				return true
			}
			created := func(what string, raw json.RawMessage, err error) (bd.Issue, bool) {
				t.Helper()
				var is bd.Issue
				if err != nil {
					t.Errorf("%s: refused: %v", what, err)
					return is, false
				}
				if err := json.Unmarshal(raw, &is); err != nil || is.ID == "" {
					t.Errorf("%s: result %s: %v", what, raw, err)
					return is, false
				}
				return is, true
			}

			// Title, as the one element --title=<title>.
			e := bd.Create(wf.KeyOf("title", in.Name), bd.Issue{Title: in.Value})
			raw, err := hostileApply(t, rt, dir, e)
			switch {
			case refusedNUL("Create title", err):
			case strings.TrimSpace(want) == "":
				if err == nil || !strings.Contains(err.Error(), "title required") && !strings.Contains(err.Error(), "whitespace-only") {
					t.Errorf("Create blank title: err %v, want bd's blank-title refusal", err)
				}
			default:
				if is, ok := created("Create title", raw, err); ok {
					if got := read[bd.Issue](t, rt, dir, bd.Show(is.ID).Desc()); got.Title != want {
						t.Errorf("title %q read back as %q", want, got.Title)
					}
					if st, err := rt.Check(dir, e); err != nil || st != wf.EffectDone {
						t.Errorf("Check after create = %v, %v; want done", st, err)
					}
				}
			}

			// Description: an empty one is none, and reads back empty.
			raw, err = hostileApply(t, rt, dir, bd.Create(wf.KeyOf("desc", in.Name), bd.Issue{Title: "d " + in.Name, Description: in.Value}))
			if !refusedNUL("Create description", err) {
				if is, ok := created("Create description", raw, err); ok {
					if got := read[bd.Issue](t, rt, dir, bd.Show(is.ID).Desc()); got.Description != want {
						t.Errorf("description %q read back as %q", want, got.Description)
					}
				}
			}

			// Label: bd stores exactly one, which decodes to the input and
			// finds the bead through ListByLabel. No key, so the bead's only
			// label is the one under test.
			raw, err = hostileApply(t, rt, dir, bd.Create(wf.NonIdempotent("hostile"), bd.Issue{Title: "l " + in.Name, Labels: []string{in.Value}}))
			switch {
			case refusedNUL("Create label", err):
			case in.Value == "":
				if !errors.Is(err, sink.ErrEmptyLabel) {
					t.Errorf("Create empty label: err %v, want sink.ErrEmptyLabel", err)
				}
			default:
				is, ok := created("Create label", raw, err)
				if !ok {
					break
				}
				var stored []string
				all, _ := showRaw(t, dir, is.ID)["labels"].([]any)
				for _, l := range all {
					s, _ := l.(string)
					stored = append(stored, s)
				}
				if len(stored) != 1 {
					t.Errorf("label %q stored as labels %q, want one", want, stored)
					break
				}
				checkStoredLabel(t, "stored label", stored[0], want)
				labels.add(t, "stored label", stored[0], want)
				// Inputs that differ only in invalid UTF-8 arrive as the same
				// label, so ListByLabel finds every bead created with it.
				labelled[want] = append(labelled[want], is.ID)
				var found []string
				for _, f := range read[[]bd.Issue](t, rt, dir, bd.ListByLabel(want).Desc()) {
					found = append(found, f.ID)
				}
				if slices.Sort(found); !slices.Equal(found, slices.Sorted(slices.Values(labelled[want]))) {
					t.Errorf("ListByLabel(%q) = %q, want %q", want, found, labelled[want])
				}
			}

			// Metadata value and close reason, on a bead of their own. No
			// corpus value is one --set-metadata typed (a number, true,
			// false, null), so each is stored as the string. A metadata
			// value travels inside a JSON object, where NUL is the escape
			// \u0000, so it is not refused and round-trips too.
			m := apply(t, rt, dir, bd.Create(wf.KeyOf("m", in.Name), bd.Issue{Title: "m " + in.Name}))
			_, err = hostileApply(t, rt, dir, bd.SetMetadata(wf.KeyOf("m", in.Name), m.ID, map[string]string{"note": in.Value}))
			if err != nil {
				t.Errorf("SetMetadata: refused: %v", err)
			} else if md, _ := showRaw(t, dir, m.ID)["metadata"].(map[string]any); md["note"] != want {
				t.Errorf("metadata value %q read back as %#v", want, md["note"])
			}
			_, err = hostileApply(t, rt, dir, bd.Close(wf.KeyOf("c", in.Name), m.ID, in.Value))
			if !refusedNUL("Close", err) {
				wantReason := want
				if in.Value == "" {
					wantReason = "Closed"
				}
				if err != nil {
					t.Errorf("Close: refused: %v", err)
				} else if r := showRaw(t, dir, m.ID)["close_reason"]; r != wantReason {
					t.Errorf("close reason %q read back as %#v, want %q", in.Value, r, wantReason)
				}
			}
		})
	}
}

// DepAdd's reason line is recorded once and makes Check done. A blank reason
// records no line; a reason holding NUL is refused before bd runs.
func TestHostileDepAddReasonRealBd(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	b := apply(t, rt, dir, bd.Create(wf.KeyOf("blocker"), bd.Issue{Title: "B"}))
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			a := apply(t, rt, dir, bd.Create(wf.KeyOf("dep", in.Name), bd.Issue{Title: "A " + in.Name}))
			e := bd.DepAdd(wf.KeyOf("dep-add", in.Name), a.ID, b.ID, in.Value)
			_, err := hostileApply(t, rt, dir, e)
			if hasNUL(in.Value) {
				if !errors.Is(err, sink.ErrNUL) {
					t.Errorf("err %v, want sink.ErrNUL", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if st, err := rt.Check(dir, e); err != nil || st != wf.EffectDone {
				t.Errorf("Check after DepAdd(reason %q) = %v, %v; want done", in.Value, st, err)
			}
			got := apply(t, rt, dir, e) // a recovery replay
			if strings.TrimSpace(in.Value) == "" {
				if got.Description != "" {
					t.Errorf("a blank reason recorded description %q, want none", got.Description)
				}
			} else if n := strings.Count(got.Description, hostileJSON(in.Value)); n != 1 {
				t.Errorf("after a replay the description holds the reason %d times: %q", n, got.Description)
			}
		})
	}
}
