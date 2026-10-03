package runtime

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// lifeNode records what happens to one instance: how often it started, whether
// and how it was closed, and every message it handled. It forwards what it
// receives on port 0.
type lifeNode struct {
	id      string
	gen     int
	started atomic.Int64
	closed  atomic.Bool
	removed atomic.Bool

	mu   sync.Mutex
	msgs []*engine.Msg
}

func (n *lifeNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	n.mu.Lock()
	n.msgs = append(n.msgs, m)
	n.mu.Unlock()
	out.Send(0, m)
	return nil
}

func (n *lifeNode) Start(context.Context, node.Emitter) error {
	n.started.Add(1)
	return nil
}

func (n *lifeNode) Close(_ context.Context, removed bool) error {
	n.closed.Store(true)
	n.removed.Store(removed)
	return nil
}

func (n *lifeNode) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.msgs)
}

func (n *lifeNode) payloads() []float64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]float64, 0, len(n.msgs))
	for _, m := range n.msgs {
		f, _ := m.Payload().(float64)
		out = append(out, f)
	}
	return out
}

// lives tracks every instance a registry builds, per node id, in build order.
type lives struct {
	mu  sync.Mutex
	all map[string][]*lifeNode
}

func newLives() *lives { return &lives{all: map[string][]*lifeNode{}} }

func (l *lives) build(id string) node.Node {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := &lifeNode{id: id, gen: len(l.all[id]) + 1}
	l.all[id] = append(l.all[id], n)
	return n
}

// of returns every instance built for an id.
func (l *lives) of(id string) []*lifeNode {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*lifeNode(nil), l.all[id]...)
}

// latest returns the newest instance for an id.
func (l *lives) latest(t *testing.T, id string) *lifeNode {
	t.Helper()
	all := l.of(id)
	if len(all) == 0 {
		t.Fatalf("node %s was never built", id)
	}
	return all[len(all)-1]
}

// lifeRegistry registers a "life" flow-node type and a "lifecfg" config type,
// both backed by l.
func lifeRegistry(l *lives) *testRegistry {
	tr := newTestRegistry()
	tr.add("life", 1, 1, l.build)
	tr.add("lifecfg", 0, 0, l.build)
	return tr
}

func update(t *testing.T, rt *Runtime, js string, opts UpdateOptions) UpdateResult {
	t.Helper()
	if opts.Mode == "" {
		opts.Mode = DeployNodes
	}
	res, err := rt.Update(context.Background(), mustFlows(t, js), opts)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	return res
}

func builds(l *lives, id string) int { return len(l.of(id)) }

// ---------------------------------------------------------------------------

// The point of the whole feature: a deploy that changes one node leaves the
// others exactly as they were. Not rebuilt, not restarted, not closed.
func TestPartialDeployRestartsOnlyTheChangedNode(t *testing.T) {
	l := newLives()
	rt := New(lifeRegistry(l).Registry, mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"a","type":"life","z":"t1","x":1,"y":1,"wires":[["b"]]},
        {"id":"b","type":"life","z":"t1","x":2,"y":1,"name":"old","wires":[["c"]]},
        {"id":"c","type":"life","z":"t1","x":3,"y":1,"wires":[]}
    ]`), Options{})
	drain(rt)
	if f := rt.Start(context.Background()); len(f) > 0 {
		t.Fatalf("start failures: %v", f)
	}
	defer rt.Stop(context.Background())

	res := update(t, rt, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"a","type":"life","z":"t1","x":1,"y":1,"wires":[["b"]]},
        {"id":"b","type":"life","z":"t1","x":2,"y":1,"name":"new","wires":[["c"]]},
        {"id":"c","type":"life","z":"t1","x":3,"y":1,"wires":[]}
    ]`, UpdateOptions{})

	if got := builds(l, "a") + builds(l, "c"); got != 2 {
		t.Errorf("unchanged nodes were built %d times in total, want 2 (once each)", got)
	}
	for _, id := range []string{"a", "c"} {
		n := l.latest(t, id)
		if n.closed.Load() || n.started.Load() != 1 {
			t.Errorf("unchanged node %s: closed=%v started=%d, want left alone",
				id, n.closed.Load(), n.started.Load())
		}
	}
	if builds(l, "b") != 2 {
		t.Fatalf("changed node b built %d times, want 2", builds(l, "b"))
	}
	if first := l.of("b")[0]; !first.closed.Load() || first.removed.Load() {
		t.Errorf("old b: closed=%v removed=%v, want closed and not removed (it was replaced)",
			first.closed.Load(), first.removed.Load())
	}
	if !slices.Equal(res.Restarted, []string{"b"}) || len(res.Started) != 0 ||
		len(res.Stopped) != 0 || res.Unchanged != 2 {
		t.Errorf("result = %+v, want b restarted and two unchanged", res)
	}

	// And the graph still works end to end through the new b.
	rt.Inject("a", engine.NewMsgWithPayload(1.0))
	waitFor(t, "a message through the new b", func() bool { return l.latest(t, "c").count() == 1 })
	if l.latest(t, "b").count() != 1 {
		t.Error("the message did not pass through the new b")
	}
}

