package wf

import "fmt"

const pathParamMarker = "$wf.pathParam"

// markedPathParam is an explicit reference to a run's typed path parameter.
// The value itself lives once in RunStarted, so durable effects do not capture
// an invocation's absolute root.
func markedPathParam(value any) (string, bool, error) {
	object, ok := value.(map[string]any)
	if !ok || len(object) != 1 {
		return "", false, nil
	}
	raw, ok := object[pathParamMarker]
	if !ok {
		return "", false, nil
	}
	name, ok := raw.(string)
	if !ok || name == "" {
		return "", true, fmt.Errorf("invalid path parameter marker")
	}
	return name, true, nil
}

// resolvePathValues resolves only declared path markers into a fresh value.
func (c *cycle) resolvePathValues(value any) (any, error) {
	if p, marked, err := markedOwnedPath(value); marked || err != nil {
		if err != nil {
			return nil, err
		}
		return c.resolveOwnedPath(p)
	}
	if name, marked, err := markedPathParam(value); marked || err != nil {
		if err != nil {
			return nil, err
		}
		if _, ok := c.st.started.ParamPaths[name]; !ok {
			return nil, fmt.Errorf("path parameter %s is not recorded as a path", name)
		}
		return c.paramValue(name)
	}
	if _, marked, err := markedRunPath(value); marked || err != nil {
		if err != nil {
			return nil, err
		}
		return resolveRunPathValue(value, c.resultDir())
	}
	switch v := value.(type) {
	case map[string]any:
		if v == nil {
			return v, nil
		}
		out := make(map[string]any, len(v))
		for k, item := range v {
			resolved, err := c.resolvePathValues(item)
			if err != nil {
				return nil, err
			}
			out[k] = resolved
		}
		return out, nil
	case []any:
		if v == nil {
			return v, nil
		}
		out := make([]any, len(v))
		for i, item := range v {
			resolved, err := c.resolvePathValues(item)
			if err != nil {
				return nil, err
			}
			out[i] = resolved
		}
		return out, nil
	}
	return value, nil
}
