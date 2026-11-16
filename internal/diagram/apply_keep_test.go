package diagram

import (
	"testing"
	"time"
)

// A deploy of something else must not restart a run that did not change, and a
// deploy of anything the run reads must.

func slowDiagram(t *testing.T, ms string, x float64) *flowBuilder {
	b := newFlow(t).tab("main", nil).
		node("main", "w", "dia-wait", map[string]any{"ms": ms, "x": x}).
		node("main", "i", "dia-indicator", map[string]any{"kind": "number"}).
		wire("w.done", "i.value")
	return b
}

func running(t *testing.T, m *Manager) *Run {
	t.Helper()
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func stillRunning(r *Run) bool {
	select {
	case <-r.Done():
		return false
	default:
		return true
	}
}

func TestADeployThatChangesNothingKeepsTheRunGoing(t *testing.T) {
	m := slowDiagram(t, "60000", 100).manager()
	r := running(t, m)
	time.Sleep(50 * time.Millisecond)
	// The same diagram again, with a node moved on the canvas: not a change.
	m.Apply(slowDiagram(t, "60000", 340).flows())
	if m.Current("main") != r || !stillRunning(r) {
		t.Fatal("a deploy that changed nothing a run reads stopped the run")
	}
	if _, err := m.Start("main", StartOptions{}); err == nil {
		t.Fatal("a second run started beside the one that was kept")
	}
	r.Stop()
	<-r.Done()
}

func TestADeployThatChangesTheDiagramStopsTheRun(t *testing.T) {
	m := slowDiagram(t, "60000", 100).manager()
	r := running(t, m)
	m.Apply(slowDiagram(t, "70000", 100).flows())
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a deploy that changed a node's property left the old run going")
	}
}

func TestADeployThatChangesABodyStopsTheRunOfItsOwner(t *testing.T) {
	build := func(v string) *flowBuilder {
		return newFlow(t).tab("main", nil).tab("body", map[string]any{"role": "body"}).
			node("main", "w", "dia-wait", map[string]any{"ms": "60000"}).
			node("main", "sub", "dia-sub", map[string]any{"body": "body", "ins": "", "outs": ""}).
			node("body", "c", "dia-const", map[string]any{"value": v})
	}
	m := build("1").manager()
	r := running(t, m)
	m.Apply(build("1").flows())
	if !stillRunning(r) {
		t.Fatal("an unchanged body stopped the run")
	}
	m.Apply(build("2").flows())
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a changed body left the old run going")
	}
}

func TestADeployThatChangesAConfigNodeStopsTheRun(t *testing.T) {
	build := func(host string) *flowBuilder {
		b := slowDiagram(t, "60000", 100)
		b.entries = append(b.entries, map[string]any{"id": "cfg", "type": "mqtt-broker", "broker": host})
		return b
	}
	m := build("a").manager()
	r := running(t, m)
	m.Apply(build("b").flows())
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a changed config node left the old run going")
	}
}
