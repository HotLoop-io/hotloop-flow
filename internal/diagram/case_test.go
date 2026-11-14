package diagram

import (
	"strings"
	"testing"
)

// caseFlow is a case structure whose bodies each mark that they ran and send a
// result named for their case. sel is the selector's JSON.
func caseFlow(t *testing.T, sel, cases, def string) *flowBuilder {
	b := newFlow(t).tab("main", nil)
	props := map[string]any{"cases": cases, "outs": "r"}
	if def != "" {
		props["default"] = def
	}
	b.node("main", "sel", "dia-const", map[string]any{"value": sel}).
		node("main", "case", "dia-case", props).
		node("main", "out", "dia-indicator", nil).
		node("main", "which", "dia-indicator", nil).
		wire("sel.value", "case.selector").wire("case.r", "out.value").wire("case.case", "which.value")
	for _, name := range []string{"a", "b", "c", "yes", "no", "two", "other"} {
		b.tab("body-"+name, map[string]any{"role": "body"}).
			node("body-"+name, "m"+name, "dia-test-mark", map[string]any{"tag": name}).
			node("body-"+name, "k"+name, "dia-const", map[string]any{"value": `"` + name + `"`}).
			node("body-"+name, "o"+name, "dia-body-out", map[string]any{"name": "r"}).
			wire("k"+name+".value", "m"+name+".value").wire("m"+name+".value", "o"+name+".value")
	}
	return b
}

// X5: a string selector runs only its case.
func TestCaseRunsOnlyTheChosenBody(t *testing.T) {
	resetMarks()
	r, err := caseFlow(t, `"b"`, `{"a":"body-a","b":"body-b","c":"body-c"}`, "body-other").run("main")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	if got := marks(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("ran %v, want only b", got)
	}
	if got := lastOn(t, r, "main/case.r>out.value"); got != "b" {
		t.Fatalf("result %v", got)
	}
	if got := lastOn(t, r, "main/case.case>which.value"); got != "b" {
		t.Fatalf("case %v", got)
	}
}

// X5: a value no case names runs the default case.
func TestCaseUnmatchedRunsDefault(t *testing.T) {
	resetMarks()
	r, err := caseFlow(t, `"zzz"`, `{"a":"body-a","b":"body-b"}`, "body-other").run("main")
	if err != nil {
		t.Fatal(err)
	}
	if got := marks(); len(got) != 1 || got[0] != "other" {
		t.Fatalf("ran %v, want only other", got)
	}
	if got := lastOn(t, r, "main/case.case>which.value"); got != DefaultCase {
		t.Fatalf("case %v", got)
	}
}

// X5: booleans select "true" and "false"; numbers match however they are written.
func TestCaseSelectsOnBooleanAndNumber(t *testing.T) {
	for _, tc := range []struct{ sel, cases, want string }{
		{`true`, `{"true":"body-yes","false":"body-no"}`, "yes"},
		{`false`, `{"true":"body-yes","false":"body-no"}`, "no"},
		{`2`, `{"1":"body-a","2.0":"body-two"}`, "two"},
	} {
		resetMarks()
		r, err := caseFlow(t, tc.sel, tc.cases, "").run("main")
		if err != nil {
			t.Fatal(err)
		}
		if got := lastOn(t, r, "main/case.r>out.value"); got != tc.want {
			t.Fatalf("selector %s: result %v, want %s", tc.sel, got, tc.want)
		}
		if got := marks(); len(got) != 1 || got[0] != tc.want {
			t.Fatalf("selector %s: ran %v", tc.sel, got)
		}
	}
}

// X5: with no case and no default the node reports an error and runs nothing.
func TestCaseWithNoMatchAndNoDefaultIsAnError(t *testing.T) {
	resetMarks()
	cf := caseFlow(t, `"zzz"`, `{"a":"body-a"}`, "")
	cf.node("main", "e", "dia-indicator", nil).wire("case.err", "e.value")
	r, err := cf.run("main")
	if err != nil {
		t.Fatal(err)
	}
	if got := marks(); len(got) != 0 {
		t.Fatalf("ran %v, want nothing", got)
	}
	e, _ := lastOn(t, r, "main/case.err>e.value").(map[string]any)
	src, _ := e["source"].(string)
	if e["status"] != true || e["code"] != float64(CodeBadInput) && e["code"] != CodeBadInput || !strings.Contains(src, "no case is named") {
		t.Fatalf("error out %v", e)
	}
}

// X5: a missing default is a deploy warning and not a problem; a boolean with
// both cases needs none.
func TestCaseMissingDefaultWarns(t *testing.T) {
	m := caseFlow(t, `"a"`, `{"a":"body-a"}`, "").manager()
	mustProblemFree(t, m, "main")
	d, _ := m.Diagram("main")
	if len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0].Text, "no default case") {
		t.Fatalf("warnings %v", d.Warnings)
	}
	m = caseFlow(t, `true`, `{"true":"body-yes","false":"body-no"}`, "").manager()
	if d, _ := m.Diagram("main"); len(d.Warnings) != 0 {
		t.Fatalf("a boolean with both cases warned: %v", d.Warnings)
	}
}

// A case whose body does not send out a promised result is a problem at deploy.
func TestCaseBodyMissingResultIsAProblem(t *testing.T) {
	b := caseFlow(t, `"a"`, `{"a":"body-a"}`, "")
	b.byID["case"]["outs"] = "r,extra"
	m := b.manager()
	d, _ := m.Diagram("main")
	found := false
	for _, p := range d.Problems {
		found = found || strings.Contains(p.Text, `"extra"`)
	}
	if !found {
		t.Fatalf("problems %v", d.Problems)
	}
}
