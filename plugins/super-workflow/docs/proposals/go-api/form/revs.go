package form

import (
	"errors"
	"strings"
)

// Revisions have one type per meaning. A CommitID is one full object name.
// A Rev is any expression git resolves to one commit (a branch, a tag, a
// SHA, HEAD~2). A BranchName names a branch and is never read as a
// revision. A RevRange is two Revs, written "a..b" or "a...b"; it is input
// syntax only: a review resolves it once into two recorded CommitIDs, and
// git is given those.

// CommitID is a full lowercase git object name: 40 (SHA-1) or 64 (SHA-256)
// hex digits.
type CommitID struct{}

// ErrNotCommitID is the refusal of a string that is not a full object name.
var ErrNotCommitID = errors.New("not a full lowercase git object name")

func (CommitID) Name() string { return "commit-id" }

func (CommitID) Check(s string) error {
	if len(s) != 40 && len(s) != 64 {
		return ErrNotCommitID
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return ErrNotCommitID
		}
	}
	return nil
}

func (CommitID) Pattern() string { return "^([0-9a-f]{40}|[0-9a-f]{64})$" }

func (CommitID) Samples() []string {
	return []string{"eba2fc6900000000000000000000000000000000", strings.Repeat("0123456789abcdef", 4)}
}

// Rev is one revision expression: not starting with "-" (git would read it
// as an option) or "." (no ref name does), no whitespace or control byte, no ".." (that is a range),
// and no ":" (that names a blob or searches commit messages).
type Rev struct{}

// ErrNotRev is the refusal of a string that is not a single revision.
var ErrNotRev = errors.New("not a single git revision")

// revMax bounds a revision expression.
const revMax = 1000

func (Rev) Name() string { return "rev" }

func (Rev) Check(s string) error {
	if len(s) > revMax || strings.HasPrefix(s, "-") || strings.HasPrefix(s, ".") || strings.Contains(s, "..") || strings.ContainsAny(s, ": \t\n\r\\") || hasControl(s) {
		return ErrNotRev
	}
	return nil
}

func (Rev) Samples() []string {
	return []string{"main", "HEAD~2", "origin/main", "v1.2.0", "eba2fc6900000000000000000000000000000000", "main^{commit}", "@{u}"}
}

// BranchName is a branch name git accepts (git check-ref-format --branch):
// slash-separated elements, none empty or starting with "." or ending with
// ".lock"; no "..", "@{", control byte, space, or any of ~ ^ : ? * [ \; not
// "@", not starting with "-", not ending with "/" or ".".
type BranchName struct{}

// ErrNotBranchName is the refusal of a string that is not a branch name.
var ErrNotBranchName = errors.New("not a git branch name")

func (BranchName) Name() string { return "branch-name" }

func (BranchName) Check(s string) error {
	if len(s) > revMax || s == "@" || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") ||
		strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.ContainsAny(s, " ~^:?*[\\") || hasControl(s) {
		return ErrNotBranchName
	}
	for _, element := range strings.Split(s, "/") {
		if element == "" || strings.HasPrefix(element, ".") || strings.HasSuffix(element, ".lock") {
			return ErrNotBranchName
		}
	}
	return nil
}

func (BranchName) Samples() []string {
	return []string{"main", "super-auto/demo", "task-spw-12.3", "feature/a_b"}
}

// RevRangeText is a revision range as written: "base..head" or
// "base...head", each side a Rev. ParseRevRange reads it into a RevRange.
type RevRangeText struct{}

// ErrNotRevRange is the refusal of a string that is not a revision range.
var ErrNotRevRange = errors.New(`not a revision range "base..head" or "base...head"`)

func (RevRangeText) Name() string { return "rev-range" }

func (RevRangeText) Check(s string) error {
	_, err := splitRange(s)
	return err
}

func (RevRangeText) Samples() []string {
	return []string{"main..feature", "main...feature", "v1.0..HEAD"}
}

// RevRange is a parsed revision range. Symmetric is the three-dot form: the
// range starts at the merge base of Base and Head.
type RevRange struct {
	Base, Head Of[Rev]
	Symmetric  bool
}

// ParseRevRange reads "base..head" or "base...head". Both sides must be
// present and each must be a Rev.
func ParseRevRange(s string) (RevRange, error) {
	if _, err := Parse[RevRangeText](s); err != nil {
		return RevRange{}, err
	}
	r, _ := splitRange(s)
	return r, nil
}

// String writes the range in its own syntax.
func (r RevRange) String() string {
	sep := ".."
	if r.Symmetric {
		sep = "..."
	}
	return r.Base.String() + sep + r.Head.String()
}

func splitRange(s string) (RevRange, error) {
	i := strings.Index(s, "..")
	if i < 0 {
		return RevRange{}, ErrNotRevRange
	}
	base, head, symmetric := s[:i], s[i+2:], false
	if strings.HasPrefix(head, ".") {
		head, symmetric = head[1:], true
	}
	b, berr := Parse[Rev](base)
	h, herr := Parse[Rev](head)
	if berr != nil || herr != nil {
		return RevRange{}, ErrNotRevRange
	}
	return RevRange{Base: b, Head: h, Symmetric: symmetric}, nil
}

func init() {
	Register[CommitID]()
	Register[Rev]()
	Register[BranchName]()
	Register[RevRangeText]()
}
