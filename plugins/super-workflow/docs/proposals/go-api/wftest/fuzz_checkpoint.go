package wftest

import (
	"encoding/json"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// livenessCheckpointEligible selects prefixes before hostile actions have spent
// an active visit's exhaustion-recovery reserve. It does not judge the suffix:
// once all-good begins, a missing repair route must still fail liveness.
func (f *fuzzRun) livenessCheckpointEligible(current wf.RunStatus) bool {
	return recoveryCheckpointEligible(current, f.runner.trace().Journal())
}

// recoveryCheckpointEligible follows only the journal facts that spend recovery
// reserves: a Refused recovery halt followed by Retried. Ended-child (F1) offers
// and answers do not themselves spend this reserve; RecoveryView.Restarts is
// absent on those offers, so its zero value cannot establish exhaustion.
func recoveryCheckpointEligible(current wf.RunStatus, records []wf.JournalRecord) bool {
	type visit struct{ offered, spent bool }
	type stateKey struct{ instance, state string }
	visits := map[string]visit{}
	restarts := map[stateKey]int{}
	settled := map[string]bool{}
	for _, record := range records {
		v := visits[record.Instance]
		switch record.Kind {
		case wf.JournalRefused, wf.JournalRouteOffCandidates, wf.JournalPromptOffCandidates:
			var refused wf.RefusedData
			if json.Unmarshal(record.Data, &refused) == nil {
				v.offered = refused.Halt && refused.Recovery
			}
		case wf.JournalEffectSettled:
			var settlement wf.EffectSettledData
			if json.Unmarshal(record.Data, &settlement) == nil && settlement.Refused != nil {
				v.offered = settlement.Refused.Halt && settlement.Refused.Recovery
			}
		case wf.JournalRetried:
			var retried wf.RetriedData
			if v.offered && json.Unmarshal(record.Data, &retried) == nil {
				v.offered, v.spent = false, true
				if retried.Answer != wf.RetryStop {
					restarts[stateKey{record.Instance, record.State}]++
				}
			}
		case wf.JournalTransition:
			// Every arrival is a new visit, including a self-loop. Run-wide restart
			// counts stay intact, just as the engine keeps them across arrivals.
			v = visit{}
		case wf.JournalChildSettled:
			var child wf.ChildSettledData
			if json.Unmarshal(record.Data, &child) == nil {
				settled[child.ID] = true
			}
		}
		visits[record.Instance] = v
	}
	for _, instance := range current.Instances {
		if settled[instance.ID] || instance.Phase == "done" || instance.Phase == "budget_stopped" {
			continue
		}
		if visits[instance.ID].spent || restarts[stateKey{instance.ID, instance.State}] >= wf.RecoveryRestarts {
			return false
		}
	}
	return true
}
