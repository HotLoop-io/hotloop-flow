package runtime

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// A wire whose depth says messages wait while its table is empty is stuck: the
// pump has nothing to read and nothing will ever lower the depth. The pump
// counts it and says what it saw. The fault is made with a done mark for a
// message that was never delivered, which deletes every row up to its number
// and lowers the depth by one.
func TestDurablePumpCountsAWireItFindsStuck(t *testing.T) {
	sink := &flakySink{gate: make(chan struct{})}
	rig := startDurable(t, filepath.Join(t.TempDir(), "state.db"), spillFlow, Options{}, 0, sink)
	defer rig.stop(t)
	defer close(sink.gate)

	key := "a:0>b"
	q := rig.sp.Wire(key)
	for i := 0; i < 2; i++ {
		body, _, err := store.EncodeMsg(engine.NewMsgWithPayload(float64(i)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.Append(body); err != nil {
			t.Fatal(err)
		}
	}
	q.Ack(store.Entry{Seq: 1 << 40, Size: 70})
	waitFor(t, "the rows to be deleted", func() bool {
		p, err := q.Probe()
		return err == nil && p.Rows == 0 && p.Depth == 1
	})
	if q.Stranded() != 0 {
		t.Fatalf("stranded is %d before the pump has looked", q.Stranded())
	}

	rig.rt.durableMu.Lock()
	w := rig.rt.durable[key]
	rig.rt.durableMu.Unlock()
	w.mu.Lock()
	w.setSpill(true)
	w.mu.Unlock()
	w.notify()

	waitFor(t, "the pump to count the stuck wire", func() bool { return q.Stranded() > 0 })
	var said bool
	for _, e := range rig.el.byTopic(TopicLog) {
		if m, _ := e.Data["message"].(string); strings.Contains(m, "is stuck (depth-without-rows)") {
			said = true
		}
	}
	if !said {
		t.Fatal("the pump counted the wire but did not say why in the log")
	}
}