// Moving a node or rewiring it is not a reason to restart it. The new wires
// still have to take effect on the running node.
func TestPartialDeployRewiresWithoutRestarting(t *testing.T) {
	l := newLives()
	rt := New(lifeRegistry(l).Registry, mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"src","type":"life","z":"t1","x":1,"y":1,"wires":[["s1"]]},
        {"id":"s1","type":"life","z":"t1","x":2,"y":1,"wires":[]},
        {"id":"s2","type":"life","z":"t1","x":2,"y":2,"wires":[]}
    ]`), Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	res := update(t, rt, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"src","type":"life","z":"t1","x":500,"y":300,"wires":[["s2"]]},
        {"id":"s1","type":"life","z":"t1","x":2,"y":1,"wires":[]},
        {"id":"s2","type":"life","z":"t1","x":2,"y":2,"wires":[]}
    ]`, UpdateOptions{})

	for _, id := range []string{"src", "s1", "s2"} {
		if builds(l, id) != 1 {
			t.Errorf("%s was rebuilt; moving and rewiring must not restart anything", id)
		}
	}
	if res.Unchanged != 3 || len(res.Restarted) != 0 {
		t.Errorf("result = %+v, want all three unchanged", res)
	}

	rt.Inject("src", engine.NewMsgWithPayload(1.0))
	waitFor(t, "the message on the new wire", func() bool { return l.latest(t, "s2").count() == 1 })
	if l.latest(t, "s1").count() != 0 {
		t.Error("the message still went down the old wire")
	}
}

// A config node that changes takes everything that names it with it, and
// anything that names those, and nothing else.
func TestPartialDeployFollowsConfigReferences(t *testing.T) {
	l := newLives()
	rt := New(lifeRegistry(l).Registry, mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"tls","type":"lifecfg","cert":"one"},
        {"id":"brk","type":"lifecfg","tls":"tls"},
        {"id":"other","type":"lifecfg"},
        {"id":"user","type":"life","z":"t1","x":1,"y":1,"server":"brk","wires":[]},
        {"id":"nested","type":"life","z":"t1","x":1,"y":2,"opts":{"cfg":["brk"]},"wires":[]},
        {"id":"bystander","type":"life","z":"t1","x":1,"y":3,"server":"other","wires":[]}
    ]`), Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	res := update(t, rt, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"tls","type":"lifecfg","cert":"two"},
        {"id":"brk","type":"lifecfg","tls":"tls"},
        {"id":"other","type":"lifecfg"},
        {"id":"user","type":"life","z":"t1","x":1,"y":1,"server":"brk","wires":[]},
        {"id":"nested","type":"life","z":"t1","x":1,"y":2,"opts":{"cfg":["brk"]},"wires":[]},
        {"id":"bystander","type":"life","z":"t1","x":1,"y":3,"server":"other","wires":[]}
    ]`, UpdateOptions{})

	for _, id := range []string{"tls", "brk", "user", "nested"} {
		if builds(l, id) != 2 {
			t.Errorf("%s built %d times, want 2: it depends on the changed TLS config", id, builds(l, id))
		}
	}
	for _, id := range []string{"other", "bystander"} {
		if builds(l, id) != 1 {
			t.Errorf("%s was rebuilt, but nothing it depends on changed", id)
		}
	}
	if old := l.of("brk")[0]; !old.closed.Load() {
		t.Error("the old broker config was never closed")
	}
	slices.Sort(res.Restarted)
	if !slices.Equal(res.Restarted, []string{"brk", "nested", "tls", "user"}) {
		t.Errorf("restarted = %v", res.Restarted)
	}
}

