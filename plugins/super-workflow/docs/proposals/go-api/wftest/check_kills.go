package wftest

import "fmt"

// killReach remembers, per agent state, the configuration of the first
// earlier Check run that issued it. A run is the same up to its first
// instruction at a state whatever is injected there, so that configuration
// with a kill added reaches the state again: a repair route no all-good run
// takes is still swept.
type killReach map[string]FuzzConfig

func (k killReach) add(result *FuzzResult) {
	if result.config.Kill != nil {
		return
	}
	for _, state := range result.Reached {
		if _, ok := k[state]; !ok {
			k[state] = result.config
		}
	}
}

// killState is one agent state of the sweep: the configurations that may
// reach it, the one that did, and whether none does.
type killState struct {
	key       string
	starts    []FuzzConfig
	found     int
	unreached bool
}

// checkKillPaths injects every FuzzKillKinds entry at every agent state of
// the definition tree and checks the run recovers with the coordinator's and
// operator's verbs. A state is reached by an all-good seed or, failing that,
// by the earlier run reach recorded for it. The sweep is breadth first: each
// pass gives every state one kind, rotated so the first pass already spans
// the kinds, and a budget that ends the sweep early has still touched every
// state. It returns the runs made; unreached gets each state no run reaches.
func checkKillPaths(factory func() *Sim, cfg CheckConfig, reach killReach, record func(*FuzzResult), unreached func(string)) int {
	good := func(seed int) FuzzConfig {
		return FuzzConfig{Seed: int64(seed), MaxSteps: cfg.MaxSteps, Valid: cfg.Progress, Progress: cfg.Progress, Artifacts: cfg.Artifacts, Liveness: true}
	}
	var states []*killState
	for _, key := range agentStateKeys(factory().def) {
		// An all-good seed first, then the earlier run that reached the
		// state, then the other all-good seeds.
		s := &killState{key: key, starts: []FuzzConfig{good(0)}, found: -1}
		if earlier, ok := reach[key]; ok {
			s.starts = append(s.starts, earlier)
		}
		for seed := 1; seed < cfg.Seeds; seed++ {
			s.starts = append(s.starts, good(seed))
		}
		states = append(states, s)
	}
	runs := 0
	for pass := range FuzzKillKinds {
		for i, s := range states {
			if s.unreached {
				continue
			}
			kind := FuzzKillKinds[(i+pass)%len(FuzzKillKinds)]
			try := func(start int) (reached, refused bool) {
				c := s.starts[start]
				c.Kill = &FuzzKill{State: s.key, Kind: kind}
				result := cfg.run(factory, fmt.Sprintf("kill %s %s", s.key, kind), c)
				if result == nil {
					return false, true
				}
				record(result)
				runs++
				return result.KillInjected, false
			}
			// The start that reached the state for one kind reaches it
			// for all: the run is the same up to the kill.
			if s.found >= 0 {
				if reached, refused := try(s.found); reached || refused {
					continue
				}
			}
			refused := false
			for start := range s.starts {
				if start == s.found {
					continue
				}
				reached, r := try(start)
				if r {
					refused = true
					break
				}
				if reached {
					s.found = start
					break
				}
			}
			if s.found < 0 && !refused {
				s.unreached = true
				unreached(s.key)
			}
		}
	}
	return runs
}
