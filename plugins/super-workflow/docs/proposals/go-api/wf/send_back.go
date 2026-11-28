package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Send-back (sw-3op4): a downstream state returns work to the agent state
// that produced it, with reasons, a bounded number of times, then takes a
// required fallback edge.
//
// Owns: the mechanism. The edge's budget is counted per instance from the
// journal; the reasons are the workflow's data, read from the sender's
// result when the edge is chosen and journaled in the plan; the producer's
// next visit is a fresh unit that sees them through wf.Returned(), with its
// prior output and its allocated artifacts copied forward to edit in place.
// Does not: hold instruction text (the producer's prompt says what to do
// with a return), repair the output itself, or undo effects already applied.

type sendBackData struct {
	max      Budget
	fallback edgeUse
	reasons  func(json.RawMessage) ([]string, error)
}

// SendBackStep is the journaled half of a send-back on an EdgeStep: the
// return's round over this edge in the instance (from 1), the budget when it
// was taken, and the sender's reasons.
type SendBackStep struct {
	Round   int
	Of      int
	Reasons []string `json:",omitempty"`
}

// Return is a send-back as its producer sees it: the state and edge that
// returned the work, the round of the budget, and the reasons.
type Return struct {
	From    string   `json:"from"`
	Label   string   `json:"label"`
	Round   int      `json:"round"`
	Of      int      `json:"of"`
	Reasons []string `json:"reasons"`
	// Auto marks a return planned for a wf.Reject with no declared edge.
	Auto bool `json:"auto,omitempty"`
}

func sendBackEdge[S any](n *node, to State, label string, max Budget, fallback Edge[S], reasons func(json.RawMessage) ([]string, error)) Edge[S] {
	e := edgeTo[S](n, to, label)
	switch {
	case e.e == nil:
		return e
	case fallback.e == nil:
		n.def.misuse(fmt.Sprintf("state %q: SendBack %q has a zero fallback Edge", n.name, label))
		return e
	}
	e.e.sendBack = &sendBackData{max: max, fallback: fallback.use(), reasons: reasons}
	return e
}

func decodedReasons[O any](fn func(O) []string) func(json.RawMessage) ([]string, error) {
	return func(raw json.RawMessage) (rs []string, err error) {
		var o O
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &o); err != nil {
				return nil, err
			}
		}
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("reasons panicked: %v", r)
			}
		}()
		return fn(o), nil
	}
}

// SendBack declares an edge that returns the work to producer, the agent
// state upstream whose output this state judged. reasons reads them from the
// accepted output; they reach the producer through wf.Returned(). max bounds
// the returns over this edge in the instance; once they are spent, choosing
// the edge takes fallback instead. Use the edge as any route or case
// candidate.
func (a *Agent[S, O]) SendBack(producer State, label string, reasons func(O) []string, max Budget, fallback Edge[S]) Edge[S] {
	return sendBackEdge(a.n, producer, label, max, fallback, decodedReasons(reasons))
}

// SendBack is Agent.SendBack for a compute's result.
func (c *Compute[S, O]) SendBack(producer State, label string, reasons func(O) []string, max Budget, fallback Edge[S]) Edge[S] {
	return sendBackEdge(c.n, producer, label, max, fallback, decodedReasons(reasons))
}

// SendBack is Agent.SendBack for a command's result (a failing test run
// returned to the implementer with its output, say).
func (c *Command[S]) SendBack(producer State, label string, reasons func(RunResult) []string, max Budget, fallback Edge[S]) Edge[S] {
	return sendBackEdge(c.n, producer, label, max, fallback, decodedReasons(reasons))
}

func sendBackKey(from, label string) string { return from + "\x00" + label }

type returnedInput struct{}

func (returnedInput) inputRef() inputDesc { return inputDesc{kind: "returned", name: "returned"} }

// Returned is the producer's own prompt input for a return: on a visit
// entered by a send-back it is a JSON document with the sender ("from"), its
// edge ("label"), the round of the budget ("round", "of"), the reasons, the
// producer's prior accepted output ("prior") and, for each allocated
// artifact, the prior file and the path it was copied to for this visit
// ("artifacts"); on any other visit it is empty. It is delivered like any
// output input, paged when the run pages prompts. What the producer should
// do with it is the prompt's text.
func Returned() Input { return returnedInput{} }

// ReturnedOf reads the return that entered agent state a's latest visit in
// this instance; the zero Return when that visit was not entered by a
// send-back.
func ReturnedOf[S, O any](c Ctx, a *Agent[S, O]) Return { return read[Return](c, "returned", a.n) }

// noteReturns folds a transition's plan: every send-back step counts against
// its edge, and the last one sets the return of the state it arrives at.
// Any other arrival clears that state's return.
func (in *instance) noteReturns(p *pendingEdge, to string) {
	delete(in.ReturnedTo, to)
	if p == nil {
		return
	}
	for _, s := range p.Plan.Steps {
		if s.SendBack == nil {
			continue
		}
		if in.Returns == nil {
			in.Returns = map[string]int{}
		}
		in.Returns[sendBackKey(s.From, s.Label)]++
		if s.To == to {
			if in.ReturnedTo == nil {
				in.ReturnedTo = map[string]*Return{}
			}
			in.ReturnedTo[to] = &Return{From: s.From, Label: s.Label, Round: s.SendBack.Round, Of: s.SendBack.Of, Reasons: s.SendBack.Reasons, Auto: s.Auto}
		}
	}
}

