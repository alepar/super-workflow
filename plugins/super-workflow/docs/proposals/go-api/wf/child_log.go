package wf

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// childErrLog is the file a child of claim writes its stderr to:
// <state dir>/child-logs/<run>/wf-<claim>.stderr. It is the one place that
// layout is spelled; the base name is pi's session id for the claim plus
// .stderr, which pi's launch grammar checks (a test pins the two agree).
// "" when the engine has no state dir: a library engine with no StateRoot
// records no stderr file, checks no path and creates no directory (its pi
// offers end 2>/dev/null, which pi's launch grammar refuses; the CLI always
// has a state dir).
func (c *cycle) childErrLog(claim string) string {
	if c.e.opts.StateRoot == "" {
		return ""
	}
	return filepath.Join(c.e.opts.StateRoot, "child-logs", string(c.run), "wf-"+claim+".stderr")
}

// errLogPathOK: absolute, and only letters, digits and / . _ - (the launch
// grammar's literal path).
func errLogPathOK(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '/', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// errNotRealDir marks a child-log path component that is not a real
// directory (a symlink, or not a directory): the hand-out is refused.
var errNotRealDir = errors.New("not a real directory")

// makeChildLogDir creates dir, which lies under the state dir state, one
// component at a time below state, and requires each to be a real directory
// (Lstat: a symlink is refused, never followed), so the stderr file the
// child's shell writes and the engine reads stays under the state dir.
func makeChildLogDir(state, dir string) error {
	rel, err := filepath.Rel(state, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s is not under the state dir %s", dir, state)
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		return err
	}
	cur := state
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		if err := os.Mkdir(cur, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		st, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return fmt.Errorf("%s: %w", cur, errNotRealDir)
		}
	}
	return nil
}

// Limits of the stderr excerpt a not-started offer quotes: its lines, the
// bytes of each, and the bytes read from the end of the file.
const (
	errLogExcerptLines = 3
	errLogExcerptBytes = 200
	errLogTailBytes    = 4 << 10
)

// errLogExcerpt is the last non-empty lines (at most 3) of the stderr file at
// path, each one line with its control characters escaped (escapeControl)
// and then cut to at most 200 bytes, cut ones ending in "…"; nil when the
// file is missing, empty, not a regular file, a symlink, or in a directory
// that is a symlink. Only the last errLogTailBytes are read (errLogTail).
func errLogExcerpt(path string) []string {
	if st, err := os.Lstat(filepath.Dir(path)); err != nil || !st.IsDir() {
		return nil
	}
	b, ok := errLogTail(path, errLogTailBytes)
	if !ok {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > errLogExcerptLines {
		lines = lines[len(lines)-errLogExcerptLines:]
	}
	for i, l := range lines {
		lines[i] = cutLine(escapeControl(l), errLogExcerptBytes)
	}
	return lines
}

// errLogTail is at most the last limit bytes of the regular file at path,
// opened without following a symlink (O_NOFOLLOW) and without blocking on a
// FIFO; when the read starts inside the file, the partial first line is
// dropped. false when the file cannot be opened or is not a regular file.
func errLogTail(path string, limit int64) ([]byte, bool) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	off := max(0, st.Size()-limit)
	b := make([]byte, st.Size()-off)
	n, err := f.ReadAt(b, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false
	}
	b = b[:n]
	if off > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return b, true
}

// escapeControl is s with every control character (C0, DEL and C1, ESC
// among them) and every invalid UTF-8 byte written as a visible escape
// (\x1b, \u009b), so a quoted stderr line cannot drive the terminal or the
// layout of the text it is printed in.
func escapeControl(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, "\\x%02x", s[i])
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		case r >= 0x80 && r < 0xa0:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// cutLine is l if it fits max bytes, else its longest whole-rune prefix that,
// with the 3-byte "…", fits.
func cutLine(l string, max int) string {
	const ell = "…"
	if len(l) <= max {
		return l
	}
	n := max - len(ell)
	for n > 0 && !utf8.RuneStart(l[n]) {
		n--
	}
	return l[:n] + ell
}

// startLogOf is the one StartLog assertion: a's StartLog, or nil.
func startLogOf(a harness.Adapter) harness.StartLog {
	sl, _ := a.(harness.StartLog)
	return sl
}
