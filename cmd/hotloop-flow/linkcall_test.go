package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/nodes"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// Link Call through the real runtime and the real deploy path. Every flow here
// is written the way Node-RED's editor writes it into flows.json.

// A shared piece of logic on its own tab, called from another.
const lookupTab = `
    {"id":"tLib","type":"tab","label":"Library"},
    {"id":"lin","type":"link in","z":"tLib","name":"Lookup","x":100,"y":100,"wires":[["work"]]},
    {"id":"work","type":"change","z":"tLib","x":300,"y":100,"rules":[
        {"t":"set","p":"payload","pt":"msg","to":"payload & '-looked-up'","tot":"jsonata"}],"wires":[["back"]]},
    {"id":"back","type":"link out","z":"tLib","mode":"return","x":500,"y":100,"wires":[]}`

func deployLinkFlow(t *testing.T, app *application, entries string, mode runtime.DeployMode) {
	t.Helper()
	nodes.Links.Reset()
	if _, err := app.deploy(context.Background(), api.DeployRequest{Flows: parse(t, "["+entries+"]"), Mode: mode}); err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, app *application, nodeID string, data map[string]any) {
	t.Helper()
	if err := app.currentRuntime().Inject(nodeID, engine.WrapMsg(data)); err != nil {
		t.Fatal(err)
	}
}

func TestLinkCallReturnsThroughLinkOut(t *testing.T) {
	app, rec := newApp(t)
	deployLinkFlow(t, app, lookupTab+`,
    {"id":"tA","type":"tab","label":"Line 3"},
    {"id":"caller","type":"link call","z":"tA","links":["lin"],"linkType":"static","timeout":"30","x":100,"y":100,"wires":[["seen","whole"]]},
    {"id":"seen","type":"debug","z":"tA","complete":"payload","x":300,"y":100,"wires":[]},
    {"id":"whole","type":"debug","z":"tA","complete":"true","x":300,"y":200,"wires":[]}`, "")

	call(t, app, "caller", map[string]any{"payload": "batch-7"})
	if got := rec.debugFrom(t, "seen"); got != "batch-7-looked-up" {
		t.Fatalf("the call came back as %q", got)
	}
	// The call stack is gone once the last return pops it, as in Node-RED.
	if got := rec.debugFrom(t, "whole"); strings.Contains(got, "_linkSource") {
		t.Errorf("the returned message still carries its call stack: %s", got)
	}
}

// A called flow that makes a call of its own returns to the right caller at
// each level.
func TestLinkCallNests(t *testing.T) {
	app, rec := newApp(t)
	deployLinkFlow(t, app, lookupTab+`,
    {"id":"tMid","type":"tab","label":"Middle"},
    {"id":"midIn","type":"link in","z":"tMid","name":"Middle","x":100,"y":100,"wires":[["inner"]]},
    {"id":"inner","type":"link call","z":"tMid","links":["lin"],"timeout":"30","x":300,"y":100,"wires":[["mark"]]},
    {"id":"mark","type":"change","z":"tMid","x":500,"y":100,"rules":[
        {"t":"set","p":"payload","pt":"msg","to":"payload & '-middle'","tot":"jsonata"}],"wires":[["midBack"]]},
    {"id":"midBack","type":"link out","z":"tMid","mode":"return","x":700,"y":100,"wires":[]},
    {"id":"tA","type":"tab","label":"Line 3"},
    {"id":"outer","type":"link call","z":"tA","links":["midIn"],"timeout":"30","x":100,"y":100,"wires":[["seen"]]},
    {"id":"seen","type":"debug","z":"tA","complete":"payload","x":300,"y":100,"wires":[]}`, "")

	call(t, app, "outer", map[string]any{"payload": "x"})
	if got := rec.debugFrom(t, "seen"); got != "x-looked-up-middle" {
		t.Fatalf("the nested call came back as %q", got)
	}
}

