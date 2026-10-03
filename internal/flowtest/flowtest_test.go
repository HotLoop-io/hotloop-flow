package flowtest_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"regexp"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
	_ "github.com/HotLoop-io/hotloop-flow/internal/nodes" // the real palette
)

// line3 is a small, ordinary flow: a reading comes in, a Switch sends anything
// over 80 one way and the rest the other, the high side is turned into an
// alarm, and a Function node that can't handle a negative reading throws to a
// Catch node.
const line3 = `[
  {"id":"t1","type":"tab","label":"Line 3"},
  {"id":"reading","type":"inject","z":"t1","name":"reading","props":[{"p":"payload","v":"50","vt":"num"},{"p":"topic","v":"line3/temp","vt":"str"}],"x":100,"y":100,"wires":[["sanity"]]},
  {"id":"sanity","type":"function","z":"t1","name":"sanity","func":"if (msg.payload < 0) { throw new Error('reading out of range: ' + msg.payload) }\nreturn msg;","outputs":1,"x":200,"y":100,"wires":[["chk"]]},
  {"id":"chk","type":"switch","z":"t1","name":"over limit?","property":"payload","propertyType":"msg","rules":[{"t":"gt","v":"80","vt":"num"},{"t":"else"}],"checkall":"false","outputs":2,"x":300,"y":100,"wires":[["alarm"],["fine"]]},
  {"id":"alarm","type":"change","z":"t1","name":"raise alarm","rules":[{"t":"set","p":"payload","pt":"msg","to":"HIGH","tot":"str"}],"x":500,"y":80,"wires":[["alarm-out"]]},
  {"id":"fine","type":"debug","z":"t1","name":"fine","x":500,"y":120,"wires":[]},
  {"id":"alarm-out","type":"debug","z":"t1","name":"alarm out","x":700,"y":80,"wires":[]},
  {"id":"errors","type":"catch","z":"t1","name":"errors","x":100,"y":200,"wires":[[]]}
]`

func run(t *testing.T, flows, suite string) flowtest.Report {
	t.Helper()
	s, err := flowtest.Parse([]byte(suite))
	if err != nil {
		t.Fatalf("parsing the suite: %v", err)
	}
	return flowtest.Run(context.Background(), []byte(flows), s, flowtest.Options{})
}

// one runs a suite of exactly one test and returns its result.
func one(t *testing.T, flows, suite string) flowtest.Result {
	t.Helper()
	r := run(t, flows, suite)
	if len(r.Tests) != 1 {
		t.Fatalf("got %d results, want 1", len(r.Tests))
	}
	return r.Tests[0]
}

func problems(r flowtest.Result) string {
	var b strings.Builder
	for _, p := range r.Problems {
		b.WriteString(p.String() + "\n")
	}
	for _, l := range r.Log {
		b.WriteString("log: " + l + "\n")
	}
	return b.String()
}

func wantStatus(t *testing.T, r flowtest.Result, want flowtest.Status) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%q came out %s, want %s\n%s", r.Name, r.Status, want, problems(r))
	}
}

func TestAPassingTestPasses(t *testing.T) {
	r := one(t, line3, `
tests:
  - name: a high reading raises the alarm and nothing else
    inject:
      - node: reading
        msg: {topic: line3/temp, payload: 92.5}
    expect:
      - node: over limit?
        port: 1
        msg: {payload: 92.5, topic: line3/temp}
      - node: over limit?
        port: 2
        nothing: true
      - node: alarm out
        msg: {payload: HIGH}
        within: 1s
`)
	wantStatus(t, r, flowtest.Pass)
}

