package wf

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// scratchName is the builtin placeholder for the scratch directory of the
// instance's current state.
const scratchName = "scratch"

// scratchDir is the {scratch} binding: <RunDir(run)>/scratch/<scratchLeaf>,
// created when it is bound, so a step never makes it itself (a mkdir is a
// command an agent's permissions may deny).
//
// Owns: the directory's identity (the qualified state and a hash of the
// instance id: two instances never share one, and attempts and revisits of
// the same state in the same instance always get the same) and its creation.
// Does not: clean it (it lives until the run directory is removed; nothing
// removes it automatically), or carry it into a snapshot: it lies beside the
// run's records, outside the run data a checkpoint captures, so a fork never
// copies it and binds its own. MkdirAll follows a link already inside this
// engine-owned tree; the tree is the engine's, and the path is built
// lexically from a slug and a digest, so nothing a definition binds can
// traverse out of it. Without a RunDir there is no scratch, and binding
// {scratch} is an error, never a relative path.
func (c *cycle) scratchDir(in *instance) (string, error) {
	dir := c.resultDir()
	if dir == "" {
		return "", errors.New("{scratch} needs the engine's RunDir option, which is unset")
	}
	abs, err := filepath.Abs(filepath.Join(dir, "scratch", scratchLeaf(c.st.qualified(in), in.ID)))
	if err != nil {
		return "", err
	}
	return abs, os.MkdirAll(abs, 0o755)
}

// scratchLeaf is a scratch directory's name: the qualified state as a file
// name slug (a readable prefix only, truncated), then a 128-bit digest of the
// full qualified state and the instance id, NUL-separated (neither holds a
// NUL), so the identity alone tells two directories apart.
func scratchLeaf(qualified, instance string) string {
	b := []byte(qualified)
	for i, ch := range b {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-') {
			b[i] = '-'
		}
	}
	if len(b) > slugMax {
		b = b[:slugMax]
	}
	sum := sha256.Sum256([]byte(qualified + "\x00" + instance))
	return string(b) + "-" + hex.EncodeToString(sum[:16])
}
