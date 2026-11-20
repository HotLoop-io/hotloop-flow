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
