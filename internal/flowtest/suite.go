// Package flowtest runs tests against flows: the real runtime, the real nodes,
// in this process, with no server and no editor.
//
// "Did my change break the line?" has to be answerable before the deploy, not
// after, by the person making the change and by the pipeline that ships it. So a
// test is YAML that sits next to the flow file and goes through review with it:
// put this message in here, expect that out of there within so long, expect
// nothing out of the other port, expect the error to reach the Catch node.
//
// It is an engine package on purpose. The command line, the deploy gate and the
// editor all run tests through here, so they can't disagree about what passed.
package flowtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultTimeout is how long a test waits for its expectations when it doesn't
// say. Most tests finish long before it, because a test ends as soon as every
// expectation is met and the flow has gone quiet.
const DefaultTimeout = 5 * time.Second

// Suite is one test file.
type Suite struct {
	// Flows is the flow file the tests run against, relative to the test file.
	// Empty means flows.json next to it.
	Flows string `yaml:"flows,omitempty" json:"flows,omitempty"`

	Tests []Case `yaml:"tests" json:"tests"`
}

// Case is one test.
type Case struct {
	Name string `yaml:"name" json:"name"`

	// Timeout bounds the whole test, as a Go duration: "500ms", "2s", "5m".
	// It's time on the test's clock, which only moves as fast as the flow
	// can keep up: a five-minute timeout costs what the flow does in those
	// five minutes, not five minutes.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`

	// Clock is when the test starts, as an RFC 3339 time, for a flow that
	// cares what time it is: a crontab, a timestamp, a shift change. Empty
	// starts it at the real time the test runs.
	Clock string `yaml:"clock,omitempty" json:"clock,omitempty"`

	// Inject is what goes into the flow, in order. Each one is handled before
	// the next goes in, so a test reads in the order things happen.
	Inject []Injection `yaml:"inject,omitempty" json:"inject,omitempty"`

	// Expect is what has to come out.
	Expect []Expectation `yaml:"expect,omitempty" json:"expect,omitempty"`

	// Replies script the outside world. Nothing a test runs reaches a broker,
	// a database, a web service, a socket, a file or a command: every node
	// that would have is handed a stand-in that records what it would have
	// sent, and answers with what's scripted here. Unscripted, the answer is
	// nothing and success.
	Replies []Reply `yaml:"replies,omitempty" json:"replies,omitempty"`

	// Context is what flow, global and node context hold when the test
	// starts. Every test starts from empty context otherwise, whatever the
	// test before it left behind, so a test that needs a counter at 41 says
	// so here rather than leaning on another test to get it there.
	Context *ContextValues `yaml:"context,omitempty" json:"context,omitempty"`

	// Credentials are the secrets a node runs with during the test, by node
	// name or id, the same fields its edit dialog has: a broker's password,
	// an HTTP request's, an InfluxDB token. A test never sees the real ones.
	Credentials map[string]map[string]any `yaml:"credentials,omitempty" json:"credentials,omitempty"`
}

// ContextValues is context, by scope: global, flow context by tab (label or
// id), node context by node (name or id). In a test's context it's what the
// test starts with; in an expectation it's what has to be there at the end,
// matched like msg, where null means the key must not be set at all.
type ContextValues struct {
	Global map[string]any            `yaml:"global,omitempty" json:"global,omitempty"`
	Flow   map[string]map[string]any `yaml:"flow,omitempty" json:"flow,omitempty"`
	Node   map[string]map[string]any `yaml:"node,omitempty" json:"node,omitempty"`
}

func (cv *ContextValues) empty() bool {
	return cv == nil || (len(cv.Global) == 0 && len(cv.Flow) == 0 && len(cv.Node) == 0)
}

