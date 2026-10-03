package flowtest

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/jsonata"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// Status is how a test came out.
type Status string

const (
	// Pass means every expectation was met.
	Pass Status = "pass"
	// Fail means the flow ran and did something other than what was expected.
	Fail Status = "fail"
	// Error means the test couldn't be run as written: a node it names isn't
	// in the flow, or a node in the flow wouldn't start. That's a broken test
	// or a broken flow, and either way it isn't a pass.
	Error Status = "error"
)

// Options control a run.
type Options struct {
	// Registry holds the node types. Nil means the built-in palette.
	Registry *node.Registry

	// Runtime tunes the scheduler each test runs on, the same settings a
	// deploy gets, so a test sees the queues the flow will really have.
	Runtime runtime.Options

	// Match, when set, runs only the tests whose names it matches.
	Match *regexp.Regexp
}

// Report is the outcome of one test file.
type Report struct {
	// File is the test file, as the caller named it.
	File string `json:"file,omitempty"`

	Tests   []Result `json:"tests"`
	Passed  int      `json:"passed"`
	Failed  int      `json:"failed"`
	Errored int      `json:"errored"`
	Seconds float64  `json:"seconds"`
}

// OK reports whether every test passed.
func (r *Report) OK() bool { return r.Failed == 0 && r.Errored == 0 }

// Result is the outcome of one test.
type Result struct {
	Name    string  `json:"name"`
	Status  Status  `json:"status"`
	Seconds float64 `json:"seconds"`

	// Problems says why a test didn't pass, one entry per unmet expectation.
	Problems []Problem `json:"problems,omitempty"`

	// Log is every error the flow raised and every warning it logged while the
	// test ran, caught or not. Not a verdict: a test can expect an error and
	// pass. It's what you read when a test fails and the reason isn't obvious.
	Log []string `json:"log,omitempty"`
}

// Problem is one reason a test didn't pass.
type Problem struct {
	// Expect is which expectation, counting from 1. Zero is the test itself,
	// like a node that wouldn't start.
	Expect int `json:"expect,omitempty"`

	// Node is the id of the node the problem is about, so the editor can put
	// it on the canvas.
	Node string `json:"node,omitempty"`

	Message string `json:"message"`
}

func (p Problem) String() string {
	if p.Expect > 0 {
		return fmt.Sprintf("expect %d: %s", p.Expect, p.Message)
	}
	return p.Message
}

// Run runs every test in a suite against a flow file.
func Run(ctx context.Context, flows []byte, s *Suite, opts Options) Report {
	if opts.Registry == nil {
		opts.Registry = node.Default
	}
	began := time.Now()
	var rep Report
	for i := range s.Tests {
		c := &s.Tests[i]
		if opts.Match != nil && !opts.Match.MatchString(c.Name) {
			continue
		}
		res := runCase(ctx, flows, c, opts)
		switch res.Status {
		case Pass:
			rep.Passed++
		case Fail:
			rep.Failed++
		default:
			rep.Errored++
		}
		rep.Tests = append(rep.Tests, res)
	}
	rep.Seconds = time.Since(began).Seconds()
	return rep
}

// tap is one place a message can be seen: a node's output port, or, with port
// -1, what a node received.
type tap struct {
	node string
	port int
}

// target is a node an expectation watches, resolved against the flow.
type target struct {
	id    string
	label string
	taps  []tap
	// what is "came out of port 2" or "arrived", for messages.
	what string
}

// watch is one expectation, resolved.
type watch struct {
	index  int // 1-based position in the test's expect list
	exp    *Expectation
	target target
	assert *jsonata.Expr
	within time.Duration
}

// injection is one injection, resolved.
type injection struct {
	id    string
	label string
	// press sends the message into the node's input, or presses an Inject
	// node's button; otherwise it leaves the node's first output.
	press bool
	msg   map[string]any
}

