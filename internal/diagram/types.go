package diagram

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Shared record types. A record type is named once, on a diagram tab's settings
// under "types", and every node that says it holds a value of that type is
// checked against the one definition:
//
//	{"types": {"Reading": {"fields": {"pressure": "number", "ok": "boolean",
//	                                  "state": {"enum": ["idle", "run"]}}}}}
//
// A field is one of number, integer, boolean, string, timestamp, array, waveform,
// "record:Name" (another shared type) or an enum written as
// {"enum": [...]}. Change the definition and every use is checked again at the
// next deploy: a bundle that names a field the type no longer has, or leaves out
// one it gained, is a problem on that node, in the same error list as an unwired
// input. The same definitions are available as JSON Schema, which is what typed
// wires check.
type RecordType struct {
	Name   string
	Fields map[string]FieldType
	Tab    string // the diagram that defines it
}

// FieldType is the type of one field.
type FieldType struct {
	Kind   string   // number, integer, boolean, string, timestamp, array, record, enum
	Record string   // for a record, the name of its type
	Enum   []string // for an enum, the allowed values
}

func (f FieldType) String() string {
	switch f.Kind {
	case "record":
		return "record " + f.Record
	case "enum":
		return "one of " + strings.Join(f.Enum, ", ")
	}
	return f.Kind
}

