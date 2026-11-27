package wftest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func calibrateRecording(view wf.StateView, entry wf.CorpusEntry, cfg CalibrationConfig, report *CalibrationReport) {
	label := fmt.Sprintf("%s recording %s seq %d", entry.State, entry.Run, entry.Seq)
	evaluate := func(raw json.RawMessage, suffix string, mutation bool) (ok bool) {
		report.Samples++
		defer func() {
			if p := recover(); p != nil {
				report.addReal(fmt.Sprintf("%s %s: check panic: %v", label, suffix, p))
			}
		}()
		refused, err := view.CheckRecordedOutput(raw, entry.Context)
		if err != nil {
			message := fmt.Sprintf("%s %s: %v", label, suffix, err)
			report.NotChecked = append(report.NotChecked, message)
			// A changed mutation may consume a different context branch. Report
			// it as not checked, not evidence that a check rejected that mutation.
			if !mutation {
				if errors.Is(err, wf.ErrContextUnknown) && unknownBeforeChecks(view, entry.Context) {
					report.ContextUnknown = append(report.ContextUnknown, message)
				} else {
					report.addReal(message)
				}
			}
			return false
		}
		if mutation {
			if reason, decode := refused["$decode"]; decode && len(refused) == 1 {
				report.TypeRejected = append(report.TypeRejected, label+" "+suffix+": "+reason)
				report.NotChecked = append(report.NotChecked, label+" "+suffix)
			} else if len(refused) == 0 {
				report.NotChecked = append(report.NotChecked, label+" "+suffix)
			}
		} else if len(refused) > 0 {
			report.addReal(fmt.Sprintf("%s %s: known-good result refused: %v", label, suffix, refused))
		}
		return true
	}
	if !evaluate(entry.Output, "", false) {
		return
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(entry.Output))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return
	}
	evaluate(orderedJSON(value, true), "parser variant", false)
	f := cfg.Rewrites[entry.State]
	if f == nil {
		f = cfg.Rewrites[view.Name]
	}
	if f != nil {
		for i, raw := range f(append(json.RawMessage(nil), entry.Output...)) {
			evaluate(raw, fmt.Sprintf("rewrite %d", i), false)
		}
	}
	for i, raw := range fieldMutations(value) {
		evaluate(raw, fmt.Sprintf("mutation %d: %s", i, raw), true)
	}
}

// Only pre-evaluation context gaps are advisory. A later replay error may
// conceal a refusal already observed by an earlier check, so remains strict.
func unknownBeforeChecks(view wf.StateView, context *wf.CheckContexts) bool {
	if context == nil || context.Version != 1 || len(context.Checks) != len(view.Checks) {
		return true
	}
	for i, check := range context.Checks {
		if check.Name != view.Checks[i] || len(check.Incomplete) > 0 {
			return true
		}
	}
	return false
}