func runCase(ctx context.Context, doc []byte, c *Case, opts Options) (res Result) {
	res = Result{Name: c.Name, Status: Error}
	began := time.Now()
	defer func() { res.Seconds = time.Since(began).Seconds() }()

	problem := func(msg string, args ...any) Result {
		res.Problems = append(res.Problems, Problem{Message: fmt.Sprintf(msg, args...)})
		return res
	}

	// Parsed afresh for every test. A test gets a runtime, contexts and node
	// instances of its own, and nothing one test does can be seen by the next.
	flows, err := engine.ParseFlows(doc)
	if err != nil {
		return problem("the flow file doesn't parse: %v", err)
	}
	g := newGraphView(flows, opts.Registry)

	injections, watches, replies, problems := g.resolve(c)
	if len(problems) > 0 {
		res.Problems = problems
		return res
	}
	if out := g.reachingOut(); len(out) > 0 {
		// Every node in the palette that reaches outside the process can be
		// stood in for. A node that can't, from a registry that isn't the
		// palette or one added without a stand-in, would do it for real, so
		// the test refuses to run rather than letting it.
		return problem("this flow has nodes that would talk to the world outside the test, "+
			"and they have no stand-in to talk to instead: %s", strings.Join(out, ", "))
	}

	timeout, _ := c.timeout()
	deadline := timeout
	for _, w := range watches {
		deadline = max(deadline, w.within)
	}

	rt := runtime.New(opts.Registry, flows, opts.Runtime)
	rt.SetContexts(store.NewScopedContexts())
	rec := newRecorder(watches)
	rt.SetObserver(rec)
	rt.SetStandIns(newOutside(rec, replies).forNode)

	logs := newLogCollector(rt)

	startErrs := rt.Start(ctx)
	defer func() {
		// Anything a Delay lets go of while stopping is not something the flow
		// did during the test, so the recorder stops listening first.
		rec.close()
		stopCtx, cancel := context.WithTimeout(context.Background(), runtime.DefaultCloseTimeout)
		defer cancel()
		rt.Stop(stopCtx)
		logs.wait()
		res.Log = logs.lines()
	}()
	for _, w := range rt.Warnings() {
		logs.add("warning: " + w)
	}
	if len(startErrs) > 0 {
		for _, f := range startErrs {
			res.Problems = append(res.Problems, Problem{Node: f.NodeID,
				Message: fmt.Sprintf("%s wouldn't start: %v", g.label(f.NodeID), f.Err)})
		}
		return res
	}

	rec.begin()
	end := time.Now().Add(deadline)

	for _, in := range injections {
		m := engine.WrapMsg(cloneData(in.msg))
		var err error
		if in.press {
			err = rt.Inject(in.id, m)
		} else {
			err = rt.SendFrom(in.id, 0, m)
		}
		if err != nil {
			return problem("injecting into %s: %v", in.label, err)
		}
		// Each injection is handled before the next goes in, so two messages
		// into two different nodes arrive in the order the test lists them
		// rather than whichever goroutine wins. Messages a Delay is holding
		// stay held: settled means nothing is moving, not that nothing is
		// waiting.
		if !waitFor(ctx, end, rt.Settled) {
			break
		}
	}

	// The test is over when every expectation that waits for a message has
	// one, or can't have one any more because its within has passed, and the
	// flow has nothing left to do. Or when time runs out. Waiting for quiet as
	// well is what makes "nothing came out of port 2" mean something: it's
	// checked once everything that was going to happen has.
	waitFor(ctx, end, func() bool {
		return rec.decided(ctx) && rt.Quiet()
	})
	rec.close()

	if ctx.Err() != nil {
		return problem("the run was cancelled: %v", ctx.Err())
	}

	res.Problems = rec.evaluate(ctx)
	if len(res.Problems) == 0 {
		res.Status = Pass
	} else {
		res.Status = Fail
	}
	return res
}

