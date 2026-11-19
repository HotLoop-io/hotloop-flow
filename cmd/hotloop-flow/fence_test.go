package main

import (
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/diagram"
	"github.com/HotLoop-io/hotloop-flow/internal/fence"
)

type holdAll struct{ err error }

func (h holdAll) Check(string) error { return h.err }

// A diagram's write to HotLoop's gate from a standby, or from a holder whose lease
// has run out, is an error on the error wire and HotLoop is never asked.
func TestAStaleHoldersDiagramWriteThroughTheGateNeverReachesIt(t *testing.T) {
	g := newGateServer(t)
	restore := fence.Set(holdAll{fence.ErrStandby})
	r := runGateDiagram(t, gateFlow(g.URL, "60"))
	restore()
	if got := len(g.seen()); got != 0 {
		t.Fatalf("the gate was asked %d times by a standby", got)
	}
	e, _ := wireLast(t, r, "main/w.err>bad.value").(map[string]any)
	src, _ := e["source"].(string)
	if e["status"] != true || !strings.Contains(src, "standby") {
		t.Fatalf("error out %v", e)
	}
	_ = diagram.CodeGate

	// The same diagram with no fence writes, so the test can see a write.
	r = runGateDiagram(t, gateFlow(g.URL, "60"))
	if got := len(g.seen()); got != 1 {
		t.Fatalf("without the fence the gate was asked %d times, want 1", got)
	}
	_ = r
}
