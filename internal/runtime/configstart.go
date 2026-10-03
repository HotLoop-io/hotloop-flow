package runtime

import (
	"context"
	"fmt"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// startConfig starts a configuration node that has work of its own: a
// websocket-listener claiming its path, a websocket-client dialling out.
//
// Until this existed nothing called Start on a configuration node. Flow nodes
// get started through their runners and configuration nodes have no runner, so
// a websocket-listener never claimed its path and a websocket-client never
// dialled, in every deployment, while their unit tests, which call Start by
// hand, passed. The node gets a context of its own, cancelled when it is
// retired, and an emitter whose status, errors and log lines are reported
// against its id.
//
// A configuration node that fails to start is taken back out, so nothing can
// resolve a half-started instance, and the failure is reported like any other.
func (rt *Runtime) startConfig(g *graph, id string) (StartError, bool) {
	rt.configsMu.RLock()
	inst, ok := rt.configs[id]
	rt.configsMu.RUnlock()
	if !ok {
		return StartError{}, true
	}
	s, ok := inst.(node.Starter)
	if !ok {
		return StartError{}, true
	}
	n := g.flows.Nodes[id]
	ctx, cancel := context.WithCancel(rt.ctx)
	if err := s.Start(ctx, configEmitter{rt: rt, id: id, typ: n.Type, name: n.Name}); err != nil {
		cancel()
		rt.configsMu.Lock()
		delete(rt.configs, id)
		rt.configsMu.Unlock()
		if c, ok := inst.(node.Closer); ok {
			_ = c.Close(context.Background(), false)
		}
		f := StartError{NodeID: id, Type: n.Type, Err: err}
		g.failures[id] = f
		return f, false
	}
	rt.configsMu.Lock()
	rt.configStops[id] = cancel
	rt.configsMu.Unlock()
	return StartError{}, true
}

// configEmitter is what a configuration node is started with. It has no ports
// and no tab, so there is nothing to send to and no Catch node in scope;
// everything it reports goes out as an event against its id, which is where
// the editor shows a config node's state.
type configEmitter struct {
	rt            *Runtime
	id, typ, name string
}

var _ node.Emitter = configEmitter{}

func (e configEmitter) Send(int, *engine.Msg)   {}
func (e configEmitter) SendAll([][]*engine.Msg) {}
func (e configEmitter) Done(*engine.Msg, error) {}

func (e configEmitter) Status(s node.Status) {
	e.rt.emit(Event{Topic: TopicStatus, Data: map[string]any{
		"nodeId": e.id,
		"fill":   s.Fill, "shape": s.Shape, "text": s.Text,
		"cleared": s.Cleared(),
	}})
}

func (e configEmitter) Error(err error, _ *engine.Msg) {
	if err == nil {
		return
	}
	e.rt.emit(Event{Topic: TopicError, Data: map[string]any{
		"nodeId": e.id, "type": e.typ, "name": e.name, "error": err.Error(),
	}})
}

func (e configEmitter) Publish(topic string, data map[string]any) {
	e.rt.Publish(Event{Topic: topic, Data: data})
}

func (e configEmitter) Log(level node.LogLevel, format string, args ...any) {
	e.rt.emit(Event{Topic: TopicLog, Data: map[string]any{
		"nodeId": e.id, "type": e.typ, "name": e.name,
		"level": string(level), "message": fmt.Sprintf(format, args...),
	}})
}
