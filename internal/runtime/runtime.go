package runtime

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// Node types with runtime-wide routing behaviour rather than ordinary wiring.
const (
	TypeCatch    = "catch"
	TypeStatus   = "status"
	TypeComplete = "complete"
)

// maxErrorDepth bounds how many times one message may be re-raised as an error.
//
// A Catch node wired back into the flow it catches for is a normal pattern and
// a trivially easy way to build an infinite error loop. Node-RED carries a
// counter on the error for the same reason.
const maxErrorDepth = 10

// Event is something the runtime wants the editor to know about: a status
// change, a debug message, a dropped message, a log line.
type Event struct {
	Topic string         `json:"topic"`
	Data  map[string]any `json:"data"`
	At    time.Time      `json:"at"`
}

// Event topics.
const (
	TopicStatus  = "status"
	TopicError   = "error"
	TopicDropped = "dropped"
	TopicLog     = "log"
	TopicDebug   = "debug"
)

// Runtime owns a running flow graph.
type Runtime struct {
	reg *node.Registry

	opts Options

	ctx    context.Context
	cancel context.CancelFunc

	// cur is the graph that is running now. Every reader loads it once and
	// works from that snapshot; a deploy builds a new one and swaps it in, so
	// delivery and error routing never take a lock and never see a graph that
	// is half rebuilt.
	cur atomic.Pointer[graph]

	// configs holds started configuration-node instances. It lives outside the
	// graph snapshot because it fills in while a graph is being built: a
	// config node's factory, and every flow node's, may look up a config node
	// built a moment earlier.
	configsMu sync.RWMutex
	configs   map[string]node.Node
	// configStops cancels the context a started configuration node was given.
	// Guarded by configsMu with configs.
	configStops map[string]context.CancelFunc

	contexts *store.ScopedContexts

	// credentials returns a node's decrypted credentials.
	credentials func(nodeID string) map[string]string

	// secretFiles reads a file named in ew_credentialFiles; see
	// SetSecretFiles.
	secretFiles func(path string) ([]byte, error)

	events chan Event
	// eventsMu guards both the channel and eventsClosed. Both are touched only
	// under this mutex — the send path and the close path have to agree on one
	// lock, or a late emit panics on a closed channel.
	eventsMu     sync.Mutex
	eventsClosed bool

	started bool
	stopped bool
	mu      sync.Mutex

	// observers are optional hooks for metrics. Nil-safe.
	onExecTime     func(nodeID, typ string, d time.Duration)
	onQueueLatency func(nodeID, typ string, d time.Duration)

	// standIns, when set, hands every node a stand-in for the world outside
	// the process. Only a flow test sets it.
	standIns func(nodeID string) node.StandIn

	// observer sees every message a node is handed and every message it sends.
	// Only a flow test sets one: it is how a test knows what came out of a port
	// without wiring anything into the flow it is testing.
	observer Observer

	// outstanding counts messages that have been offered to a node and not yet
	// finished with: queued in an inbox or inside a handler. A message is
	// counted before it becomes visible to the node that will take it, and the
	// handler finishes sending downstream before its own count drops, so the
	// total cannot touch zero while a message is still moving. Settled reads
	// it.
	outstanding atomic.Int64
}

// Observer watches messages move through a running graph. Both methods are
// handed a copy, called on the goroutine doing the work, and must not block.
type Observer interface {
	// Received is called as a node is about to handle a message.
	Received(nodeID string, msg *engine.Msg)
	// Sent is called for every message a node sends, wired or not, with the
	// output port it left by.
	Sent(nodeID string, port int, msg *engine.Msg)
}

// SetObserver installs an observer. Call it before Start.
func (rt *Runtime) SetObserver(o Observer) { rt.observer = o }

// SetStandIns gives every node built from here on a stand-in for the outside
// world, so that nothing it does reaches a broker, a database, a socket, a file
// or a command. A flow test calls it before Start.
func (rt *Runtime) SetStandIns(fn func(nodeID string) node.StandIn) { rt.standIns = fn }

// Settled reports whether no message is queued for, or being handled by, any
// node. Messages a node is holding on purpose, in a Delay or a Trigger, don't
// count against it: see Quiet.
func (rt *Runtime) Settled() bool { return rt.outstanding.Load() == 0 }

// Quiet reports whether the graph has nothing left to do that it knows about:
// settled, and no node holding messages to send later. A source that fires on
// its own schedule, like a repeating Inject, can still wake it up.
func (rt *Runtime) Quiet() bool {
	if !rt.Settled() {
		return false
	}
	for _, r := range rt.graph().runners {
		if r.deferred != nil && r.deferred.Pending() > 0 {
			return false
		}
	}
	return rt.Settled()
}

