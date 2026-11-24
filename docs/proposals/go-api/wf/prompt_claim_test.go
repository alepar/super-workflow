package wf

import "testing"

func TestRecordedPromptClaimUsesCLIFlags(t *testing.T) {
	c := &cli{}
	for _, tc := range []struct {
		name        string
		args        []string
		named, want bool
	}{
		{"direct", []string{"r1", "--claim", "token", "--state-dir", "/state", "--json"}, false, true},
		{"named", []string{"--state-dir", "/state", "--run-id", "r1", "--claim=token"}, true, true},
		{"direct-consumed", []string{"r1", "--state-dir", "--claim", "token"}, false, false},
		{"named-consumed", []string{"--state-dir", "--claim", "token"}, true, false},
		{"unknown", []string{"r1", "--unknown", "x", "--claim", "token"}, false, false},
		{"invalid-bool", []string{"r1", "--json=invalid", "--claim", "token"}, false, false},
		{"trailing-positional", []string{"r1", "--claim", "token", "extra"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim, ok := c.recordedPromptClaim(tc.args, tc.named)
			if ok != tc.want || (ok && claim != "token") {
				t.Fatalf("claim=%q ok=%v want=%v", claim, ok, tc.want)
			}
		})
	}
}
