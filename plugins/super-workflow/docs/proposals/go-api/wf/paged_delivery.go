package wf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// PromptPageRequest selects one page. Code is printed on its predecessor only.
type PromptPageRequest struct {
	Envelope  harness.PromptEnvelope
	Page      int
	Code      string
	Reference string
}
type PromptPage struct {
	Delivered   string
	Page, Pages int
	NextCode    string
}

// PromptPacket is immutable once the first fetch binds it to a unit and child.
// Original inputs are retained separately from their readable presentation.
type PromptPacket struct {
	Run                         RunID
	Unit                        string
	Attempt                     int
	Child                       string
	PromptHash, PromptShapeHash string
	Digest                      string
	Pages                       []string
	Codes                       []string
	Originals                   map[string]string
	References                  map[string]PromptReference
	// CommandPrefix recognizes the executable as printed when the packet was built.
	CommandPrefix string `json:",omitempty"`
}
type PromptReference struct {
	Sections     []string
	Name         string
	Required     bool
	Pages, Codes []string
}

func pageKey(ref string, page int) string { return fmt.Sprintf("%s:%d", ref, page) }

func (e *Engine) pagedPrompts(run RunID) (bool, error) {
	// The choice is made at the run's start and never changes: its first
	// record says it, with no fold.
	var started RunStartedData
	var decodeErr error
	err := e.withRecords(run, func(recs []JournalRecord) { decodeErr = json.Unmarshal(recs[0].Data, &started) })
	if err == nil && decodeErr != nil {
		err = fmt.Errorf("%w: run_started: %v", ErrCorruptJournal, decodeErr)
	}
	return started.PagedPrompts, err
}

func (e *Engine) PromptPage(run RunID, name, claim string, who harness.Caller, req PromptPageRequest) (PromptPage, error) {
	return e.deliverPage(run, name, claim, who, req, nil)
}

// deliverPage writes through deliver before recording credit. A failed output or
// flush leaves the page uncredited; a crash after output is repaired by re-fetch.
func (e *Engine) deliverPage(run RunID, name, claim string, who harness.Caller, req PromptPageRequest, deliver func(string) error) (PromptPage, error) {
	if err := e.ChildVerbCheck(run, "prompt", name, claim, who); err != nil {
		return PromptPage{}, e.childClaimError(run, "prompt", err)
	}
	var result PromptPage
	err := e.withRun(run, func(c *cycle) error {
		if !c.st.started.PagedPrompts {
			return fmt.Errorf("%w: paged prompt delivery is not enabled for this run", ErrRejected)
		}
		in, u, resolved, err := c.fetchable(name, claim, who)
		if err != nil {
			return err
		}
		name = resolved
		packet := u.Packet
		fresh := packet == nil
		if fresh {
			packet, err = c.buildPromptPacket(in, u, claim, who, req.Envelope)
			if err != nil {
				return err
			}
		}
		pages, codes := packet.Pages, packet.Codes
		if req.Reference != "" {
			ref, ok := packet.References[req.Reference]
			if !ok {
				return fmt.Errorf("%w: unknown prompt reference", ErrRejected)
			}
			pages, codes = ref.Pages, ref.Codes
		}
		if req.Page == 0 {
			req.Page = 1
		}
		if req.Page < 1 || req.Page > len(pages) {
			return fmt.Errorf("%w: page must be between 1 and %d", ErrRejected, len(pages))
		}
		key := pageKey(req.Reference, req.Page)
		if req.Envelope.Bytes > 0 && !pageFits(pages[req.Page-1], req.Envelope, packet.CommandPrefix) {
			return fmt.Errorf("%w: immutable page exceeds the current harness envelope; report this delivery failure", ErrRejected)
		}
		result = PromptPage{Delivered: pages[req.Page-1], Page: req.Page, Pages: len(pages)}
		if req.Page < len(pages) {
			result.NextCode = codes[req.Page-1]
		}
		if deliver != nil {
			if err = deliver(result.Delivered); err != nil {
				return err
			}
		}
		if u.PagesFetched[key] {
			return nil
		}
		startedFirst := u.State.claimed()
		if u.State == UnitHanded {
			d := AttemptStartedData{Name: name, Harness: who.Harness, Session: who.Session, Agent: who.Agent, Root: who.Root, Env: who.FromEnv}
			if lease := c.mapLeaseOf(in); lease > 0 {
				d.Expires = c.now().Add(lease)
			}
			if err = c.emit(in, JournalAttemptStarted, 0, d); err != nil {
				return err
			}
		}
		if startedFirst && u.Bound == "" && claim != "" && u.Claim != nil && who.FromEnv {
			// Claimed by a started before its child's first fetch: the child's
			// own environment fills what the started left empty.
			add := MetadataAttachedData{Name: name, State: u.State.String(), Resolved: true, Env: true}
			if u.Claim.Agent == "" {
				add.Agent = who.Agent
			}
			if u.Claim.Root == "" {
				add.Root = who.Root
			}
			if u.Claim.Harness == "" {
				add.Harness = who.Harness
			}
			if u.Claim.Session == "" {
				add.Session = who.Session
			}
			if err := c.emit(in, JournalMetadataAttached, 0, add); err != nil {
				return err
			}
		}
		d := PromptFetchedData{Name: name, Caller: callerClaim(who), PacketDigest: packet.Digest, Page: req.Page, Reference: req.Reference, PromptHash: packet.PromptHash, PromptShapeHash: packet.PromptShapeHash}
		if fresh {
			d.Packet = packet
		}
		if u.Bound == "" {
			d.Claim = claim
		}
		return c.emit(in, JournalPromptFetched, 0, d)
	})
	return result, e.childClaimError(run, "prompt", err)
}

