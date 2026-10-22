// Package schema is Flow's JSON Schema validator. The json node uses it, and
// so does anything else in Flow that needs to check a value against a schema,
// so there is one validator and one error shape.
//
// It wraps github.com/santhosh-tekuri/jsonschema/v6 (Apache-2.0, pure Go).
// Schemas without a "$schema" keyword are read as draft 2020-12, pinned here
// rather than left to the library's default, which its authors say will move.
// Drafts 4, 6, 7 and 2019-09 are accepted when a schema names them.
//
// A schema is self-contained. A "$ref" to another URL is an error at compile
// time: nothing here reaches out to the network or the disk to finish a
// schema, so a flow behaves the same on an edge box with no route out.
package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const resourceURL = "flow-schema:///schema.json"

// maxReported caps how many violations an error carries, so one badly wrong
// message cannot build an error the size of the message.
const maxReported = 10

// Schema is a compiled JSON Schema. It is safe for concurrent use.
type Schema struct {
	compiled *jsonschema.Schema
}

// Compile compiles a schema given as JSON text.
func Compile(text []byte) (*Schema, error) {
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("schema is not valid JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("schema is not valid JSON: more than one value")
	}
	return CompileValue(doc)
}

// CompileValue compiles a schema that is already a decoded JSON value, the way
// msg.schema arrives.
func CompileValue(doc any) (*Schema, error) {
	switch doc.(type) {
	case map[string]any, bool:
	default:
		return nil, fmt.Errorf("a schema is a JSON object or a boolean, got %s", jsonKind(doc))
	}
	doc, err := normalize(doc)
	if err != nil {
		return nil, fmt.Errorf("schema is not valid JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	// Draft 2019-09 and later treat "format" as a note by default. A line that
	// says a field is a date-time means to refuse one that isn't.
	c.AssertFormat()
	if err := c.AddResource(resourceURL, doc); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	compiled, err := c.Compile(resourceURL)
	if err != nil {
		return nil, fmt.Errorf("schema: %s", cleanCompileError(err))
	}
	return &Schema{compiled: compiled}, nil
}

// Validate checks value against the schema. root names the value in paths, so
// a bad field comes back as "payload.temp" rather than "/temp". An empty root
// is for a value that is the whole message: its fields come back as "payload". The error is a
// *Violations, nil when the value conforms.
func (s *Schema) Validate(root string, value any) error {
	v, err := normalize(value)
	if err != nil {
		return &Violations{List: []Violation{{Path: root, Message: "not representable as JSON: " + err.Error()}}}
	}
	err = s.compiled.Validate(v)
	if err == nil {
		return nil
	}
	verr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return &Violations{List: []Violation{{Path: root, Message: err.Error()}}}
	}
	var out Violations
	collect(verr, root, &out)
	sort.SliceStable(out.List, func(i, j int) bool { return out.List[i].Path < out.List[j].Path })
	if len(out.List) > maxReported {
		out.More = len(out.List) - maxReported
		out.List = out.List[:maxReported]
	}
	return &out
}

// Violation is one place a value broke the schema.
type Violation struct {
	// Path is the bad field, "payload.sensors[2].temp".
	Path string `json:"path"`
	// Keyword is the schema keyword that failed, "type" or "required".
	Keyword string `json:"keyword,omitempty"`
	// Message says what is wrong, "got string, want number".
	Message string `json:"message"`
}

// Violations is every place a value broke the schema, and the error Validate
// returns.
type Violations struct {
	List []Violation `json:"violations"`
	// More counts the violations left out past the first few.
	More int `json:"more,omitempty"`
}

func (v *Violations) Error() string {
	parts := make([]string, 0, len(v.List)+1)
	for _, x := range v.List {
		parts = append(parts, x.Path+": "+x.Message)
	}
	if v.More > 0 {
		parts = append(parts, fmt.Sprintf("and %d more", v.More))
	}
	return strings.Join(parts, "; ")
}

// ViolationsOf finds the *Violations in an error chain, or nil.
func ViolationsOf(err error) *Violations {
	var v *Violations
	if errors.As(err, &v) {
		return v
	}
	return nil
}

// Paths lists the bad fields.
func (v *Violations) Paths() []string {
	out := make([]string, len(v.List))
	for i, x := range v.List {
		out[i] = x.Path
	}
	return out
}

var printer = message.NewPrinter(language.English)

func collect(e *jsonschema.ValidationError, root string, out *Violations) {
	if len(e.Causes) > 0 {
		for _, c := range e.Causes {
			collect(c, root, out)
		}
		return
	}
	path := formatPath(root, e.InstanceLocation)
	kw := ""
	if kp := e.ErrorKind.KeywordPath(); len(kp) > 0 {
		kw = kp[len(kp)-1]
	}
	// A missing property is reported against the property, not the object that
	// lacks it, because the field is what the person has to go and look for.
	if req, ok := e.ErrorKind.(*kind.Required); ok {
		for _, name := range req.Missing {
			out.List = append(out.List, Violation{
				Path:    formatPath(root, append(append([]string{}, e.InstanceLocation...), name)),
				Keyword: "required",
				Message: "missing property " + strconv.Quote(name),
			})
		}
		return
	}
	out.List = append(out.List, Violation{Path: path, Keyword: kw, Message: e.ErrorKind.LocalizedString(printer)})
}

// Path renders a place in a value the way a Change node writes a property:
// "payload.sensors[2].temp". root is the first segment; when it is empty the
// tokens start the path, and a path with no tokens at all reads "msg".
func Path(root string, tokens ...string) string {
	var b strings.Builder
	b.WriteString(root)
	for _, t := range tokens {
		if _, err := strconv.Atoi(t); err == nil {
			b.WriteString("[" + t + "]")
		} else if isIdent(t) {
			if b.Len() > 0 {
				b.WriteString(".")
			}
			b.WriteString(t)
		} else {
			b.WriteString("[" + strconv.Quote(t) + "]")
		}
	}
	if b.Len() == 0 {
		return "msg"
	}
	return b.String()
}

func formatPath(root string, tokens []string) string { return Path(root, tokens...) }

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r)) {
			continue
		}
		return false
	}
	return true
}

func cleanCompileError(err error) string {
	s := err.Error()
	s = strings.ReplaceAll(s, resourceURL, "schema")
	return s
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return "a number"
	}
}

// normalize returns value in the shapes the validator reads: nil, bool, the
// numeric types, string, []any and map[string]any. Anything else, a []byte or a
// typed slice or a struct, goes through JSON, which is what it would become on
// the wire anyway. A value that is already plain comes back unchanged and
// uncopied.
func normalize(v any) (any, error) {
	out, _, err := norm(v)
	return out, err
}

// norm is normalize plus whether it had to change anything.
func norm(v any) (any, bool, error) {
	switch t := v.(type) {
	case nil, bool, string, float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		return v, false, nil
	case map[string]any:
		var out map[string]any
		for k, x := range t {
			n, changed, err := norm(x)
			if err != nil {
				return nil, false, err
			}
			if changed && out == nil {
				out = make(map[string]any, len(t))
				for k2, x2 := range t {
					out[k2] = x2
				}
			}
			if changed {
				out[k] = n
			}
		}
		if out != nil {
			return out, true, nil
		}
		return t, false, nil
	case []any:
		var out []any
		for i, x := range t {
			n, changed, err := norm(x)
			if err != nil {
				return nil, false, err
			}
			if changed && out == nil {
				out = append([]any(nil), t...)
			}
			if changed {
				out[i] = n
			}
		}
		if out != nil {
			return out, true, nil
		}
		return t, false, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, false, err
	}
	return out, true, nil
}
