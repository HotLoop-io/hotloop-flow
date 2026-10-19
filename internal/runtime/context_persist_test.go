package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// counterNode counts in flow context, through the same Services a real node
// gets.
type counterNode struct{ ctx node.Context }

func (c counterNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	n, err := c.ctx.Increment("count", 1)
	if err != nil {
		return err
	}
	m.Data["count"] = n
	out.Send(0, m)
	return nil
}

func runCounter(t *testing.T, path string, n int) (last float64) {
	t.Helper()
	db, err := store.OpenSQLite(path, store.SQLiteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pc := store.NewPersistentContexts(db, time.Hour)

	reg := node.NewRegistry()
	sink := &sinkNode{}
	if err := reg.Register(node.Descriptor{
		Type: "counter", Category: node.CategoryCommon, Color: "#000000", Icon: "cog",
		Inputs: 1, Outputs: 1, Compatibility: node.Compatibility{Level: node.CompatOnly},
	}, func(def *node.Definition) (node.Node, error) {
		return counterNode{ctx: def.Services.Context(node.ScopeFlow)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(node.Descriptor{
		Type: "sink", Category: node.CategoryCommon, Color: "#000000", Icon: "cog",
		Inputs: 1, Compatibility: node.Compatibility{Level: node.CompatOnly},
	}, func(*node.Definition) (node.Node, error) { return sink, nil }); err != nil {
		t.Fatal(err)
	}

	flows := mustFlows(t, `[
	  {"id":"t1","type":"tab","label":"T"},
	  {"id":"a","type":"counter","z":"t1","x":1,"y":1,"wires":[["b"]]},
	  {"id":"b","type":"sink","z":"t1","x":2,"y":1,"wires":[]}
	]`)
	rt := New(reg, flows, Options{})
	rt.SetContexts(pc.Scoped())
	drain(rt)
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatal(fails)
	}
	for i := 0; i < n; i++ {
		if err := rt.Inject("a", engine.NewMsgWithPayload(float64(i))); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "messages through", func() bool { return sink.count() == n })
	last = sink.last().Data["count"].(float64)

	rt.Stop(context.Background())
	// A clean stop writes what the flush interval was holding back.
	if err := pc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return last
}

// The gap this phase exists for. A counter in a flow used to start from zero on
// every restart, and now it carries on from where it was.
func TestFlowContextCarriesAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if got := runCounter(t, path, 5); got != 5 {
		t.Fatalf("first run counted to %v, want 5", got)
	}
	if got := runCounter(t, path, 3); got != 8 {
		t.Fatalf("after a restart the counter reached %v, want 8: it started over", got)
	}
}