func (c *cycle) buildPromptPacket(in *instance, u *unit, claim string, who harness.Caller, envelope harness.PromptEnvelope) (*PromptPacket, error) {
	n := in.node()
	p, ok := n.kind.(prompter)
	if !ok {
		return nil, fmt.Errorf("state has no prompt")
	}
	output, err := c.outputPath(u.Name)
	if err != nil {
		return nil, err
	}
	args := []string{u.Name}
	if claim != "" {
		args = []string{"--claim", claim}
	}
	if output == "" {
		args = append(args, "--output", "<file>")
	}

	env := envelope
	if env.Bytes == 0 {
		env = harness.PromptEnvelopeFor(who.Harness, nil)
	}
	commandPrefix := c.e.pagedCommandPrefix()
	packet := &PromptPacket{
		Run: c.run, Unit: u.Name, Attempt: in.Attempt, Child: who.Agent,
		References: map[string]PromptReference{}, CommandPrefix: commandPrefix,
	}
	if packet.Child == "" && u.Claim != nil {
		packet.Child = u.Claim.Agent
	}
	nonce := c.e.opts.Nonce()
	policies := map[string]string{}
	if agent, ok := n.kind.(*agentKind); ok {
		for _, input := range agent.prompt.inputs {
			d := input.inputRef()
			key := d.name
			if d.alias != "" {
				key = d.alias
			}
			policies[key] = d.reference
		}
	}
	manifest := ""
	c.promptPresentation = func(key, value string) (string, error) {
		rendered := readableInput(value)
		refID := hashBytes([]byte(u.Name + "/" + key))[:16]
		original := filepath.ToSlash(filepath.Join("prompt-inputs", hashBytes([]byte(u.Name))[:16], hashBytes([]byte(key))[:16]+".original"))
		originalNote := "Original retained in the packet journal artifact."
		if c.e.opts.RunDir != nil && c.e.opts.RunDir(c.run) != "" {
			originalNote = "Original artifact relative to run directory: " + original
		}
		header := "Input: " + key + "\n" + originalNote + "\n"
		pages, codes, e := pageText(rendered, header, env, func(page int, code string) string {
			return c.pageFetchLine("read", refID, claim, page, code)
		}, nonce+refID, commandPrefix, false)
		if e != nil {
			return "", e
		}
		if len(pages) > 4 || policies[key] != "" {
			policy := policies[key]
			if policy == "" {
				policy = "required"
			}
			if policy == "optional" {
				for i := range pages {
					pages[i] = strings.Replace(pages[i], "Required prompt page", "Optional input page", 1)
					pages[i] = strings.Replace(pages[i], "End of required pages.", "End of optional pages.", 1)
				}
			}
			sections := referenceSections(pages, header, env.Columns, commandPrefix)
			packet.References[refID] = PromptReference{Name: key, Required: policy == "required", Pages: pages, Codes: codes, Sections: sections}
			cmd := c.pageFetchLine("read", refID, claim, 1, "")
			rendered = fmt.Sprintf("%s reference %s (%s), sections/pages 1-%d.\n%s", policy, key, refID, len(pages), cmd)
			manifest += "\n" + rendered + "\n"
			rendered += "\nSection index:\n" + strings.Join(sections, "\n")
		} else {
			rendered += "\n" + originalNote
		}
		return rendered, nil
	}
	defer func() { c.promptPresentation = nil }()
	text, inputs, err := p.renderPrompt(c, in, n, c.verbLine("done", args...))
	if err != nil {
		return nil, err
	}
	packet.Originals = inputs
	goal, _, _ := strings.Cut(strings.TrimPrefix(text, c.rulesHead()), "\n")
	if len([]rune(goal)) > 240 {
		goal = string([]rune(goal)[:240]) + " (continued below)"
	}
	contract := "Unit: " + u.Name + "\nGoal: " + goal + "\nFetch every required page before finishing. Coverage means fetched, not read.\n"
	for _, verb := range []string{"validate", "done", "failed"} {
		argv := args
		if verb == "failed" {
			argv = []string{u.Name, "--reason", "<text>"}
			if claim != "" {
				argv = []string{"--claim", claim, "--reason", "<text>"}
			}
		}
		cmd := c.verbLine(verb, argv...)
		contract += verb + ":\n" + cmd + "\n"
	}
	contract += manifest
	text += "\n\n" + c.childRules(u.Name, output, claim)
	// Semantic hashes exclude pagination nonces; input bytes remain significant.
	semantic, _ := json.Marshal(struct {
		Text   string
		Inputs map[string]string
	}{text, inputs})
	packet.PromptHash = "sha256:" + hashBytes(semantic)
	inputBytes, _ := json.Marshal(inputs)
	shapeText := text
	for refID, ref := range packet.References {
		command := c.pageFetchLine("read", refID, claim, 1, "")
		shapeText = strings.ReplaceAll(shapeText, command, "{command:read "+ref.Name+"}")
	}
	packet.PromptShapeHash = "sha256:" + hashBytes([]byte(c.promptShapeHash(in, shapeText)+c.promptShapeHash(in, string(inputBytes))))
	packet.Pages, packet.Codes, err = pageText(text, contract, env, func(page int, code string) string {
		return c.pageFetchLine("prompt", u.Name, claim, page, code)
	}, nonce, commandPrefix, true)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(packet)
	packet.Digest = "sha256:" + hashBytes(raw)

	if c.e.opts.RunDir != nil && c.e.opts.RunDir(c.run) != "" {
		dir := filepath.Join(c.e.opts.RunDir(c.run), "prompt-inputs", hashBytes([]byte(u.Name))[:16])
		if err = os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		for key, value := range inputs {
			if err = os.WriteFile(filepath.Join(dir, hashBytes([]byte(key))[:16]+".original"), []byte(value), 0600); err != nil {
				return nil, err
			}
		}
	}
	return packet, nil
}