// Credentials are not in the flow file. A node whose password changed has the
// same settings as before and still has to restart.
func TestPartialDeployRestartsANodeWhoseCredentialsChanged(t *testing.T) {
	l := newLives()
	flow := `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"brk","type":"lifecfg"},
        {"id":"user","type":"life","z":"t1","x":1,"y":1,"server":"brk","wires":[]},
        {"id":"plain","type":"life","z":"t1","x":1,"y":2,"wires":[]}
    ]`
	rt := New(lifeRegistry(l).Registry, mustFlows(t, flow), Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	update(t, rt, flow, UpdateOptions{Credentials: map[string]bool{"brk": true}})

	if builds(l, "brk") != 2 || builds(l, "user") != 2 {
		t.Errorf("brk built %d, user built %d; a credential change must restart the config and its users",
			builds(l, "brk"), builds(l, "user"))
	}
	if builds(l, "plain") != 1 {
		t.Error("an unrelated node restarted")
	}
}

// A deleted node is closed with removed=true and reported as stopped. A newly
// added node is started. A disabled one counts as gone.
func TestPartialDeployAddsRemovesAndDisables(t *testing.T) {
	l := newLives()
	rt := New(lifeRegistry(l).Registry, mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"keep","type":"life","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"gone","type":"life","z":"t1","x":1,"y":2,"wires":[]},
        {"id":"off","type":"life","z":"t1","x":1,"y":3,"wires":[]}
    ]`), Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	res := update(t, rt, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"keep","type":"life","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"off","type":"life","z":"t1","x":1,"y":3,"d":true,"wires":[]},
        {"id":"new","type":"life","z":"t1","x":1,"y":4,"wires":[]}
    ]`, UpdateOptions{})

	if g := l.latest(t, "gone"); !g.closed.Load() || !g.removed.Load() {
		t.Errorf("deleted node: closed=%v removed=%v, want both", g.closed.Load(), g.removed.Load())
	}
	if o := l.latest(t, "off"); !o.closed.Load() || !o.removed.Load() {
		t.Errorf("disabled node: closed=%v removed=%v, want both", o.closed.Load(), o.removed.Load())
	}
	if n := l.latest(t, "new"); n.started.Load() != 1 {
		t.Error("the added node was not started")
	}
	if rt.Running("gone") || rt.Running("off") || !rt.Running("new") || !rt.Running("keep") {
		t.Error("Running disagrees with the deploy")
	}
	if !slices.Equal(res.Started, []string{"new"}) || !slices.Equal(res.Stopped, []string{"gone", "off"}) ||
		res.Unchanged != 1 {
		t.Errorf("result = %+v", res)
	}
}

