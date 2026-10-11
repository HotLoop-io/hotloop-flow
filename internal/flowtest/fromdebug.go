package flowtest

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// ErrNotCaptured means the message a test was to be made from isn't among the
// ones the runtime kept: it was too big to keep, or enough came in since that
// it was pushed out.
var ErrNotCaptured = errors.New("that message is no longer kept: send it again and make the test from the new one")

// FromDebug turns a message the debug sidebar showed into a test: the message
// that came in at the source, put in there again, and what the Debug node
// showed, expected there. It's what the flow did, pinned, so a change that
// makes it do something else fails.
func FromDebug(flowsDoc []byte, debugID string, captured []runtime.Captured) (Case, error) {
	flows, err := engine.ParseFlows(flowsDoc)
	if err != nil {
		return Case{}, err
	}
	dbg, ok := flows.Nodes[debugID]
	if !ok {
		if _, _, derived := engine.SplitDerivedID(debugID); derived {
			return Case{}, fmt.Errorf("that Debug node is inside a subflow, and a test watches the subflow instance, not its insides")
		}
		return Case{}, fmt.Errorf("there's no node %q in the flows that are running", debugID)
	}
	if dbg.Type != "debug" {
		return Case{}, fmt.Errorf("%s is a %s, not a Debug node", debugID, dbg.Type)
	}

	var in, out *runtime.Captured
	for i := range captured {
		c := &captured[i]
		switch {
		case c.NodeID == debugID && c.Port < 0:
			out = c // the latest arrival wins
		case c.Port >= 0 && in == nil:
			in = c // the first source to send it is where it came in
		}
	}
	if in == nil || out == nil {
		return Case{}, ErrNotCaptured
	}
	src, ok := flows.Nodes[in.NodeID]
	if !ok {
		return Case{}, fmt.Errorf("it came in at %s, which is inside a subflow; a test puts messages into the instance instead", in.NodeID)
	}
	if in.Port != 0 {
		return Case{}, fmt.Errorf("it came in from output %d of %s, and a test puts a message in at a source's first output", in.Port+1, nodeName(src))
	}

	sent, _ := plain(withoutID(in.Data)).(map[string]any)
	shown, err := shownPart(dbg, withoutID(out.Data))
	if err != nil {
		return Case{}, err
	}
	return Case{
		Name:   fmt.Sprintf("%s gets %s from %s", nodeName(dbg), describeShown(shown), nodeName(src)),
		Inject: []Injection{{Node: src.ID, Msg: sent}},
		Expect: []Expectation{{Node: dbg.ID, Msg: shown}},
	}, nil
}

// shownPart is the part of a message the Debug node displays: the property it
// is set to show, or the whole message.
func shownPart(dbg *engine.Node, data map[string]any) (map[string]any, error) {
	whole, _ := plain(data).(map[string]any)
	prop := dbg.PropString("complete", "payload")
	if prop == "" {
		prop = "payload"
	}
	if prop == "true" || prop == "complete" || dbg.PropString("targetType", "msg") != "msg" {
		return whole, nil
	}
	v, ok, err := (&engine.Msg{Data: whole}).Get(prop)
	if err != nil {
		return nil, fmt.Errorf("the Debug node shows %q: %w", prop, err)
	}
	out := &engine.Msg{Data: map[string]any{}}
	if !ok {
		v = nil
	}
	if err := out.Set(prop, v); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func describeShown(shown map[string]any) string {
	if len(shown) == 1 {
		for _, v := range shown {
			return clip(show(v), 40)
		}
	}
	return "the message"
}

func nodeName(n *engine.Node) string {
	if n.Name != "" {
		return n.Name
	}
	return n.Type + " " + n.ID
}

func withoutID(data map[string]any) map[string]any {
	out := make(map[string]any, len(data))
	for k, v := range data {
		if k != engine.PropMsgID {
			out[k] = v
		}
	}
	return out
}

// plain keeps what a test file can say: objects, lists, text, numbers,
// booleans and null. Anything else a message carries, an HTTP response
// handle on msg.res, is left out rather than written as nonsense.
func plain(v any) any {
	switch t := v.(type) {
	case nil, string, bool, float64:
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			if p := plain(e); p != nil || e == nil {
				out[k] = p
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, plain(e))
		}
		return out
	case []byte:
		if utf8.Valid(t) {
			return string(t)
		}
		return nil
	case engine.ImmutableBytes:
		if utf8.Valid(t) {
			return string(t)
		}
		return nil
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	}
	if f, ok := number(v); ok {
		return f
	}
	return nil
}

// AppendCase adds a test to a suite and returns the suite as it now reads,
// with the test's name made unique if the suite already has one like it. The
// suite is edited as YAML, not rewritten from scratch, so the comments and
// the order of what somebody wrote by hand survive a test added from the
// editor.
func AppendCase(suite []byte, c Case) ([]byte, string, error) {
	var root yaml.Node
	if len(bytes.TrimSpace(suite)) > 0 {
		if err := yaml.Unmarshal(suite, &root); err != nil {
			return nil, "", fmt.Errorf("reading the test file: %w", err)
		}
	}
	if root.Kind == 0 {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, "", fmt.Errorf("the test file isn't a mapping with a tests list")
	}
	top := root.Content[0]
	var list *yaml.Node
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "tests" {
			list = top.Content[i+1]
		}
	}
	if list == nil {
		list = &yaml.Node{Kind: yaml.SequenceNode}
		top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "tests"}, list)
	}
	if list.Kind != yaml.SequenceNode {
		return nil, "", fmt.Errorf("the test file's tests isn't a list")
	}

	taken := map[string]bool{}
	for _, item := range list.Content {
		var named struct {
			Name string `yaml:"name"`
		}
		if err := item.Decode(&named); err == nil {
			taken[named.Name] = true
		}
	}
	base := c.Name
	for i := 2; taken[c.Name]; i++ {
		c.Name = fmt.Sprintf("%s (%d)", base, i)
	}

	var item yaml.Node
	if err := item.Encode(c); err != nil {
		return nil, "", err
	}
	list.Content = append(list.Content, &item)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, "", err
	}
	if err := enc.Close(); err != nil {
		return nil, "", err
	}
	if _, err := Parse(buf.Bytes()); err != nil {
		return nil, "", fmt.Errorf("the test file wouldn't read back with the new test in it: %w", err)
	}
	return buf.Bytes(), c.Name, nil
}

// CaseYAML is one test as it would appear in a suite, for the editor to show
// before anybody saves it.
func CaseYAML(c Case) (string, error) {
	out, err := yaml.Marshal(Suite{Tests: []Case{c}})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)) + "\n", nil
}
