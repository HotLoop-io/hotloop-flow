package diagram

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// P3: an intensity chart takes a grid and keeps the last one.

func intensityFlow(t *testing.T, grid string) *flowBuilder {
	return newFlow(t).tab("main", nil).
		node("main", "g", "dia-const", map[string]any{"value": grid}).
		node("main", "c", "dia-chart", map[string]any{"mode": "intensity", "label": "Heat"}).
		wire("g.value", "c.y")
}

func TestAnIntensityChartKeepsTheGridItWasGiven(t *testing.T) {
	m := intensityFlow(t, "[[1,2,3],[4,5,6]]").manager()
	mustProblemFree(t, m, "main")
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	if st, err := r.State(); st != StateDone {
		t.Fatalf("%s %v", st, err)
	}
	p, _ := m.Panel("main")
	c := p.Snapshot().Charts["c"]
	if c == nil || c.Mode != "intensity" || !reflect.DeepEqual(c.Z, [][]float64{{1, 2, 3}, {4, 5, 6}}) {
		t.Fatalf("%+v", c)
	}
}

func TestAnIntensityChartRefusesWhatIsNotAGrid(t *testing.T) {
	for _, c := range []struct{ grid, want string }{
		{"[1,2,3]", "row 1 is not an array of numbers"},
		{"[[1,2],[3]]", "row 2 has 1 columns and row 1 has 2"},
		{`[["a"]]`, "row 1 is not an array of numbers"},
	} {
		_, err := gridOf(mustJSONList(t, c.grid))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.grid, err, c.want)
		}
	}
	big := make([]any, intensityMax+1)
	if _, err := gridOf(big); err == nil || !strings.Contains(err.Error(), "more than an intensity chart draws") {
		t.Fatalf("%v", err)
	}
}

func TestAChartThatIsNotIntensityIsUnchanged(t *testing.T) {
	m := newFlow(t).tab("main", nil).
		node("main", "a", "dia-const", map[string]any{"value": "[1,2,3]"}).
		node("main", "c", "dia-chart", map[string]any{"mode": "graph"}).wire("a.value", "c.y").manager()
	r, _ := m.Start("main", StartOptions{})
	<-r.Done()
	p, _ := m.Panel("main")
	if c := p.Snapshot().Charts["c"]; c == nil || !reflect.DeepEqual(c.Y, []float64{1, 2, 3}) || c.Z != nil {
		t.Fatalf("%+v", c)
	}
}

func mustJSONList(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
