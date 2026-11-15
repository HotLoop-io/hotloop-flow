package flowtest

import (
	"fmt"

	"github.com/HotLoop-io/hotloop-flow/internal/diagram"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// DiagramIssue is one reason a diagram in the flow file cannot run, or, when
// Warning is set, one thing worth fixing that does not stop it. It is the same
// check the editor's error list shows, so a problem found here is a problem the
// person would have seen on the canvas.
type DiagramIssue struct {
	Diagram string `json:"diagram"` // the diagram tab's name
	Node    string `json:"node,omitempty"`
	Port    string `json:"port,omitempty"`
	Message string `json:"message"`
	Warning bool   `json:"warning,omitempty"`
}

func (d DiagramIssue) String() string {
	where := d.Diagram
	if d.Node != "" {
		where += ": " + d.Node
		if d.Port != "" {
			where += " input " + d.Port
		}
	}
	kind := ""
	if d.Warning {
		kind = "warning: "
	}
	return fmt.Sprintf("%s%s: %s", kind, where, d.Message)
}

// CheckDiagrams compiles every diagram tab in a flow file and lists what keeps it
// from running, so a test run, a CI job and a deploy gate refuse a diagram that
// the editor would have flagged. A file that is not a flow file, or has no
// diagrams, has none.
func CheckDiagrams(doc []byte) []DiagramIssue {
	flows, err := engine.ParseFlows(doc)
	if err != nil {
		return nil // the tests report a file that does not parse
	}
	set := diagram.CompileAll(flows)
	var out []DiagramIssue
	for _, id := range set.Order {
		d := set.Diagrams[id]
		for _, p := range d.Problems {
			out = append(out, DiagramIssue{Diagram: d.Label, Node: p.Node, Port: p.Port, Message: p.Text})
		}
		for _, p := range d.Warnings {
			out = append(out, DiagramIssue{Diagram: d.Label, Node: p.Node, Port: p.Port, Message: p.Text, Warning: true})
		}
	}
	return out
}

// Blocking is how many of the issues stop a diagram from running.
func Blocking(issues []DiagramIssue) int {
	n := 0
	for _, i := range issues {
		if !i.Warning {
			n++
		}
	}
	return n
}
