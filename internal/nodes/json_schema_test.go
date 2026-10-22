package nodes

import (
	"strings"
	"testing"
)

const jsonNodeSchema = `{"type":"object","required":["id","temp"],"properties":{"id":{"type":"string"},"temp":{"type":"number"}}}`

func jsonSchemaNode(t *testing.T, action string) (cfg string) {
	t.Helper()
	cfg, err := jsonConfig(map[string]any{"action": action, "schema": jsonNodeSchema})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestJSONNodeSchemaPassesAMatchingText(t *testing.T) {
	n := build(t, "json", jsonSchemaNode(t, ""), newTestServices())
	e, err := send(t, n, msg(t, `{"payload":"{\"id\":\"p1\",\"temp\":20}"}`))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	got, _ := e.on(0)[0].Payload().(map[string]any)
	if got["id"] != "p1" {
		t.Fatalf("payload %v", e.on(0)[0].Payload())
	}
}

func TestJSONNodeSchemaRefusesAMismatchAndNamesTheField(t *testing.T) {
	n := build(t, "json", jsonSchemaNode(t, ""), newTestServices())
	m := msg(t, `{"payload":"{\"id\":\"p1\",\"temp\":\"hot\"}"}`)
	e, err := send(t, n, m)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "payload.temp: got string, want number") {
		t.Fatalf("error text: %v", err)
	}
	if len(e.on(0)) != 0 {
		t.Fatal("a message that fails the schema must not continue")
	}
	list, ok, _ := m.Get("schemaError")
	if !ok {
		t.Fatal("msg.schemaError not set")
	}
	first := list.([]any)[0].(map[string]any)
	if first["path"] != "payload.temp" || first["keyword"] != "type" {
		t.Fatalf("schemaError %v", list)
	}
}

func TestJSONNodeSchemaChecksObjectsAndStringModes(t *testing.T) {
	// An object in "always object" mode and in toggle mode (which renders it).
	for _, action := range []string{"", "obj"} {
		n := build(t, "json", jsonSchemaNode(t, action), newTestServices())
		if _, err := send(t, n, msg(t, `{"payload":{"id":"p1"}}`)); err == nil || !strings.Contains(err.Error(), `payload.temp: missing property "temp"`) {
			t.Fatalf("action %q: %v", action, err)
		}
	}
	// "Always text": text in stays text out, and is checked.
	n := build(t, "json", jsonSchemaNode(t, "str"), newTestServices())
	e, err := send(t, n, msg(t, `{"payload":"{\"id\":\"p1\",\"temp\":1}"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, isStr := e.on(0)[0].Payload().(string); !isStr {
		t.Fatal("text should stay text")
	}
	if _, err := send(t, n, msg(t, `{"payload":"{\"id\":1,\"temp\":1}"}`)); err == nil {
		t.Fatal("text that parses to a mismatch should be refused")
	}
	if _, err := send(t, n, msg(t, `{"payload":{"id":1,"temp":1}}`)); err == nil {
		t.Fatal("an object that mismatches should be refused before it is rendered")
	}
}

func TestJSONNodeInvalidSchemaRefusesTheDeploy(t *testing.T) {
	cfg, _ := jsonConfig(map[string]any{"schema": `{"type":"strnig"}`})
	err := buildErr(t, "json", cfg, newTestServices())
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("want a schema error, got %v", err)
	}
	cfg, _ = jsonConfig(map[string]any{"schema": `{`})
	if err := buildErr(t, "json", cfg, newTestServices()); err == nil {
		t.Fatal("unparseable schema must fail construction")
	}
}

func TestJSONNodeMsgSchemaAsNodeREDDoes(t *testing.T) {
	n := build(t, "json", `{}`, newTestServices())

	e, err := send(t, n, msg(t, `{"payload":"{\"a\":1}","schema":{"type":"object","properties":{"a":{"type":"number"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	out := e.on(0)[0]
	if _, has, _ := out.Get("schema"); has {
		t.Fatal("msg.schema should be removed once the message passes")
	}

	bad := msg(t, `{"payload":"{\"a\":\"x\"}","schema":{"type":"object","properties":{"a":{"type":"number"}}}}`)
	e, err = send(t, n, bad)
	if err == nil || !strings.Contains(err.Error(), "payload.a: got string, want number") {
		t.Fatalf("error: %v", err)
	}
	if len(e.on(0)) != 0 {
		t.Fatal("must not continue")
	}
	if _, has, _ := bad.Get("schema"); !has {
		t.Fatal("msg.schema stays on a message that failed")
	}

	if _, err := send(t, n, msg(t, `{"payload":"{}","schema":{"type":"strnig"}}`)); err == nil || !strings.Contains(err.Error(), "msg.schema") {
		t.Fatalf("bad msg.schema: %v", err)
	}
}

func TestJSONNodeSettingsSchemaWinsOverMsgSchema(t *testing.T) {
	n := build(t, "json", jsonSchemaNode(t, ""), newTestServices())
	// msg.schema would accept anything; the node's own schema still refuses.
	if _, err := send(t, n, msg(t, `{"payload":"{\"id\":1}","schema":{}}`)); err == nil {
		t.Fatal("the settings schema is the schema")
	}
}

// Without a schema the node behaves as it did before this existed.
func TestJSONNodeWithoutSchemaIsUnchanged(t *testing.T) {
	n := build(t, "json", `{}`, newTestServices())
	e, err := send(t, n, msg(t, `{"payload":"{\"id\":1}"}`))
	if err != nil || len(e.on(0)) != 1 {
		t.Fatalf("%v", err)
	}
	if _, has, _ := e.on(0)[0].Get("schemaError"); has {
		t.Fatal("no schema, no schemaError")
	}
	e, err = send(t, n, msg(t, `{"payload":{"a":[1,2]}}`))
	if err != nil || e.on(0)[0].Payload() != `{"a":[1,2]}` {
		t.Fatalf("%v %v", err, e.on(0)[0].Payload())
	}
}
