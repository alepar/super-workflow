package wf

import (
	"reflect"
	"strings"
	"testing"
)

// A field tagged render:"omitzero" is left out of an argument's rendering
// while it is zero, so adding one to an argument type keeps the hash of
// every definition that does not set it (sw-abl); set, it renders.
func TestRenderOmitsZeroTaggedFields(t *testing.T) {
	type arg struct {
		Title string
		Extra []byte `render:"omitzero"`
		Plain []byte
	}
	unset := encodeArg(reflect.ValueOf(arg{Title: "T"}))
	if strings.Contains(unset, "Extra") || !strings.Contains(unset, "Plain: nil") {
		t.Errorf("unset = %s, want Extra left out and the untagged field kept", unset)
	}
	if set := encodeArg(reflect.ValueOf(arg{Title: "T", Extra: []byte("x")})); !strings.Contains(set, "Extra:") {
		t.Errorf("set = %s, want Extra rendered", set)
	}
}