// readableLines is a presentation only. Originals retain byte-exact values.
// Non-ASCII runes conservatively cost two columns; control codes are escaped.
func readableLines(text string, width int, commandPrefix string) []string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if isCommandLine(line, commandPrefix) {
			lines = append(lines, line)
			continue
		}
		var b strings.Builder
		cols := 0
		for _, r := range line {
			value := string(r)
			cost := 1
			if r > 127 {
				cost = 2
			}
			if unicode.IsControl(r) {
				value = fmt.Sprintf("\\u%04x", r)
				cost = len(value)
			}
			if cols+cost > width {
				lines = append(lines, b.String())
				b.Reset()
				cols = 0
			}
			b.WriteString(value)
			cols += cost
		}
		lines = append(lines, b.String())
	}
	return lines
}

func pageText(text, contract string, env harness.PromptEnvelope, next func(int, string) string, nonce, commandPrefix string, commandFirst bool) ([]string, []string, error) {
	if env.Bytes <= 0 || env.Lines <= 0 || env.Columns < 8 {
		return nil, nil, fmt.Errorf("invalid prompt envelope")
	}
	body := readableLines(text, env.Columns, commandPrefix)
	rules := strings.Join(readableLines(contract, env.Columns, commandPrefix), "\n") + "\n"
	var pages, codes []string
	fit := func(fixed string) int {
		size, lines := len(fixed), strings.Count(fixed, "\n")
		take := 0
		for take < len(body) && size+len(body[take])+1 <= env.Bytes && lines+1 <= env.Lines {
			size += len(body[take]) + 1
			lines++
			take++
		}
		return take
	}
	for len(body) > 0 {
		index := len(pages) + 1
		header := fmt.Sprintf("Required prompt page %06d of 999999\n", index)
		if index == 1 && commandFirst {
			command := next(1, "")
			header = command + "\n" + header
		}
		code := hashBytes([]byte(nonce + ":" + itoa(index)))[:12]
		ending := "\nEnd of required pages.\n"
		if fit(header+rules+ending) == len(body) {
			pages = append(pages, header+rules+strings.Join(body, "\n")+"\n"+ending)
			codes = append(codes, code)
			break
		}
		command := next(index+1, code)
		footer := "\nNext (one page per command):\n" + command + "\n"
		framing := header
		if index == 1 {
			framing += rules
		}
		take := fit(framing + footer)
		// Keep at least one payload line for a last page carrying the contract.
		if take == len(body) {
			take--
		}
		if take == 0 {
			return nil, nil, fmt.Errorf("prompt envelope cannot fit contract, payload and continuation")
		}
		pages = append(pages, framing+strings.Join(body[:take], "\n")+"\n"+footer)
		codes = append(codes, code)
		body = body[take:]
	}
	for i := range pages {
		pages[i] = strings.Replace(pages[i], "of 999999", fmt.Sprintf("of %06d", len(pages)), 1)
		if !pageFits(pages[i], env, commandPrefix) {
			return nil, nil, fmt.Errorf("prompt framing exceeds the delivery envelope")
		}

	}
	return pages, codes, nil
}

