package wf

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// DecodeRunValue decodes a recorded value for operational inspection, resolving
// explicit path markers against the run's recorded workspace and supplied run
// directory. Pass decoded journal records (for example from ReadJournal).
// The records and raw value remain unchanged. Use ordinary json.Unmarshal when
// inspecting the durable marker representation itself.
func DecodeRunValue(records []JournalRecord, runDir string, raw json.RawMessage, target any) error {
	if !json.Valid(raw) {
		return fmt.Errorf("decode run value: invalid JSON")
	}
	if len(records) == 0 || records[0].Kind != JournalRunStarted {
		return fmt.Errorf("decode run value: missing run_started")
	}
	var started RunStartedData
	if err := json.Unmarshal(records[0].Data, &started); err != nil {
		return err
	}
	c := &cycle{run: records[0].Run, e: &Engine{opts: EngineOptions{RunDir: func(RunID) string { return runDir }}}, st: &runState{started: &started}}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	value, err := c.resolvePathValues(value)
	if err != nil {
		return err
	}
	resolved, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(resolved, target)
}
