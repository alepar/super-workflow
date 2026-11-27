package harness

// PromptEnvelope bounds the entire printed page, including all framing.
type PromptEnvelope struct{ Bytes, Lines, Columns int }

// PromptEnvelopeFor conservatively uses the adapter's known output route.
// Unknown routes get 3 KiB; 6 KiB is only used when the known cap permits it.
func PromptEnvelopeFor(name string, env Env) PromptEnvelope {
	e := PromptEnvelope{Bytes: 3 << 10, Lines: 150, Columns: 100}
	if env == nil {
		return e
	}
	bytes, lines := For(name).OutputLimit(env)
	if bytes >= 8<<10 {
		e.Bytes = 6 << 10
	} else if bytes > 0 {
		e.Bytes = min(e.Bytes, bytes*3/4)
	}
	if lines > 0 {
		e.Lines = min(e.Lines, lines*3/4)
	}
	return e
}