// FieldNames is the field names of a type in order of name.
func (t RecordType) FieldNames() []string {
	out := make([]string, 0, len(t.Fields))
	for n := range t.Fields {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func parseFieldType(v any) (FieldType, error) {
	switch x := v.(type) {
	case string:
		switch x {
		case "number", "integer", "boolean", "string", "timestamp", "array", "waveform":
			return FieldType{Kind: x}, nil
		}
		if name, ok := strings.CutPrefix(x, "record:"); ok && name != "" {
			return FieldType{Kind: "record", Record: name}, nil
		}
		return FieldType{}, fmt.Errorf("%q is not a field type (number, integer, boolean, string, timestamp, array, waveform, record:Name or {\"enum\": [...]})", x)
	case map[string]any:
		if e, ok := x["enum"].([]any); ok && len(e) > 0 {
			ft := FieldType{Kind: "enum"}
			for _, v := range e {
				s, ok := v.(string)
				if !ok {
					return FieldType{}, fmt.Errorf("an enum holds text values, not %v", v)
				}
				ft.Enum = append(ft.Enum, s)
			}
			return ft, nil
		}
	}
	return FieldType{}, fmt.Errorf("%v is not a field type", v)
}

// collectTypes reads the shared types from every diagram's settings, and says
// what is wrong with the definitions.
func collectTypes(s *Set) {
	s.Types = map[string]RecordType{}
	for _, id := range s.Order {
		d := s.Diagrams[id]
		defs, _ := d.Settings["types"].(map[string]any)
		names := make([]string, 0, len(defs))
		for n := range defs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			body, _ := defs[name].(map[string]any)
			fields, _ := body["fields"].(map[string]any)
			if len(fields) == 0 {
				d.Problems = append(d.Problems, Problem{Text: fmt.Sprintf("the type %q has no fields", name)})
				continue
			}
			if prev, dup := s.Types[name]; dup {
				d.Problems = append(d.Problems, Problem{Text: fmt.Sprintf("the type %q is already defined in %q", name, s.Diagrams[prev.Tab].Label)})
				continue
			}
			rt := RecordType{Name: name, Fields: map[string]FieldType{}, Tab: id}
			for fn, fv := range fields {
				ft, err := parseFieldType(fv)
				if err != nil {
					d.Problems = append(d.Problems, Problem{Text: fmt.Sprintf("the type %q, field %q: %v", name, fn, err)})
					continue
				}
				rt.Fields[fn] = ft
			}
			s.Types[name] = rt
		}
	}
	// A record field has to name a type that exists.
	for _, name := range sortedTypeNames(s) {
		t := s.Types[name]
		for _, fn := range t.FieldNames() {
			if f := t.Fields[fn]; f.Kind == "record" {
				if _, ok := s.Types[f.Record]; !ok {
					d := s.Diagrams[t.Tab]
					d.Problems = append(d.Problems, Problem{Text: fmt.Sprintf("the type %q, field %q: there is no type %q", name, fn, f.Record)})
				}
			}
		}
	}
}

func sortedTypeNames(s *Set) []string {
	out := make([]string, 0, len(s.Types))
	for n := range s.Types {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Schema is the type as JSON Schema, which is how typed wires check a value.
func (s *Set) Schema(name string) (map[string]any, bool) {
	t, ok := s.Types[name]
	if !ok {
		return nil, false
	}
	props := map[string]any{}
	req := []any{}
	for _, fn := range t.FieldNames() {
		props[fn] = s.fieldSchema(t.Fields[fn])
		req = append(req, fn)
	}
	return map[string]any{"type": "object", "title": name, "properties": props, "required": req, "additionalProperties": false}, true
}

func (s *Set) fieldSchema(f FieldType) map[string]any {
	switch f.Kind {
	case "number", "boolean", "string", "integer":
		return map[string]any{"type": f.Kind}
	case "timestamp":
		return map[string]any{"type": "string", "format": "date-time"}
	case "array":
		return map[string]any{"type": "array"}
	case "waveform":
		return map[string]any{"type": "object", "required": []any{"type", "dt", "y"}, "properties": map[string]any{"type": map[string]any{"const": waveType}}}
	case "enum":
		e := make([]any, len(f.Enum))
		for i, v := range f.Enum {
			e[i] = v
		}
		return map[string]any{"type": "string", "enum": e}
	case "record":
		if sch, ok := s.Schema(f.Record); ok {
			return sch
		}
	}
	return map[string]any{}
}

// ValidateRecord says what is wrong with a value held to a type, or nothing.
func (s *Set) ValidateRecord(typ string, v any) error {
	t, ok := s.Types[typ]
	if !ok {
		return fmt.Errorf("there is no type %q", typ)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%v is not a record", cell(v))
	}
	for _, fn := range t.FieldNames() {
		fv, has := m[fn]
		if !has {
			return fmt.Errorf("%s has no field %q", typ, fn)
		}
		if err := s.validateField(t.Fields[fn], fv); err != nil {
			return fmt.Errorf("%s.%s: %w", typ, fn, err)
		}
	}
	for fn := range m {
		if _, ok := t.Fields[fn]; !ok {
			return fmt.Errorf("%s has no field %q, but the value does", typ, fn)
		}
	}
	return nil
}

func (s *Set) validateField(f FieldType, v any) error {
	switch f.Kind {
	case "number":
		if _, ok := num(v); !ok {
			return fmt.Errorf("%v is not a number", cell(v))
		}
	case "integer":
		n, ok := num(v)
		if !ok || n != float64(int64(n)) {
			return fmt.Errorf("%v is not a whole number", cell(v))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%v is not true or false", cell(v))
		}
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%v is not text", cell(v))
		}
	case "timestamp":
		str, _ := v.(string)
		if _, err := time.Parse(time.RFC3339Nano, str); err != nil {
			return fmt.Errorf("%v is not a time (RFC 3339)", cell(v))
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return fmt.Errorf("%v is not an array", cell(v))
		}
	case "waveform":
		if _, ok := asWave(v); !ok {
			return fmt.Errorf("%v is not a waveform", cell(v))
		}
	case "enum":
		str, _ := v.(string)
		for _, e := range f.Enum {
			if e == str {
				return nil
			}
		}
		return fmt.Errorf("%v is not one of %s", cell(v), strings.Join(f.Enum, ", "))
	case "record":
		return s.ValidateRecord(f.Record, v)
	}
	return nil
}
