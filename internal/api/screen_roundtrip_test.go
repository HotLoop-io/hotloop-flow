package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

// A Flow screen is the dashboard nodes of a flow (ui_base, ui_tab, ui_group and
// the widgets), so it already travels as a file: Export, or GET /flows/export,
// writes it and Import, or a POST of it, loads it. This holds that to what a
// customer's screen needs: a flow with a whole dashboard on it, saved from one
// install and loaded into another, comes back as the same bytes with every
// dashboard node still there.
func TestADashboardFlowExportedFromOneInstallImportsIntoAnotherUnchanged(t *testing.T) {
	doc, err := os.ReadFile("../../testdata/nodered/types.json")
	if err != nil {
		t.Fatal(err)
	}
	var nodes []map[string]any
	if err := json.Unmarshal(doc, &nodes); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{}
	for _, n := range nodes {
		if typ, _ := n["type"].(string); len(typ) > 3 && typ[:3] == "ui_" {
			want[typ]++
		}
	}
	for _, typ := range []string{"ui_base", "ui_tab", "ui_group", "ui_text", "ui_button", "ui_gauge", "ui_chart", "ui_switch", "ui_numeric"} {
		if want[typ] == 0 {
			t.Fatalf("test bug: the fixture has no %s", typ)
		}
	}

	a := newTestServer(t, map[string][]string{"admin": {"*"}})
	atok := a.mustLogin(t, "admin")
	if res, out := a.do(t, "POST", "/flows", atok, doc, "HotLoop-Flow-Deployment-Type", "full"); res.StatusCode != http.StatusOK {
		t.Fatalf("deploy on A: %d %s", res.StatusCode, out)
	}
	res, exported := a.do(t, "GET", "/flows/export", atok, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export from A: %d %s", res.StatusCode, exported)
	}

	b := newTestServer(t, map[string][]string{"admin": {"*"}})
	btok := b.mustLogin(t, "admin")
	if res, out := b.do(t, "POST", "/flows", btok, exported, "HotLoop-Flow-Deployment-Type", "full"); res.StatusCode != http.StatusOK {
		t.Fatalf("import into B: %d %s", res.StatusCode, out)
	}
	res, again := b.do(t, "GET", "/flows/export", btok, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export from B: %d %s", res.StatusCode, again)
	}
	if !bytes.Equal(exported, again) {
		t.Errorf("the flow changed on its way from one install to the other:\nA %s\nB %s", exported, again)
	}
	var back []map[string]any
	if err := json.Unmarshal(again, &back); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, n := range back {
		if typ, _ := n["type"].(string); len(typ) > 3 && typ[:3] == "ui_" {
			got[typ]++
		}
	}
	for typ, n := range want {
		if got[typ] != n {
			t.Errorf("%s: %d came across of %d", typ, got[typ], n)
		}
	}
}