// TestAWrongValueFailsAndSaysWhere: a failing test is only useful if it says
// what came out instead and where it differs.
func TestAWrongValueFailsAndSaysWhere(t *testing.T) {
	r := one(t, line3, `
tests:
  - name: the alarm says LOW, which it doesn't
    inject:
      - node: reading
        msg: {payload: 92.5}
    expect:
      - node: alarm out
        msg: {payload: LOW}
    timeout: 300ms
`)
	wantStatus(t, r, flowtest.Fail)
	text := problems(r)
	for _, want := range []string{`debug "alarm out" (alarm-out)`, `msg.payload: wanted "LOW", got "HIGH"`, `"payload":"HIGH"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the failure doesn't say %q:\n%s", want, text)
		}
	}
	if len(r.Problems) != 1 || r.Problems[0].Node != "alarm-out" || r.Problems[0].Expect != 1 {
		t.Errorf("the problem should point at expect 1 and node alarm-out: %+v", r.Problems)
	}
}

func TestNothingAndCount(t *testing.T) {
	t.Run("nothing that came", func(t *testing.T) {
		r := one(t, line3, `
tests:
  - name: a normal reading goes to the alarm side
    inject:
      - node: reading
        msg: {payload: 20}
    expect:
      - node: over limit?
        port: 2
        count: 1
      - node: over limit?
        port: 1
        nothing: true
`)
		wantStatus(t, r, flowtest.Pass)
	})
	t.Run("something that shouldn't have", func(t *testing.T) {
		r := one(t, line3, `
tests:
  - name: claims a high reading raises nothing
    inject:
      - node: reading
        msg: {payload: 99}
    expect:
      - node: alarm out
        nothing: true
`)
		wantStatus(t, r, flowtest.Fail)
		if !strings.Contains(problems(r), "expected nothing, and 1 message(s) received") {
			t.Errorf("unhelpful failure:\n%s", problems(r))
		}
	})
	t.Run("the wrong count", func(t *testing.T) {
		r := one(t, line3, `
tests:
  - name: two readings, but expects three on the fine side
    inject:
      - node: reading
        msg: {payload: 1}
      - node: reading
        msg: {payload: 2}
    expect:
      - node: fine
        count: 3
`)
		wantStatus(t, r, flowtest.Fail)
		if !strings.Contains(problems(r), "expected 3 message(s) received, got 2") {
			t.Errorf("unhelpful failure:\n%s", problems(r))
		}
	})
}

// TestNothingWaitsForWhatADelayIsHolding: "nothing came out" is only true once
// the flow has nothing left to do. A test that checked the moment the injected
// message stopped moving would pass right past the Delay holding it.
func TestNothingWaitsForWhatADelayIsHolding(t *testing.T) {
	const flows = `[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"in","type":"junction","z":"t1","x":1,"y":1,"wires":[["wait"]]},
  {"id":"wait","type":"delay","z":"t1","pauseType":"delay","timeout":"100","timeoutUnits":"milliseconds","x":2,"y":1,"wires":[["out"]]},
  {"id":"out","type":"debug","z":"t1","name":"out","x":3,"y":1,"wires":[]}
]`
	r := one(t, flows, `
tests:
  - name: claims the delay never lets go
    inject: [{node: in, msg: {payload: 1}}]
    expect:
      - {node: out, nothing: true}
`)
	wantStatus(t, r, flowtest.Fail)
	if r.Seconds < 0.1 {
		t.Errorf("the test ended after %.3fs, before the delay could let go", r.Seconds)
	}
}

// TestAnErrorReachesTheCatch: "expect an error to reach a Catch" is a Catch
// node watched for the error's text.
func TestAnErrorReachesTheCatch(t *testing.T) {
	r := one(t, line3, `
tests:
  - name: a negative reading is caught
    inject:
      - node: reading
        msg: {payload: -4}
    expect:
      - node: errors
        error: "reading out of range: -4"
      - node: over limit?
        port: 1
        nothing: true
      - node: over limit?
        port: 2
        nothing: true
`)
	wantStatus(t, r, flowtest.Pass)

	r = one(t, line3, `
tests:
  - name: expects an error that never happens
    timeout: 300ms
    inject:
      - node: reading
        msg: {payload: 4}
    expect:
      - node: errors
        error: out of range
`)
	wantStatus(t, r, flowtest.Fail)
	if !strings.Contains(problems(r), `catch "errors" (errors): nothing sent from port 1`) {
		t.Errorf("unhelpful failure:\n%s", problems(r))
	}
}

// TestWithinIsADeadline: a message that comes, but too late, fails, and says
// how late.
func TestWithinIsADeadline(t *testing.T) {
	const slow = `[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"in","type":"junction","z":"t1","x":1,"y":1,"wires":[["wait"]]},
  {"id":"wait","type":"delay","z":"t1","name":"wait","pauseType":"delay","timeout":"300","timeoutUnits":"milliseconds","x":2,"y":1,"wires":[["out"]]},
  {"id":"out","type":"junction","z":"t1","x":3,"y":1,"wires":[[]]}
]`
	r := one(t, slow, `
