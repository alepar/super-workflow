package wf

import (
	"fmt"
	"strings"
)

// Mermaid exports the same graph as Render for docs and richer viewers: a
// stateDiagram-v2 built from the shared walk (Inspect). Sub and Map children
// become composite states; gate reservations become notes.
func (d *Def) Mermaid() string {
	var b strings.Builder
	b.WriteString("stateDiagram-v2\n")
	mermaidBody(&b, Inspect(d), "", "  ")
	return b.String()
}

func mermaidID(prefix, name string) string {
	r := strings.NewReplacer(" ", "_", "-", "_", ".", "_", "/", "_")
	return r.Replace(prefix + name)
}

func mermaidBody(b *strings.Builder, v DefView, prefix, ind string) {
	if len(v.States) > 0 {
		fmt.Fprintf(b, "%s[*] --> %s\n", ind, mermaidID(prefix, v.States[0].Name))
	}
	for _, s := range v.States {
		id := mermaidID(prefix, s.Name)
		for _, e := range s.Edges {
			l := e.Label
			if l == "" {
				l = "ok"
			}
			if e.When != "" {
				l += " when " + e.When
			}
			if e.Timeout > 0 {
				l += " after " + e.Timeout.String()
			}
			fmt.Fprintf(b, "%s%s --> %s : %s\n", ind, id, mermaidID(prefix, e.To), l)
		}
		if s.Kind == "terminal" {
			fmt.Fprintf(b, "%s%s --> [*]\n", ind, id)
		}
		if s.Reservation != "" {
			fmt.Fprintf(b, "%snote right of %s : %s\n", ind, id, s.Reservation)
		}
		if s.Child != "" {
			fmt.Fprintf(b, "%snote right of %s : %s %s\n", ind, id, s.Kind, s.Child)
		}
	}
	for _, c := range v.Children {
		cp := mermaidID(prefix, c.Name) + "_"
		fmt.Fprintf(b, "%sstate %s {\n", ind, strings.TrimSuffix(cp, "_"))
		mermaidBody(b, c, cp, ind+"  ")
		fmt.Fprintf(b, "%s}\n", ind)
	}
}
