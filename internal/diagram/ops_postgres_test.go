package diagram

import (
	"fmt"
	"strings"
	"testing"
)

// A table or column that is not an identifier is a problem in the error list at
// deploy, never an injected statement in the middle of a run.
func TestAPostgresOutWithABadNameIsAProblemAtDeploy(t *testing.T) {
	for _, c := range []struct {
		name  string
		props map[string]any
		want  string
	}{
		{"table", map[string]any{"server": "pg", "table": "t; drop table x"}, "only letters"},
		{"column", map[string]any{"server": "pg", "table": "t", "columns": "a,b c"}, "only letters"},
		{"time column", map[string]any{"server": "pg", "table": "t", "columns": "a", "timeColumn": "x-y"}, "time column"},
		{"time column twice", map[string]any{"server": "pg", "table": "t", "columns": "a,b", "timeColumn": "B"}, "both a column and the time column"},
		{"no server", map[string]any{"table": "t"}, "no server"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newFlow(t).tab("main", nil).node("main", "out", "dia-postgres-out", c.props)
			d, ok := b.manager().Diagram("main")
			if !ok {
				t.Fatal("no diagram")
			}
			var got []string
			for _, p := range d.Problems {
				got = append(got, p.Text)
			}
			if len(got) == 0 || !strings.Contains(strings.Join(got, "; "), c.want) {
				t.Fatalf("problems %v, want one with %q", got, c.want)
			}
		})
	}
}

func TestAPostgresOutWithGoodSettingsHasNoProblem(t *testing.T) {
	b := newFlow(t).tab("main", nil).
		node("main", "row", "dia-const", map[string]any{"value": `{"a":1}`}).
		node("main", "out", "dia-postgres-out", map[string]any{"server": "pg", "table": "public.readings", "columns": "a", "timeColumn": "at", "durable": true}).
		wire("row.value", "out.row")
	mustProblemFree(t, b.manager(), "main")
}

// With no server configured the run says so on the error wire, with the server's
// name and the Device code, and writes nothing.
func TestAPostgresOutWithNoRunningServerFaults(t *testing.T) {
	b := newFlow(t).tab("main", nil).
		node("main", "row", "dia-const", map[string]any{"value": `{"a":1}`}).
		node("main", "out", "dia-postgres-out", map[string]any{"server": "pg", "table": "readings", "columns": "a"}).
		node("main", "err", "dia-indicator", map[string]any{"kind": "error"}).
		wire("row.value", "out.row").wire("out.err", "err.value")
	r, err := b.run("main")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := lastOn(t, r, "main/out.err>err.value").(map[string]any)
	if e["code"] != float64(CodeDevice) || !strings.Contains(fmt.Sprint(e["source"]), "pg") {
		t.Fatalf("error out %v, want a device fault naming the server", e)
	}
}
