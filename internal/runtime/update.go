package runtime

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// Partial deploy.
//
// A full deploy stops every node and starts every node. That is simple, and on a
// plant floor it is a small outage every time somebody saves: every MQTT session
// drops and reconnects, every TCP listener closes, every Delay node flushes, and
// a client that was connected to an HTTP In endpoint gets a 503. Operators learn
// not to deploy during a shift, which means fixes wait for the weekend.
//
// Update restarts only what the deploy changed. A node whose settings, scope,
// group, environment and credentials are all the same keeps its goroutine, its
// inbox, its connections and its counters, and is only rewired if its wires
// moved. Everything that did change is retired the same careful way a full stop
// retires everything, and its replacement is started in its place.

// DeployMode says how much of the running graph a deploy restarts. The names are
// the values of Node-RED's Node-RED-Deployment-Type header, so a script written
// against that admin API means the same thing here.
type DeployMode string

const (
	// DeployFull stops everything and starts the new flow set from scratch.
	// Update does not do it: a full deploy is a Stop and a Start of a fresh
	// runtime, exactly what a process restart does, and the caller owns that.
	DeployFull DeployMode = "full"

	// DeployNodes restarts the nodes that changed and nothing else. The
	// default, because it is the one that leaves the rest of the plant alone.
	DeployNodes DeployMode = "nodes"

	// DeployFlows restarts every node on any flow that has a change in it, and
	// leaves the other flows alone. For a flow whose nodes share state in a way
	// a node-by-node restart would split.
	DeployFlows DeployMode = "flows"
)

// ParseDeployMode reads a deployment type. Empty means DeployNodes.
func ParseDeployMode(s string) (DeployMode, error) {
	switch m := DeployMode(s); m {
	case "":
		return DeployNodes, nil
	case DeployFull, DeployNodes, DeployFlows:
		return m, nil
	}
	return "", fmt.Errorf("unknown deployment type %q: use full, nodes or flows", s)
}

// UpdateOptions are what a deploy knows that the flow file does not.
type UpdateOptions struct {
	Mode DeployMode

	// Credentials lists the nodes whose credentials changed in this deploy.
	// Credentials never live in the flow file, so the graph alone cannot see
	// that a broker password changed, and a broker that kept running on the
	// old password would look deployed and keep failing to authenticate.
	Credentials map[string]bool
}

// UpdateResult says what a deploy did to the running graph. Node ids include
// configuration nodes, because a restarted broker connection is the thing an
// operator most wants to know about.
type UpdateResult struct {
	Mode DeployMode `json:"type"`

	// Started were not running before and are now.
	Started []string `json:"started"`
	// Restarted were running, and were stopped and started on new settings.
	Restarted []string `json:"restarted"`
	// Stopped were running and are not any more: deleted, or disabled.
	Stopped []string `json:"stopped"`
	// Unchanged counts the nodes left running exactly as they were.
	Unchanged int `json:"unchanged"`

	// Failures are every node that is meant to run and is not, including ones
	// this deploy did not touch.
	Failures []StartError `json:"-"`
	// CloseErrors are what retired nodes said when they were closed.
	CloseErrors []error `json:"-"`
}

