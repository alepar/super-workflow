package wf_test

import (
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/form/formtest"
	_ "github.com/alepar/super-workflow/docs/proposals/go-api/wf" // registers its grammars (run-id)
)

// Every grammar wf registers holds to the hostile corpus.
func TestFormCorpus(t *testing.T) { formtest.CorpusTest(t) }