// Dynamic mode finds a Link In by name, on the calling tab before anywhere
// else, and refuses to guess between two.
func TestLinkCallDynamicTargets(t *testing.T) {
	app, rec := newApp(t)
	deployLinkFlow(t, app, lookupTab+`,
    {"id":"tA","type":"tab","label":"Line 3"},
    {"id":"dyn","type":"link call","z":"tA","links":[],"linkType":"dynamic","timeout":"30","x":100,"y":100,"wires":[["seen"]]},
    {"id":"seen","type":"debug","z":"tA","complete":"payload","x":300,"y":100,"wires":[]},
    {"id":"localIn","type":"link in","z":"tA","name":"Local","x":100,"y":300,"wires":[["localMark"]]},
    {"id":"localMark","type":"change","z":"tA","x":300,"y":300,"rules":[
        {"t":"set","p":"payload","pt":"msg","to":"payload & '-here'","tot":"jsonata"}],"wires":[["localBack"]]},
    {"id":"localBack","type":"link out","z":"tA","mode":"return","x":500,"y":300,"wires":[]},
    {"id":"tB","type":"tab","label":"Elsewhere"},
    {"id":"farIn","type":"link in","z":"tB","name":"Local","x":100,"y":100,"wires":[["farBack"]]},
    {"id":"farBack","type":"link out","z":"tB","mode":"return","x":300,"y":100,"wires":[]},
    {"id":"tC","type":"tab","label":"Twins"},
    {"id":"twin1","type":"link in","z":"tC","name":"Twin","x":100,"y":100,"wires":[]},
    {"id":"twin2","type":"link in","z":"tC","name":"Twin","x":100,"y":200,"wires":[]},
    {"id":"sf","type":"subflow","name":"Wrapped","in":[],"out":[]},
    {"id":"hidden","type":"link in","z":"sf","name":"Hidden","x":100,"y":100,"wires":[]},
    {"id":"inst","type":"subflow:sf","z":"tC","x":100,"y":300,"wires":[]},
    {"id":"catch","type":"catch","z":"tA","x":100,"y":500,"wires":[["err"]]},
    {"id":"err","type":"debug","z":"tA","complete":"error.message","x":300,"y":500,"wires":[]}`, "")

	call(t, app, "dyn", map[string]any{"payload": "a", "target": "Lookup"})
	if got := rec.debugFrom(t, "seen"); got != "a-looked-up" {
		t.Fatalf("by name on another tab: %q", got)
	}
	rec.reset()
	call(t, app, "dyn", map[string]any{"payload": "b", "target": "Local"})
	if got := rec.debugFrom(t, "seen"); got != "b-here" {
		t.Fatalf("a name on the calling tab has to win over the same name elsewhere: %q", got)
	}
	rec.reset()
	call(t, app, "dyn", map[string]any{"payload": "c", "target": "Twin"})
	if got := rec.debugFrom(t, "err"); !strings.Contains(got, `multiple link in nodes are named "Twin"`) {
		t.Fatalf("two link ins with one name: %q", got)
	}
	rec.reset()
	// A Link In inside a subflow instance belongs to that instance. A call by
	// name from outside never reaches in, even when the name is unique.
	call(t, app, "dyn", map[string]any{"payload": "e", "target": "Hidden"})
	if got := rec.debugFrom(t, "err"); !strings.Contains(got, `"Hidden" is not running`) {
		t.Fatalf("a name inside a subflow instance: %q", got)
	}
	rec.reset()
	call(t, app, "dyn", map[string]any{"payload": "d", "target": "Nobody"})
	if got := rec.debugFrom(t, "err"); !strings.Contains(got, `"Nobody" is not running`) {
		t.Fatalf("a name nobody has: %q", got)
	}
}

