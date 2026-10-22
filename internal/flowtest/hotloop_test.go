package flowtest_test

import (
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
)

// The HotLoop nodes under a flow test: nothing reaches HotLoop, the test says
// what HotLoop answered, and what the nodes would have sent is checked.
const hotloopFlow = `[
  {"id":"t1","type":"tab","label":"Level"},
  {"id":"hl","type":"hotloop-flow-hotloop","url":"http://hotloop.plant.example:8080","username":"flow"},
  {"id":"plc","type":"hotloop-flow-modbus","host":"192.0.2.10"},
  {"id":"go","type":"inject","z":"t1","name":"go","x":1,"y":1,"wires":[["level"]]},
  {"id":"level","type":"hotloop state","z":"t1","name":"level","server":"hl","entity":"sensor.tank_1_level","numeric":true,"x":2,"y":1,"wires":[["write"]]},
  {"id":"write","type":"modbus write","z":"t1","name":"write","server":"plc","area":"holding","address":"0","dataType":"uint16","value":"payload","valueType":"msg","hotloop":"hl","hotloopTag":"plant/level_sp","x":3,"y":1,"wires":[["out"]]},
  {"id":"out","type":"debug","z":"t1","name":"out","x":4,"y":1,"wires":[]},
  {"id":"errors","type":"catch","z":"t1","name":"errors","x":1,"y":3,"wires":[[]]}
]`

func TestHotloopNodesUnderAFlowTest(t *testing.T) {
	rep := run(t, hotloopFlow, `
tests:
  - name: the level is read from HotLoop and written back through its gate
    inject: [{node: go}]
    replies:
      - {node: level, reply: {state: "42", quality: good, attributes: {unit_of_measurement: "%"}}}
    expect:
      - {node: level, sent: {op: state, entity_id: sensor.tank_1_level}}
      - {node: write, sent: {op: write, tag: plant/level_sp, value: 42, reason: "flow:write"}}
      - {node: out, msg: {payload: 42}}
  - name: a write the gate refuses arrives at the Catch node with the gate's reason
    inject: [{node: go}]
    replies:
      - {node: level, reply: {state: "120"}}
      - {node: write, error: "value is outside the tag's configured range: 120 is outside 0..95"}
    expect:
      - node: errors
        error: "HotLoop's gate refused the write to plant/level_sp: value is outside the tag's configured range: 120 is outside 0..95"
`)
	if len(rep.Tests) != 2 {
		t.Fatalf("got %d results, want 2", len(rep.Tests))
	}
	for _, r := range rep.Tests {
		wantStatus(t, r, flowtest.Pass)
	}
}
