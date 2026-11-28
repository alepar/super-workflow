package wf

import "encoding/json"

// OutputSchema returns an agent state's declared JSON result schema, using
// the same enums and field rules as its delivered prompt. Other state kinds
// return nil. Reading it never runs a user closure or changes the definition.
func (s StateView) OutputSchema() json.RawMessage {
	if s.node == nil {
		return nil
	}
	agent, ok := s.node.kind.(*agentKind)
	if !ok {
		return nil
	}
	return json.RawMessage(outputSchema(agent.ops.(outputTyper).outputType(), s.node.def.enums))
}

// OutputExample returns a filled example of an agent state's declared
// result: every field present, with a form value's grammar example and an
// Enum's first value. Other state kinds return nil. It is not part of the
// delivered prompt.
func (s StateView) OutputExample() json.RawMessage {
	if s.node == nil {
		return nil
	}
	agent, ok := s.node.kind.(*agentKind)
	if !ok {
		return nil
	}
	return json.RawMessage(outputExample(agent.ops.(outputTyper).outputType(), s.node.def.enums))
}
