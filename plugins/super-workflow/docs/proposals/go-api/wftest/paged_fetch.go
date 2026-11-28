package wftest

import (
	"fmt"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// PagedPrompts starts the Sim's run with paged prompt delivery, as
// --paged-prompts or a Registration with PagedPrompts does. It needs Cap: the
// paged coordinator fetches each unit's first prompt page when it starts the
// unit. Before a Submit or Fail its subagent fetches every required page (the
// prompt and the required references) unless the step says otherwise with
// Fetching.
func PagedPrompts() Option { return Option{func(s *Sim) { s.paged = true }} }

// Fetch is the pages one input's reference, or the prompt itself, is fetched
// at: build one with Pages.
type Fetch struct {
	input string
	pages []int
}

// Pages fetches pages of the reference input called input (its As alias if it
// has one), or of the prompt when input is "". No pages fetches every page; a
// page listed twice is fetched twice, which coverage counts once.
func Pages(input string, pages ...int) Fetch { return Fetch{input: input, pages: pages} }

// Fetching scripts exactly which pages the subagent fetches before this
// Submit or Fail, in a Sim with PagedPrompts, in place of every required
// page. The first prompt page is always fetched: the coordinator fetches it
// when it starts the unit. No fetches leaves it at that.
func (s Step) Fetching(fetches ...Fetch) Step {
	s.fetches = append([]Fetch{}, fetches...)
	s.fetchSet = true
	return s
}

// fetchPages plays the subagent's page fetches before st's Done or Failed.
func (c *coordinator) fetchPages(i int, st Step, name string) bool {
	if !c.r.s.paged {
		return true
	}
	e := c.r.e.(*wf.Engine)
	var cov wf.FetchCoverage
	if !c.call("coverage "+name, func() (err error) {
		var ok bool
		cov, ok, err = e.UnitCoverage(c.r.id, name)
		if err == nil && !ok {
			err = fmt.Errorf("unit %s fetched no prompt page", name)
		}
		return err
	}) {
		return false
	}
	plan := st.fetches
	if !st.fetchSet {
		plan = []Fetch{Pages("")}
		for _, r := range cov.References {
			if r.Required {
				plan = append(plan, Pages(r.Name))
			}
		}
	}
	for _, f := range plan {
		ref, total := "", cov.Prompt.Pages
		if f.input != "" {
			found := false
			for _, r := range cov.References {
				if r.Name == f.input {
					ref, total, found = r.ID, r.Pages, true
					break
				}
			}
			if !found {
				c.r.fail("script step %d (%s): unit %s has no reference input %q", i+1, st.desc, name, f.input)
				return false
			}
		}
		pages := f.pages
		if len(pages) == 0 {
			for p := 1; p <= total; p++ {
				pages = append(pages, p)
			}
		}
		for _, p := range pages {
			req := wf.PromptPageRequest{Page: p, Reference: ref}
			if !c.call(fmt.Sprintf("fetch %s page %d of %q", name, p, f.input), func() error {
				_, err := e.PromptPage(c.r.id, name, "", harness.Caller{}, req)
				return err
			}) {
				return false
			}
		}
	}
	return true
}