// SendFrom sends a message out of a node's output port as though the node had
// produced it. A flow test uses it to stand in for a source, like an MQTT In
// whose broker the test never connects to.
func (rt *Runtime) SendFrom(nodeID string, port int, msg *engine.Msg) error {
	r, ok := rt.graph().runners[nodeID]
	if !ok {
		return fmt.Errorf("node %s is not running", nodeID)
	}
	if port < 0 {
		return fmt.Errorf("port %d does not exist", port)
	}
	rt.deliver(r, port, msg)
	return nil
}

// handler is a Catch or Status node together with the scope it watches.
type handler struct {
	r *runner

	// scope lists the node ids this handler watches. Empty means the whole
	// containing flow.
	scope map[string]bool

	// uncaught marks a Catch node that only fires when no other handler took
	// the error.
	uncaught bool

	// group is the id of the group containing the handler, "" if ungrouped.
	group string
}

// completeHandler is a Complete node and the nodes whose completion it observes.
type completeHandler struct {
	r     *runner
	scope map[string]bool
}

// graph is one immutable snapshot of what is running: the flow set, the runners
// built from it, and the routing tables resolved against those runners.
type graph struct {
	// flows is the graph that runs: the parsed flow file with every subflow
	// instance replaced by a copy of its template. For a flow set with no
	// subflows it is the parsed file itself.
	flows *engine.Flows

	// expansion carries what instantiating the subflows produced besides the
	// graph — the environment chain per node, and which scope contains which
	// instance.
	expansion *engine.Expansion

	// runners holds every started flow node, keyed by node id. Never written
	// once the snapshot is published, so delivery needs no lock.
	runners map[string]*runner

	// Routing tables, resolved against runners.
	catches   []*handler
	statuses  []*handler
	completes []*completeHandler

	// failures are the nodes that are configured to run and are not running,
	// with the reason. Kept with the graph rather than handed out once,
	// because a partial deploy leaves a broken node it did not touch exactly
	// as broken as it was, and the deploy response has to say so rather than
	// reporting a clean deploy over a node that still is not there.
	failures map[string]StartError
}

// New builds a runtime for a parsed flow set. Nothing starts until Start.
func New(reg *node.Registry, flows *engine.Flows, opts Options) *Runtime {
	rt := &Runtime{
		reg:         reg,
		opts:        opts.withDefaults(),
		configs:     map[string]node.Node{},
		configStops: map[string]context.CancelFunc{},
		contexts:    store.NewScopedContexts(),
		events:      make(chan Event, 1024),
	}
	// Subflows are instantiated here rather than at start, so a caller can read
	// the warnings before anything runs and so Start has one kind of graph to
	// walk instead of two.
	rt.cur.Store(newGraph(engine.ExpandSubflows(flows)))
	return rt
}

func newGraph(ex *engine.Expansion) *graph {
	return &graph{
		flows:     ex.Flows,
		expansion: ex,
		runners:   map[string]*runner{},
		failures:  map[string]StartError{},
	}
}

// graph returns the snapshot that is running now.
func (rt *Runtime) graph() *graph { return rt.cur.Load() }

// Warnings returns anything the subflow expansion found worth saying: a template
// declaring more inputs than a v1 wire can address, an output wired from a node
// that is not in the template, a subflow that contains itself.
func (rt *Runtime) Warnings() []string {
	if g := rt.graph(); g.expansion != nil {
		return g.expansion.Warnings
	}
	return nil
}

// Instances returns the scope id of every expanded subflow instance.
func (rt *Runtime) Instances() []string {
	if g := rt.graph(); g.expansion != nil {
		return g.expansion.Instances
	}
	return nil
}

// SetContexts replaces the context stores, which is how a persistent store is
// substituted for the default in-memory one.
func (rt *Runtime) SetContexts(c *store.ScopedContexts) { rt.contexts = c }

// SetCredentials installs the lookup used to hand a node its decrypted secrets.
func (rt *Runtime) SetCredentials(fn func(nodeID string) map[string]string) {
	rt.credentials = fn
}

// SetMetricsHooks installs optional observers for per-node timings.
func (rt *Runtime) SetMetricsHooks(execTime, queueLatency func(nodeID, typ string, d time.Duration)) {
	rt.onExecTime = execTime
	rt.onQueueLatency = queueLatency
}

// Events returns the channel the editor subscribes to. The channel is buffered;
// when it fills, events are dropped rather than blocking message delivery,
// because a slow editor must never be able to stall the plant floor.
func (rt *Runtime) Events() <-chan Event { return rt.events }

// StartError records a node that failed to build or start. A failure is scoped
// to the one node: the rest of the flow still runs, and the editor is told which
// node is broken. Failing the whole deploy because one node's config is wrong is
// how a single typo takes a line down.
type StartError struct {
	NodeID string
	Type   string
	Err    error
}

func (e StartError) Error() string {
	return fmt.Sprintf("node %s (%s): %v", e.NodeID, e.Type, e.Err)
}