// A call with no return inside its timeout raises an error with the message as
// it was sent. A return that turns up later still goes out, as in Node-RED.
func TestLinkCallTimesOutAndALateReturnStillArrives(t *testing.T) {
	app, rec := newApp(t)
	deployLinkFlow(t, app, `
    {"id":"tLib","type":"tab","label":"Library"},
    {"id":"slowIn","type":"link in","z":"tLib","x":100,"y":100,"wires":[["wait"]]},
    {"id":"wait","type":"delay","z":"tLib","pauseType":"delay","timeout":"400","timeoutUnits":"milliseconds","x":300,"y":100,"wires":[["slowBack"]]},
    {"id":"slowBack","type":"link out","z":"tLib","mode":"return","x":500,"y":100,"wires":[]},
    {"id":"tA","type":"tab","label":"Line 3"},
    {"id":"caller","type":"link call","z":"tA","links":["slowIn"],"timeout":"0.1","x":100,"y":100,"wires":[["seen"]]},
    {"id":"seen","type":"debug","z":"tA","complete":"payload","x":300,"y":100,"wires":[]},
    {"id":"catch","type":"catch","z":"tA","x":100,"y":300,"wires":[["err","errPayload"]]},
    {"id":"err","type":"debug","z":"tA","complete":"error.message","x":300,"y":300,"wires":[]},
    {"id":"errPayload","type":"debug","z":"tA","complete":"payload","x":300,"y":400,"wires":[]}`, "")

	call(t, app, "caller", map[string]any{"payload": "slow"})
	if got := rec.debugFrom(t, "err"); !strings.Contains(got, "timed out") {
		t.Fatalf("the timeout said %q", got)
	}
	if got := rec.debugFrom(t, "errPayload"); got != "slow" {
		t.Errorf("the timeout error carried %q, want the message as it was sent", got)
	}
	if got := rec.debugFrom(t, "seen"); got != "slow" {
		t.Errorf("the late return came out as %q", got)
	}
}

// A return with nothing to return to is a broken flow, and a Catch sees it.
func TestLinkOutReturnWithoutACallIsAnError(t *testing.T) {
	app, rec := newApp(t)
	deployLinkFlow(t, app, `
    {"id":"tA","type":"tab","label":"Line 3"},
    {"id":"lout","type":"link out","z":"tA","mode":"return","x":100,"y":100,"wires":[]},
    {"id":"catch","type":"catch","z":"tA","x":100,"y":300,"wires":[["err"]]},
    {"id":"err","type":"debug","z":"tA","complete":"error.message","x":300,"y":300,"wires":[]}`, "")

	call(t, app, "lout", map[string]any{"payload": "orphan"})
	if got := rec.debugFrom(t, "err"); !strings.Contains(got, "not sent by a link call") {
		t.Fatalf("an orphan return said %q", got)
	}
}

// A partial deploy that replaces the Link Call while a call is out: the return
// comes back out of the new instance instead of vanishing with the old one.
func TestLinkCallReturnSurvivesAPartialDeploy(t *testing.T) {
	app, rec := newApp(t)
	flow := func(name string) string {
		return `
    {"id":"tLib","type":"tab","label":"Library"},
    {"id":"slowIn","type":"link in","z":"tLib","x":100,"y":100,"wires":[["wait"]]},
    {"id":"wait","type":"delay","z":"tLib","pauseType":"delay","timeout":"400","timeoutUnits":"milliseconds","x":300,"y":100,"wires":[["slowBack"]]},
    {"id":"slowBack","type":"link out","z":"tLib","mode":"return","x":500,"y":100,"wires":[]},
    {"id":"tA","type":"tab","label":"Line 3"},
    {"id":"caller","type":"link call","z":"tA","name":"` + name + `","links":["slowIn"],"timeout":"30","x":100,"y":100,"wires":[["seen"]]},
    {"id":"seen","type":"debug","z":"tA","complete":"payload","x":300,"y":100,"wires":[]}`
	}
	deployLinkFlow(t, app, flow("one"), "")
	call(t, app, "caller", map[string]any{"payload": "in flight"})

	time.Sleep(50 * time.Millisecond)
	if _, err := app.deploy(context.Background(), api.DeployRequest{Flows: parse(t, "["+flow("two")+"]"), Mode: runtime.DeployNodes}); err != nil {
		t.Fatal(err)
	}
	if got := rec.debugFrom(t, "seen"); got != "in flight" {
		t.Fatalf("the return across the deploy came out as %q", got)
	}
}
