package wf

import (
	"fmt"
	"strings"
)

// ResolvePath resolves an explicit PathTemplate in the current workflow scope.
// Empty optional paths remain empty. Ordinary strings are not inferred as paths.
func ResolvePath(c Ctx, template string) (string, error) {
	if template == "" {
		return "", nil
	}
	if !strings.HasPrefix(template, pathTemplatePrefix) {
		return "", fmt.Errorf("ResolvePath requires an explicit PathTemplate")
	}
	value, err := c.value(ctxKey{kind: "pathTemplate", ref: template})
	if err != nil {
		return "", err
	}
	return decodeValue[string](value), nil
}
