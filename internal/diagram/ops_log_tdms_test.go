package diagram

import (
	"math"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/tdms"
)

func tdmsFlow(t *testing.T, path, record string, props map[string]any) *flowBuilder {
	cfg := map[string]any{"path": path, "group": "Bench"}
	for k, v := range props {
		cfg[k] = v
	}
	return newFlow(t).tab("d", nil).
		node("d", "rec", "dia-const", map[string]any{"value": record}).
		node("d", "log", "dia-log-tdms", cfg).
		node("d", "n", "dia-indicator", nil).node("d", "e", "dia-indicator", nil).
		wire("rec.value", "log.row").wire("log.rows", "n.value").wire("log.err", "e.value")
}

// Two runs append to one file, and a reader sees both runs' rows in order, the
// columns as channels of one group, the file properties on the file.
func TestLogTDMSTwoRunsAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bench.tdms")
	b := tdmsFlow(t, path, `{"pressure": 101.5, "ok": true, "operator": "lee"}`, map[string]any{"properties": `{"title": "run log", "station": 4}`})
	m := b.manager()
	mustProblemFree(t, m, "d")
	for i := 0; i < 2; i++ {
		r, err := m.Start("d", StartOptions{})
		if err != nil {
			t.Fatal(err)
		}
		<-r.Done()
		if v := lastOn(t, r, "d/log.rows>n.value"); v != 1.0 {
			t.Fatalf("run %d wrote %v rows", i, v)
		}
	}
	f, err := tdms.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	g := f.Group("Bench")
	if g == nil || len(g.Channels) != 3 {
		t.Fatalf("group = %+v", g)
	}
	if got := g.Channel("pressure").Values; !reflect.DeepEqual(got, []float64{101.5, 101.5}) {
		t.Errorf("pressure = %v", got)
	}
	if got := g.Channel("ok").Values; !reflect.DeepEqual(got, []bool{true, true}) {
		t.Errorf("ok = %v", got)
	}
	if got := g.Channel("operator").Values; !reflect.DeepEqual(got, []string{"lee", "lee"}) {
		t.Errorf("operator = %v", got)
	}
	if v, _ := tdms.Prop(f.Properties, "title"); v != "run log" {
		t.Errorf("title = %v", v)
	}
	if v, _ := tdms.Prop(f.Properties, "station"); v != 4.0 {
		t.Errorf("station = %v", v)
	}
}

// A column that holds a list adds all of it, a block of samples at a time.
func TestLogTDMSBlocksOfSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rows.tdms")
	for i := 0; i < 3; i++ {
		r, err := tdmsFlow(t, path, `{"v": [1, 2, 3], "label": "x"}`, nil).run("d")
		if err != nil {
			t.Fatal(err)
		}
		if v := lastOn(t, r, "d/log.rows>n.value"); v != 1.0 {
			t.Fatal(v)
		}
	}
	f, _ := tdms.ReadFile(path)
	if got := f.Group("Bench").Channel("v").Values; !reflect.DeepEqual(got, []float64{1, 2, 3, 1, 2, 3, 1, 2, 3}) {
		t.Errorf("v = %v", got)
	}
}

// A path that cannot be a file is a fault on the error wire, code CodeFile.
func TestLogTDMSFileFault(t *testing.T) {
	dir := t.TempDir()
	r, err := tdmsFlow(t, dir, `{"v": 1}`, nil).run("d")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := lastOn(t, r, "d/log.err>e.value").(map[string]any)
	if !ok || e["status"] != true || e["code"] != float64(CodeFile) && e["code"] != CodeFile {
		t.Fatalf("err = %#v", lastOn(t, r, "d/log.err>e.value"))
	}
}

func TestLogTDMSColumnsSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.tdms")
	r, err := tdmsFlow(t, path, `{"a": 1, "b": 2, "c": 3}`, map[string]any{"columns": "c, a"}).run("d")
	if err != nil {
		t.Fatal(err)
	}
	_ = r
	f, _ := tdms.ReadFile(path)
	g := f.Group("Bench")
	if len(g.Channels) != 2 || g.Channels[0].Name != "c" || g.Channels[1].Name != "a" {
		t.Fatalf("channels = %+v", g.Channels)
	}
}

func TestTDMSColumnKinds(t *testing.T) {
	if got, _ := tdmsColumn(kindNumber, nil); !math.IsNaN(got.([]float64)[0]) {
		t.Errorf("nil number = %v", got)
	}
	if _, err := tdmsColumn(kindNumber, "x"); err == nil {
		t.Error("text in a number column")
	}
	if _, err := tdmsColumn(kindBool, 1.0); err == nil {
		t.Error("number in a bool column")
	}
	if k := kindOf([]any{1.0, 2.0}); k != kindNumber {
		t.Error("a list of numbers is a number column")
	}
	if _, err := tdmsFileProps(`{"a": [1]}`); err == nil {
		t.Error("an array property")
	}
}