// sendBackStep applies a chosen send-back edge to its planned step: it reads
// the reasons from output, or reports spent when the edge's budget is used.
func (c *cycle) sendBackStep(in *instance, step *EdgeStep, sb *sendBackData, steps []EdgeStep, output []json.RawMessage) (spent bool, err error) {
	max := evalCounters(sb.max, in.countersAfter(append(steps[:len(steps):len(steps)], *step)))
	n := in.Returns[sendBackKey(step.From, step.Label)]
	if n >= max {
		return true, nil
	}
	var raw json.RawMessage
	if len(output) > 0 {
		raw = output[0]
	}
	reasons, err := sb.reasons(raw)
	if err != nil {
		return false, fmt.Errorf("send-back %q: %w", step.Label, err)
	}
	if reasons == nil {
		reasons = []string{}
	}
	step.SendBack = &SendBackStep{Round: n + 1, Of: max, Reasons: reasons}
	return false, nil
}

// returnedDocument is the Returned() input of producer n's current visit.
func (s scope) returnedDocument(n *node) (string, error) {
	r := s.in.ReturnedTo[n.name]
	if r == nil {
		return "", nil
	}
	doc := struct {
		Return
		Prior     json.RawMessage             `json:"prior,omitempty"`
		Artifacts map[string]returnedArtifact `json:"artifacts,omitempty"`
	}{Return: *r, Prior: s.in.Outputs[n.name]}
	for _, a := range allocatedOf(n) {
		prior, ok := s.in.Artifacts[a.name]
		if !ok {
			continue
		}
		p, err := s.c.allocatedPath(s.in, a)
		if err != nil {
			return "", err
		}
		if doc.Artifacts == nil {
			doc.Artifacts = map[string]returnedArtifact{}
		}
		doc.Artifacts[a.name] = returnedArtifact{Prior: prior.Path, Path: p}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return s.promptDocument(raw, "prompt input returned")
}

type returnedArtifact struct {
	Prior string `json:"prior"`
	Path  string `json:"path"`
}

// copyReturnedArtifacts copies each allocated artifact the producer's prior
// accepted visit recorded into this visit's allocated path, once, so a
// returned producer edits its own files in place. A path this visit already
// holds is left as it is; a prior file that is gone or not a regular file is
// not copied. A templated artifact keeps one path across visits and needs no
// copy.
func (c *cycle) copyReturnedArtifacts(in *instance, n *node) error {
	if in.ReturnedTo[n.name] == nil || c.e.recordedReplay {
		return nil
	}
	for _, a := range allocatedOf(n) {
		prior, ok := in.Artifacts[a.name]
		if !ok {
			continue
		}
		dst, err := c.allocatedPath(in, a)
		if err != nil {
			return err
		}
		src := prior.Path
		if !filepath.IsAbs(src) {
			src = filepath.Join(c.workdir(in), src)
		}
		if src == dst {
			continue
		}
		if _, err := os.Lstat(dst); err == nil || !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		fi, err := os.Lstat(src)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("copying returned artifact %s: %w", a.name, err)
		}
		tmp := dst + ".returned.tmp"
		if err := os.WriteFile(tmp, b, fi.Mode().Perm()); err != nil {
			return fmt.Errorf("copying returned artifact %s: %w", a.name, err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			return fmt.Errorf("copying returned artifact %s: %w", a.name, err)
		}
	}
	return nil
}

// ruleSendBack is RuleSendBackTarget and RuleSendBackFallback over every
// send-back edge of the definition.
func ruleSendBack(c *lintCtx) []Problem {
	var out []Problem
	for i := range c.states {
		sv := &c.states[i]
		n := sv.node
		for j, u := range sv.uses {
			if u.e != nil && u.e.sendBack != nil && sv.useKinds[j] == failedUse {
				out = append(out, n.problem(RuleSendBackTarget, "send-back %q is used as a failure, timeout or fallback edge: take it from a route or case", u.e.label))
			}
		}
		for _, e := range n.edges {
			sb := e.sendBack
			if sb == nil || e.to == nil {
				continue
			}
			to := e.to
			_, agent := to.kind.(*agentKind)
			switch {
			case to == n:
				out = append(out, n.problem(RuleSendBackTarget, "send-back %q targets the state itself: a retry is wf.Retry", e.label))
			case !agent || to.def != c.d:
				out = append(out, n.problem(RuleSendBackTarget, "send-back %q targets %s, which is not an agent state of %s", e.label, to.name, c.d.name))
			case !reach(to)[n]:
				out = append(out, n.problem(RuleSendBackTarget, "send-back %q targets %s, which is not upstream of %s", e.label, to.name, n.name))
			}
			fb := sb.fallback.e
			if fb == nil {
				continue
			}
			if fb.sendBack != nil {
				out = append(out, n.problem(RuleSendBackFallback, "send-back %q falls back to send-back %q: the fallback must leave the loop", e.label, fb.label))
			}
			if fb.to != nil && fb.ends == "" {
				if tk, ok := fb.to.kind.(*terminalKind); ok && (tk.outcome == Stopped || tk.outcome == Stalled) {
					out = append(out, n.problem(RuleSendBackFallback, "send-back %q falls back to %q, a stop with no reason: mark it Ends(reason)", e.label, fb.label))
				}
			}
		}
	}
	return out
}

func bindsReturned(k *agentKind) bool {
	for _, in := range k.prompt.inputs {
		if in.inputRef().kind == "returned" {
			return true
		}
	}
	return false
}