// Start builds and starts every enabled node.
//
// It returns the per-node failures it tolerated. A non-nil slice does not mean
// the runtime failed to start.
func (rt *Runtime) Start(ctx context.Context) []StartError {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.started {
		return []StartError{{Err: fmt.Errorf("runtime already started")}}
	}
	rt.started = true
	rt.ctx, rt.cancel = context.WithCancel(ctx)

	ng := newGraph(rt.graph().expansion)

	// Configuration nodes first: a flow node's factory may look one up.
	failures := rt.startConfigs(ng, ng.flows.ConfigNodes())

	// Flow nodes. Build every runner before wiring any, because a wire may
	// point forward in file order.
	var fresh []*runner
	for _, id := range ng.flows.Order {
		n, ok := ng.flows.Nodes[id]
		if !ok || !ng.shouldRun(n) {
			continue
		}
		r := rt.newRunner(ng, n)
		if f, ok := rt.attachNew(ng, r, n); !ok {
			failures = append(failures, f)
			continue
		}
		ng.runners[id] = r
		fresh = append(fresh, r)
	}

	rt.wire(ng)
	rt.buildRoutingTables(ng)
	rt.cur.Store(ng)

	return append(failures, rt.launch(ng, fresh)...)
}

// shouldRun reports whether a flow node is meant to be running in this graph:
// not a config node, not disabled, and not on a disabled tab.
func (g *graph) shouldRun(n *engine.Node) bool {
	return !n.IsConfig && !n.Disabled && g.scopeEnabled(n.Z)
}

// scopeEnabled reports whether a node's containing tab is enabled. A node on a
// disabled tab is built into no runner at all.
func (g *graph) scopeEnabled(z string) bool {
	if z == "" {
		return true
	}
	if tab, ok := g.flows.Tabs[z]; ok {
		return !tab.Disabled
	}
	if _, ok := g.flows.Subflows[z]; ok {
		return true
	}
	// A node whose scope is missing entirely was already reported as a parse
	// warning. Do not start it.
	return false
}

// startConfigs builds the listed configuration nodes, each after any other
// listed config it points at, and makes each resolvable as soon as it exists so
// the next one can find it. Then it starts the ones that have work of their
// own; see startConfig.
func (rt *Runtime) startConfigs(g *graph, ids []string) []StartError {
	var failures []StartError
	var built []string
	for _, id := range configBuildOrder(g, ids) {
		n := g.flows.Nodes[id]
		if n.Disabled {
			continue
		}
		inst, err := rt.build(g, n)
		if err != nil {
			f := StartError{NodeID: id, Type: n.Type, Err: err}
			g.failures[id] = f
			failures = append(failures, f)
			continue
		}
		rt.configsMu.Lock()
		rt.configs[id] = inst
		rt.configsMu.Unlock()
		built = append(built, id)
	}
	for _, id := range built {
		if f, ok := rt.startConfig(g, id); !ok {
			failures = append(failures, f)
		}
	}
	return failures
}

// configBuildOrder puts each config after the configs it references, keeping
// file order otherwise.
//
// A config can point at another: an MQTT broker at its tls-config. File order
// says nothing about which comes first, and the editor writes whichever was
// created first, so a broker saved before its TLS settings would look the TLS
// config up before it existed and fail the deploy for no reason the user could
// see. A cycle, which no real config has, falls back to file order for
// whatever is left.
func configBuildOrder(g *graph, ids []string) []string {
	deps := make(map[string][]string, len(ids))
	for _, id := range ids {
		for _, other := range ids {
			if other != id && references(g.flows.Nodes[id].Raw, map[string]bool{other: true}, id) {
				deps[id] = append(deps[id], other)
			}
		}
	}
	out := make([]string, 0, len(ids))
	placed := make(map[string]bool, len(ids))
	for len(out) < len(ids) {
		progress := false
		for _, id := range ids {
			if placed[id] {
				continue
			}
			ready := true
			for _, d := range deps[id] {
				if !placed[d] {
					ready = false
					break
				}
			}
			if ready {
				out = append(out, id)
				placed[id] = true
				progress = true
			}
		}
		if !progress {
			for _, id := range ids {
				if !placed[id] {
					out = append(out, id)
					placed[id] = true
				}
			}
		}
	}
	return out
}

// attachNew builds the node instance for a runner. On failure the failure is
// recorded against the graph and returned.
func (rt *Runtime) attachNew(g *graph, r *runner, n *engine.Node) (StartError, bool) {
	inst, err := rt.build(g, n)
	if err != nil {
		f := StartError{NodeID: n.ID, Type: n.Type, Err: err}
		g.failures[n.ID] = f
		return f, false
	}
	r.attach(inst)
	return StartError{}, true
}

