// Command reanchor re-anchors the Go excerpts of a proposal document after the
// compiled sources moved.
//
//	reanchor [--write] [--report] [--root dir] --pkg dir DOC.md
//
// For each "<!-- src: path#Lstart-Lend -->" annotation followed by a go fence,
// it looks for the fence text in the file as it is now. A unique match gives
// the block's new range (rewritten with --write, together with the "Lines
// a-b:" label above it); a block whose text no longer occurs exactly once is
// listed as stale and left for a human to refresh. --report also lists the
// top-level declarations of --pkg (required) that no excerpt covers, using excerptcheck.
// The exit status is 1 while anything is left to do.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/tools/excerptcheck"
)

var (
	srcRe   = regexp.MustCompile(`^<!-- src: (\S+)#L(\d+)-L(\d+) -->$`)
	labelRe = regexp.MustCompile(`^Lines \d+-\d+:`)
)

func main() {
	write := flag.Bool("write", false, "rewrite the ranges of moved blocks")
	report := flag.Bool("report", false, "list top-level declarations no excerpt covers")
	root := flag.String("root", "", "repository root that src paths are relative to (default: the document's directory)")
	pkg := flag.String("pkg", "", "package directory, relative to the root, checked by --report (required)")
	flag.Parse()
	if flag.NArg() != 1 || *pkg == "" {
		fmt.Fprintln(os.Stderr, "usage: reanchor [--write] [--report] [--root dir] --pkg dir DOC.md")
		os.Exit(2)
	}
	doc := flag.Arg(0)
	if *root == "" {
		// the document sits at the repository root; run resolves relative paths against it
		*root, doc = filepath.Dir(doc), filepath.Base(doc)
	}
	n, err := run(os.Stdout, *root, doc, *pkg, *write, *report)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reanchor:", err)
		os.Exit(2)
	}
	if n > 0 {
		os.Exit(1)
	}
}

func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n"), nil
}

func trimBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// find returns the 1-based start lines at which body occurs in src.
func find(src, body []string) []int {
	var at []int
	for i := 0; i+len(body) <= len(src); i++ {
		ok := true
		for j, l := range body {
			if src[i+j] != l {
				ok = false
				break
			}
		}
		if ok {
			at = append(at, i+1)
		}
	}
	return at
}

// run re-anchors doc, prints one line per moved or stale block (and per
// uncovered declaration with report), and returns how many items remain to do:
// stale blocks, moves not written, and uncovered declarations.
func run(out io.Writer, root, doc, pkgDir string, write, report bool) (int, error) {
	docPath := doc
	if !filepath.IsAbs(docPath) {
		docPath = filepath.Join(root, doc)
	}
	lines, err := readLines(docPath)
	if err != nil {
		return 0, err
	}
	pending, changed := 0, false
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "```go") || strings.TrimSpace(lines[i][5:]) != "" {
			continue
		}
		fence := i
		var body []string
		for i++; i < len(lines) && !strings.HasPrefix(lines[i], "```"); i++ {
			body = append(body, lines[i])
		}
		if fence == 0 {
			continue
		}
		m := srcRe.FindStringSubmatch(strings.TrimSpace(lines[fence-1]))
		if m == nil {
			continue
		}
		body = trimBlank(body)
		file := m[1]
		start, _ := strconv.Atoi(m[2])
		end, _ := strconv.Atoi(m[3])
		src, err := readLines(filepath.Join(root, file))
		if err != nil {
			fmt.Fprintf(out, "stale: %s#L%d-L%d (doc line %d): %v\n", file, start, end, fence+1, err)
			pending++
			continue
		}
		if start >= 1 && end >= start && end <= len(src) && strings.Join(trimBlank(src[start-1:end]), "\n") == strings.Join(body, "\n") {
			continue
		}
		at := find(src, body)
		if len(body) == 0 || len(at) != 1 {
			why := "text no longer in the file"
			if len(at) > 1 {
				why = fmt.Sprintf("text occurs %d times", len(at))
			}
			fmt.Fprintf(out, "stale: %s#L%d-L%d (doc line %d): %s\n", file, start, end, fence+1, why)
			pending++
			continue
		}
		ns, ne := at[0], at[0]+len(body)-1
		fmt.Fprintf(out, "moved: %s#L%d-L%d -> L%d-L%d (doc line %d)\n", file, start, end, ns, ne, fence+1)
		if !write {
			pending++
			continue
		}
		lines[fence-1] = fmt.Sprintf("<!-- src: %s#L%d-L%d -->", file, ns, ne)
		if fence >= 3 && labelRe.MatchString(lines[fence-3]) {
			lines[fence-3] = labelRe.ReplaceAllString(lines[fence-3], fmt.Sprintf("Lines %d-%d:", ns, ne))
		}
		changed = true
	}
	if changed {
		if err := os.WriteFile(docPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			return pending, err
		}
	}
	if report {
		for _, u := range excerptcheck.Uncovered(root, doc, pkgDir) {
			fmt.Fprintf(out, "uncovered: %s\n", u)
			pending++
		}
	}
	return pending, nil
}
