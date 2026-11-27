package wf

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

// ResolveRunParams resolves parameters for a fresh run exactly as the CLI does:
// file inputs and required values first, then defaults and creation-only defaults.
// params must be non-nil. Derivations may read the host; embedding callers supply
// explicit values for host facts their environment does not provide.
func (r *Registration) ResolveRunParams(params map[string]string, rc RunContext) error {
	rc.Params = params
	if err := r.resolveParams(params, rc); err != nil {
		return err
	}
	return r.deriveAtCreation(params, rc)
}

// resolveParams completes the --param values of a run of r in place: @<path>
// values of file params become the file's text, the required params are
// checked, and every param still absent takes its default, in spec order. A
// param given under an old alias counts as given.
func (r *Registration) resolveParams(params map[string]string, rc RunContext) error {
	given := func(name string) bool { return r.given(params, name) }
	for _, p := range r.Params {
		v, ok := params[p.Name]
		if !p.File || !ok || !strings.HasPrefix(v, "@") {
			continue
		}
		path := v[1:]
		if !filepath.IsAbs(path) {
			path = filepath.Join(rc.Workspace, path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("--param %s=%s: %w", p.Name, v, err)
		}
		params[p.Name] = string(raw)
	}
	var missing []string
	required := append([]string(nil), r.Required...)
	for _, p := range r.Params {
		if p.Required {
			required = append(required, p.Name)
		}
	}
	for _, name := range required {
		if !given(name) {
			missing = append(missing, name+"="+r.shape(name))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("requires --param %s", strings.Join(missing, ", --param "))
	}
	return r.derive(params, rc, false)
}

// deriveAtCreation derives the AtCreation params the caller did not give; the
// CLI calls it once the run chain says this call creates a run.
func (r *Registration) deriveAtCreation(params map[string]string, rc RunContext) error {
	return r.derive(params, rc, true)
}

// derive fills the defaults of the params not given: those with AtCreation
// equal to atCreation.
func (r *Registration) derive(params map[string]string, rc RunContext, atCreation bool) error {
	for _, p := range r.Params {
		if p.Required || p.AtCreation != atCreation || r.given(params, p.Name) {
			continue
		}
		v := p.Value
		if p.Derive != nil {
			var err error
			if v, err = p.Derive(rc); err != nil {
				return fmt.Errorf("cannot derive %s (pass --param %s=%s): %w", p.Name, p.Name, r.shape(p.Name), err)
			}
		}
		params[p.Name] = v
	}
	return nil
}

// given reports whether the caller passed name, or an alias of it.
func (r *Registration) given(params map[string]string, name string) bool {
	if _, ok := params[name]; ok {
		return true
	}
	for old, to := range r.Def.aliases {
		if _, ok := params[old]; ok && to == name {
			return true
		}
	}
	return false
}

// shape is how a usage line names the value of a param: its spec's Value
// when that is a <shape>, else <value>.
func (r *Registration) shape(name string) string {
	for _, p := range r.Params {
		if p.Name == name && strings.HasPrefix(p.Value, "<") {
			return p.Value
		}
	}
	return "<value>"
}

// wantsHelp reports whether args ask for help.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "-help" || a == "--help" {
			return true
		}
	}
	return false
}

// runHelp prints the run params of the named definition: every spec with its
// default and source, then the declared params without one, then the default
// options.
func (c *cli) runHelp(name string) int {
	reg := c.registration(name)
	if reg == nil {
		return c.failf(exitUsage, "unknown definition %s", name)
	}
	bin := c.binaryName()
	fmt.Fprintf(c.stdout, "usage: %s run %s --state-dir <dir> --workspace <repo> [--assets <dir>] [--options <file>] [--model-map key=model,...] [--param k=v ...]\n\n", bin, name)
	fmt.Fprintln(c.stdout, "params:")
	tw := tabwriter.NewWriter(c.stdout, 2, 8, 2, ' ', 0)
	listed := map[string]bool{}
	row := func(name, value, source string) {
		listed[name] = true
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", name, value, source)
	}
	for _, k := range reg.Required {
		row(k, reg.shape(k), "required")
	}
	for _, p := range reg.Params {
		if listed[p.Name] {
			continue
		}
		value, source := p.Value, p.Source
		if value == "" {
			value = `""`
		}
		switch {
		case p.Required && source == "":
			source = "required"
		case p.Required:
			source = "required: " + source
		}
		if p.File {
			value += " | @<path>"
		}
		row(p.Name, value, source)
	}
	for _, p := range reg.Def.params {
		if !listed[p.name] {
			row(p.name, "<value>", "no default")
		}
	}
	tw.Flush()
	if reg.DefaultOptions != nil {
		raw, err := json.Marshal(reg.DefaultOptions)
		if err != nil {
			return c.failf(exitInfra, "run --help: %v", err)
		}
		fmt.Fprintf(c.stdout, "\noptions (--options <file>, JSON; fields it leaves out keep the default) default: %s\n", raw)
	}
	return exitOK
}
