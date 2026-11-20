package nodes

import (
	"reflect"
	"strconv"
	"testing"
)

// "Recreate message sequences" on a Switch: each output gets a sequence of its
// own, with its own id, index and count, so a Join behind it knows how many to
// wait for. Node-RED's own example for it depends on this.
func TestSwitchRepairBuildsASequenceForEachOutput(t *testing.T) {
	n := build(t, "switch", `{"property":"payload","propertyType":"msg","rules":[{"t":"gt","v":"0","vt":"num"},{"t":"else"}],"checkall":"true","repair":true,"outputs":2}`, newTestServices())
	e := newTestEmitter()
	for i, p := range []int{3, -2, -1, 1} {
		m := msg(t, `{"payload":`+itoaT(p)+`,"parts":{"id":"S","index":`+itoaT(i)+`,"count":4}}`)
		if err := pushTo(t, n, e, m); err != nil {
			t.Fatal(err)
		}
	}
	pos, neg := e.on(0), e.on(1)
	if !reflect.DeepEqual(payloads(pos), []any{3.0, 1.0}) || !reflect.DeepEqual(payloads(neg), []any{-2.0, -1.0}) {
		t.Fatalf("routed %v and %v", payloads(pos), payloads(neg))
	}
	for i, m := range pos {
		p := parts(t, m)
		if p["count"] != 2 || p["index"] != i || p["id"] == "S" {
			t.Errorf("positive part %d = %v", i, p)
		}
	}
	if parts(t, pos[0])["id"] == parts(t, neg[0])["id"] {
		t.Errorf("both outputs share one sequence id")
	}
}

func itoaT(i int) string { return strconv.Itoa(i) }
