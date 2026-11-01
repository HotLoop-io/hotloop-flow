package diagram

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// RunState is where a run is.
type RunState string

const (
	StateRunning RunState = "running"
	StatePaused  RunState = "paused"
	StateDone    RunState = "done"
	StateStopped RunState = "stopped"
	StateFailed  RunState = "failed"
)

// Run is one execution of a diagram, from its first node to its last, or until
// someone stops it.
type Run struct {
	ID      string
	Diagram string
	Started time.Time

	set *Set
	mgr *Manager
	ctl *Controller

	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	state    RunState
	err      error
	finished time.Time

	// highlight is how long, in nanoseconds, each node waits before it runs, so a
	// person can follow the run. Zero is full speed.
	highlight atomic.Int64
	hlMu      sync.Mutex
	hlTimer   *time.Timer

	wires    wireStore
	sessions sessions

	localMu sync.Mutex
	local   map[string]any

	nodeRuns   atomic.Int64
	errorsSeen atomic.Int64
	limit      eventLimit
}

// Ctx is what a node is handed when it runs.
type Ctx struct {
	Run     *Run
	Node    *DNode
	Diagram *Diagram
	Depth   int
	// Boundary holds the values a structure hands the body it is running, by
	// name. Only the nodes that stand at the edge of a body read it.
	Boundary map[string]any
	// Out records a value a body hands back to its structure, by name.
	out func(name string, v any)
}

// Emit records a value at the edge of a body for the structure to read.
func (c *Ctx) Emit(name string, v any) {
	if c.out != nil {
		c.out(name, v)
	}
}

// Wait is how long a node should pause for; it honors a stop.
func (c *Ctx) Wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Local returns state a node keeps for the length of a run, making it the first
// time it is asked for. A node inside a loop body asks every pass and gets the
// same one, which is how a log file stays open from one pass to the next.
func (r *Run) Local(key string, make func() any) any {
	r.localMu.Lock()
	defer r.localMu.Unlock()
	if r.local == nil {
		r.local = map[string]any{}
	}
	v, ok := r.local[key]
	if !ok {
		v = make()
		r.local[key] = v
	}
	return v
}

// Panel is the run's panel: the controls and indicators of the diagram.
func (r *Run) Panel() *Panel { return r.mgr.panelFor(r.Diagram) }

// State returns where the run is and the error that ended it, if one did.
func (r *Run) State() (RunState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == StateRunning && r.ctl.Paused() {
		return StatePaused, nil
	}
	return r.state, r.err
}

// Done is closed when the run has finished.
func (r *Run) Done() <-chan struct{} { return r.done }

// Wait blocks until the run finishes and returns how it ended.
func (r *Run) Wait() error {
	<-r.done
	_, err := r.State()
	return err
}

// Stop ends the run. It is safe to call more than once.
func (r *Run) Stop() {
	r.mu.Lock()
	if r.state == StateRunning {
		r.state = StateStopped
	}
	r.mu.Unlock()
	r.ctl.Release()
	r.cancel()
}

// HighlightLimit is how long highlight execution stays on without being asked
// for again. It slows every node of a run, so one left on by a browser that went
// away would quietly slow a logger on a box with no screen.
const HighlightLimit = 10 * time.Minute

// SetHighlight sets the pause before each node, as highlight execution does. It
// turns itself off after HighlightLimit.
func (r *Run) SetHighlight(d time.Duration) {
	r.highlight.Store(int64(d))
	r.hlMu.Lock()
	defer r.hlMu.Unlock()
	if r.hlTimer != nil {
		r.hlTimer.Stop()
		r.hlTimer = nil
	}
	if d > 0 {
		r.hlTimer = time.AfterFunc(HighlightLimit, func() {
			r.highlight.Store(0)
			r.emit("run", map[string]any{"run": r.ID, "state": string(StateRunning), "highlight": "off"})
		})
	}
}

// Controller is the run's debugger: breakpoints, stepping and the pause state.
func (r *Run) Controller() *Controller { return r.ctl }