// launch starts the goroutines of freshly built runners, then their message
// sources. Sources start last so that no message can be produced before every
// consumer is draining. The graph must already be published, because a source
// may send the moment it starts.
func (rt *Runtime) launch(g *graph, fresh []*runner) []StartError {
	for _, r := range fresh {
		go r.loop()
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].id < fresh[j].id })
	var failures []StartError
	for _, r := range fresh {
		s, ok := r.node.(node.Starter)
		if !ok {
			continue
		}
		if err := s.Start(r.ctx, r.emitter()); err != nil {
			f := StartError{NodeID: r.id, Type: r.typ, Err: err}
			g.failures[r.id] = f
			failures = append(failures, f)
		}
	}
	return failures
}

// build instantiates one node from its flow entry.
func (rt *Runtime) build(g *graph, n *engine.Node) (node.Node, error) {
	if tmpl, isInstance := n.SubflowTemplateID(); isInstance {
		// The instance node survives expansion as the entry point: upstream
		// wires still target its id, and its own wires now point at the copies
		// of the nodes the template's input feeds. All it has to do is forward.
		//
		// Built here rather than registered as a node type because it has no
		// edit dialog, no palette entry and no compatibility story. Registering
		// it would put scheduler plumbing in the editor's palette and in the
		// compatibility matrix.
		if _, ok := g.flows.Subflows[tmpl]; !ok {
			return nil, fmt.Errorf("subflow template %q is missing", tmpl)
		}
		return subflowEntry{}, nil
	}

	reg, ok := rt.reg.Lookup(n.Type)
	if !ok {
		return nil, fmt.Errorf("unknown node type %q", n.Type)
	}
	files, err := rt.credentialFiles(n)
	if err != nil {
		return nil, err
	}
	return reg.New(&node.Definition{
		Node:     n,
		Services: &services{rt: rt, nodeID: n.ID, z: n.Z, fileCreds: files},
	})
}

