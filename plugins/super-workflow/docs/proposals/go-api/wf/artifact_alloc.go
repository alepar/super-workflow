package wf

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Allocated artifact paths (artifact-identity A1-A3).
//
// Owns: the rendered path of an artifact declared without a template:
// <records>/artifacts/<instance slug>/<name>, where the slug is the producing
// instance's id, state and, from its second visit on, the visit (a state a
// loop revisits writes each round's artifact apart), filesystem-safe, with a
// hash of the full id so ids that slug alike stay apart. One writer per path
// follows from that identity plus the lint that a definition declares each
// allocated name once (artifact-name).
// Does not: write the artifact (its producer does), publish human-facing copies
// (the client's phase that owns them), or allocate a templated path (bound
// from params, as before).

// artifactDir is the directory of the artifacts the state producer writes in
// instance in.
func (c *cycle) artifactDir(in *instance, producer *node) (string, error) {
	rec, err := c.recordsDir()
	if err != nil {
		return "", err
	}
	id := in.ID + "/" + producer.name
	if v := in.Visits[producer.name]; v > 1 {
		id += fmt.Sprintf("#%d", v)
	}
	b := []byte(strings.TrimPrefix(id, "/"))
	for i, ch := range b {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-') {
			b[i] = '-'
		}
	}
	if len(b) > slugMax {
		b = b[:slugMax]
	}
	sum := sha256.Sum256([]byte(id))
	dir := filepath.Join(rec, "artifacts", string(b)+"-"+hex.EncodeToString(sum[:6]))
	return dir, os.MkdirAll(dir, 0o755)
}

// artifactPath is a's path as instance in sees it: the path the producer's
// submission recorded, else the allocated path (a declared without a
// template), else its template bound in in's scope.
func (c *cycle) artifactPath(in *instance, a *artifactData) (string, error) {
	if ref, ok := in.Artifacts[a.name]; ok {
		return ref.Path, nil
	}
	if a.path != "" {
		return c.scope(in).bindString(a.path)
	}
	return c.allocatedPath(in, a)
}

// allocatedPath is the allocated path of a (declared without a template) in
// instance in, ignoring what a submission recorded.
func (c *cycle) allocatedPath(in *instance, a *artifactData) (string, error) {
	dir, err := c.artifactDir(in, a.producer)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, a.name), nil
}

// allocatedOf lists the allocated artifacts state n produces, in declaration
// order.
func allocatedOf(n *node) []*artifactData {
	var out []*artifactData
	for _, a := range n.def.artifacts {
		if a.producer == n && a.path == "" && a.via == nil {
			out = append(out, a)
		}
	}
	return out
}

// artifactByName is the artifact of def d named name.
func artifactByName(d *Def, name string) *artifactData {
	for _, a := range d.artifacts {
		if a.name == name {
			return a
		}
	}
	return nil
}
