package wftest

import (
	"encoding/json"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// DecodeValue decodes a journal payload or nested value with explicit path
// markers resolved against this simulation's roots. Journal returns the original
// durable representation for assertions about markers and ownership.
func (t *Trace) DecodeValue(raw json.RawMessage, target any) error {
	return wf.DecodeRunValue(t.journal, t.runDir, raw, target)
}