func readableInput(value string) string {
	var out bytes.Buffer
	if json.Indent(&out, []byte(value), "", "  ") == nil {
		return out.String()
	}
	return value
}

// A command stays on one line, even if it is wider than the page's prose.
// An unchained CLI command starts with the exact shell-quoted executable.
func isCommandLine(line, commandPrefix string) bool {
	l := strings.TrimLeft(line, " ")
	return strings.HasPrefix(l, "cd ") && strings.Contains(l, " && ") ||
		strings.HasPrefix(l, "workflow ") ||
		commandPrefix != "" && strings.HasPrefix(l, commandPrefix)
}

func (e *Engine) pagedCommandPrefix() string {
	if bin := e.opts.caller.Executable; bin != "" {
		return shellQuote(bin) + " "
	}
	return ""
}

// promptPage is the only model-facing transport for an opted-in run.
func (c *cli) promptPage(e *Engine, verb string, run RunID, name string, f *flags) int {
	if verb == "read" && name == "" {
		err := e.withDryRun(run, func(c *cycle) error {
			for _, id := range c.st.order {
				in := c.st.insts[id]
				if in.Unit != nil && in.Unit.Packet != nil {
					if _, ok := in.Unit.Packet.References[f.reference]; ok {
						name = in.Unit.Name
						return nil
					}
				}
			}
			return fmt.Errorf("%w: unknown reference; fetch its unit prompt first", ErrRejected)
		})
		if err != nil {
			return c.fromError(verb, err)
		}
	}

	e.opts.operatorChild = c.operatorChild
	f.args = []string{name}
	if err := e.prepareChild(run, verb, f); err != nil {
		return c.fromError(verb, err)
	}
	name = f.args[0]
	if f.json {
		return c.failf(exitUsage, "paged prompt delivery uses plain text; omit --json and fetch one page per command")
	}
	if f.page < 1 {
		return c.failf(exitUsage, "page must be at least 1")
	}
	h := envHarness(c.env)
	who := harness.For(h).Caller(c.env)
	_, err := e.deliverPage(run, name, f.claim, who, PromptPageRequest{Page: f.page, Code: f.pageCode, Reference: f.reference, Envelope: harness.PromptEnvelopeFor(h, c.env)}, func(text string) error {
		if n, err := io.WriteString(c.stdout, text); err != nil {
			return err
		} else if n != len(text) {
			return io.ErrShortWrite
		}
		if w, ok := c.stdout.(interface{ Flush() error }); ok {
			return w.Flush()
		}
		return nil
	})
	if err != nil {
		return c.fromError(verb, err)
	}
	return exitOK
}