// WireValues is the last value on every wire and the recent history of each.
func (r *Run) WireValues() map[string]WireSnapshot { return r.wires.snapshot() }

// Stats counts what the run has done.
func (r *Run) Stats() (nodeRuns, errors int64) { return r.nodeRuns.Load(), r.errorsSeen.Load() }

func (r *Run) finish(err error) {
	r.mu.Lock()
	switch {
	case r.state == StateStopped:
		// stopped on purpose: not an error
	case err != nil && !errors.Is(err, context.Canceled):
		r.state, r.err = StateFailed, err
	default:
		r.state = StateDone
	}
	r.finished = time.Now()
	st := r.state
	rerr := r.err
	r.mu.Unlock()
	r.sessions.closeAll()
	r.closeLocals()
	r.SetHighlight(0)
	data := map[string]any{"run": r.ID, "state": string(st)}
	if rerr != nil {
		data["error"] = rerr.Error()
	}
	r.emit("run", data)
	close(r.done)
}

func (r *Run) emit(kind string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	if kind == "node" && r.highlight.Load() == 0 && !r.limit.allow() {
		return
	}
	data["diagram"] = r.Diagram
	r.mgr.publish("diagram/"+r.Diagram+"/"+kind, data)
}

// instance is one execution of one diagram: the top of a run, or a body running
// once inside a structure.
type instance struct {
	run   *Run
	d     *Diagram
	depth int
	scope string // prefix for wire keys, so a body's wires don't collide with its parent's
	bound map[string]any

	chans map[string]map[string]chan any

	mu   sync.Mutex
	outs map[string]any
	fail error

	cancel context.CancelFunc
}

// exec runs a diagram to the end and returns what its body-out nodes collected.
func (r *Run) exec(ctx context.Context, d *Diagram, depth int, boundary map[string]any) (map[string]any, error) {
	if len(d.Problems) > 0 {
		return nil, fmt.Errorf("the diagram %q cannot run: %s", d.Label, d.Problems[0])
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	in := &instance{run: r, d: d, depth: depth, scope: d.ID + "/", bound: boundary,
		chans: map[string]map[string]chan any{}, outs: map[string]any{}, cancel: cancel}
	for _, id := range d.Order {
		dn := d.Nodes[id]
		for _, p := range dn.Needs {
			if in.chans[id] == nil {
				in.chans[id] = map[string]chan any{}
			}
			// One writer per input, so the send never blocks.
			in.chans[id][p] = make(chan any, 1)
		}
	}

	if err := r.ctl.before(ctx, "diagram:"+d.ID, depth, d.ID); err != nil {
		return nil, err
	}

	var wg sync.WaitGroup
	for _, id := range d.Order {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			in.runNode(ctx, d.Nodes[id])
		}(id)
	}
	wg.Wait()

	in.mu.Lock()
	defer in.mu.Unlock()
	if in.fail != nil {
		return in.outs, in.fail
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return in.outs, err
	}
	return in.outs, nil
}

func (in *instance) setFail(err error) {
	in.mu.Lock()
	if in.fail == nil {
		in.fail = err
	}
	in.mu.Unlock()
	in.cancel()
}

