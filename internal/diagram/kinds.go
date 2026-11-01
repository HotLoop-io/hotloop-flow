package diagram

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Op is what a diagram node does when it runs. It runs once per run of its
// diagram, after every wired input holds a value, and is handed those values by
// port name. A port nobody wired is absent from in. It returns the values for its
// output ports by name; a port left out holds nothing, and what is wired to it
// receives nil.
//
// An error that the diagram should carry on its error wire is returned as one;
// the run turns it into the error record on the node's err output, so the next
// node sees it. A Go error is never swallowed: a node without an err output puts
// it on the run, which stops.
type Op interface {
	Run(ctx context.Context, rc *Ctx, in map[string]any) (map[string]any, error)
}

// Kind is one node type that runs in a diagram.
type Kind struct {
	Descriptor node.Descriptor
	// New reads the node's own settings, once, when the diagram is compiled, so a
	// bad regular expression is a problem in the error list and not a failure in
	// the middle of a run.
	New func(raw map[string]any) (Op, error)
	// Check adds problems that only this type can see, once every diagram of the
	// file is compiled, so it can look into the diagrams a structure runs.
	Check func(s *Set, d *Diagram, n *DNode) []Problem
	// Structure is true for a node that runs a body diagram.
	Structure bool
	// Terminal is true for a control or an indicator: something on the panel.
	Terminal bool
	// TakesErrors is true for a node that runs even when an error comes in,
	// because looking at the error is its job.
	TakesErrors bool
}

var kinds = map[string]*Kind{}

// Kinds returns the registered type names, for tests.
func Kinds() []string {
	out := make([]string, 0, len(kinds))
	for t := range kinds {
		out = append(out, t)
	}
	return out
}

// register adds a type to the diagram engine and, so the editor's palette and
// dialogs draw it, to the node registry. In the message runtime such a node is a
// node that never gets a message: a diagram tab is left out of it entirely.
func register(k Kind) {
	d := k.Descriptor
	d.Category = node.CategoryDiagram
	d.Inputs = 1
	d.Outputs = len(OutPorts(d, nil))
	d.Compatibility = node.Compatibility{Level: node.CompatOnly}
	d.InputPorts = append([]string(nil), d.InputPorts...)
	d.OutputPorts = append([]string(nil), d.OutputPorts...)
	k.Descriptor = d
	if _, dup := kinds[d.Type]; dup {
		panic("diagram: type registered twice: " + d.Type)
	}
	kk := k
	kinds[d.Type] = &kk
	node.MustRegister(d, func(*node.Definition) (node.Node, error) { return notMessages{}, nil })
}

type notMessages struct{}

func (notMessages) Receive(context.Context, *engine.Msg, node.Emitter) error {
	return fmt.Errorf("a diagram node runs in a diagram run and never takes a message")
}

// Error is the record that travels on an error wire: whether there is an error,
// a numeric code, where it came from, and the node it happened in. A wire that
// carries no error carries nil.
type Error struct {
	Status bool   `json:"status"`
	Code   int    `json:"code"`
	Source string `json:"source"`
	Node   string `json:"node,omitempty"`
}

func (e Error) Error() string { return fmt.Sprintf("%s (code %d)", e.Source, e.Code) }

// Fault is how an Op reports an error for the error wire, with a code.
type Fault struct {
	Code   int
	Source string
}

func (f *Fault) Error() string { return f.Source }

// Error codes. Negative codes are the engine's own; the others belong to the type
// that raised them.
const (
	CodeTimeout    = 5001
	CodeDevice     = 5002
	CodeParse      = 5003
	CodeFile       = 5004
	CodeBadInput   = 5005
	CodeInternal   = -1
	CodeCancelled  = -2
	CodeIterations = -3
)

func asError(v any) (Error, bool) {
	switch e := v.(type) {
	case Error:
		return e, e.Status
	case *Error:
		if e == nil {
			return Error{}, false
		}
		return *e, e.Status
	}
	return Error{}, false
}

// jsonValue makes a token safe to hand to a browser or a log: what it holds, as
// JSON would write it.
func jsonValue(v any) any {
	switch x := v.(type) {
	case nil, bool, float64, string:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return string(b)
	}
	return out
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case nil:
		return false
	case float64:
		return x != 0
	case string:
		return x != "" && x != "false"
	case Error:
		return x.Status
	}
	return true
}