func promptCoverage(u *unit) string {
	if u.Packet == nil {
		return ""
	}
	fetched, total := 0, 0
	count := func(ref string, pages []string) {
		for i := range pages {
			total++
			if u.PagesFetched[pageKey(ref, i+1)] {
				fetched++
			}
		}
	}
	count("", u.Packet.Pages)
	for id, r := range u.Packet.References {
		if r.Required {
			count(id, r.Pages)
		}
	}
	return fmt.Sprintf("%d of %d required pages fetched", fetched, total)
}

// PromptDeliveryStatus records service coverage, never model-visible receipt.
type PromptDeliveryStatus struct{ Unit, Digest, Coverage string }

func (c *cycle) promptDeliveryStatus() []PromptDeliveryStatus {
	units := map[string]*unit{}
	var order []string
	for _, rec := range c.recs {
		if rec.Kind != JournalPromptFetched {
			continue
		}
		var d PromptFetchedData
		if json.Unmarshal(rec.Data, &d) != nil || d.Page == 0 {
			continue
		}
		u := units[d.Name]
		if d.Packet != nil {
			u = &unit{Packet: d.Packet, PagesFetched: map[string]bool{}}
			units[d.Name] = u
			order = append(order, d.Name)
		}
		if u != nil && u.Packet.Digest == d.PacketDigest {
			u.PagesFetched[pageKey(d.Reference, d.Page)] = true
		}
	}
	var out []PromptDeliveryStatus
	for _, name := range order {
		u := units[name]
		out = append(out, PromptDeliveryStatus{Unit: name, Digest: u.Packet.Digest, Coverage: promptCoverage(u)})
	}
	return out
}

func referenceSections(pages []string, header string, width int, commandPrefix string) []string {
	contractOffset := 1 + len(readableLines(header, width, commandPrefix))
	var sections []string
	for i, page := range pages {
		offset := 1
		if i == 0 || i == len(pages)-1 {
			offset = contractOffset
		}
		lines := strings.Split(page, "\n")
		title := ""
		if offset < len(lines) {
			title = strings.TrimSpace(lines[offset])
		}
		r := []rune(title)
		if len(r) > 32 {
			title = string(r[:32]) + "…"
		}
		sections = append(sections, fmt.Sprintf("Page %d: %s", i+1, title))
	}
	return sections
}

func pageFits(page string, env harness.PromptEnvelope, commandPrefix string) bool {
	if len(page) > env.Bytes || strings.Count(page, "\n") > env.Lines {
		return false
	}
	for _, line := range strings.Split(page, "\n") {
		if isCommandLine(line, commandPrefix) {
			continue
		}
		columns := 0
		for _, r := range line {
			if r > 127 {
				columns += 2
			} else {
				columns++
			}
		}
		if columns > env.Columns {
			return false
		}
	}
	return true
}

func (c *cycle) pageFetchLine(verb, target, claim string, page int, code string) string {
	args := []string{target}
	if claim != "" {
		if verb == "prompt" {
			args = nil
		}
		args = append(args, "--claim", claim)
	}
	args = append(args, "--page", itoa(page))
	if code != "" {
		args = append(args, "--code", code)
	}
	return c.verbLine(verb, args...)
}
