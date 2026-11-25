package sink_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/alepar/super-workflow/docs/proposals/go-api/sink"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest/hostile"
)

func TestHostileShellLine(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			if strings.IndexByte(in.Value, 0) >= 0 {
				if err := sink.CheckArg(in.Value); !errors.Is(err, sink.ErrNUL) {
					t.Fatalf("CheckArg with NUL: err %v, want ErrNUL", err)
				}
				return
			}
			a := sink.Argv{"printf", "%s\\0", in.Value, "-x"}
			for _, sh := range shells(t) {
				out, err := exec.Command(sh[0], append(sh[1:], "-c", a.Line())...).Output()
				if err != nil {
					t.Fatalf("%s -c %q: %v", sh, a.Line(), err)
				}
				got := strings.Split(string(out), "\x00")
				if len(got) != 3 || got[0] != in.Value || got[1] != "-x" {
					t.Errorf("%s: argv %q, want [%q -x]", sh, got, in.Value)
				}
			}
		})
	}
}

// shells are the shells a quoted line must read the same in: sh, bash, and
// zsh when it is installed (without extendedglob, under which Quote's bare #
// is a pattern; prompt words quote it: wf.ShellBound).
func shells(t *testing.T) [][]string {
	t.Helper()
	out := [][]string{{"sh"}, {"bash"}}
	if _, err := exec.LookPath("zsh"); err == nil {
		out = append(out, []string{"zsh"})
	}
	return out
}

// mdInline is hostile.ReadInline, failing the test on refused syntax.
func mdInline(t *testing.T, md string) string {
	t.Helper()
	text, err := hostile.ReadInline(md)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestHostileMarkdownText(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			if got := mdInline(t, sink.Text(in.Value).String()); got != hostile.Readable(in.Value) {
				t.Errorf("Text(%q) reads back %q", in.Value, got)
			}
		})
	}
}

func TestHostileMarkdownCode(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			if got := mdInline(t, sink.Code(in.Value).String()); got != hostile.Readable(in.Value) {
				t.Errorf("Code(%q) = %q reads back %q", in.Value, sink.Code(in.Value), got)
			}
		})
	}
}

func TestHostileMarkdownLink(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			raw := "http://example.com/" + in.Value
			md := sink.Link(sink.Text(in.Value), raw).String()
			// [text](<dest>): the text ends at the first unescaped ], the
			// destination is the rest inside <...>.
			if !strings.HasPrefix(md, "[") || !strings.HasSuffix(md, ">)") {
				t.Fatalf("link %q", md)
			}
			end := -1
			for i := 1; i < len(md); i++ {
				if md[i] == '\\' {
					i++
					continue
				}
				if md[i] == ']' {
					end = i
					break
				}
			}
			if end < 0 || !strings.HasPrefix(md[end:], "](<") {
				t.Fatalf("link %q: text does not close before the destination", md)
			}
			if got := mdInline(t, md[1:end]); got != hostile.Readable(in.Value) {
				t.Errorf("link text reads back %q", got)
			}
			dest := md[end+3 : len(md)-2]
			// A destination reads backslash escapes and character
			// references; nothing may stay that it would decode or end on.
			var dec strings.Builder
			for i := 0; i < len(dest); i++ {
				c := dest[i]
				switch {
				case c == '\\' && i+1 < len(dest) && strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", dest[i+1]) >= 0:
					dec.WriteByte(dest[i+1])
					i++
				case c == '&' || c == '<' || c == '>' || c == '\\' || c <= ' ' || c == 0x7f:
					t.Fatalf("destination %q holds a raw %q", dest, c)
				default:
					dec.WriteByte(c)
				}
			}
			// The URL as a URL: a space or control byte and its %XX
			// spelling are the same URL. Invalid UTF-8 reads as U+FFFD.
			want := strings.ReplaceAll(hostile.Readable(strings.ReplaceAll(raw, "\x00", "\x01")), "\x01", "\x00")
			if got := pctControls(dec.String()); got != pctControls(want) {
				t.Errorf("destination reads %q, want %q", got, pctControls(want))
			}
		})
	}
}

func TestHostileMarkdownRow(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			for _, cell := range []sink.Inline{sink.Text(in.Value), sink.Code(in.Value)} {
				row := string(sink.Row(sink.Text("k"), cell))
				cells, err := hostile.ReadRow(row)
				if err != nil || len(cells) != 2 {
					t.Fatalf("row %q: cells %q, %v; want 2", row, cells, err)
				}
				if got := cells[1]; got != hostile.Readable(in.Value) {
					t.Errorf("cell reads back %q", got)
				}
			}
		})
	}
}

