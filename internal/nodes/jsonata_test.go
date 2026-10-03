package nodes

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// JSONata through the nodes that take it, written the way Node-RED's editor
// writes them into flows.json, so an imported flow is what is under test.

func TestChangeSetsFromAJSONataExpression(t *testing.T) {
	svc := newTestServices()
	if err := svc.contexts.Flow("test-flow").Set("scale", 2.0); err != nil {
		t.Fatal(err)
	}
	n := build(t, "change", `{"rules":[
        {"t":"set","p":"payload","pt":"msg","to":"$sum(payload) * $flowContext('scale')","tot":"jsonata"},
        {"t":"set","p":"total","pt":"flow","to":"msg.payload","tot":"jsonata"},
        {"t":"set","p":"gone","pt":"msg","to":"nothing.here","tot":"jsonata"}
    ]}`, svc)
	e, err := send(t, n, msg(t, `{"payload":[1,2,3],"gone":"still here"}`))
	if err != nil {
		t.Fatal(err)
	}
	out := e.on(0)[0]
	if got := out.Payload(); got != 12.0 {
		t.Errorf("payload = %v, want 12", got)
	}
	if v, _, _ := svc.contexts.Flow("test-flow").Get("total"); v != 12.0 {
		t.Errorf("flow.total = %v, want 12 from a legacy-mode expression", v)
	}
	// An expression that comes out undefined deletes the target, the same as
	// any other source that does not resolve.
	if _, ok := out.Data["gone"]; ok {
		t.Error("an undefined result left the old value in place")
	}
}

// A typo in an expression fails the node at deploy with the parser's message,
// not on every message afterwards.
func TestBrokenExpressionsFailAtDeploy(t *testing.T) {
	svc := newTestServices()
	for typ, cfg := range map[string]string{
		"change": `{"rules":[{"t":"set","p":"payload","pt":"msg","to":"$sum(payload","tot":"jsonata"}]}`,
		"switch": `{"property":"payload","rules":[{"t":"jsonata_exp","v":"payload >","vt":"jsonata"}]}`,
		"sort":   `{"target":"payload","targetType":"msg","msgKey":"(","msgKeyType":"jsonata"}`,
		"inject": `{"props":[{"p":"payload","v":"[1,2","vt":"jsonata"}]}`,
		"debug":  `{"complete":"payload.","targetType":"jsonata"}`,
	} {
		err := buildErr(t, typ, cfg, svc)
		if err == nil {
			t.Errorf("%s: a broken expression was accepted", typ)
			continue
		}
		if !strings.Contains(err.Error(), "JSONata") {
			t.Errorf("%s: error %q does not say it was the expression", typ, err)
		}
	}
}

