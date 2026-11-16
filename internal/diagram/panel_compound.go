package diagram

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The two controls that hold more than one value. An array control is a list of
// one kind of element that the operator grows and shrinks, and a record control is
// a fixed set of named fields of mixed kinds. Both are checked on the server the
// way a number or a switch is, and both hand the diagram one value when the
// control node runs: a list, or an object.

// recField is one field of a record control.
type recField struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // number, bool or string
}

// elemKind is the kind of one element of an array control.
func elemKind(raw map[string]any) (string, error) {
	switch k := strProp(raw, "elem", "number"); k {
	case "number", "bool", "string":
		return k, nil
	default:
		return "", fmt.Errorf("an array of %q is not one of number, bool or string", k)
	}
}

// recordFields reads the fields of a record control from "name:kind, name:kind".
// A field with no kind is a number.
func recordFields(raw map[string]any) ([]recField, error) {
	list := splitList(strProp(raw, "fields", ""))
	if len(list) == 0 {
		return nil, fmt.Errorf("a record control needs its fields, written as name:number, name:bool or name:string, comma separated")
	}
	seen := map[string]bool{}
	out := make([]recField, 0, len(list))
	for _, item := range list {
		name, kind, _ := strings.Cut(item, ":")
		name, kind = strings.TrimSpace(name), strings.TrimSpace(kind)
		if kind == "" {
			kind = "number"
		}
		if name == "" {
			return nil, fmt.Errorf("a field in %q has no name", item)
		}
		if kind != "number" && kind != "bool" && kind != "string" {
			return nil, fmt.Errorf("the field %q is %q, and a field is number, bool or string", name, kind)
		}
		if seen[name] {
			return nil, fmt.Errorf("the field %q is listed twice", name)
		}
		seen[name] = true
		out = append(out, recField{Name: name, Kind: kind})
	}
	return out, nil
}

// zeroOf is what a control of a scalar kind holds before anyone touches it.
func zeroOf(kind string) any {
	switch kind {
	case "bool":
		return false
	case "string":
		return ""
	}
	return 0.0
}

// coerceElement checks one value against a scalar kind and, for a number, the
// control's range.
func coerceElement(kind string, raw map[string]any, v any) (any, error) {
	switch kind {
	case "bool":
		return coerceBool(v)
	case "string":
		return coerceText(v)
	}
	return coerceNumber(raw["min"], raw["max"], v)
}

// asList reads a list from what the browser or the API sent.
func asList(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case []float64:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out, true
	case []string:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out, true
	case []bool:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out, true
	}
	return nil, false
}

func coerceArray(raw map[string]any, v any) (any, error) {
	kind, err := elemKind(raw)
	if err != nil {
		return nil, err
	}
	list, ok := asList(v)
	if !ok {
		return nil, fmt.Errorf("an array control takes a list")
	}
	if max := intProp(raw, "maxlen", 0); max > 0 && len(list) > max {
		return nil, fmt.Errorf("%d items is more than the most this control holds, %d", len(list), max)
	}
	out := make([]any, len(list))
	for i, e := range list {
		c, err := coerceElement(kind, raw, e)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i+1, err)
		}
		out[i] = c
	}
	return out, nil
}

func coerceRecord(raw map[string]any, v any) (any, error) {
	fields, err := recordFields(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("a record control takes an object of its fields")
	}
	known := map[string]bool{}
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		known[f.Name] = true
		x, given := m[f.Name]
		if !given {
			out[f.Name] = zeroOf(f.Kind)
			continue
		}
		// A record's fields have no range: min and max belong to an array's items.
		switch f.Kind {
		case "number":
			c, err := coerceNumber(nil, nil, x)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", f.Name, err)
			}
			out[f.Name] = c
		default:
			c, err := coerceElement(f.Kind, raw, x)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", f.Name, err)
			}
			out[f.Name] = c
		}
	}
	for k := range m {
		if !known[k] {
			return nil, fmt.Errorf("%q is not a field of this record", k)
		}
	}
	return out, nil
}

// compoundDefault is the starting value of an array or a record control. The
// starting value is written as JSON, and for an array a plain comma list of
// elements reads too.
func compoundDefault(raw map[string]any, kind string) (any, error) {
	d, _ := raw["default"].(string)
	d = strings.TrimSpace(d)
	if kind == "array" {
		if d == "" {
			return coerceArray(raw, []any{})
		}
		var list []any
		if strings.HasPrefix(d, "[") {
			if err := json.Unmarshal([]byte(d), &list); err != nil {
				return nil, fmt.Errorf("the starting value %q is not a JSON list: %v", d, err)
			}
		} else {
			// Each piece is checked as the kind of element it must be, so "1, 2" is
			// two numbers and "on, off" is not a list of switches.
			for _, p := range splitList(d) {
				list = append(list, p)
			}
		}
		return coerceArray(raw, list)
	}
	// A record.
	if _, err := recordFields(raw); err != nil {
		return nil, err
	}
	m := map[string]any{}
	if d != "" {
		if err := json.Unmarshal([]byte(d), &m); err != nil {
			return nil, fmt.Errorf("the starting value %q is not a JSON object: %v", d, err)
		}
	}
	return coerceRecord(raw, m)
}
