package nodes

import (
	"reflect"
	"testing"
)

// A CSV node that lists no columns and expects no header row names them col1,
// col2 and so on, as Node-RED does. It used to refuse to start, so a flow that
// left the columns empty (Node-RED's own example does) never ran.
func TestCSVWithNoColumnsAndNoHeaderNamesThemColN(t *testing.T) {
	n := build(t, "csv", `{"sep":",","hdrin":"","hdrout":"none","multi":"one","temp":"","strings":true}`, newTestServices())
	e, err := send(t, n, msg(t, `{"payload":"Apple,100,Canada\nOrange,120,USA"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"col1": "Apple", "col2": 100.0, "col3": "Canada"},
		map[string]any{"col1": "Orange", "col2": 120.0, "col3": "USA"},
	}
	if got := payloads(e.on(0)); !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// Node-RED's CSV node says which columns it used in msg.columns, on parsing and on rendering, and a flow
// from its library relies on it: the parse names the columns, a Change adds one, and the render writes the
// header from them (Node-RED 5.0.10 70-CSV.js, run on the same flow by the oracle).
func TestCSVSaysItsColumnsInMsgColumnsOnParseAndRender(t *testing.T) {
	in := build(t, "csv", `{"sep":",","hdrin":true,"hdrout":"","multi":"one","temp":"","strings":true}`, newTestServices())
	e, err := send(t, in, msg(t, `{"payload":"Station,Month,Consumption\nRio,January,4000"}`))
	if err != nil {
		t.Fatal(err)
	}
	row := e.on(0)[0]
	if row.Data["columns"] != "Station,Month,Consumption" {
		t.Fatalf("columns after parse = %v", row.Data["columns"])
	}
	row.Data["columns"] = row.Data["columns"].(string) + ",PartsProduced"
	row.Set("payload.PartsProduced", 400.0)
	out := build(t, "csv", `{"sep":",","hdrin":"","hdrout":"all","multi":"one","temp":"","ret":"\\n"}`, newTestServices())
	e, err = send(t, out, row)
	if err != nil {
		t.Fatal(err)
	}
	res := e.on(0)[0]
	if got, want := res.Payload(), "Station,Month,Consumption,PartsProduced\nRio,January,4000,400\n"; got != want {
		t.Errorf("csv = %q, want %q", got, want)
	}
	if res.Data["columns"] != "Station,Month,Consumption,PartsProduced" {
		t.Errorf("columns after render = %v", res.Data["columns"])
	}
}

// A node that lists its own columns keeps them: msg.columns names them only for a node that lists none.
func TestCSVOwnColumnsComeBeforeMsgColumns(t *testing.T) {
	n := build(t, "csv", `{"sep":",","hdrout":"all","multi":"one","temp":"b,a"}`, newTestServices())
	e, err := send(t, n, msg(t, `{"payload":{"a":1,"b":2,"c":3},"columns":"c"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := e.on(0)[0].Payload(); got != "b,a\n2,1\n" {
		t.Errorf("csv = %q", got)
	}
}
