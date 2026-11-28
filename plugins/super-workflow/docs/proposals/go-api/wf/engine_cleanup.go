package wf

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

type queuedCleanup struct {
	Deferred int64
	Instance string
	Effect   EffectData
}

// worktreeRemovalBlocked protects the directories issued to still-open units.
// It deliberately does not infer a harness process's current directory.
func (c *cycle) worktreeRemovalBlocked(effect EffectData) (bool, error) {
	if effect.Adapter != "git" || effect.Op != "worktree_remove" {
		return false, nil
	}
	resolved, err := c.resolvePathValues(effect.Args)
	if err != nil {
		return false, err
	}
	path, _ := resolved.(map[string]any)["path"].(string)
	if path == "" {
		return false, nil
	} // adapter validates malformed arguments
	path = cleanupPath(c.workspace(), path)
	for _, id := range c.st.order {
		in := c.st.insts[id]
		live := in.Unit != nil && (in.Unit.State.live() || in.Unit.State == UnitHeld)
		pending := !in.Settled && in.Issued != nil && in.Phase == phaseIssued && (in.Unit == nil || in.Unit.State == UnitPending || (in.Unit.State.terminal() && in.Unit.Token != in.Token))
		if !live && !pending {
			continue
		}
		dir := ""
		if in.Issued != nil {
			dir = in.Issued.Workspace
		}
		if live && in.Issued == nil {
			// A quorum can settle the child for its join while its claimed
			// agent is still running. Recover that attempt's issued directory.
			for i := len(c.recs) - 1; i >= 0; i-- {
				r := c.recs[i]
				if r.Instance != id || r.Kind != JournalInstructionIssued {
					continue
				}
				var issued InstructionIssuedData
				if err := json.Unmarshal(r.Data, &issued); err != nil {
					return false, err
				}
				if issued.Token == in.Unit.Token {
					dir = issued.Workspace
					break
				}
			}
		}
		if dir == "" {
			continue
		}
		rel, err := filepath.Rel(path, cleanupPath(c.workspace(), dir))
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true, nil
		}
	}
	return false, nil
}

func cleanupPath(workspace, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}

// drainCleanups retries durable removals independently of the originating edge.
// Check makes a crash after Apply and before cleanup_applied recoverable without
// reporting success before the adapter has confirmed it.
func (c *cycle) drainCleanups() error {
	if len(c.st.cleanups) == 0 {
		return nil
	}
	for _, rec := range c.recs {
		pending, ok := c.st.cleanups[rec.Seq]
		if !ok {
			continue
		}
		blocked, err := c.worktreeRemovalBlocked(pending.Effect)
		if err != nil {
			return err
		}
		if blocked {
			continue
		}
		args, err := c.resolvePathValues(pending.Effect.Args)
		if err != nil {
			return err
		}
		effect := ExternalEffect{Adapter: pending.Effect.Adapter, Op: pending.Effect.Op, Args: args.(map[string]any), Key: keyOfData(pending.Effect)}
		state, err := c.e.reg.CheckAt(c.workspace(), EffectContext{Run: c.run, Instance: pending.Instance, Sandboxed: c.st.forked, SandboxBin: c.sandboxBin()}, effect)
		if err != nil {
			return fmt.Errorf("cleanup %d check: %w", pending.Deferred, err)
		}
		switch state {
		case EffectDone:
		case EffectNotDone:
			if _, err := c.e.reg.ApplyAt(c.workspace(), EffectContext{Run: c.run, Instance: pending.Instance, State: rec.State, Sandboxed: c.st.forked, SandboxBin: c.sandboxBin()}, effect); err != nil {
				return fmt.Errorf("cleanup %d: %w", pending.Deferred, err)
			}
		default:
			return fmt.Errorf("cleanup %d: removal outcome is unknown; cleanup remains pending", pending.Deferred)
		}
		if err := c.emit(c.st.insts[""], JournalCleanupApplied, 0, CleanupAppliedData{Deferred: pending.Deferred}); err != nil {
			return err
		}
	}
	return nil
}

func (c *cycle) pendingCleanups() []PendingCleanup {
	var out []PendingCleanup
	for _, rec := range c.recs {
		if pending, ok := c.st.cleanups[rec.Seq]; ok {
			out = append(out, PendingCleanup{Deferred: pending.Deferred, Instance: pending.Instance, Effect: pending.Effect.Adapter + "." + pending.Effect.Op})
		}
	}
	return out
}