// waitFor polls cond until it's true, the deadline passes or ctx ends. It
// reports whether cond came true.
func waitFor(ctx context.Context, end time.Time, cond func() bool) bool {
	for {
		if cond() {
			return true
		}
		if ctx.Err() != nil || !time.Now().Before(end) {
			return false
		}
		time.Sleep(100 * time.Microsecond)
	}
}

func cloneData(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return engine.WrapMsg(m).Clone().Data
}

// ---------------------------------------------------------------------------
// resolving a test against the flow
// ---------------------------------------------------------------------------

// graphView answers the questions a test asks of a flow before it runs: which
// node is called this, what does it have for inputs and outputs, and will it
// run at all.
type graphView struct {
	flows    *engine.Flows
	expanded *engine.Flows
	reg      *node.Registry
}

func newGraphView(flows *engine.Flows, reg *node.Registry) *graphView {
	return &graphView{flows: flows, expanded: engine.ExpandSubflows(flows).Flows, reg: reg}
}

// find resolves a reference by id, then by name. A name has to be unique,
// because a test that quietly picked one of two nodes called "check" would be
// testing whichever the file listed first.
func (g *graphView) find(ref string) (*engine.Node, error) {
	if n, ok := g.flows.Nodes[ref]; ok {
		if _, inTemplate := g.flows.Subflows[n.Z]; inTemplate && !n.IsConfig {
			return nil, fmt.Errorf("node %q is inside subflow %q, which only runs as copies; "+
				"watch the subflow instance instead", ref, n.Z)
		}
		return n, nil
	}
	var named []*engine.Node
	for _, id := range g.flows.Order {
		n, ok := g.flows.Nodes[id]
		if !ok || n.IsConfig || n.Name != ref {
			continue
		}
		if _, inTemplate := g.flows.Subflows[n.Z]; inTemplate {
			continue
		}
		named = append(named, n)
	}
	switch len(named) {
	case 1:
		return named[0], nil
	case 0:
		return nil, fmt.Errorf("there's no node with the id or name %q", ref)
	}
	ids := make([]string, len(named))
	for i, n := range named {
		ids[i] = n.ID
	}
	return nil, fmt.Errorf("%d nodes are called %q (%s); use an id", len(named), ref, strings.Join(ids, ", "))
}

// runs reports whether a node will be started, and if not, why.
func (g *graphView) runs(n *engine.Node) error {
	if n.Disabled {
		return fmt.Errorf("%s is disabled", g.label(n.ID))
	}
	if tab, ok := g.flows.Tabs[n.Z]; ok && tab.Disabled {
		return fmt.Errorf("%s is on a disabled flow", g.label(n.ID))
	}
	return nil
}

func (g *graphView) label(id string) string {
	n, ok := g.expanded.Nodes[id]
	if !ok {
		n, ok = g.flows.Nodes[id]
	}
	if !ok {
		return id
	}
	if n.Name != "" {
		return fmt.Sprintf("%s %q (%s)", n.Type, n.Name, n.ID)
	}
	return fmt.Sprintf("%s %s", n.Type, n.ID)
}

// inputs and outputs read the node's shape. A subflow instance has the shape
// its template declares.
func (g *graphView) inputs(n *engine.Node) int {
	if tmpl, ok := n.SubflowTemplateID(); ok {
		if sf, ok := g.flows.Subflows[tmpl]; ok && len(sf.In) > 0 {
			return 1
		}
		return 0
	}
	if reg, ok := g.reg.Lookup(n.Type); ok {
		return reg.Descriptor.Inputs
	}
	return 0
}

func (g *graphView) outputs(n *engine.Node) int {
	if tmpl, ok := n.SubflowTemplateID(); ok {
		if sf, ok := g.flows.Subflows[tmpl]; ok {
			return len(sf.Out)
		}
		return 0
	}
	return len(n.Wires)
}

func (g *graphView) hasButton(n *engine.Node) bool {
	reg, ok := g.reg.Lookup(n.Type)
	return ok && reg.Descriptor.HasButton
}

func (g *graphView) standsIn(n *engine.Node) bool {
	reg, ok := g.reg.Lookup(n.Type)
	return ok && reg.Descriptor.StandIn
}