func (in *instance) runNode(ctx context.Context, dn *DNode) {
	r := in.run
	vals := map[string]any{}
	for _, p := range dn.Needs {
		select {
		case v := <-in.chans[dn.ID][p]:
			vals[p] = v
		case <-ctx.Done():
			return
		}
	}

	if err := r.ctl.before(ctx, dn.ID, in.depth, in.d.ID); err != nil {
		return
	}
	if hl := time.Duration(r.highlight.Load()); hl > 0 {
		r.emit("node", map[string]any{"node": dn.ID, "state": "ready", "depth": in.depth})
		select {
		case <-time.After(hl):
		case <-ctx.Done():
			return
		}
	}
	r.emit("node", map[string]any{"node": dn.ID, "state": "start", "depth": in.depth})

	started := time.Now()
	var out map[string]any
	state := "done"

	if e, bad := asError(vals[PortErr]); bad && hasPort(dn.In, PortErr) && dn.Kind.SkipsOnError() {
		// An error in: do no work, and hand the error on.
		out = map[string]any{PortErr: e}
		state = "skipped"
	} else {
		var err error
		out, err = in.call(ctx, dn, vals)
		if err != nil {
			if ctx.Err() != nil && !hasFault(err) {
				return
			}
			e := toError(dn, err)
			r.errorsSeen.Add(1)
			state = "error"
			if hasPort(dn.Out, PortErr) {
				out = map[string]any{PortErr: e}
			} else {
				in.setFail(fmt.Errorf("%s (%s): %w", dn.ID, dn.Type, err))
				r.emit("node", map[string]any{"node": dn.ID, "state": "error", "error": e.Source, "depth": in.depth})
				return
			}
		}
	}
	r.nodeRuns.Add(1)
	dur := time.Since(started)
	r.emit("node", map[string]any{"node": dn.ID, "state": state, "ms": float64(dur.Microseconds()) / 1000, "depth": in.depth})

	if out == nil {
		out = map[string]any{}
	}
	out[PortDone] = true
	for _, p := range dn.Out {
		v := out[p.Name]
		for _, w := range in.d.From[dn.ID][p.Name] {
			if err := in.send(ctx, w, v); err != nil {
				return
			}
		}
	}
}

func (in *instance) send(ctx context.Context, w *Wire, v any) error {
	r := in.run
	key := in.scope + w.Key
	r.wires.record(key, v, r.mgr.probeDepth(r.Diagram, key))
	if r.mgr.wantsToken(r.Diagram, key) || r.highlight.Load() > 0 {
		r.emit("token", map[string]any{"wire": key, "value": preview(v)})
	}
	if err := r.ctl.before(ctx, "wire:"+key, in.depth, in.d.ID); err != nil {
		return err
	}
	select {
	case in.chans[w.To][w.ToPort] <- v:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (in *instance) call(ctx context.Context, dn *DNode, vals map[string]any) (out map[string]any, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &Fault{Code: CodeInternal, Source: fmt.Sprintf("%s crashed: %v", dn.Type, p)}
		}
	}()
	if dn.Op == nil {
		return nil, nil
	}
	rc := &Ctx{Run: in.run, Node: dn, Diagram: in.d, Depth: in.depth, Boundary: in.bound,
		out: func(name string, v any) {
			in.mu.Lock()
			in.outs[name] = v
			in.mu.Unlock()
		}}
	return dn.Op.Run(ctx, rc, vals)
}

func hasFault(err error) bool {
	var f *Fault
	return errors.As(err, &f)
}

func toError(dn *DNode, err error) Error {
	var f *Fault
	if errors.As(err, &f) {
		return Error{Status: true, Code: f.Code, Source: dn.Type + ": " + f.Source, Node: dn.ID}
	}
	return Error{Status: true, Code: CodeInternal, Source: dn.Type + ": " + err.Error(), Node: dn.ID}
}

// SkipsOnError is true for every node type except the ones that exist to look at
// an error: they run with it.
func (k *Kind) SkipsOnError() bool { return !k.TakesErrors }

// eventLimit keeps a tight loop from flooding the editors with events about
// nodes. Highlight execution and a probe are for slow, watched runs, and they are
// not limited.
type eventLimit struct {
	mu     sync.Mutex
	window time.Time
	n      int
}

func (l *eventLimit) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.window) > 250*time.Millisecond {
		l.window, l.n = now, 0
	}
	l.n++
	return l.n <= 200
}

// preview is a token as a browser should see it: short enough for a wire label.
func preview(v any) any {
	j := jsonValue(v)
	if s, ok := j.(string); ok && len(s) > 200 {
		return s[:200] + "..."
	}
	if a, ok := j.([]any); ok && len(a) > 64 {
		return map[string]any{"length": len(a), "head": a[:16]}
	}
	return j
}

// closeLocals closes whatever nodes kept for the run and can be closed.
func (r *Run) closeLocals() {
	r.localMu.Lock()
	vals := r.local
	r.local = nil
	r.localMu.Unlock()
	for _, v := range vals {
		if c, ok := v.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}
}