// shaped normalises every value the way a message's are.
func (cv *ContextValues) shaped() error {
	if cv == nil {
		return nil
	}
	var err error
	if cv.Global, err = jsonShaped(cv.Global); err != nil {
		return err
	}
	for _, scope := range []map[string]map[string]any{cv.Flow, cv.Node} {
		for k, v := range scope {
			if scope[k], err = jsonShaped(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// Reply is one scripted answer from the outside world. The first reply listed
// for a node answers its first call, the second its second, and the last one
// keeps answering after that.
type Reply struct {
	// Node is the node that makes the call, by id or by name.
	Node string `yaml:"node" json:"node"`

	// Reply is what comes back. Its shape is the node's: statusCode, headers
	// and payload for an HTTP request; rows for a query; payload for a TCP
	// request or a file read; stdout, stderr and code for exec; devices for a
	// scan; interfaces for netinfo.
	Reply map[string]any `yaml:"reply,omitempty" json:"reply,omitempty"`

	// Error makes the call fail with this text, the way a database that's
	// down or a host that refuses the connection would.
	Error string `yaml:"error,omitempty" json:"error,omitempty"`
}

// Injection puts one message into the flow.
type Injection struct {
	// Node is the node the message goes into, by id or by name. A node with an
	// input gets the message on it. A node without one, like an MQTT In or an
	// Inject, sends it out of its first output as though it had produced it.
	// An Inject node given no message is pressed instead, so it sends what it
	// is configured to.
	Node string `yaml:"node" json:"node"`

	// Msg is the message. Omitted, it's an empty one.
	Msg map[string]any `yaml:"msg,omitempty" json:"msg,omitempty"`

	// At is when it arrives, counted from the start of the test on the
	// test's clock. Everything due before then happens first. Empty is as
	// soon as the injection before it has been handled.
	At string `yaml:"at,omitempty" json:"at,omitempty"`
}

// Expectation is one thing the test checks.
type Expectation struct {
	// Node is the node to watch, by id or by name.
	Node string `yaml:"node" json:"node"`

	// Port is the output to watch, counting from 1 the way the editor does.
	// Zero means the first. A node with no outputs at all, like a Debug or an
	// MQTT Out, is checked on what it received instead.
	Port int `yaml:"port,omitempty" json:"port,omitempty"`

	// Msg is what the message has to contain. Every property given has to be
	// there with that value, objects are compared the same way all the way
	// down, and anything the message carries beyond that is ignored.
	Msg map[string]any `yaml:"msg,omitempty" json:"msg,omitempty"`

	// Sent is what the node would have sent to the outside world, matched the
	// same way msg is: an MQTT Out's topic, payload, qos and retain; an HTTP
	// request's method, url, headers and payload; the query and params a
	// database would have run; a line of InfluxDB line protocol.
	Sent map[string]any `yaml:"sent,omitempty" json:"sent,omitempty"`

	// Assert is a JSONata expression evaluated against the message, which has
	// to come out true. For the checks a literal can't make: a range, a
	// pattern, a count of array elements.
	Assert string `yaml:"assert,omitempty" json:"assert,omitempty"`

	// Error is text msg.error.message has to contain. It's how a test says an
	// error reached a Catch node: watch the Catch and give the text.
	Error string `yaml:"error,omitempty" json:"error,omitempty"`

	// Within is how soon after the test starts the message has to arrive.
	// Empty means any time before the test's timeout.
	Within string `yaml:"within,omitempty" json:"within,omitempty"`

	// Context, instead of a node, checks what context holds once the test is
	// over: a counter a Function node kept, a latch a Change node set.
	Context *ContextValues `yaml:"context,omitempty" json:"context,omitempty"`

	// Count, when set, is exactly how many messages the port sends during the
	// test, matching or not. Nothing is the same as a count of zero.
	Count   *int `yaml:"count,omitempty" json:"count,omitempty"`
	Nothing bool `yaml:"nothing,omitempty" json:"nothing,omitempty"`
}

// Parse reads a test file.
//
// Unknown keys are an error, not something to skip. A misspelt "expcet" that
// was quietly ignored would leave a test with nothing to check, and a test with
// nothing to check passes.
func Parse(data []byte) (*Suite, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Suite
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("the test file is empty")
		}
		return nil, fmt.Errorf("reading the test file: %w", err)
	}
	// YAML numbers decode as ints and maps can carry non-string keys, neither
	// of which a message ever holds. Round-tripping through JSON gives the
	// values exactly the shapes a message decoded off the wire has, so a
	// comparison is like for like.
	for i := range s.Tests {
		c := &s.Tests[i]
		for j := range c.Inject {
			m, err := jsonShaped(c.Inject[j].Msg)
			if err != nil {
				return nil, fmt.Errorf("test %q, inject %d: %w", c.Name, j+1, err)
			}
			c.Inject[j].Msg = m
		}
		for j := range c.Expect {
			m, err := jsonShaped(c.Expect[j].Msg)
			if err != nil {
				return nil, fmt.Errorf("test %q, expect %d: %w", c.Name, j+1, err)
			}
			c.Expect[j].Msg = m
			if c.Expect[j].Sent, err = jsonShaped(c.Expect[j].Sent); err != nil {
				return nil, fmt.Errorf("test %q, expect %d: %w", c.Name, j+1, err)
			}
		}
		for j := range c.Replies {
			m, err := jsonShaped(c.Replies[j].Reply)
			if err != nil {
				return nil, fmt.Errorf("test %q, reply %d: %w", c.Name, j+1, err)
			}
			c.Replies[j].Reply = m
		}
		if err := c.Context.shaped(); err != nil {
			return nil, fmt.Errorf("test %q, context: %w", c.Name, err)
		}
		for j := range c.Expect {
			if err := c.Expect[j].Context.shaped(); err != nil {
				return nil, fmt.Errorf("test %q, expect %d: %w", c.Name, j+1, err)
			}
		}
	}
	if err := s.Check(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Check finds the mistakes that can be found without the flow: a test with no
// name, nothing to check, or a duration that isn't one.
func (s *Suite) Check() error {
	if len(s.Tests) == 0 {
		return fmt.Errorf("the test file has no tests")
	}
	seen := map[string]bool{}
	for i := range s.Tests {
		c := &s.Tests[i]
		if c.Name == "" {
			return fmt.Errorf("test %d has no name", i+1)
		}
		if seen[c.Name] {
			// Results are reported by name, in JUnit and in the editor. Two
			// tests with one name would read as one test passing and failing.
			return fmt.Errorf("two tests are called %q", c.Name)
		}
		seen[c.Name] = true
		if _, err := c.timeout(); err != nil {
			return err
		}
		if _, err := c.start(); err != nil {
			return err
		}
		if len(c.Expect) == 0 {
			return fmt.Errorf("test %q expects nothing, so it can't fail", c.Name)
		}
		for j, in := range c.Inject {
			if in.Node == "" {
				return fmt.Errorf("test %q, inject %d: no node", c.Name, j+1)
			}
			if in.At != "" {
				if d, err := time.ParseDuration(in.At); err != nil || d < 0 {
					return fmt.Errorf("test %q, inject %d: at %q is not a time from the start of the test, like 0s, 90s or 5m", c.Name, j+1, in.At)
				}
			}
		}
		for j, e := range c.Expect {
			if err := e.check(); err != nil {
				return fmt.Errorf("test %q, expect %d: %w", c.Name, j+1, err)
			}
		}
		for j, r := range c.Replies {
			switch {
			case r.Node == "":
				return fmt.Errorf("test %q, reply %d: no node", c.Name, j+1)
			case r.Reply != nil && r.Error != "":
				return fmt.Errorf("test %q, reply %d: a call either answers or fails; give reply or error, not both", c.Name, j+1)
			}
		}
	}
	return nil
}

func (e *Expectation) check() error {
	if e.Context != nil {
		if e.Node != "" || e.Port != 0 || e.Msg != nil || e.Sent != nil || e.Assert != "" ||
			e.Error != "" || e.Within != "" || e.Count != nil || e.Nothing {
			return fmt.Errorf("context checks what context holds at the end, on its own; " +
				"give it an expectation of its own, with no node")
		}
		if e.Context.empty() {
			return fmt.Errorf("context names nothing to check")
		}
		return nil
	}
	if e.Node == "" {
		return fmt.Errorf("no node")
	}
	if e.Port < 0 {
		return fmt.Errorf("port %d: outputs count from 1", e.Port)
	}
	if e.Count != nil && *e.Count < 0 {
		return fmt.Errorf("a count can't be negative")
	}
	quiet := e.Nothing || e.Count != nil
	if e.Nothing && e.Count != nil {
		return fmt.Errorf("nothing and count say the same thing; give one")
	}
	if e.Sent != nil && (e.Msg != nil || e.Error != "") {
		// One expectation looks at one thing: the message the node was
		// handed, or what it would have sent on. Two things is two
		// expectations.
		return fmt.Errorf("sent checks what the node would have sent; msg and error check a message. Use two expectations")
	}
	if quiet && (e.Msg != nil || e.Sent != nil || e.Assert != "" || e.Error != "") {
		// "Exactly three, and one looks like this" is two expectations, and
		// reads better as two.
		return fmt.Errorf("a count or nothing checks how many messages, not what they hold; " +
			"put msg, assert or error in an expectation of its own")
	}
	if e.Within != "" {
		if quiet {
			return fmt.Errorf("within applies to a message arriving, and this expects a count")
		}
		if _, err := parseDuration(e.Within); err != nil {
			return fmt.Errorf("within: %w", err)
		}
	}
	return nil
}

// positive reports whether the expectation waits for a message, as opposed to
// counting them.
func (e *Expectation) positive() bool { return !e.Nothing && e.Count == nil && e.Context == nil }

func (c *Case) timeout() (time.Duration, error) {
	if c.Timeout == "" {
		return DefaultTimeout, nil
	}
	d, err := parseDuration(c.Timeout)
	if err != nil {
		return 0, fmt.Errorf("test %q, timeout: %w", c.Name, err)
	}
	return d, nil
}

// start is when the test's clock starts.
func (c *Case) start() (time.Time, error) {
	if c.Clock == "" {
		return time.Now(), nil
	}
	t, err := time.Parse(time.RFC3339, c.Clock)
	if err != nil {
		return time.Time{}, fmt.Errorf("test %q, clock: %q is not an RFC 3339 time, like 2026-10-05T06:29:00Z", c.Name, c.Clock)
	}
	return t, nil
}

func parseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration; write it like 500ms, 2s or 5m", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q has to be longer than zero", s)
	}
	return d, nil
}

func jsonShaped(m map[string]any) (map[string]any, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("the message can't be written as JSON: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
