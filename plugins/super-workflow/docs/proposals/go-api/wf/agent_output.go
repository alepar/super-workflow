package wf

import (
	"encoding/json"
	"errors"
	"reflect"
)

// checkedAgentOutput is the shared shape guard for submission and both
// calibration probes. The stage preserves live refusal fingerprints.
func checkedAgentOutput[O any](raw json.RawMessage, d *Def) (out O, stage string, err error) {
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, "decode", err
	}
	if err = validateEnums(reflect.ValueOf(out), d.enums, "output", raw); err != nil {
		return out, "enum", err
	}
	if err = validatePatterns(reflect.ValueOf(out), "output"); err != nil {
		var long *maxlenError
		if errors.As(err, &long) {
			return out, "maxlen", err
		}
		return out, "pattern", err
	}
	return out, "", nil
}
