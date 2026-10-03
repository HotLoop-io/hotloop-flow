package main

import (
	"context"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// An imported flow that leans on JSONata at every step, deployed through the
// same path the API uses: an Inject building its payload with an expression, a
// Change working it over with flow context, a Switch routing on an expression,
// and a Debug showing an expression of the result. Before JSONata ran here,
// every one of these nodes refused to work.
func TestImportedJSONataFlowRunsEndToEnd(t *testing.T) {
	app, rec := newApp(t)
	flow := `[
    {"id":"t1","type":"tab","label":"Line 3"},
    {"id":"inj","type":"inject","z":"t1","x":100,"y":100,
     "props":[{"p":"payload","v":"{'readings': [71, 84, 90], 'line': $env('LINE_NAME')}","vt":"jsonata"}],
     "repeat":"","once":false,"wires":[["chg"]]},
    {"id":"chg","type":"change","z":"t1","x":300,"y":100,"rules":[
        {"t":"set","p":"limit","pt":"flow","to":"80","tot":"num"},
        {"t":"set","p":"payload.hot","pt":"msg","to":"$count(payload.readings[$ > $flowContext('limit')])","tot":"jsonata"}
     ],"wires":[["sw"]]},
    {"id":"sw","type":"switch","z":"t1","x":500,"y":100,"property":"payload","propertyType":"msg",
     "rules":[{"t":"jsonata_exp","v":"payload.hot >= 2","vt":"jsonata"},{"t":"else"}],
     "checkall":"true","outputs":2,"wires":[["alarm"],["calm"]]},
    {"id":"alarm","type":"debug","z":"t1","x":700,"y":80,"active":true,"tosidebar":true,
     "complete":"payload.line & ': ' & $string(payload.hot) & ' readings over the limit'","targetType":"jsonata","wires":[]},
    {"id":"calm","type":"debug","z":"t1","x":700,"y":120,"active":true,"tosidebar":true,
     "complete":"payload","targetType":"msg","wires":[]}
]`
	t.Setenv("LINE_NAME", "Line 3")
	if _, err := app.deploy(context.Background(), api.DeployRequest{Flows: parse(t, flow)}); err != nil {
		t.Fatal(err)
	}
	if err := app.currentRuntime().Inject("inj", engine.NewMsg()); err != nil {
		t.Fatal(err)
	}
	if got := rec.debugFrom(t, "alarm"); got != "Line 3: 2 readings over the limit" {
		t.Fatalf("the alarm debug showed %q", got)
	}
}
