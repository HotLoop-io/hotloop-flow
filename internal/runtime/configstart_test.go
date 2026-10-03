package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// startable is a configuration node with work of its own, recording each start
// and the context it was given.
type startable struct {
	label string
	fail  bool
	log   *startLog
}

type startLog struct {
	mu      sync.Mutex
	started map[string]context.Context
	closed  []string
}

func (s startable) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (s startable) Start(ctx context.Context, out node.Emitter) error {
	if s.fail {
		return errors.New("the port is taken")
	}
	s.log.mu.Lock()
	s.log.started[s.label] = ctx
	s.log.mu.Unlock()
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: "listening"})
	return nil
}

func (s startable) Close(context.Context, bool) error {
	s.log.mu.Lock()
	s.log.closed = append(s.log.closed, s.label)
	s.log.mu.Unlock()
	return nil
}

func (l *startLog) ctx(label string) context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.started[label]
}

func startableRegistry(t *testing.T, log *startLog) *testRegistry {
	t.Helper()
	tr := newTestRegistry()
	if err := tr.Register(node.Descriptor{
		Type: "listener-config", Category: node.CategoryConfig, Color: "#E31837", Icon: "cog",
		IsConfig: true, Compatibility: node.Compatibility{Level: node.CompatOnly},
	}, func(def *node.Definition) (node.Node, error) {
		return startable{label: def.Node.PropString("label", ""), fail: def.Node.PropBool("fail", false), log: log}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return tr
}

// The bug this closes: nothing started a configuration node, so a
// websocket-listener never claimed its path and a websocket-client never
// dialled, in every deployment.
func TestConfigNodesThatStartAreStarted(t *testing.T) {
	log := &startLog{started: map[string]context.Context{}}
	tr := startableRegistry(t, log)
	doc := func(label string) *engine.Flows {
		return mustFlows(t, `[{"id":"t1","type":"tab","label":"T"},{"id":"ws","type":"listener-config","label":"`+label+`"}]`)
	}
	rt := New(tr.Registry, doc("first"), Options{})
	el := drain(rt)
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatal(fails)
	}
	first := log.ctx("first")
	if first == nil {
		t.Fatal("the configuration node was never started")
	}
	if first.Err() != nil {
		t.Fatal("it was started with a context that is already done")
	}
	waitFor(t, "its status to reach the editor", func() bool {
		for _, e := range el.byTopic(TopicStatus) {
			if e.Data["nodeId"] == "ws" && e.Data["text"] == "listening" {
				return true
			}
		}
		return false
	})

	// A partial deploy that replaces it stops the old one before starting
	// the new, so the new one can claim what the old one held.
	if _, err := rt.Update(context.Background(), doc("second"), UpdateOptions{Mode: DeployNodes}); err != nil {
		t.Fatal(err)
	}
	if first.Err() == nil {
		t.Fatal("the replaced configuration node's context is still live")
	}
	second := log.ctx("second")
	if second == nil || second.Err() != nil {
		t.Fatal("the replacement was not started")
	}

	rt.Stop(context.Background())
	if second.Err() == nil {
		t.Fatal("stopping the runtime left the configuration node running")
	}
}

// One that fails to start is reported and cannot be resolved, so nothing
// leans on a half-started instance.
func TestAConfigNodeThatFailsToStart(t *testing.T) {
	log := &startLog{started: map[string]context.Context{}}
	tr := startableRegistry(t, log)
	rt := New(tr.Registry, mustFlows(t, `[{"id":"t1","type":"tab","label":"T"},{"id":"ws","type":"listener-config","label":"x","fail":true}]`), Options{})
	drain(rt)
	fails := rt.Start(context.Background())
	defer rt.Stop(context.Background())
	if len(fails) != 1 || fails[0].NodeID != "ws" || !strings.Contains(fails[0].Err.Error(), "port is taken") {
		t.Fatalf("failures %v", fails)
	}
	svc := &services{rt: rt}
	if _, ok := svc.ConfigNode("ws"); ok {
		t.Fatal("a configuration node that failed to start can still be resolved")
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.closed) != 1 {
		t.Fatalf("closed %v, want the failed node closed once", log.closed)
	}
}
