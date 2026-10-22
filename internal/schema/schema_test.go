package schema

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

const sensorSchema = `{
  "type": "object",
  "required": ["id", "temp"],
  "properties": {
    "id": {"type": "string"},
    "temp": {"type": "number", "minimum": -50, "maximum": 400},
    "tags": {"type": "array", "items": {"type": "string"}},
    "a b": {"type": "integer"}
  },
  "additionalProperties": false
}`

func mustCompile(t *testing.T, text string) *Schema {
	t.Helper()
	s, err := Compile([]byte(text))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return s
}

func decode(t *testing.T, text string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestValidValuePasses(t *testing.T) {
	s := mustCompile(t, sensorSchema)
	if err := s.Validate("payload", decode(t, `{"id":"p1","temp":21.5,"tags":["a"]}`)); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestViolationsNameTheBadField(t *testing.T) {
	s := mustCompile(t, sensorSchema)
	cases := []struct {
		name, value, path, keyword, contains string
	}{
		{"wrong type", `{"id":"p1","temp":"hot"}`, "payload.temp", "type", "got string, want number"},
		{"missing", `{"id":"p1"}`, "payload.temp", "required", `missing property "temp"`},
		{"range", `{"id":"p1","temp":900}`, "payload.temp", "maximum", "900"},
		{"array item", `{"id":"p1","temp":1,"tags":["a",2]}`, "payload.tags[1]", "type", "got number, want string"},
		{"odd key", `{"id":"p1","temp":1,"a b":"x"}`, `payload["a b"]`, "type", "got string, want integer"},
		{"extra", `{"id":"p1","temp":1,"zz":1}`, "payload", "additionalProperties", "zz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.Validate("payload", decode(t, c.value))
			v, ok := err.(*Violations)
			if !ok {
				t.Fatalf("want *Violations, got %T %v", err, err)
			}
			found := false
			for _, x := range v.List {
				if x.Path == c.path && x.Keyword == c.keyword && strings.Contains(x.Message, c.contains) {
					found = true
				}
			}
			if !found {
				t.Fatalf("no violation at %s/%s containing %q in %+v", c.path, c.keyword, c.contains, v.List)
			}
		})
	}
}

func TestEveryViolationIsReportedAndCapped(t *testing.T) {
	s := mustCompile(t, `{"type":"array","items":{"type":"number"}}`)
	items := make([]any, 25)
	for i := range items {
		items[i] = "x"
	}
	err := s.Validate("payload", items).(*Violations)
	if len(err.List) != maxReported || err.More != 15 {
		t.Fatalf("got %d listed and %d more", len(err.List), err.More)
	}
	if !strings.HasSuffix(err.Error(), "and 15 more") {
		t.Fatalf("error text: %s", err.Error())
	}
}

func TestDraftsAndDefault(t *testing.T) {
	// No $schema reads as 2020-12: prefixItems is a 2020-12 keyword.
	s := mustCompile(t, `{"prefixItems":[{"type":"string"},{"type":"number"}]}`)
	if s.Validate("payload", decode(t, `["a",1]`)) != nil {
		t.Fatal("2020-12 prefixItems should pass")
	}
	if s.Validate("payload", decode(t, `["a","b"]`)) == nil {
		t.Fatal("2020-12 prefixItems should refuse")
	}
	// A schema that names draft-07 is read as draft-07, where "exclusiveMinimum" is a number.
	d7 := mustCompile(t, `{"$schema":"http://json-schema.org/draft-07/schema#","type":"number","exclusiveMinimum":0}`)
	if d7.Validate("payload", 0.0) == nil || d7.Validate("payload", 1.0) != nil {
		t.Fatal("draft-07 exclusiveMinimum")
	}
}

func TestCompileErrors(t *testing.T) {
	for name, text := range map[string]string{
		"not json":      `{`,
		"not an object": `[1]`,
		"bad type":      `{"type":"strnig"}`,
		"remote ref":    `{"$ref":"https://example.com/a.json"}`,
		"file ref":      `{"$ref":"file:///etc/hosts"}`,
	} {
		if _, err := Compile([]byte(text)); err == nil {
			t.Errorf("%s: want a compile error", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
}

func TestLocalRefsWork(t *testing.T) {
	s := mustCompile(t, `{"$defs":{"pos":{"type":"number","exclusiveMinimum":0}},"properties":{"n":{"$ref":"#/$defs/pos"}}}`)
	if s.Validate("payload", decode(t, `{"n":-1}`)) == nil {
		t.Fatal("ref should refuse")
	}
}

func TestNonJSONShapedValuesAreValidatedAsTheyWouldSerialize(t *testing.T) {
	s := mustCompile(t, `{"type":"object","properties":{"n":{"type":"integer"},"b":{"type":"string"}}}`)
	type reading struct {
		N int    `json:"n"`
		B []byte `json:"b"`
	}
	if err := s.Validate("payload", reading{N: 3, B: []byte("hi")}); err != nil {
		t.Fatalf("%v", err)
	}
	v := map[string]any{"n": int64(3), "b": engine.ImmutableBytes("x"), "f": float32(1)}
	if err := s.Validate("payload", v); err != nil {
		t.Fatalf("%v", err)
	}
	// The caller's value is never modified.
	if _, ok := v["b"].(engine.ImmutableBytes); !ok {
		t.Fatal("validation changed the message")
	}
}

func TestIntegerValuedFloatIsAnInteger(t *testing.T) {
	s := mustCompile(t, `{"type":"integer"}`)
	if s.Validate("payload", 5.0) != nil {
		t.Fatal("5.0 is an integer in JSON Schema")
	}
	if s.Validate("payload", 5.5) == nil {
		t.Fatal("5.5 is not")
	}
}

func TestFormatIsAsserted(t *testing.T) {
	s := mustCompile(t, `{"type":"object","properties":{"at":{"type":"string","format":"date-time"},"x":{"type":"string","format":"made-up"}}}`)
	if s.Validate("payload", decode(t, `{"at":"2026-10-22T04:40:26Z","x":"anything"}`)) != nil {
		t.Fatal("a real date-time and an unknown format should pass")
	}
	err := s.Validate("payload", decode(t, `{"at":"yesterday"}`))
	if err == nil || !strings.Contains(err.Error(), "payload.at") {
		t.Fatalf("a bad date-time should be refused, got %v", err)
	}
}

func TestEmptyRootIsTheWholeMessage(t *testing.T) {
	s := mustCompile(t, `{"type":"object","required":["topic"],"properties":{"payload":{"type":"object","properties":{"temp":{"type":"number"}}}}}`)
	v := s.Validate("", decode(t, `{"payload":{"temp":"x"}}`)).(*Violations)
	got := strings.Join(v.Paths(), ",")
	if got != "payload.temp,topic" {
		t.Fatalf("paths %s", got)
	}
	whole := mustCompile(t, `{"type":"string"}`).Validate("", 1.0).(*Violations)
	if whole.List[0].Path != "msg" {
		t.Fatalf("path %q", whole.List[0].Path)
	}
}