tests:
  - name: too slow
    inject: [{node: in, msg: {payload: 1}}]
    expect:
      - node: out
        msg: {payload: 1}
        within: 100ms
`)
	wantStatus(t, r, flowtest.Fail)
	if !regexp.MustCompile(`it matched, but 3\d\d(\.\d+)?ms in, and within is 100ms`).MatchString(problems(r)) {
		t.Errorf("unhelpful failure:\n%s", problems(r))
	}

	r = one(t, slow, `
tests:
  - name: in time
    inject: [{node: in, msg: {payload: 1}}]
    expect:
      - node: out
        msg: {payload: 1}
        within: 2s
`)
	wantStatus(t, r, flowtest.Pass)
}

// TestTwoExpectationsOnOnePortAreInOrder: a Trigger sends "on" and then "off".
// Expecting them in that order passes and the other way round doesn't.
func TestTwoExpectationsOnOnePortAreInOrder(t *testing.T) {
	const watchdog = `[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"dog","type":"trigger","z":"t1","name":"watchdog","op1":"on","op1type":"str","op2":"off","op2type":"str","duration":"50","units":"ms","x":1,"y":1,"wires":[[]]}
]`
	r := one(t, watchdog, `
tests:
  - name: on then off
    inject: [{node: watchdog, msg: {payload: go}}]
    expect:
      - {node: watchdog, msg: {payload: "on"}}
      - {node: watchdog, msg: {payload: "off"}}
`)
	wantStatus(t, r, flowtest.Pass)

	r = one(t, watchdog, `
tests:
  - name: off then on
    timeout: 500ms
    inject: [{node: watchdog, msg: {payload: go}}]
    expect:
      - {node: watchdog, msg: {payload: "off"}}
      - {node: watchdog, msg: {payload: "on"}}
`)
	wantStatus(t, r, flowtest.Fail)
	if !strings.Contains(problems(r), "expectations before this one used them all") {
		t.Errorf("unhelpful failure:\n%s", problems(r))
	}
}

// TestSourcesAndButtons: an Inject node given no message is pressed and sends
// what it's configured to; given one, it sends that instead.
func TestSourcesAndButtons(t *testing.T) {
	r := one(t, line3, `
tests:
  - name: the button sends the configured reading
    inject: [{node: reading}]
    expect:
      - {node: fine, msg: {payload: 50, topic: line3/temp}}
  `)
	wantStatus(t, r, flowtest.Pass)
}

func TestAssert(t *testing.T) {
	r := one(t, line3, `
tests:
  - name: a JSONata check on the message
    inject: [{node: reading, msg: {payload: 81, topic: line3/temp}}]
    expect:
      - node: over limit?
        assert: payload > 80 and payload < 90 and $contains(topic, "line3")
`)
	wantStatus(t, r, flowtest.Pass)

	r = one(t, line3, `
tests:
  - name: an assert that isn't true
    timeout: 300ms
    inject: [{node: reading, msg: {payload: 95}}]
    expect:
      - node: over limit?
        assert: payload < 90
