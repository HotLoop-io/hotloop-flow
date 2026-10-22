package flowtest_test

import (
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
)

// The MCP nodes under a flow test. Nothing is served and nothing is called: a
// test puts the message a client's call would have made where the tool node
// would have, says what the other server answered, and checks what the flow
// would have sent back.
const mcpFlow = `[
  {"id":"t1","type":"tab","label":"Line"},
  {"id":"srv","type":"hotloop-flow-mcp-server","url":"http://hotloop.plant.example:8080/mcp"},
  {"id":"tool","type":"mcp tool","z":"t1","toolName":"line_status","description":"Status of a line","schema":"{\"type\":\"object\",\"properties\":{\"line\":{\"type\":\"string\"}},\"required\":[\"line\"]}","x":1,"y":1,"wires":[["status"]]},
  {"id":"status","type":"mcp client","z":"t1","name":"status","server":"srv","tool":"get_plant_status","arguments":"{}","argumentsType":"json","x":2,"y":1,"wires":[["answer"]]},
  {"id":"answer","type":"mcp result","z":"t1","name":"answer","x":3,"y":1,"wires":[]},
  {"id":"errors","type":"catch","z":"t1","name":"errors","x":1,"y":3,"wires":[["answer"]]}
]`

func TestMCPNodesUnderAFlowTest(t *testing.T) {
	rep := run(t, mcpFlow, `
tests:
  - name: a call is answered with what the other server said
    inject: [{node: tool, msg: {payload: {line: L3}}}]
    replies:
      - {node: status, reply: {text: "2 devices online, 0 alarms"}}
    expect:
      - {node: status, sent: {op: call, tool: get_plant_status, arguments: {}}}
      - {node: answer, sent: {isError: false, text: "2 devices online, 0 alarms"}}
  - name: a structured answer goes back as an object
    inject: [{node: tool, msg: {payload: {line: L3}}}]
    replies:
      - {node: status, reply: {structuredContent: {online: 2}}}
    expect:
      - {node: answer, sent: {isError: false, text: '{"online":2}', structuredContent: {online: 2}}}
  - name: a tool that reports an error reaches the Catch node, and the caller is told
    inject: [{node: tool, msg: {payload: {line: L3}}}]
    replies:
      - {node: status, reply: {isError: true, text: "the gateway is starting"}}
    expect:
      - {node: answer, sent: {isError: true, text: "the tool get_plant_status on stand-in reported an error: the gateway is starting"}}
`)
	if len(rep.Tests) != 3 {
		t.Fatalf("got %d results, want 3", len(rep.Tests))
	}
	for _, r := range rep.Tests {
		wantStatus(t, r, flowtest.Pass)
	}
}
