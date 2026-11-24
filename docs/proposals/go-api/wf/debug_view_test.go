package wf

import (
	"encoding/json"
	"testing"
)

func TestJournalViewNamesKindsAndClassesWithoutChangingStorage(t *testing.T) {
	rec := JournalRecord{Kind: JournalRefused, Class: Blocked}
	raw, err := json.Marshal(journalRecordView(rec))
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if view["KindName"] != "refused" || view["ClassName"] != "blocked" {
		t.Fatalf("journal view=%s", raw)
	}
	var roundTrip JournalRecord
	if err := json.Unmarshal(raw, &roundTrip); err != nil || roundTrip.Kind != JournalRefused || roundTrip.Class != Blocked {
		t.Fatalf("numeric machine fields lost: %s %v", raw, err)
	}
}