`)
	wantStatus(t, r, flowtest.Fail)
	if !strings.Contains(problems(r), `assert "payload < 90" came out false`) {
		t.Errorf("unhelpful failure:\n%s", problems(r))
	}
}

// TestSubflowOutputsCanBeWatched: an instance doesn't send anything itself once
// it's running, so watching it means watching what inside it feeds its output.
func TestSubflowOutputsCanBeWatched(t *testing.T) {
	const flows = `[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"sf","type":"subflow","name":"double","in":[{"x":0,"y":0,"wires":[{"id":"dbl"}]}],"out":[{"x":0,"y":0,"wires":[{"id":"dbl","port":0}]}]},
  {"id":"dbl","type":"change","z":"sf","rules":[{"t":"set","p":"doubled","pt":"msg","to":"true","tot":"bool"}],"x":1,"y":1,"wires":[[]]},
  {"id":"inst","type":"subflow:sf","z":"t1","name":"doubler","x":1,"y":1,"wires":[["end"]]},
  {"id":"end","type":"debug","z":"t1","name":"end","x":2,"y":1,"wires":[]}
]`
	r := one(t, flows, `
tests:
  - name: through the subflow
    inject: [{node: doubler, msg: {payload: 3}}]
    expect:
      - {node: doubler, msg: {payload: 3, doubled: true}}
      - {node: end, msg: {doubled: true}}
`)
	wantStatus(t, r, flowtest.Pass)
}

// TestTestsThatCantRunAreErrorsNotPasses: a test naming a node that isn't
// there, or a flow with a node that won't start, didn't pass.
func TestTestsThatCantRunAreErrorsNotPasses(t *testing.T) {
	for name, tc := range map[string]struct{ flows, suite, says string }{
		"no such node": {line3, `
tests:
  - name: x
    expect: [{node: nobody}]
`, `there's no node with the id or name "nobody"`},
		"two nodes with the name": {strings.Replace(line3, `"name":"fine"`, `"name":"alarm out"`, 1), `
tests:
  - name: x
    expect: [{node: alarm out}]
`, `2 nodes are called "alarm out" (fine, alarm-out); use an id`},
		"a port that isn't there": {line3, `
tests:
  - name: x
    expect: [{node: "over limit?", port: 3}]
`, `has 2 output(s), so there's no port 3`},
		"a node that won't start": {strings.Replace(line3, `"type":"change"`, `"type":"no-such-type"`, 1), `
tests:
  - name: x
    expect: [{node: fine}]
`, `wouldn't start: unknown node type "no-such-type"`},
		"a disabled node": {strings.Replace(line3, `"name":"fine",`, `"name":"fine","d":true,`, 1), `
tests:
  - name: x
    expect: [{node: fine}]
`, `is disabled`},
	} {
		t.Run(name, func(t *testing.T) {
			r := one(t, tc.flows, tc.suite)
			wantStatus(t, r, flowtest.Error)
			if !strings.Contains(problems(r), tc.says) {
				t.Errorf("wanted %q in:\n%s", tc.says, problems(r))
			}
		})
	}
}

// TestNodesThatReachOutAreNotRun: a test that ran an MQTT Out would publish to
// the real broker. Until those nodes can be stood in for, a test refuses them
// rather than doing it.
func TestNodesThatReachOutAreNotRun(t *testing.T) {
	flows := strings.Replace(line3, `{"id":"errors"`, `{"id":"pub","type":"mqtt out","z":"t1","name":"to the plant","broker":"b1","topic":"line3/alarm","x":1,"y":1,"wires":[]},
  {"id":"b1","type":"mqtt-broker","broker":"192.0.2.1","port":1883},
  {"id":"errors"`, 1)
	r := one(t, flows, `
tests:
  - name: x
    inject: [{node: reading, msg: {payload: 1}}]
    expect: [{node: fine}]
`)
	wantStatus(t, r, flowtest.Error)
	if !strings.Contains(problems(r), `mqtt out "to the plant" (pub)`) {
		t.Errorf("the refusal doesn't name the node:\n%s", problems(r))
	}
}

