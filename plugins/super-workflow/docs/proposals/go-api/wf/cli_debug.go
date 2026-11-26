package wf

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (c *cli) binaryName() string {
	if c.cfg.Binary != "" {
		return c.cfg.Binary
	}
	return filepath.Base(os.Args[0])
}

func (c *cli) topHelp() int {
	var b bytes.Buffer
	cfg := c.cfg
	if len(cfg.Defs) == 0 {
		cfg.Binary = ""
	} // help works even before definitions register
	usage(cfg, &b)
	name := c.binaryName()
	text := b.String()
	if !cfg.named() {
		text = strings.Replace(text, "usage: workflow ", "usage: "+name+" ", 1)
	}
	fmt.Fprint(c.stdout, text)
	return exitOK
}

// Only a registered definition with a complete run key can discover and
// create its successor. An explicit-id, unkeyed run cannot resume when ended.
func (c *cli) canSucceed(started RunStartedData) bool {
	reg := c.registration(started.Def)
	if reg == nil || len(reg.Def.runKey) == 0 {
		return false
	}
	for _, p := range reg.Def.runKey {
		if _, ok := started.Params[p.name]; !ok {
			return false
		}
	}
	return true
}

func (c *cli) explain(x Executor, run RunID, name string) int {
	text, err := x.(*Engine).Explain(run, name)
	if err != nil {
		return c.fromError("explain", err)
	}
	fmt.Fprint(c.stdout, text)
	return exitOK
}
