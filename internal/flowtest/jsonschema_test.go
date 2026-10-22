package flowtest_test

import (
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
)

// The json node's schema check, through the real runtime: a matching message
// continues, and a mismatch reaches the Catch node with the path of the bad
// field in the error text.
const schemaFlow = `[
  {"id":"t1","type":"tab","label":"Schema"},
  {"id":"in","type":"junction","z":"t1","x":1,"y":1,"wires":[["check"]]},
  {"id":"check","type":"json","z":"t1","name":"reading","action":"obj","schema":"{\"type\":\"object\",\"required\":[\"temp\"],\"properties\":{\"temp\":{\"type\":\"number\"}}}","x":2,"y":1,"wires":[["good"]]},
  {"id":"good","type":"junction","z":"t1","x":3,"y":1,"wires":[[]]},
  {"id":"errors","type":"catch","z":"t1","name":"errors","x":1,"y":2,"wires":[[]]}
]`

func TestJSONNodeSchemaReachesCatchWithThePath(t *testing.T) {
	rep := run(t, schemaFlow, `
tests:
  - name: a bad field goes to catch
    inject:
      - node: in
        msg: {payload: "{\"temp\": \"hot\"}"}
    expect:
      - node: errors
        error: "payload.temp: got string, want number"
      - node: good
        nothing: true
  - name: a good message goes on
    inject:
      - node: in
        msg: {payload: "{\"temp\": 20}"}
    expect:
      - node: good
        msg: {payload: {temp: 20}}
`)
	if len(rep.Tests) != 2 {
		t.Fatalf("results: %d", len(rep.Tests))
	}
	for _, r := range rep.Tests {
		wantStatus(t, r, flowtest.Pass)
	}
}