// The no-loss claim. A source keeps sending through a node while that node is
// replaced, three times over. Every message has to come out the far end, in
// order: the ones queued before a swap through the old instance, the ones sent
// during it through the new one. Traffic is paced so it is genuinely in transit
// at every deploy, not sitting in a queue that drains before the swap starts.
func TestPartialDeployLosesNoMessagesInFlight(t *testing.T) {
	l := newLives()
	tr := lifeRegistry(l)

	// mid takes a moment per message, so the old instance has a small backlog
	// when a deploy lands and goes idle between messages as well. It also takes
	// a while to close, the way a broker connection does, which is the window a
	// message sent mid-swap could fall into.
	var midBuilds atomic.Int64
	tr.add("slow", 1, 1, func(string) node.Node {
		midBuilds.Add(1)
		return slowCloser{nodeFunc(func(_ context.Context, m *engine.Msg, out node.Emitter) error {
			time.Sleep(20 * time.Microsecond)
			out.Send(0, m)
			return nil
		})}
	})

	flow := func(label string) string {
		return `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"src","type":"life","z":"t1","x":1,"y":1,"wires":[["mid"]]},
        {"id":"mid","type":"slow","z":"t1","x":2,"y":1,"name":"` + label + `","wires":[["sink"]]},
        {"id":"sink","type":"life","z":"t1","x":3,"y":1,"wires":[]}
    ]`
	}
	rt := New(tr.Registry, mustFlows(t, flow("v1")), Options{})
	events := drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	const n = 3000
	deployAt := map[int]string{600: "v2", 1400: "v3", 2200: "v4"}
	deployNow := make(chan string)
	deployed := make(chan struct{})
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := range n {
			if label, ok := deployAt[i]; ok {
				deployNow <- label
			}
			rt.Inject("src", engine.NewMsgWithPayload(float64(i)))
			time.Sleep(30 * time.Microsecond)
		}
	}()

	sink := l.latest(t, "sink")
	for range deployAt {
		label := <-deployNow
		// Let the sender start again before the deploy runs, so messages are
		// moving the whole time it does.
		go func() {
			update(t, rt, flow(label), UpdateOptions{})
			deployed <- struct{}{}
		}()
		<-deployed
		if c := sink.count(); c == 0 || c >= n {
			t.Fatalf("the deploy did not land mid-stream: %d of %d messages through", c, n)
		}
	}
	<-sent

	waitFor(t, "every message at the sink", func() bool { return sink.count() == n })
	for i, p := range sink.payloads() {
		if p != float64(i) {
			t.Fatalf("message %d arrived as %v: order was not kept across the deploys", i, p)
		}
	}
	if midBuilds.Load() != 4 {
		t.Errorf("mid was built %d times, want 4", midBuilds.Load())
	}
	if d := events.byTopic(TopicDropped); len(d) > 0 {
		t.Errorf("%d messages were dropped during the deploys: %v", len(d), d[0].Data)
	}
}

// slowCloser takes 20ms to close.
type slowCloser struct{ nodeFunc }

func (slowCloser) Close(context.Context, bool) error {
	time.Sleep(20 * time.Millisecond)
	return nil
}

