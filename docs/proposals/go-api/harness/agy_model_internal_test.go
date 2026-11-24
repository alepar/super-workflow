package harness

import (
	"strings"
	"testing"
)

func TestAgyCheckModel(t *testing.T) {
	mc, ok := For("agy").(ModelChecker)
	if !ok {
		t.Fatal("agy must implement ModelChecker")
	}
	for _, m := range []string{"inherit", "flash_lite", "flash", "pro"} {
		if err := mc.CheckModel(m); err != nil {
			t.Errorf("%s refused: %v", m, err)
		}
	}
	for _, m := range []string{"gemini-3.8-flash-high", "", "Flash", "pro ", "opus"} {
		err := mc.CheckModel(m)
		if err == nil || !strings.Contains(err.Error(), "inherit, flash_lite, flash, pro") {
			t.Errorf("%q: %v, want a refusal naming the four values", m, err)
		}
	}
	if _, ok := For("claude").(ModelChecker); ok {
		t.Error("claude takes any model value")
	}
}
