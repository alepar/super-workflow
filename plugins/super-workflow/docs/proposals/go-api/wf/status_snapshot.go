package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// engineAdapter is the adapter name of the effects the engine serves itself.
const engineAdapter = "wf"

// StatusSnapshot writes the run's status to path, the document wf status
// --json prints, so a workflow need not run its own binary's wf status nor
// find the state dir from {records} (F4). With predecessors, a successor's
// document gains "Predecessors": each predecessor's document, newest first;
// one whose status cannot be read is {"Run": <id>, "Unreadable": true} and
// ends the chain. A relative path is the workspace's. A file already at path
// (a predecessor's, carried in its records) is removed first and the new one
// written whole, so a failed snapshot leaves none behind it.
//
// Idempotent: it writes the status as of its application, every time it is
// applied (Check answers not done).
func StatusSnapshot(key Key, path string, predecessors bool) ExternalEffect {
	return ExternalEffect{Adapter: engineAdapter, Op: "status_snapshot", Args: map[string]any{"path": path, "predecessors": predecessors}, Key: key}
}

// engineRuntime serves the engine's own effects. A run's status is read
// without its lock, which the cycle applying the effect holds: on a journal
// with no lock-free read the effect is unavailable, never a wait.
type engineRuntime struct{ e *Engine }

func (engineRuntime) Ops() []string { return []string{engineAdapter + ".status_snapshot"} }

func (engineRuntime) Read(string, ObservationDesc) (json.RawMessage, error) {
	return nil, fmt.Errorf("%w: the engine serves no observation", ErrUnavailable)
}

func (r engineRuntime) Apply(string, ExternalEffect) (json.RawMessage, error) {
	return nil, fmt.Errorf("%w: status_snapshot needs its run", ErrUnavailable)
}

func (engineRuntime) Check(string, ExternalEffect) (EffectStatus, error) { return EffectNotDone, nil }

// CheckAt: never done, so a recovery writes the snapshot again.
func (engineRuntime) CheckAt(string, EffectContext, ExternalEffect) (EffectStatus, error) {
	return EffectNotDone, nil
}

// ApplyAt writes at.Run's status snapshot.
func (r engineRuntime) ApplyAt(workspace string, at EffectContext, eff ExternalEffect) (json.RawMessage, error) {
	path, _ := eff.Args["path"].(string)
	if path == "" {
		return nil, errors.New("status_snapshot: no path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	preds, _ := eff.Args["predecessors"].(bool)
	// An earlier file at path (a predecessor's, carried in its records) goes
	// first, so a failed snapshot never reads as this run's.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	doc, err := r.e.statusSnapshot(at.Run, preds)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(doc, '\n')); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"path": path})
}

// statusSnapshot is run's status document, as wf status --json prints it,
// with its predecessors' when preds is set. Every read is strictly lock-free
// (status strict), through every journal wrapper: the applying cycle holds
// the run's lock, and a read that would need it leaves the snapshot
// unavailable instead.
func (e *Engine) statusSnapshot(run RunID, preds bool) ([]byte, error) {
	rs, err := e.status(run, true)
	if err != nil {
		return nil, fmt.Errorf("%w: status_snapshot: %v", ErrUnavailable, err)
	}
	raw, err := json.Marshal(rs)
	if err != nil {
		return nil, err
	}
	if !preds || rs.Predecessor == "" {
		return raw, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var chain []json.RawMessage
	seen := map[RunID]bool{run: true}
	for pred := rs.Predecessor; pred != "" && !seen[pred]; {
		seen[pred] = true
		ps, err := e.status(pred, true)
		if err != nil {
			item, _ := json.Marshal(map[string]any{"Run": pred, "Unreadable": true})
			chain = append(chain, item)
			break
		}
		item, err := json.Marshal(ps)
		if err != nil {
			return nil, err
		}
		chain = append(chain, item)
		pred = ps.Predecessor
	}
	if doc["Predecessors"], err = json.Marshal(chain); err != nil {
		return nil, err
	}
	return json.Marshal(doc)
}

// withRuntime is a copy of a with rt's ops added, a's own taking precedence:
// the engine's own runtime joins the client's registry without changing it.
// A registry with a "*" runtime (a fake world keyed by descriptor, wftest)
// serves every op itself, the engine's too.
func (a *Adapters) withRuntime(rt AdapterRuntime) *Adapters {
	out := &Adapters{ops: make(map[string]AdapterRuntime, len(a.ops)+1)}
	for k, v := range a.ops {
		out.ops[k] = v
	}
	if _, fake := out.ops["*"]; fake {
		return out
	}
	for _, op := range rt.Ops() {
		if _, ok := out.ops[op]; !ok {
			out.ops[op] = rt
		}
	}
	return out
}
