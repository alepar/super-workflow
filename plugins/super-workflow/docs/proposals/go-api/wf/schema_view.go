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
