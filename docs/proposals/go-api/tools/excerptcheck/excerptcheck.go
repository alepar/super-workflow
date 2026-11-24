// Package excerptcheck verifies that fenced code excerpts in the proposal
// documents are verbatim copies of the compiled sources and captured output.
//
// A ```go fence must be preceded immediately by
//
//	<!-- src: <repo-relative path>#L<start>-L<end> -->
//
// and its body must equal those source lines. A ```text fence preceded by
//
//	<!-- output: <section id> -->
//
// must equal that section of OUTPUT.txt (sections start at "#### <id>" lines).
package excerptcheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Mismatch is one failed check in a document.
type Mismatch struct {
	Doc    string
	Line   int
	Kind   string // no-src, bad-src, excerpt-mismatch, output-mismatch, unknown-section
	Detail string
}

var (
	srcRe    = regexp.MustCompile(`^<!-- src: (\S+)#L(\d+)-L(\d+) -->$`)
	outputRe = regexp.MustCompile(`^<!-- output: (.+?) -->$`)
)

// ParseOutput splits OUTPUT.txt into sections keyed by the text after "#### ".
// Bodies have trailing blank lines removed.
func ParseOutput(text string) map[string]string {
	out := map[string]string{}
	var id string
	var body []string
	flush := func() {
		if id != "" {
			out[id] = strings.TrimRight(strings.Join(body, "\n"), "\n")
		}
	}
	for _, l := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(l, "#### "); ok {
			flush()
			id, body = rest, nil
			continue
		}
		body = append(body, l)
	}
	flush()
	return out
}

// resolve returns docPath as given when absolute, else joined to repoRoot.
func resolve(repoRoot, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(repoRoot, p)
}

func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n"), nil
}

// CheckDoc checks every go fence and annotated output fence of docPath.
func CheckDoc(repoRoot, docPath string, output map[string]string) []Mismatch {
	lines, err := readLines(resolve(repoRoot, docPath))
	if err != nil {
		return []Mismatch{{Doc: docPath, Kind: "bad-src", Detail: err.Error()}}
	}
	var ms []Mismatch
	add := func(line int, kind, format string, a ...any) {
		ms = append(ms, Mismatch{Doc: docPath, Line: line, Kind: kind, Detail: fmt.Sprintf(format, a...)})
	}
	for i := 0; i < len(lines); i++ {
		lang := ""
		switch {
		case strings.HasPrefix(lines[i], "```go") && strings.TrimSpace(lines[i][5:]) == "":
			lang = "go"
		case strings.HasPrefix(lines[i], "```text") && strings.TrimSpace(lines[i][7:]) == "":
			lang = "text"
		case strings.HasPrefix(lines[i], "```"):
			// other fence: skip to its end so its body is not misread
			for i++; i < len(lines) && !strings.HasPrefix(lines[i], "```"); i++ {
			}
			continue
		default:
			continue
		}
		fence := i + 1
		var body []string
		for i++; i < len(lines) && !strings.HasPrefix(lines[i], "```"); i++ {
			body = append(body, lines[i])
		}
		prev := ""
		if fence >= 2 {
			prev = strings.TrimSpace(lines[fence-2])
		}
		got := strings.TrimRight(strings.Join(body, "\n"), "\n")
		if lang == "go" {
			m := srcRe.FindStringSubmatch(prev)
			if m == nil {
				add(fence, "no-src", "go fence has no <!-- src: path#Lstart-Lend --> line before it")
				continue
			}
			start, _ := strconv.Atoi(m[2])
			end, _ := strconv.Atoi(m[3])
			src, err := readLines(resolve(repoRoot, m[1]))
			if err != nil || start < 1 || end < start || end > len(src) {
				add(fence, "bad-src", "cannot use %s#L%d-L%d (file lines: %d, err: %v)", m[1], start, end, len(src), err)
				continue
			}
			want := strings.TrimRight(strings.Join(src[start-1:end], "\n"), "\n")
			if got != want {
				add(fence, "excerpt-mismatch", "excerpt differs from %s#L%d-L%d: %s", m[1], start, end, firstDiff(want, got))
			}
			continue
		}
		m := outputRe.FindStringSubmatch(prev)
		if m == nil {
			continue
		}
		want, ok := output[m[1]]
		if !ok {
			add(fence, "unknown-section", "OUTPUT.txt has no section %q", m[1])
			continue
		}
		if got != strings.TrimRight(want, "\n") {
			add(fence, "output-mismatch", "block differs from OUTPUT.txt section %q: %s", m[1], firstDiff(want, got))
		}
	}
	return ms
}

func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if i >= len(w) || i >= len(g) || a != b {
			return fmt.Sprintf("line %d: source %q, document %q", i+1, a, b)
		}
	}
	return "identical"
}

type span struct{ start, end int }

// Uncovered lists top-level declarations of the non-test .go files in pkgDir
// that no src excerpt of docPath covers, as "file.go:Name".
func Uncovered(repoRoot, docPath, pkgDir string) []string {
	lines, err := readLines(resolve(repoRoot, docPath))
	if err != nil {
		return []string{"cannot read " + docPath + ": " + err.Error()}
	}
	covered := map[string][]span{} // repo-relative path -> ranges
	for _, l := range lines {
		if m := srcRe.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			s, _ := strconv.Atoi(m[2])
			e, _ := strconv.Atoi(m[3])
			covered[filepath.ToSlash(filepath.Clean(m[1]))] = append(covered[filepath.ToSlash(filepath.Clean(m[1]))], span{s, e})
		}
	}
	dir := resolve(repoRoot, pkgDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{"cannot read " + pkgDir + ": " + err.Error()}
	}
	var out []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			out = append(out, name+": "+err.Error())
			continue
		}
		rel := filepath.ToSlash(filepath.Join(pkgDir, name))
		for _, d := range f.Decls {
			start, end := fset.Position(d.Pos()).Line, fset.Position(d.End()).Line
			in := false
			for _, s := range covered[filepath.ToSlash(filepath.Clean(rel))] {
				if s.start <= start && end <= s.end {
					in = true
					break
				}
			}
			if !in {
				out = append(out, name+":"+declName(d))
			}
		}
	}
	sort.Strings(out)
	return out
}

func declName(d ast.Decl) string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) > 0 {
			return recvName(d.Recv.List[0].Type) + "." + d.Name.Name
		}
		return d.Name.Name
	case *ast.GenDecl:
		var names []string
		for _, s := range d.Specs {
			switch s := s.(type) {
			case *ast.TypeSpec:
				names = append(names, s.Name.Name)
			case *ast.ValueSpec:
				for _, n := range s.Names {
					names = append(names, n.Name)
				}
			case *ast.ImportSpec:
				names = append(names, "import")
			}
		}
		return strings.Join(names, ",")
	}
	return "?"
}

func recvName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return recvName(e.X)
	case *ast.IndexExpr:
		return recvName(e.X)
	case *ast.IndexListExpr:
		return recvName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return "?"
}