// TestEveryTestStartsFromNothing: each test gets its own runtime. A Delay still
// holding a message from one test can't release it into the next.
func TestEveryTestStartsFromNothing(t *testing.T) {
	const flows = `[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"in","type":"junction","z":"t1","x":1,"y":1,"wires":[["wait"]]},
  {"id":"wait","type":"delay","z":"t1","pauseType":"delay","timeout":"200","timeoutUnits":"milliseconds","x":2,"y":1,"wires":[["out"]]},
  {"id":"out","type":"debug","z":"t1","name":"out","x":3,"y":1,"wires":[]}
]`
	rep := run(t, flows, `
tests:
  - name: leaves a message in the delay
    timeout: 50ms
    inject: [{node: in, msg: {payload: first}}]
    expect:
      - {node: out, nothing: true}
  - name: sees only its own
    inject: [{node: in, msg: {payload: second}}]
    expect:
      - {node: out, count: 1}
      - {node: out, msg: {payload: second}}
`)
	for _, r := range rep.Tests {
		wantStatus(t, r, flowtest.Pass)
	}
}

func TestParseRefusesWhatWouldTestNothing(t *testing.T) {
	for _, tc := range []struct{ suite, says string }{
		{``, "the test file is empty"},
		{`tests: []`, "has no tests"},
		{"tests:\n  - name: a\n    expcet: [{node: x}]", "field expcet not found"},
		{"tests:\n  - name: a\n    inject: [{node: x}]", `test "a" expects nothing, so it can't fail`},
		{"tests:\n  - name: a\n    expect: [{node: x}]\n  - name: a\n    expect: [{node: x}]", `two tests are called "a"`},
		{"tests:\n  - name: a\n    timeout: soon\n    expect: [{node: x}]", `"soon" is not a duration`},
		{"tests:\n  - name: a\n    expect: [{node: x, nothing: true, msg: {payload: 1}}]", "a count or nothing checks how many"},
		{"tests:\n  - expect: [{node: x}]", "test 1 has no name"},
	} {
		_, err := flowtest.Parse([]byte(tc.suite))
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("Parse(%q) = %v, want an error saying %q", tc.suite, err, tc.says)
		}
	}
}

func TestJUnit(t *testing.T) {
	rep := run(t, line3, `
tests:
  - name: passes
    inject: [{node: reading, msg: {payload: 1}}]
    expect: [{node: fine}]
  - name: fails
    timeout: 200ms
    inject: [{node: reading, msg: {payload: 1}}]
    expect: [{node: alarm out}]
  - name: errors
    expect: [{node: ghost}]
`)
	rep.File = "line3.test.yaml"
	var buf bytes.Buffer
	if err := flowtest.WriteJUnit(&buf, []flowtest.Report{rep}); err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Tests    int `xml:"tests,attr"`
		Failures int `xml:"failures,attr"`
		Errors   int `xml:"errors,attr"`
		Suites   []struct {
			Name  string `xml:"name,attr"`
			Cases []struct {
				Name    string `xml:"name,attr"`
				Failure *struct {
					Message string `xml:"message,attr"`
				} `xml:"failure"`
				Error *struct {
					Message string `xml:"message,attr"`
				} `xml:"error"`
			} `xml:"testcase"`
		} `xml:"testsuite"`
	}
	if err := xml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("the JUnit report isn't XML: %v\n%s", err, buf.String())
	}
	if doc.Tests != 3 || doc.Failures != 1 || doc.Errors != 1 {
		t.Fatalf("totals %d/%d/%d, want 3 tests, 1 failure, 1 error\n%s", doc.Tests, doc.Failures, doc.Errors, buf.String())
	}
	cases := doc.Suites[0].Cases
	if doc.Suites[0].Name != "line3.test.yaml" || cases[0].Failure != nil || cases[0].Error != nil {
		t.Errorf("the passing case carries a problem:\n%s", buf.String())
	}
	if cases[1].Failure == nil || !strings.Contains(cases[1].Failure.Message, `debug "alarm out"`) {
		t.Errorf("the failing case:\n%s", buf.String())
	}
	if cases[2].Error == nil || !strings.Contains(cases[2].Error.Message, "ghost") {
		t.Errorf("the erroring case:\n%s", buf.String())
	}
}