// A message that reaches a deleted node mid-deploy has nowhere to go. It is
// counted and announced, never lost in silence.
func TestPartialDeploySalvageAnnouncesWhatItCannotDeliver(t *testing.T) {
	l := newLives()
	rt := New(lifeRegistry(l).Registry, mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"x","type":"life","z":"t1","x":1,"y":1,"wires":[]}
    ]`), Options{})
	events := drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	old := rt.graph().runners["x"]
	update(t, rt, `[{"id":"t1","type":"tab","label":"T"}]`, UpdateOptions{})
	if !old.exited.Load() {
		t.Fatal("the deleted node's runner did not exit")
	}

	// A sender still holding the old wire.
	old.enqueue(context.Background(), engine.NewMsgWithPayload(1.0), nil)

	waitFor(t, "the drop to be announced", func() bool { return len(events.byTopic(TopicDropped)) == 1 })
	if old.stats.Dropped.Load() != 1 {
		t.Errorf("dropped counter = %d, want 1", old.stats.Dropped.Load())
	}
}

// A late message for a replaced node goes to its replacement.
func TestPartialDeploySalvageForwardsToTheReplacement(t *testing.T) {
	l := newLives()
	rt := New(lifeRegistry(l).Registry, mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"x","type":"life","z":"t1","x":1,"y":1,"name":"one","wires":[]}
    ]`), Options{})
	events := drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	old := rt.graph().runners["x"]
	update(t, rt, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"x","type":"life","z":"t1","x":1,"y":1,"name":"two","wires":[]}
    ]`, UpdateOptions{})

	old.enqueue(context.Background(), engine.NewMsgWithPayload(7.0), nil)

	waitFor(t, "the late message at the new x", func() bool { return l.latest(t, "x").count() == 1 })
	if l.of("x")[0].count() != 0 {
		t.Error("the retired instance handled a message after it was closed")
	}
	if len(events.byTopic(TopicDropped)) != 0 {
		t.Error("the late message was dropped instead of forwarded")
	}
}

// Modified Flows: a change on one tab restarts that whole tab and leaves the
// other tabs alone.
func TestPartialDeployFlowsModeRestartsTheWholeFlow(t *testing.T) {
	l := newLives()
	flow := func(name string) string {
		return `[
        {"id":"t1","type":"tab","label":"A"},
        {"id":"t2","type":"tab","label":"B"},
        {"id":"a1","type":"life","z":"t1","x":1,"y":1,"name":"` + name + `","wires":[]},
        {"id":"a2","type":"life","z":"t1","x":1,"y":2,"wires":[]},
        {"id":"b1","type":"life","z":"t2","x":1,"y":1,"wires":[]}
    ]`
	}
	rt := New(lifeRegistry(l).Registry, mustFlows(t, flow("one")), Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	res := update(t, rt, flow("two"), UpdateOptions{Mode: DeployFlows})

	if builds(l, "a1") != 2 || builds(l, "a2") != 2 {
		t.Errorf("a1 built %d, a2 built %d: the whole changed flow should restart",
			builds(l, "a1"), builds(l, "a2"))
	}
	if builds(l, "b1") != 1 {
		t.Error("a node on an untouched flow restarted")
	}
	if res.Mode != DeployFlows || res.Unchanged != 1 {
		t.Errorf("result = %+v", res)
	}

	// The same change in nodes mode leaves a2 alone.
	update(t, rt, flow("three"), UpdateOptions{Mode: DeployNodes})
	if builds(l, "a2") != 2 {
		t.Error("nodes mode restarted a node that did not change")
	}
}

// A Catch node that changes is replaced in the routing table: errors from a
// node that did not change reach the new Catch, not the closed one.
func TestPartialDeployReroutesErrorsToTheNewCatch(t *testing.T) {
	l := newLives()
	tr := lifeRegistry(l)
	tr.add("fail", 1, 0, func(string) node.Node { return failNode{err: fmt.Errorf("boom")} })
	tr.add("catch", 0, 1, l.build)

	flow := func(name string) string {
		return `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"f","type":"fail","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"c","type":"catch","z":"t1","x":1,"y":2,"name":"` + name + `","wires":[]}
    ]`
	}
	rt := New(tr.Registry, mustFlows(t, flow("one")), Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	update(t, rt, flow("two"), UpdateOptions{})
	rt.Inject("f", engine.NewMsg())

	waitFor(t, "the error at the new catch", func() bool { return l.latest(t, "c").count() == 1 })
	if l.of("c")[0].count() != 0 {
		t.Error("the error went to the retired catch node")
	}
}

// Two instances of one subflow. Changing one instance's property restarts that
// instance's internals and nothing in the other.
func TestPartialDeployScopesSubflowChangesToTheInstance(t *testing.T) {
	l := newLives()
	flow := func(v string) string {
		return `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"sf","type":"subflow","name":"S","in":[{"x":0,"y":0,"wires":[{"id":"inner"}]}],
         "out":[],"env":[{"name":"LIMIT","type":"num","value":"1"}]},
        {"id":"inner","type":"life","z":"sf","x":1,"y":1,"wires":[]},
        {"id":"i1","type":"subflow:sf","z":"t1","x":1,"y":1,"env":[{"name":"LIMIT","type":"num","value":"` + v + `"}],"wires":[]},
        {"id":"i2","type":"subflow:sf","z":"t1","x":1,"y":2,"wires":[]}
    ]`
	}
	rt := New(lifeRegistry(l).Registry, mustFlows(t, flow("5")), Options{})
	drain(rt)
	if f := rt.Start(context.Background()); len(f) > 0 {
		t.Fatalf("start failures: %v", f)
	}
	defer rt.Stop(context.Background())

	update(t, rt, flow("6"), UpdateOptions{})

	if builds(l, "i1:inner") != 2 {
		t.Errorf("the changed instance's internals were built %d times, want 2", builds(l, "i1:inner"))
	}
	if builds(l, "i2:inner") != 1 {
		t.Error("the other instance restarted")
	}
}

// A node that failed to start and was not touched by the deploy is still
// broken, and the deploy has to say so.
func TestPartialDeployKeepsReportingStandingFailures(t *testing.T) {
	l := newLives()
	tr := lifeRegistry(l)
	tr.add("badstart", 0, 0, func(string) node.Node { return &badStarter{} })

	flow := func(name string) string {
		return `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"bad","type":"badstart","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"ghost","type":"nosuchtype","z":"t1","x":1,"y":2,"wires":[]},
        {"id":"ok","type":"life","z":"t1","x":1,"y":3,"name":"` + name + `","wires":[]}
    ]`
	}
	rt := New(tr.Registry, mustFlows(t, flow("one")), Options{})
	drain(rt)
	if f := rt.Start(context.Background()); len(f) != 2 {
		t.Fatalf("start failures = %v, want two", f)
	}
	defer rt.Stop(context.Background())

	res := update(t, rt, flow("two"), UpdateOptions{})
	var ids []string
	for _, f := range res.Failures {
		ids = append(ids, f.NodeID)
	}
	if !slices.Equal(ids, []string{"bad", "ghost"}) {
		t.Errorf("failures after an unrelated deploy = %v, want bad and ghost still reported", ids)
	}
}

type badStarter struct{}

func (badStarter) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }
func (badStarter) Start(context.Context, node.Emitter) error {
	return fmt.Errorf("port already in use")
}

func TestUpdateRefusesWhatIsNotAPartialDeploy(t *testing.T) {
	l := newLives()
	flow := `[{"id":"t1","type":"tab","label":"T"}]`
	rt := New(lifeRegistry(l).Registry, mustFlows(t, flow), Options{})
	drain(rt)

	if _, err := rt.Update(context.Background(), mustFlows(t, flow), UpdateOptions{Mode: DeployNodes}); err == nil {
		t.Error("Update before Start succeeded")
	}
	rt.Start(context.Background())
	if _, err := rt.Update(context.Background(), mustFlows(t, flow), UpdateOptions{Mode: DeployFull}); err == nil ||
		!strings.Contains(err.Error(), "not a partial deploy") {
		t.Errorf("a full deploy through Update: err = %v", err)
	}
	rt.Stop(context.Background())
	if _, err := rt.Update(context.Background(), mustFlows(t, flow), UpdateOptions{Mode: DeployNodes}); err == nil {
		t.Error("Update after Stop succeeded")
	}
}

func TestParseDeployMode(t *testing.T) {
	for in, want := range map[string]DeployMode{
		"": DeployNodes, "nodes": DeployNodes, "flows": DeployFlows, "full": DeployFull,
	} {
		got, err := ParseDeployMode(in)
		if err != nil || got != want {
			t.Errorf("ParseDeployMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseDeployMode("reload"); err == nil {
		t.Error("an unknown deployment type was accepted")
	}
}