// outputTaps finds where a node's output port can be seen. For an ordinary
// node that's the port itself. A subflow instance doesn't send anything: once
// it's running, its outputs are the internal nodes wired to the template's
// output port, so those are what get watched.
func (g *graphView) outputTaps(n *engine.Node, port, depth int) []tap {
	tmplID, isInstance := n.SubflowTemplateID()
	if !isInstance {
		return []tap{{node: n.ID, port: port}}
	}
	sf, ok := g.flows.Subflows[tmplID]
	if !ok || port >= len(sf.Out) || depth > 32 {
		return nil
	}
	var taps []tap
	for _, w := range sf.Out[port].Wires {
		if w.ID == tmplID {
			// The template's input wired straight to this output: whatever
			// goes in comes out of the instance's entry point.
			taps = append(taps, tap{node: n.ID, port: 0})
			continue
		}
		inner, ok := g.expanded.Nodes[n.ID+engine.SubflowScopeSeparator+w.ID]
		if !ok {
			continue
		}
		taps = append(taps, g.outputTaps(inner, w.Port, depth+1)...)
	}
	return taps
}

func (g *graphView) resolve(c *Case) ([]injection, []watch, map[string][]Reply, []Problem) {
	var problems []Problem

	replies := map[string][]Reply{}
	for i, r := range c.Replies {
		n, err := g.find(r.Node)
		if err == nil && !g.standsIn(n) {
			err = fmt.Errorf("%s doesn't talk to anything outside the flow, so there's nothing to reply to", g.label(n.ID))
		}
		if err != nil {
			problems = append(problems, Problem{Message: fmt.Sprintf("reply %d: %v", i+1, err)})
			continue
		}
		replies[n.ID] = append(replies[n.ID], r)
	}

	var injections []injection
	for i, in := range c.Inject {
		n, err := g.find(in.Node)
		if err == nil {
			err = g.runs(n)
		}
		if err != nil {
			problems = append(problems, Problem{Message: fmt.Sprintf("inject %d: %v", i+1, err)})
			continue
		}
		inj := injection{id: n.ID, label: g.label(n.ID), msg: in.Msg}
		switch {
		case g.inputs(n) > 0:
			inj.press = true
		case in.Msg == nil && g.hasButton(n):
			inj.press = true
		case g.outputs(n) == 0:
			problems = append(problems, Problem{Node: n.ID, Message: fmt.Sprintf(
				"inject %d: %s has no input and no output, so a message has nowhere to go", i+1, inj.label)})
			continue
		}
		injections = append(injections, inj)
	}

	var watches []watch
	for i := range c.Expect {
		e := &c.Expect[i]
		w := watch{index: i + 1, exp: e}
		n, err := g.find(e.Node)
		if err == nil {
			err = g.runs(n)
		}
		if err != nil {
			problems = append(problems, Problem{Expect: i + 1, Message: err.Error()})
			continue
		}
		t := target{id: n.ID, label: g.label(n.ID)}
		outs := g.outputs(n)
		switch {
		case e.Sent != nil && !g.standsIn(n):
			problems = append(problems, Problem{Expect: i + 1, Node: n.ID, Message: fmt.Sprintf(
				"%s doesn't send anything outside the flow, so there's nothing to check sent against", t.label)})
			continue
		case e.Sent != nil:
			t.taps = []tap{{node: n.ID, port: callPort}}
			t.what = "sent outside the flow"
		case e.Port > 0 && e.Port > outs:
			problems = append(problems, Problem{Expect: i + 1, Node: n.ID, Message: fmt.Sprintf(
				"%s has %d output(s), so there's no port %d", t.label, outs, e.Port)})
			continue
		case e.Port == 0 && outs == 0:
			t.taps = []tap{{node: n.ID, port: -1}}
			t.what = "received"
		default:
			port := max(e.Port, 1)
			t.taps = g.outputTaps(n, port-1, 0)
			t.what = fmt.Sprintf("sent from port %d", port)
			if len(t.taps) == 0 {
				problems = append(problems, Problem{Expect: i + 1, Node: n.ID, Message: fmt.Sprintf(
					"nothing inside %s is wired to its port %d, so nothing can come out of it", t.label, port)})
				continue
			}
		}
		w.target = t
		if e.Assert != "" {
			x, err := jsonata.Compile(e.Assert, nil)
			if err != nil {
				problems = append(problems, Problem{Expect: i + 1, Message: fmt.Sprintf("assert: %v", err)})
				continue
			}
			w.assert = x
		}
		if e.Within != "" {
			w.within, _ = parseDuration(e.Within)
		}
		watches = append(watches, w)
	}
	return injections, watches, replies, problems
}

