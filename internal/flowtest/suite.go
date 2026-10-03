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
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`

	// Inject is what goes into the flow, in order. Each one is handled before
	// the next goes in, so a test reads in the order things happen.
	Inject []Injection `yaml:"inject,omitempty" json:"inject,omitempty"`

	// Expect is what has to come out.
	Expect []Expectation `yaml:"expect,omitempty" json:"expect,omitempty"`
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
		if len(c.Expect) == 0 {
			return fmt.Errorf("test %q expects nothing, so it can't fail", c.Name)
		}
		for j, in := range c.Inject {
			if in.Node == "" {
				return fmt.Errorf("test %q, inject %d: no node", c.Name, j+1)
			}
		}
		for j, e := range c.Expect {
			if err := e.check(); err != nil {
				return fmt.Errorf("test %q, expect %d: %w", c.Name, j+1, err)
			}
		}
	}
	return nil
}

func (e *Expectation) check() error {
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
	if quiet && (e.Msg != nil || e.Assert != "" || e.Error != "") {
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
func (e *Expectation) positive() bool { return !e.Nothing && e.Count == nil }

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