func TestSwitchJSONataRule(t *testing.T) {
	svc := newTestServices()
	n := build(t, "switch", `{"property":"payload","checkall":"true","outputs":3,"rules":[
        {"t":"jsonata_exp","v":"payload.temp > 80","vt":"jsonata"},
        {"t":"jsonata_exp","v":"$I = $N - 1","vt":"jsonata"},
        {"t":"jsonata_exp","v":"'yes'","vt":"jsonata"}
    ]}`, svc)

	e, err := send(t, n, msg(t, `{"payload":{"temp":91}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e.ports(), []int{0}) {
		t.Errorf("hot reading went to ports %v, want [0]", e.ports())
	}

	// The last message of a sequence: $I and $N come from msg.parts.
	e, err = send(t, n, msg(t, `{"payload":{"temp":20},"parts":{"id":"s1","index":4,"count":5}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e.ports(), []int{1}) {
		t.Errorf("last of a sequence went to ports %v, want [1]", e.ports())
	}

	// Only exactly true routes. A truthy string does not.
	e, err = send(t, n, msg(t, `{"payload":{"temp":20}}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.total() != 0 {
		t.Errorf("a rule evaluating to a truthy string routed to %v", e.ports())
	}
}

func TestSwitchJSONataPropertyAndValue(t *testing.T) {
	svc := newTestServices()
	n := build(t, "switch", `{"property":"$count(payload)","propertyType":"jsonata","outputs":2,"rules":[
        {"t":"gt","v":"$flowContext('limit')","vt":"jsonata"},
        {"t":"else"}
    ]}`, svc)
	if err := svc.contexts.Flow("test-flow").Set("limit", 2.0); err != nil {
		t.Fatal(err)
	}
	e, err := send(t, n, msg(t, `{"payload":[1,2,3]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e.ports(), []int{0}) {
		t.Errorf("three items against a limit of two went to %v, want [0]", e.ports())
	}
}

func TestSortByJSONataKey(t *testing.T) {
	svc := newTestServices()
	n := build(t, "sort", `{"target":"payload","targetType":"msg","msgKey":"temp","msgKeyType":"jsonata","order":"descending"}`, svc)
	e, err := send(t, n, msg(t, `{"payload":[{"id":"a","temp":20},{"id":"b","temp":95},{"id":"c","temp":61}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, el := range e.on(0)[0].Payload().([]any) {
		ids = append(ids, el.(map[string]any)["id"].(string))
	}
	if !reflect.DeepEqual(ids, []string{"b", "c", "a"}) {
		t.Errorf("sorted by temp descending = %v", ids)
	}
}

func TestSortSequenceByJSONataKeyAndByProperty(t *testing.T) {
	svc := newTestServices()
	byExpr := build(t, "sort", `{"targetType":"seq","seqKey":"payload.temp","seqKeyType":"jsonata"}`, svc)
	byProp := build(t, "sort", `{"targetType":"seq","seqKey":"topic","seqKeyType":"msg"}`, svc)

	feed := func(n node.Node) []string {
		var out []string
		e := newTestEmitter()
		for i, m := range []string{
			`{"topic":"c","payload":{"temp":20},"parts":{"id":"g","index":0,"count":3}}`,
			`{"topic":"a","payload":{"temp":95},"parts":{"id":"g","index":1,"count":3}}`,
			`{"topic":"b","payload":{"temp":61},"parts":{"id":"g","index":2,"count":3}}`,
		} {
			if err := n.Receive(context.Background(), msg(t, m), e); err != nil {
				t.Fatalf("message %d: %v", i, err)
			}
		}
		for _, m := range e.on(0) {
			out = append(out, m.Topic())
		}
		return out
	}
	if got := feed(byExpr); !reflect.DeepEqual(got, []string{"c", "b", "a"}) {
		t.Errorf("sequence sorted by payload.temp = %v", got)
	}
	if got := feed(byProp); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("sequence sorted by topic = %v; seqKey was ignored", got)
	}
}

func TestDebugShowsAJSONataExpression(t *testing.T) {
	svc := newTestServices()
	n := build(t, "debug", `{"complete":"payload.temp & ' C'","targetType":"jsonata","tosidebar":true,
        "tostatus":true,"statusType":"jsonata","statusVal":"$string(payload.temp > 80)"}`, svc)
	e, err := send(t, n, msg(t, `{"payload":{"temp":91}}`))
	if err != nil {
		t.Fatal(err)
	}
	pub := e.publishedOn("debug")
	if len(pub) != 1 || pub[0].Data["msg"] != "91 C" {
		t.Errorf("debug showed %v, want 91 C", pub)
	}
	if len(e.statuses) != 1 || e.statuses[0].Text != "true" {
		t.Errorf("status = %v, want the status expression's result", e.statuses)
	}
}

func TestInjectJSONataProperty(t *testing.T) {
	svc := newTestServices()
	if err := svc.contexts.Global().Set("line", "3"); err != nil {
		t.Fatal(err)
	}
	n := build(t, "inject", `{"props":[
        {"p":"payload","v":"{'line': $globalContext('line'), 'ok': true}","vt":"jsonata"},
        {"p":"topic","v":"'line/' & payload.line","vt":"jsonata"}
    ]}`, svc)
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	out := e.on(0)[0]
	if !reflect.DeepEqual(out.Payload(), map[string]any{"line": "3", "ok": true}) {
		t.Errorf("payload = %#v", out.Payload())
	}
	// Properties are evaluated in order against the message being built, so
	// the topic sees the payload set a line earlier.
	if out.Topic() != "line/3" {
		t.Errorf("topic = %q", out.Topic())
	}
}