// newRunner makes the runner for a flow node, without its node instance yet:
// see attach.
func (rt *Runtime) newRunner(g *graph, n *engine.Node) *runner {
	capacity := rt.opts.InboxCapacity
	overflow := rt.opts.Overflow

	// A node may override the defaults from its flow entry, so a known-bursty
	// branch can be given a deeper queue or a lossy policy without changing the
	// whole runtime.
	if c := n.PropInt("ew_inboxCapacity", 0); c > 0 {
		capacity = c
	}
	if p := OverflowPolicy(n.PropString("ew_overflow", "")); p.Valid() {
		overflow = p
	}

	ctx, cancel := context.WithCancel(rt.ctx)
	return &runner{
		id:       n.ID,
		typ:      n.Type,
		name:     n.Name,
		z:        n.Z,
		groups:   g.flows.GroupChain(n.ID),
		rt:       rt,
		ctx:      ctx,
		cancel:   cancel,
		inbox:    make(chan delivery, capacity),
		capacity: capacity,
		overflow: overflow,
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// wire resolves every node's wires from ids to runner pointers. Wires pointing
// at a node that was not started — unknown type, disabled, failed to build — are
// dropped here, which is why delivery itself never has to check.
func (rt *Runtime) wire(g *graph) {
	for id, r := range g.runners {
		n := g.flows.Nodes[id]
		wires := make([][]*runner, len(n.Wires))
		for port, targets := range n.Wires {
			for _, tid := range targets {
				if t, ok := g.runners[tid]; ok {
					wires[port] = append(wires[port], t)
				}
			}
		}
		r.wires.Store(&wires)
	}
}

// buildRoutingTables collects the Catch, Status and Complete nodes.
func (rt *Runtime) buildRoutingTables(g *graph) {
	g.catches, g.statuses, g.completes = nil, nil, nil
	for _, id := range g.sortedRunnerIDs() {
		r := g.runners[id]
		n := g.flows.Nodes[id]

		switch r.typ {
		case TypeCatch:
			g.catches = append(g.catches, &handler{
				r:        r,
				scope:    scopeSet(n),
				uncaught: n.PropBool("uncaught", false),
				group:    n.G,
			})
		case TypeStatus:
			g.statuses = append(g.statuses, &handler{
				r:     r,
				scope: scopeSet(n),
				group: n.G,
			})
		case TypeComplete:
			g.completes = append(g.completes, &completeHandler{r: r, scope: scopeSet(n)})
		}
	}
}

// scopeSet reads a handler node's "scope" property: a list of node ids it
// watches, or absent/null meaning the whole containing flow.
func scopeSet(n *engine.Node) map[string]bool {
	raw, ok := n.Raw["scope"]
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	set := make(map[string]bool, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			set[s] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

func (g *graph) sortedRunnerIDs() []string {
	return sortedKeys(g.runners)
}

// Stop shuts the runtime down: sources first so nothing new is produced, then
// inboxes drain, then resources are released.
func (rt *Runtime) Stop(ctx context.Context) []error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.started || rt.stopped {
		return nil
	}
	rt.stopped = true

	// Cancel first so Starter goroutines wind down and stop producing.
	rt.cancel()

	g := rt.graph()
	runners := make([]*runner, 0, len(g.runners))
	for _, id := range g.sortedRunnerIDs() {
		runners = append(runners, g.runners[id])
	}
	rt.configsMu.RLock()
	configIDs := sortedKeys(rt.configs)
	rt.configsMu.RUnlock()

	errs := rt.retire(ctx, runners, configIDs, nil)

	rt.eventsMu.Lock()
	rt.eventsClosed = true
	close(rt.events)
	rt.eventsMu.Unlock()

	return errs
}

// retire takes a set of runners and configuration nodes out of service: their
// sources stop, the work already moving between them finishes, their
// goroutines exit, and then their resources are released, flow nodes before
// the configuration nodes they lean on. A full stop retires everything; a
// partial deploy retires only what changed.
//
// removed marks the nodes that are gone from the flow altogether rather than
// being replaced, which is what Close's removed argument tells a node.
func (rt *Runtime) retire(ctx context.Context, runners []*runner, configIDs []string, removed map[string]bool) []error {
	closeCtx, cancel := context.WithTimeout(ctx, rt.opts.CloseTimeout)
	defer cancel()

	for _, r := range runners {
		r.cancel()
	}

	// Then let the graph go quiet before signalling anyone to exit.
	//
	// Without this, runners stop concurrently and a downstream node can drain
	// its inbox to empty and exit while an upstream node still has messages to
	// forward — the work is accepted into a channel nobody is reading any more
	// and is silently lost. Waiting for every node to be simultaneously idle is
	// what makes "a redeploy finishes work in flight" true rather than
	// usually-true.
	rt.quiesce(closeCtx, runners)

	var errs []error
	var wg sync.WaitGroup
	for _, r := range runners {
		wg.Add(1)
		go func(r *runner) {
			defer wg.Done()
			r.stop(closeCtx)
		}(r)
	}
	wg.Wait()

	// Close nodes after their inboxes have drained, so a node still holds its
	// resources while it finishes the work already queued for it.
	for _, r := range runners {
		if c, ok := r.node.(node.Closer); ok {
			if err := c.Close(closeCtx, removed[r.id]); err != nil {
				errs = append(errs, StartError{NodeID: r.id, Type: r.typ, Err: err})
			}
		}
	}
	for _, id := range configIDs {
		rt.configsMu.Lock()
		inst, ok := rt.configs[id]
		delete(rt.configs, id)
		stop := rt.configStops[id]
		delete(rt.configStops, id)
		rt.configsMu.Unlock()
		if stop != nil {
			stop()
		}
		if !ok {
			continue
		}
		if c, ok := inst.(node.Closer); ok {
			if err := c.Close(closeCtx, removed[id]); err != nil {
				errs = append(errs, StartError{NodeID: id, Err: err})
			}
		}
	}
	return errs
}

// quiesce waits until every listed runner is simultaneously idle — nothing
// queued and nothing inside a handler — so that messages still moving between
// nodes reach their destination before anything shuts down.
//
// A flow containing a self-sustaining cycle never goes quiet. That is what the
// context deadline is for: shutdown proceeds anyway rather than hanging, and the
// close timeout bounds how long a deploy can be held up by one runaway loop.
func (rt *Runtime) quiesce(ctx context.Context, runners []*runner) {
	const pollInterval = 250 * time.Microsecond
	for {
		allIdle := true
		for _, r := range runners {
			if !r.idle() {
				allIdle = false
				break
			}
		}
		if allIdle {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}
}

// salvage empties the inbox of a runner whose goroutine has exited.
//
// A partial deploy replaces a node while its upstream keeps running, and a
// sender that picked up the old wire a moment before the swap can still deliver
// to the old runner after it has gone. Those messages belong to the runner that
// replaced it, so they go there. A node that was deleted outright has nowhere
// for them to go, and they are counted and announced as dropped rather than
// left in a channel nobody will ever read.
func (rt *Runtime) salvage(old *runner) {
	for {
		select {
		case d := <-old.inbox:
			if next, ok := rt.graph().runners[old.id]; ok && next != old && !next.exited.Load() {
				// enqueue counts it again for the runner it moves to.
				next.enqueue(rt.ctx, d.msg, nil)
				rt.outstanding.Add(-1)
				continue
			}
			rt.outstanding.Add(-1)
			old.stats.Dropped.Add(1)
			rt.onDropped(old, d.msg, "node-stopped")
		default:
			return
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// deliver sends a message from one node's output port to everything wired to it.
//
// Every recipient gets its own clone. Node-RED hands the first recipient the
// original and clones only for the rest, which makes the last branch wired share
// mutable state with the sender — a documented memory optimisation and an
// undocumented source of aliasing bugs. The cost of cloning unconditionally is
// bounded by engine.ImmutableBytes, which shares rather than copies the large
// binary payloads where it would actually hurt.
func (rt *Runtime) deliver(from *runner, port int, msg *engine.Msg) {
	if o := rt.observer; o != nil {
		msg.EnsureID()
		o.Sent(from.id, port, msg.Clone())
	}
	wp := from.wires.Load()
	if wp == nil || port < 0 || port >= len(*wp) {
		return
	}
	targets := (*wp)[port]
	if len(targets) == 0 {
		return
	}
	msg.EnsureID()
	from.stats.Sent.Add(1)
	for _, t := range targets {
		t.enqueue(rt.ctx, msg.Clone(), from)
	}
}

// Inject pushes a message into a node from outside the graph. Used by the
// editor's manual Inject button and by tests.
func (rt *Runtime) Inject(nodeID string, msg *engine.Msg) error {
	r, ok := rt.graph().runners[nodeID]
	if !ok {
		return fmt.Errorf("node %s is not running", nodeID)
	}
	msg.EnsureID()
	r.enqueue(rt.ctx, msg, nil)
	return nil
}

// raiseError routes an error to the nearest eligible Catch node.
//
// Selection follows Node-RED: candidates are Catch nodes in the same flow whose
// scope covers the failing node; among those, the one closest in the group
// hierarchy wins, so a Catch inside a group beats a Catch on the tab. Catch
// nodes marked "uncaught" fire only when nothing else did.
func (rt *Runtime) raiseError(from *runner, err error, msg *engine.Msg) {
	if from == nil {
		rt.emit(Event{Topic: TopicError, At: rt.opts.Now(), Data: map[string]any{
			"error": err.Error(),
		}})
		return
	}

	depth := 0
	if msg != nil {
		if e, ok := msg.Data["_errorDepth"].(float64); ok {
			depth = int(e)
		}
	}
	if depth >= maxErrorDepth {
		// A Catch wired back into the flow it catches for is an easy infinite
		// loop. Break it and say so, rather than spinning a core.
		rt.emit(Event{Topic: TopicError, At: rt.opts.Now(), Data: map[string]any{
			"nodeId": from.id, "type": from.typ,
			"error":   err.Error(),
			"dropped": fmt.Sprintf("error loop exceeded %d hops", maxErrorDepth),
		}})
		return
	}

	rt.emit(Event{Topic: TopicError, At: rt.opts.Now(), Data: map[string]any{
		"nodeId": from.id, "type": from.typ, "name": from.name,
		"error": err.Error(),
	}})

	g := rt.graph()
	targets := rt.selectHandlers(g.catches, from)
	if len(targets) == 0 {
		// Nothing inside this scope handled it. If the scope is a subflow
		// instance, the flow that called it gets the chance — otherwise an
		// error inside a subflow is invisible to the flow using it, and the
		// Catch node the author put on the tab never fires.
		targets = rt.selectHandlersInCallingFlow(g, from)
	}
	for _, h := range targets {
		var out *engine.Msg
		if msg != nil {
			out = msg.Clone()
		} else {
			out = engine.NewMsg()
		}
		out.Data["error"] = map[string]any{
			"message": err.Error(),
			"source": map[string]any{
				"id":   from.id,
				"type": from.typ,
				"name": from.name,
				"count": func() int {
					return depth + 1
				}(),
			},
		}
		out.Data["_errorDepth"] = float64(depth + 1)
		h.r.enqueue(rt.ctx, out, from)
	}
}

// selectHandlersInCallingFlow walks out through the subflow instances containing
// a node, looking for a Catch node in an enclosing flow.
//
// Node-RED propagates an uncaught subflow error to the parent this way. The
// handler is selected against the instance node rather than the failing node, so
// the group-distance rule is applied where the subflow actually sits on the
// calling tab.
func (rt *Runtime) selectHandlersInCallingFlow(g *graph, from *runner) []*handler {
	if g.expansion == nil {
		return nil
	}
	scope := from.z
	for range maxSubflowDepth {
		parent, ok := g.expansion.ParentScope[scope]
		if !ok {
			return nil
		}
		// An instance's scope id is the instance node's id, so the runner
		// standing in for the subflow on the calling tab is found directly.
		caller, ok := g.runners[scope]
		if !ok {
			return nil
		}
		if targets := rt.selectHandlers(g.catches, caller); len(targets) > 0 {
			return targets
		}
		scope = parent
	}
	return nil
}

// maxSubflowDepth bounds the walk out of nested subflows. It matches the bound
// the expansion applies, so a graph that expanded at all cannot loop here.
const maxSubflowDepth = 32

// subflowEntry forwards a message into a subflow instance's internals.
type subflowEntry struct{}

func (subflowEntry) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	out.Send(0, m)
	return nil
}

// onStatus routes a status change to Status nodes watching the reporting node,
// and publishes it for the editor.
func (rt *Runtime) onStatus(from *runner, s node.Status) {
	rt.emit(Event{Topic: TopicStatus, At: rt.opts.Now(), Data: map[string]any{
		"nodeId": from.id,
		"fill":   s.Fill, "shape": s.Shape, "text": s.Text,
		"cleared": s.Cleared(),
	}})

	for _, h := range rt.selectHandlers(rt.graph().statuses, from) {
		m := engine.NewMsg()
		m.Data["status"] = map[string]any{
			"fill": s.Fill, "shape": s.Shape, "text": s.Text,
			"source": map[string]any{"id": from.id, "type": from.typ, "name": from.name},
		}
		h.r.enqueue(rt.ctx, m, from)
	}
}

// onComplete routes a completion to Complete nodes watching the finished node.
func (rt *Runtime) onComplete(from *runner, msg *engine.Msg, err error) {
	if err != nil {
		rt.raiseError(from, err, msg)
		return
	}
	for _, h := range rt.graph().completes {
		if h.scope == nil || !h.scope[from.id] {
			// Unlike Catch, a Complete node with no scope watches nothing.
			// Node-RED requires an explicit selection here, and defaulting to
			// "everything" would flood the flow.
			continue
		}
		var out *engine.Msg
		if msg != nil {
			out = msg.Clone()
		} else {
			out = engine.NewMsg()
		}
		h.r.enqueue(rt.ctx, out, from)
	}
}

// selectHandlers picks the eligible handlers closest to the reporting node.
//
// Distance is measured in the group hierarchy: 0 means the handler sits in the
// same innermost group as the failing node, higher numbers mean progressively
// more enclosing groups, and a handler at flow level is furthest. A handler in a
// group that does not contain the failing node is not eligible at all.
func (rt *Runtime) selectHandlers(all []*handler, from *runner) []*handler {
	if len(all) == 0 {
		return nil
	}

	chain := from.groups // innermost first
	depthOf := func(group string) (int, bool) {
		if group == "" {
			// Flow level: eligible for everything in the flow, but furthest.
			return len(chain), true
		}
		for i, g := range chain {
			if g == group {
				return i, true
			}
		}
		return 0, false
	}

	best := -1
	var chosen []*handler
	var uncaught []*handler

	for _, h := range all {
		if h.r.z != from.z {
			continue
		}
		if h.r.id == from.id {
			// A Catch node erroring must not catch itself.
			continue
		}
		if h.scope != nil && !h.scope[from.id] {
			continue
		}
		d, ok := depthOf(h.group)
		if !ok {
			continue
		}
		if h.uncaught {
			uncaught = append(uncaught, h)
			continue
		}
		switch {
		case best == -1 || d < best:
			best = d
			chosen = []*handler{h}
		case d == best:
			chosen = append(chosen, h)
		}
	}

	if len(chosen) > 0 {
		return chosen
	}
	return uncaught
}

// onDropped publishes a dropped-message event. Silently discarding a message is
// how a flow appears healthy while losing data; every drop is counted and
// announced.
func (rt *Runtime) onDropped(r *runner, msg *engine.Msg, policy string) {
	rt.emit(Event{Topic: TopicDropped, At: rt.opts.Now(), Data: map[string]any{
		"nodeId": r.id, "type": r.typ, "policy": policy,
		"msgId": msg.ID(), "queueCap": r.capacity,
	}})
}

func (rt *Runtime) log(r *runner, level node.LogLevel, format string, args ...any) {
	rt.emit(Event{Topic: TopicLog, At: rt.opts.Now(), Data: map[string]any{
		"nodeId": r.id, "type": r.typ, "name": r.name,
		"level": string(level), "message": fmt.Sprintf(format, args...),
	}})
}

// Publish sends an arbitrary event to subscribers. The Debug node uses it.
func (rt *Runtime) Publish(e Event) { rt.emit(e) }

// emit queues an event, dropping it if the channel is full.
//
// A slow or absent editor must never apply back-pressure to message delivery.
// Node-RED's comms layer has the opposite problem: every connected editor is
// subscribed to everything and cannot unsubscribe.
func (rt *Runtime) emit(e Event) {
	if e.At.IsZero() {
		e.At = rt.opts.Now()
	}
	rt.eventsMu.Lock()
	defer rt.eventsMu.Unlock()
	if rt.eventsClosed {
		return
	}
	select {
	case rt.events <- e:
	default:
	}
}

func (rt *Runtime) observeExecTime(r *runner, d time.Duration) {
	if rt.onExecTime != nil {
		rt.onExecTime(r.id, r.typ, d)
	}
}

func (rt *Runtime) observeQueueLatency(r *runner, d time.Duration) {
	if rt.onQueueLatency != nil {
		rt.onQueueLatency(r.id, r.typ, d)
	}
}

// Snapshots returns per-node counters for every running node, sorted by id.
func (rt *Runtime) Snapshots() []Snapshot {
	g := rt.graph()
	out := make([]Snapshot, 0, len(g.runners))
	for _, id := range g.sortedRunnerIDs() {
		out = append(out, g.runners[id].Snapshot())
	}
	return out
}

// NodeStatus returns the retained badge for a node.
func (rt *Runtime) NodeStatus(nodeID string) (node.Status, bool) {
	r, ok := rt.graph().runners[nodeID]
	if !ok {
		return node.Status{}, false
	}
	return r.currentStatus(), true
}

// RunningIDs lists every flow node and configuration node that is running,
// sorted.
func (rt *Runtime) RunningIDs() []string {
	ids := rt.graph().sortedRunnerIDs()
	rt.configsMu.RLock()
	for id := range rt.configs {
		ids = append(ids, id)
	}
	rt.configsMu.RUnlock()
	sort.Strings(ids)
	return ids
}

// Running reports whether a node id has a live runner.
func (rt *Runtime) Running(nodeID string) bool {
	_, ok := rt.graph().runners[nodeID]
	return ok
}

// services implements node.Services for one node instance.
type services struct {
	rt     *Runtime
	nodeID string
	z      string

	// fileCreds are the credentials read from ew_credentialFiles when the
	// node was built. A file wins over the store for the same key: naming a
	// file is the more deliberate act.
	fileCreds map[string]string
}

var _ node.Services = (*services)(nil)

func (s *services) Context(scope node.ContextScope) node.Context {
	switch scope {
	case node.ScopeGlobal:
		return s.rt.contexts.Global()
	case node.ScopeFlow:
		return s.rt.contexts.Flow(s.z)
	default:
		return s.rt.contexts.Node(s.nodeID)
	}
}

func (s *services) Credential(key string) (string, bool) {
	if v, ok := s.fileCreds[key]; ok {
		return v, true
	}
	if s.rt.credentials == nil {
		return "", false
	}
	creds := s.rt.credentials(s.nodeID)
	v, ok := creds[key]
	return v, ok
}

// StandIn is what node.StandInOf finds: the node's stand-in for the outside
// world under a flow test, and nil the rest of the time.
func (s *services) StandIn() node.StandIn {
	if s.rt.standIns == nil {
		return nil
	}
	return s.rt.standIns(s.nodeID)
}

func (s *services) ConfigNode(id string) (node.Node, bool) {
	s.rt.configsMu.RLock()
	defer s.rt.configsMu.RUnlock()
	n, ok := s.rt.configs[id]
	return n, ok
}

// Env resolves an environment variable, innermost scope first.
//
// For a node inside a subflow that means the instance's own properties, then the
// template's defaults, then the properties of whatever instance contains that
// one, and so on out to the tab and finally the process environment. This is
// Node-RED's _path resolution, and it is the mechanism that makes one template
// behave differently per instance — without it, subflow properties are
// decoration.
func (s *services) Env(name string) (string, bool) {
	g := s.rt.graph()
	if g.expansion != nil {
		for _, scope := range g.expansion.EnvChains[s.nodeID] {
			if v, ok := lookupEnvVar(scope.Vars, name); ok {
				return v, true
			}
		}
	}
	if tab, ok := g.flows.Tabs[s.z]; ok {
		if v, ok := lookupEnvVar(tab.Env, name); ok {
			return v, true
		}
	}
	if sf, ok := g.flows.Subflows[s.z]; ok {
		if v, ok := lookupEnvVar(sf.Env, name); ok {
			return v, true
		}
	}
	return lookupProcessEnv(name)
}

// lookupEnvVar reads one variable out of a scope.
//
// A variable typed "env" is an indirection: the instance said "take this from
// the environment of whoever called me", which is how a nested subflow forwards
// a property it was handed without knowing its value. Answering here would
// return the name of the variable instead of its value, so the next frame out
// gets the question.
func lookupEnvVar(vars []engine.EnvVar, name string) (string, bool) {
	for _, ev := range vars {
		if ev.Name != name {
			continue
		}
		if ev.Type == node.TypeEnv || ev.Value == nil {
			return "", false
		}
		return fmt.Sprint(ev.Value), true
	}
	return "", false
}

func (s *services) Log(level node.LogLevel, msg string, args ...any) {
	if r, ok := s.rt.graph().runners[s.nodeID]; ok {
		s.rt.log(r, level, msg, args...)
		return
	}
	s.rt.emit(Event{Topic: TopicLog, At: s.rt.opts.Now(), Data: map[string]any{
		"nodeId": s.nodeID, "level": string(level), "message": fmt.Sprintf(msg, args...),
	}})
}