// Update swaps a new flow set in, restarting only what changed.
//
// Messages already queued at a node that is being replaced are finished by the
// old instance. Messages that arrive for it while the swap is in progress queue
// for the new one, which starts on them as soon as the old one has let go of
// whatever the two would otherwise fight over: a listening port, an MQTT client
// id, an HTTP route. Nothing that was sent is lost quietly; a message for a node
// that was deleted is counted and announced as dropped.
func (rt *Runtime) Update(ctx context.Context, flows *engine.Flows, opts UpdateOptions) (UpdateResult, error) {
	if opts.Mode != DeployNodes && opts.Mode != DeployFlows {
		return UpdateResult{}, fmt.Errorf("a %q deploy is not a partial deploy: stop this runtime and start a new one", opts.Mode)
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.started || rt.stopped {
		return UpdateResult{}, fmt.Errorf("the runtime is not running")
	}

	old := rt.graph()
	next := newGraph(engine.ExpandSubflows(flows))
	p := rt.plan(old, next, opts)

	// Stage one: the new graph goes live around the nodes that are changing.
	// Survivors are rewired onto it and keep running, and every node that is
	// about to start gets its runner now, so anything sent to it from here on
	// waits in its inbox instead of reaching the old instance or nothing.
	for id, r := range p.keep {
		next.runners[id] = r
	}
	shells := make([]*runner, 0, len(p.start))
	for _, id := range p.start {
		r := rt.newRunner(next, next.flows.Nodes[id])
		next.runners[id] = r
		shells = append(shells, r)
	}
	for id, f := range old.failures {
		if _, kept := p.keep[id]; kept {
			next.failures[id] = f
		}
	}
	rt.wire(next)
	rt.buildRoutingTables(next)
	rt.cur.Store(next)

	// Stage two: retire what changed or went away.
	p.result.CloseErrors = rt.retire(ctx, p.retire, p.retireConfigs, p.removed)

	// Stage three: build the replacements, config nodes first because a flow
	// node's factory may look one up.
	rt.startConfigs(next, p.startConfigs)
	var fresh, dead []*runner
	for _, r := range shells {
		if _, ok := rt.attachNew(next, r, next.flows.Nodes[r.id]); !ok {
			dead = append(dead, r)
			continue
		}
		fresh = append(fresh, r)
	}
	if len(dead) > 0 {
		// A node that failed to build has no runner in a full start, and wires
		// to it are dropped. Same here, and whatever queued for it during the
		// swap is announced as dropped rather than held by a runner that will
		// never run.
		final := *next
		final.runners = make(map[string]*runner, len(next.runners))
		for id, r := range next.runners {
			final.runners[id] = r
		}
		for _, r := range dead {
			delete(final.runners, r.id)
		}
		rt.wire(&final)
		rt.buildRoutingTables(&final)
		rt.cur.Store(&final)
		next = &final
		for _, r := range dead {
			r.cancel()
			r.exited.Store(true)
			rt.salvage(r)
		}
	}

	rt.launch(next, fresh)
	p.result.Failures = next.failureList()
	return p.result, nil
}

// plan is what an update will do.
type plan struct {
	// keep maps the id of every node left running to its runner.
	keep map[string]*runner
	// start lists the flow nodes to build, in file order.
	start []string
	// retire lists the runners going away, sorted by id.
	retire []*runner

	startConfigs  []string // in file order
	retireConfigs []string // sorted

	// removed marks retired ids that do not come back, for Close.
	removed map[string]bool

	result UpdateResult
}

// plan compares the running graph with the one about to replace it.
func (rt *Runtime) plan(old, next *graph, opts UpdateOptions) plan {
	p := plan{
		keep:    map[string]*runner{},
		removed: map[string]bool{},
		result:  UpdateResult{Mode: opts.Mode},
	}
	credsChanged := func(id string) bool {
		if opts.Credentials[id] {
			return true
		}
		// A copy inside a subflow instance carries its template node's id
		// after the separator.
		_, tid, ok := engine.SplitDerivedID(id)
		return ok && opts.Credentials[tid]
	}

	// Configuration nodes. dirty collects every config id that is not simply
	// carrying on: new, changed, deleted, or broken last time.
	rt.configsMu.RLock()
	running := make(map[string]bool, len(rt.configs))
	for id := range rt.configs {
		running[id] = true
	}
	rt.configsMu.RUnlock()

	want := map[string]bool{}
	for _, id := range next.flows.ConfigNodes() {
		if !next.flows.Nodes[id].Disabled {
			want[id] = true
		}
	}
	dirty := map[string]bool{}
	keepConfigs := map[string]bool{}
	for id := range running {
		if !want[id] {
			dirty[id] = true
		}
	}
	for id := range want {
		if !running[id] || credsChanged(id) || !sameNode(old, next, old.flows.Nodes[id], next.flows.Nodes[id]) {
			dirty[id] = true
			continue
		}
		keepConfigs[id] = true
	}
	// A config node leaning on one that changed has to be rebuilt too, and so
	// does whatever leans on that: a broker config that names a TLS config.
	for changed := true; changed; {
		changed = false
		for id := range keepConfigs {
			if references(next.flows.Nodes[id].Raw, dirty, id) {
				delete(keepConfigs, id)
				dirty[id] = true
				changed = true
			}
		}
	}

	// Flow nodes. A node survives when it was running, is meant to run, and
	// nothing it depends on moved: its own settings, its scope and group, its
	// environment, its credentials, and every config node it names.
	for _, id := range next.flows.Order {
		n, ok := next.flows.Nodes[id]
		if !ok || !next.shouldRun(n) {
			continue
		}
		r, wasRunning := old.runners[id]
		if wasRunning && !credsChanged(id) && sameNode(old, next, old.flows.Nodes[id], n) &&
			!references(n.Raw, dirty, "") {
			p.keep[id] = r
		}
	}

	if opts.Mode == DeployFlows {
		// Anything on a flow with a change in it restarts with it.
		touched := map[string]bool{}
		for _, id := range next.flows.Order {
			if n, ok := next.flows.Nodes[id]; ok && next.shouldRun(n) {
				if _, kept := p.keep[id]; !kept {
					touched[next.flowOf(n)] = true
				}
			}
		}
		for id := range old.runners {
			if _, kept := p.keep[id]; !kept {
				touched[old.flowOf(old.flows.Nodes[id])] = true
			}
		}
		for id := range p.keep {
			if touched[next.flowOf(next.flows.Nodes[id])] {
				delete(p.keep, id)
			}
		}
	}

	starting := map[string]bool{}
	for _, id := range next.flows.Order {
		n, ok := next.flows.Nodes[id]
		if !ok || !next.shouldRun(n) {
			continue
		}
		if _, kept := p.keep[id]; kept {
			continue
		}
		p.start = append(p.start, id)
		starting[id] = true
		if _, was := old.runners[id]; was {
			p.result.Restarted = append(p.result.Restarted, id)
		} else {
			p.result.Started = append(p.result.Started, id)
		}
	}
	for _, id := range old.sortedRunnerIDs() {
		if _, kept := p.keep[id]; kept {
			continue
		}
		p.retire = append(p.retire, old.runners[id])
		if !starting[id] {
			p.removed[id] = true
			p.result.Stopped = append(p.result.Stopped, id)
		}
	}

	for _, id := range next.flows.ConfigNodes() {
		if !want[id] || keepConfigs[id] {
			continue
		}
		p.startConfigs = append(p.startConfigs, id)
		if running[id] {
			p.result.Restarted = append(p.result.Restarted, id)
		} else {
			p.result.Started = append(p.result.Started, id)
		}
	}
	for _, id := range sortedKeys(running) {
		if keepConfigs[id] {
			continue
		}
		p.retireConfigs = append(p.retireConfigs, id)
		if !want[id] {
			p.removed[id] = true
			p.result.Stopped = append(p.result.Stopped, id)
		}
	}

	p.result.Unchanged = len(p.keep) + len(keepConfigs)
	return p
}

// layoutKeys are the properties that only say where a node is drawn and what
// it is wired to. Moving a node is not a reason to restart it, and wires are
// applied to a running node without one. The same three keys Node-RED leaves
// out when it decides what a deploy changed.
var layoutKeys = map[string]bool{"x": true, "y": true, "wires": true}

// sameNode reports whether a node would run identically in both graphs, so it
// can be left running across the deploy.
func sameNode(og, ng *graph, a, b *engine.Node) bool {
	if a == nil || b == nil {
		return false
	}
	if a.Type != b.Type || a.Z != b.Z || a.G != b.G || a.IsConfig != b.IsConfig || a.Disabled != b.Disabled {
		return false
	}
	if !sameSettings(a.Raw, b.Raw) {
		return false
	}
	// Catch and Status routing reads the group chain off the runner, so a node
	// whose enclosing groups changed restarts rather than routing errors by
	// where it used to be.
	if !slices.Equal(og.flows.GroupChain(a.ID), ng.flows.GroupChain(b.ID)) {
		return false
	}
	return reflect.DeepEqual(og.envOf(a), ng.envOf(b))
}

// sameSettings compares two flow entries, ignoring layout.
func sameSettings(a, b map[string]any) bool {
	count := 0
	for k, av := range a {
		if layoutKeys[k] {
			continue
		}
		bv, ok := b[k]
		if !ok || !reflect.DeepEqual(av, bv) {
			return false
		}
		count++
	}
	for k := range b {
		if !layoutKeys[k] {
			count--
		}
	}
	return count == 0
}

// nodeEnv is everything Env would consult for a node before the process
// environment.
type nodeEnv struct {
	chain []engine.EnvScope
	tab   []engine.EnvVar
	sub   []engine.EnvVar
}

func (g *graph) envOf(n *engine.Node) nodeEnv {
	var e nodeEnv
	if g.expansion != nil {
		e.chain = g.expansion.EnvChains[n.ID]
	}
	if t, ok := g.flows.Tabs[n.Z]; ok {
		e.tab = t.Env
	}
	if s, ok := g.flows.Subflows[n.Z]; ok {
		e.sub = s.Env
	}
	return e
}

// flowOf returns the top-level tab a node runs on, walking out of any subflow
// instances it sits inside.
func (g *graph) flowOf(n *engine.Node) string {
	if n == nil {
		return ""
	}
	z := n.Z
	for range maxSubflowDepth {
		parent, ok := g.expansion.ParentScope[z]
		if !ok {
			break
		}
		z = parent
	}
	return z
}

// references reports whether any string anywhere in v names an id in set,
// other than self. Config node references are plain id strings in a node's
// properties, sometimes nested a level down, so the whole value is walked.
func references(v any, set map[string]bool, self string) bool {
	switch t := v.(type) {
	case string:
		return t != self && set[t]
	case map[string]any:
		for _, e := range t {
			if references(e, set, self) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if references(e, set, self) {
				return true
			}
		}
	}
	return false
}

// failureList returns the standing failures in file order.
func (g *graph) failureList() []StartError {
	if len(g.failures) == 0 {
		return nil
	}
	pos := make(map[string]int, len(g.flows.Order))
	for i, id := range g.flows.Order {
		pos[id] = i
	}
	out := make([]StartError, 0, len(g.failures))
	for _, f := range g.failures {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		pi, pj := pos[out[i].NodeID], pos[out[j].NodeID]
		if pi != pj {
			return pi < pj
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}
