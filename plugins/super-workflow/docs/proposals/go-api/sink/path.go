package sink

import (
	"errors"
	"path/filepath"
	"strings"
)

// ErrComponent is the refusal of a value that is not one path component.
var ErrComponent = errors.New("not a single path component")

// component checks that name is exactly one path component: not empty, not
// . or .., no separator, no NUL. It refuses rather than rewrites, so the
// name on disk is the name the value gave.
func component(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/`+string(filepath.Separator)) {
		return "", ErrComponent
	}
	if err := validText(name); err != nil {
		return "", err
	}
	return name, nil
}

// Under joins base and the names, each checked to be exactly one path
// component (not empty, not . or .., no separator, no NUL), so the result is
// inside base. Use it for a name from model output.
func Under(base string, names ...string) (string, error) {
	parts := []string{base}
	for _, n := range names {
		c, err := component(n)
		if err != nil {
			return "", err
		}
		parts = append(parts, c)
	}
	return filepath.Join(parts...), nil
}