func TestHostileLabel(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			l, err := sink.NewLabel(in.Value)
			if in.Value == "" || strings.IndexByte(in.Value, 0) >= 0 {
				if err == nil {
					t.Fatalf("NewLabel(%q) accepted", in.Value)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			other := sink.MustLabel("other")
			r := csv.NewReader(strings.NewReader(sink.Labels(l, other)))
			r.TrimLeadingSpace = true
			fields, err := r.Read()
			if err != nil || len(fields) != 2 {
				t.Fatalf("CSV read of %q: %q, %v", sink.Labels(l, other), fields, err)
			}
			if fields[0] != strings.TrimFunc(fields[0], unicode.IsSpace) {
				t.Fatalf("label %q has edge space bd trims", fields[0])
			}
			dec, err := url.PathUnescape(fields[0])
			if err != nil || dec != in.Value {
				t.Errorf("label %q decodes to %q (%v), want %q", fields[0], dec, err, in.Value)
			}
		})
	}
}

func TestHostileLabelsDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, in := range append(hostile.Corpus(), hostile.Input{Name: "pct-literal", Value: "100% , %2C"}) {
		l, err := sink.NewLabel(in.Value)
		if err != nil {
			continue
		}
		if prev, ok := seen[l.String()]; ok {
			t.Errorf("%q and %q share label %q", prev, in.Value, l)
		}
		seen[l.String()] = in.Value
	}
}

func TestHostileComponent(t *testing.T) {
	base := t.TempDir()
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			p, err := sink.Under(base, in.Value)
			if err != nil {
				return // refused
			}
			if filepath.Dir(p) != base || filepath.Base(p) != in.Value {
				t.Errorf("Under(%q) = %q, not one component under base", in.Value, p)
			}
		})
	}
	for _, bad := range []string{"", ".", "..", "a/b", "../x", "a\x00b"} {
		if _, err := sink.Under("/base", bad); err == nil {
			t.Errorf("Under(%q) accepted", bad)
		}
	}
}

func TestTextKeepsPlainTextReadable(t *testing.T) {
	for in, want := range map[string]string{
		"plain words, a colon: fine": "plain words, a colon: fine",
		"it's \"quoted\" (aside)":    "it's \"quoted\" (aside)",
		"wf/main_run.go:12":          "wf/main\\_run.go:12",
		"2. second":                  "2\\. second",
	} {
		if got := sink.Text(in).String(); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
	if !utf8.ValidString(sink.Text("a\xffb").String()) || !bytes.Contains([]byte(sink.Code("x`y").String()), []byte("``")) {
		t.Error("invalid UTF-8 or code fence")
	}
}

// Markdown reads an invalid value the way encoding/json does, byte by byte.
func TestInvalidUTF8MatchesJSON(t *testing.T) {
	for _, in := range hostile.Corpus() {
		b, _ := json.Marshal(in.Value)
		var want string
		_ = json.Unmarshal(b, &want)
		want = strings.ReplaceAll(want, "\x00", "�")
		if got := mdInline(t, sink.Text(in.Value).String()); got != want {
			t.Errorf("%s: Text reads %q, JSON %q", in.Name, got, want)
		}
		if got := hostile.Readable(in.Value); got != want {
			t.Errorf("%s: Readable %q, JSON %q", in.Name, got, want)
		}
	}
	if got := hostile.Readable("a\xff\xfeb"); got != "a��b" {
		t.Errorf("Readable of two bad bytes = %q", got)
	}
}

// pctControls decodes %XX for a space or control byte, the bytes Link
// percent-encodes, and leaves every other % as it is.
func pctControls(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil && (n <= ' ' || n == 0x7f) {
				b.WriteByte(byte(n))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// lines splits s at every line ending Markdown reads: CRLF, CR or LF.
func lines(s string) []string {
	return strings.Split(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), "\n")
}

func TestHostileCommand(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			md, want := string(sink.Command(in.Value)), hostile.Readable(in.Value)
			if !strings.ContainsAny(in.Value, "\r\n") {
				if got := mdInline(t, md); got != want {
					t.Errorf("Command(%q) reads back %q", in.Value, got)
				}
				return
			}
			// A block on lines of its own: a line break, the opener, the
			// content, the closer, a line break.
			ls := lines(md)
			if len(ls) < 4 || ls[0] != "" || ls[len(ls)-1] != "" {
				t.Fatalf("Command(%q) = %q, want a block on its own lines", in.Value, md)
			}
			fence, body := ls[1], ls[2:len(ls)-2]
			if len(fence) < 3 || strings.Trim(fence, "`") != "" || ls[len(ls)-2] != fence {
				t.Fatalf("Command(%q) = %q: opener %q and closer %q", in.Value, md, fence, ls[len(ls)-2])
			}
			for _, l := range body {
				if c := strings.TrimRight(strings.TrimLeft(l, " "), " \t"); len(c) >= len(fence) && strings.Trim(c, "`") == "" {
					t.Fatalf("Command(%q): content line %q closes the block early", in.Value, l)
				}
			}
			// The content is the value and the line break before the closer,
			// read as a Markdown reader reads lines: a final CR is that break.
			wl := lines(want + "\n")
			if got := strings.Join(body, "\n"); got != strings.Join(wl[:len(wl)-1], "\n") {
				t.Errorf("Command(%q) block holds %q", in.Value, got)
			}
		})
	}
}
