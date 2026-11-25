package feature

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest/hostile"
)

// hostileTableRows reads a GFM table's body rows back to cell text.
func hostileTableRows(t *testing.T, table string) [][]string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(table, "\n"), "\n")
	var rows [][]string
	for _, line := range lines[2:] {
		cells, err := hostile.ReadRow(line)
		if err != nil {
			t.Fatalf("%v\n%s", err, table)
		}
		rows = append(rows, cells)
	}
	return rows
}

// A requirement written into the table reads back as one row holding its
// id and its text.
func TestHostileRequirementTableRoundTrip(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			table := requirementTable([]requirement{{"c1", in.Value}, {"c2", "next"}})
			rows := hostileTableRows(t, table)
			if len(rows) != 2 || len(rows[0]) != 2 || len(rows[1]) != 2 {
				t.Fatalf("table has rows %q, want 2 rows of 2 cells:\n%s", rows, table)
			}
			if rows[0][0] != "c1" || rows[0][1] != hostile.Readable(in.Value) || rows[1][0] != "c2" || rows[1][1] != "next" {
				t.Errorf("requirement %q reads back as %q:\n%s", in.Value, rows, table)
			}
		})
	}
}