// reachingOut lists the nodes that would run in a test, talk to something
// outside the process (a broker, a database, a socket, the filesystem, a
// shell) and have no stand-in to talk to instead. Running one of those from a
// test would publish, write or connect for real, which makes the test a
// deploy.
func (g *graphView) reachingOut() []string {
	var out []string
	for _, id := range g.expanded.Order {
		n, ok := g.expanded.Nodes[id]
		if !ok || n.IsConfig || n.Disabled {
			continue
		}
		if tab, ok := g.expanded.Tabs[n.Z]; ok && tab.Disabled {
			continue
		}
		if _, inTemplate := g.expanded.Subflows[n.Z]; inTemplate {
			continue
		}
		reg, ok := g.reg.Lookup(n.Type)
		if !ok || reg.Descriptor.StandIn {
			continue
		}
		switch {
		case n.Type == "exec",
			reg.Descriptor.Category == node.CategoryNetwork,
			reg.Descriptor.Category == node.CategoryStorage,
			reg.Descriptor.Category == node.CategoryDiscover:
			out = append(out, g.label(n.ID))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// watching what happens
// ---------------------------------------------------------------------------

// arrival is one message seen at a tap.
type arrival struct {
	seq  int
	at   time.Duration
	data map[string]any
}

// recorder is the runtime's observer for a test. It keeps only what the test
// watches, so a flow with a chatty source doesn't fill memory with messages
// nobody asked about.
type recorder struct {
	watches []watch
	wanted  map[tap]bool

	mu    sync.Mutex
	open  bool
	start time.Time
	seq   int
	seen  map[tap][]arrival
}

func newRecorder(watches []watch) *recorder {
	r := &recorder{watches: watches, wanted: map[tap]bool{}, seen: map[tap][]arrival{}}
	for _, w := range watches {
		for _, t := range w.target.taps {
			r.wanted[t] = true
		}
	}
	return r
}

func (r *recorder) begin() {
	r.mu.Lock()
	r.open = true
	r.start = time.Now()
	r.mu.Unlock()
}

func (r *recorder) close() {
	r.mu.Lock()
	r.open = false
	r.mu.Unlock()
}

func (r *recorder) Received(nodeID string, m *engine.Msg) { r.add(tap{node: nodeID, port: -1}, m) }

func (r *recorder) Sent(nodeID string, port int, m *engine.Msg) {
	r.add(tap{node: nodeID, port: port}, m)
}

func (r *recorder) add(t tap, m *engine.Msg) {
	if !r.wanted[t] {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.open {
		return
	}
	r.seq++
	r.seen[t] = append(r.seen[t], arrival{seq: r.seq, at: time.Since(r.start), data: m.Data})
}

// arrivals returns what a target saw, in the order it happened.
func (r *recorder) arrivals(t target) []arrival {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []arrival
	for _, tp := range t.taps {
		out = append(out, r.seen[tp]...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// decided reports whether waiting longer could change how the expectations
// that wait for a message come out: each has its message, or its within has
// already gone by, so no message still to come could count. Counts aren't
// waited for. They're checked when the test ends.
func (r *recorder) decided(ctx context.Context) bool {
	r.mu.Lock()
	elapsed := time.Since(r.start)
	r.mu.Unlock()
	for _, group := range r.groups() {
		k, ok := r.matchInOrder(ctx, group)
		if ok {
			continue
		}
		if w := group[k]; w.within == 0 || elapsed <= w.within {
			return false
		}
	}
	return true
}

// groups collects the waiting expectations per target, in the order the test
// lists them. Two expectations on one port are two messages, the second after
// the first, which is how a test says "on, then off".
func (r *recorder) groups() [][]watch {
	var order []string
	byTarget := map[string][]watch{}
	for _, w := range r.watches {
		if !w.exp.positive() {
			continue
		}
		key := fmt.Sprint(w.target.taps)
		if _, seen := byTarget[key]; !seen {
			order = append(order, key)
		}
		byTarget[key] = append(byTarget[key], w)
	}
	out := make([][]watch, len(order))
	for i, k := range order {
		out[i] = byTarget[k]
	}
	return out
}

// matchInOrder walks a target's messages, giving each expectation the first
// message after the previous one's that it accepts. It returns how far it got.
func (r *recorder) matchInOrder(ctx context.Context, group []watch) (int, bool) {
	got := r.arrivals(group[0].target)
	next := 0
	for k, w := range group {
		found := false
		for next < len(got) {
			a := got[next]
			next++
			if ok, _ := accepts(ctx, w, a); ok {
				found = true
				break
			}
		}
		if !found {
			return k, false
		}
	}
	return len(group), true
}

// accepts reports whether one message meets one expectation, and why not.
func accepts(ctx context.Context, w watch, a arrival) (bool, string) {
	e := w.exp
	if e.Msg != nil {
		if ok, why := contains(map[string]any(e.Msg), a.data, ""); !ok {
			return false, why
		}
	}
	if e.Sent != nil {
		if ok, why := contains(map[string]any(e.Sent), a.data, ""); !ok {
			return false, strings.Replace(why, "msg.", "sent ", 1)
		}
	}
	if e.Error != "" {
		text, ok := errorText(a.data)
		if !ok {
			return false, "msg.error.message: missing, so this isn't an error a Catch node caught"
		}
		if !strings.Contains(text, e.Error) {
			return false, fmt.Sprintf("msg.error.message: %q doesn't contain %q", text, e.Error)
		}
	}
	if w.assert != nil {
		v, defined, err := w.assert.EvalMsg(ctx, &engine.Msg{Data: a.data}, nil)
		if err != nil {
			return false, fmt.Sprintf("assert %q: %v", e.Assert, err)
		}
		if b, ok := v.(bool); !defined || !ok || !b {
			if !defined {
				return false, fmt.Sprintf("assert %q came out undefined", e.Assert)
			}
			return false, fmt.Sprintf("assert %q came out %s", e.Assert, show(v))
		}
	}
	if w.within > 0 && a.at > w.within {
		return false, fmt.Sprintf("it matched, but %s in, and within is %s", round(a.at), w.within)
	}
	return true, ""
}

// evaluate turns what was seen into the test's problems, if it has any.
func (r *recorder) evaluate(ctx context.Context) []Problem {
	var problems []Problem
	failedAt := map[int]bool{}

	for _, group := range r.groups() {
		matched, ok := r.matchInOrder(ctx, group)
		if ok {
			continue
		}
		w := group[matched]
		failedAt[w.index] = true
		problems = append(problems, r.explain(ctx, group, matched))
	}

	for _, w := range r.watches {
		if w.exp.positive() {
			continue
		}
		want := 0
		if w.exp.Count != nil {
			want = *w.exp.Count
		}
		got := r.arrivals(w.target)
		if len(got) == want {
			continue
		}
		var b strings.Builder
		if want == 0 {
			fmt.Fprintf(&b, "%s: expected nothing, and %d message(s) %s", w.target.label, len(got), w.target.what)
		} else {
			fmt.Fprintf(&b, "%s: expected %d message(s) %s, got %d", w.target.label, want, w.target.what, len(got))
		}
		listArrivals(&b, got)
		problems = append(problems, Problem{Expect: w.index, Node: w.target.id, Message: b.String()})
	}

	sort.SliceStable(problems, func(i, j int) bool { return problems[i].Expect < problems[j].Expect })
	return problems
}

// explain says why the expectation at position k in a group found no message.
func (r *recorder) explain(ctx context.Context, group []watch, k int) Problem {
	w := group[k]
	got := r.arrivals(w.target)

	// Skip the messages the expectations before this one used up, so the
	// explanation is about the messages this one was actually offered.
	next := 0
	for _, prev := range group[:k] {
		for next < len(got) {
			a := got[next]
			next++
			if ok, _ := accepts(ctx, prev, a); ok {
				break
			}
		}
	}
	offered := got[next:]

	var b strings.Builder
	switch {
	case len(got) == 0:
		fmt.Fprintf(&b, "%s: nothing %s", w.target.label, w.target.what)
	case len(offered) == 0:
		fmt.Fprintf(&b, "%s: %d message(s) %s and the expectations before this one used them all",
			w.target.label, len(got), w.target.what)
	default:
		fmt.Fprintf(&b, "%s: %d message(s) %s and none matched", w.target.label, len(offered), w.target.what)
		_, why := accepts(ctx, w, offered[0])
		fmt.Fprintf(&b, "; the first one differs at %s", why)
	}
	listArrivals(&b, offered)
	return Problem{Expect: w.index, Node: w.target.id, Message: b.String()}
}

// listArrivals appends the messages a failure is about, five at most.
func listArrivals(b *strings.Builder, got []arrival) {
	const most = 5
	for i, a := range got {
		if i == most {
			fmt.Fprintf(b, "\n  ... and %d more", len(got)-most)
			break
		}
		fmt.Fprintf(b, "\n  at %s: %s", round(a.at), showMsg(a.data))
	}
}

func round(d time.Duration) time.Duration {
	switch {
	case d >= time.Second:
		return d.Round(time.Millisecond)
	case d >= time.Millisecond:
		return d.Round(10 * time.Microsecond)
	}
	return d.Round(time.Microsecond)
}

// ---------------------------------------------------------------------------
// the flow's own log
// ---------------------------------------------------------------------------

// logCollector keeps the errors and warnings a flow raises during a test. It
// also drains the runtime's event channel, which nothing else reads during a
// test.
type logCollector struct {
	mu    sync.Mutex
	out   []string
	done  chan struct{}
	limit int
}

func newLogCollector(rt *runtime.Runtime) *logCollector {
	l := &logCollector{done: make(chan struct{}), limit: 50}
	go func() {
		defer close(l.done)
		for e := range rt.Events() {
			switch e.Topic {
			case runtime.TopicError:
				line := fmt.Sprint(e.Data["error"])
				if id, ok := e.Data["nodeId"].(string); ok {
					line = fmt.Sprintf("error from %s (%v): %s", id, e.Data["type"], line)
				}
				l.add(line)
			case runtime.TopicLog:
				if lvl, _ := e.Data["level"].(string); lvl == string(node.LogError) || lvl == string(node.LogWarn) {
					l.add(fmt.Sprintf("%s from %v (%v): %v", lvl, e.Data["nodeId"], e.Data["type"], e.Data["message"]))
				}
			case runtime.TopicDropped:
				l.add(fmt.Sprintf("dropped a message at %v (%v): %v", e.Data["nodeId"], e.Data["type"], e.Data["policy"]))
			}
		}
	}()
	return l
}

func (l *logCollector) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case len(l.out) < l.limit:
		l.out = append(l.out, line)
	case len(l.out) == l.limit:
		l.out = append(l.out, "... and more, not kept")
	}
}

// wait returns once the runtime has stopped and its last event has been read.
func (l *logCollector) wait() { <-l.done }

func (l *logCollector) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.out...)
}
