package runtime_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	_ "github.com/HotLoop-io/hotloop-flow/internal/nodes" // the real palette
	rt "github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// SBF's flows hold two change nodes that set msg.payload[0] to msg.payload. The
// first Modbus answer that reached one overflowed the Go stack in
// engine.Denormalise and the whole Flow exited, then crash-looped. The node must
// report an error like any node, and a different flow beside it must keep
// answering.
func TestSelfReferencingValueFailsOnlyItsNode(t *testing.T) {
	flows, err := engine.ParseFlows([]byte(`[
        {"id":"a","type":"tab","label":"A"},
        {"id":"b","type":"tab","label":"B"},
        {"id":"selfref","type":"change","z":"a","name":"payload holds itself",
         "rules":[{"t":"set","p":"payload[0]","pt":"msg","to":"payload","tot":"msg"}],
         "wires":[["after"]]},
        {"id":"after","type":"debug","z":"a","active":false,"wires":[]},
        {"id":"fine","type":"change","z":"b",
         "rules":[{"t":"set","p":"payload","pt":"msg","to":"payload[1]","tot":"msg"}],
         "wires":[["sink"]]},
        {"id":"sink","type":"debug","z":"b","active":false,"wires":[]}
    ]`))
	if err != nil {
		t.Fatal(err)
	}
	r := rt.New(node.Default, flows, rt.Options{})

	var mu sync.Mutex
	var errs []map[string]any
	go func() {
		for ev := range r.Events() {
			if ev.Topic == rt.TopicError {
				mu.Lock()
				errs = append(errs, ev.Data)
				mu.Unlock()
			}
		}
	}()
	if fails := r.Start(context.Background()); len(fails) > 0 {
		t.Fatalf("start: %v", fails[0])
	}
	defer r.Stop(context.Background())

	// Every answer a PLC gives is a list of registers.
	for i := 0; i < 50; i++ {
		m := engine.WrapMsg(map[string]any{"payload": []any{1.0, 2.0, 3.0}})
		if err := r.Inject("selfref", m); err != nil {
			t.Fatalf("inject: %v", err)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := len(errs)
		mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the self-referencing change node reported no error")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	first := errs[0]
	mu.Unlock()
	if first["nodeId"] != "selfref" {
		t.Errorf("error reported on %v, want the change node selfref", first["nodeId"])
	}
	if s, _ := first["error"].(string); !strings.Contains(s, "refers to itself") {
		t.Errorf("error = %q, want it to say the value refers to itself", s)
	}

	// The other flow is untouched.
	if err := r.Inject("fine", engine.WrapMsg(map[string]any{"payload": []any{1.0, 2.0}})); err != nil {
		t.Fatalf("the other flow stopped taking messages: %v", err)
	}
}
