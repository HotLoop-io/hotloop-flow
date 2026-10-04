package flowtest_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
)

// counter keeps a running count in flow context, remembers the last reading in
// its own node context, and compares it with a limit in global context.
const counter = `[
  {"id":"t1","type":"tab","label":"Line 3"},
  {"id":"count","type":"function","z":"t1","name":"count","func":"let n = (flow.get('count') || 0) + 1;\nflow.set('count', n);\ncontext.set('last', msg.payload);\nif (msg.payload > global.get('limit')) { global.set('alarm', true) }\nmsg.count = n;\nreturn msg;","outputs":1,"x":1,"y":1,"wires":[["out"]]},
  {"id":"out","type":"debug","z":"t1","name":"out","x":2,"y":1,"wires":[]}
]`

func TestContextFixturesAndChecks(t *testing.T) {
	r := one(t, counter, `
tests:
  - name: picks up where the line left off
    context:
      global: {limit: 90}
      flow: {Line 3: {count: 41}}
    inject: [{node: count, msg: {payload: 95}}]
    expect:
      - {node: out, msg: {count: 42}}
      - context:
          global: {limit: 90, alarm: true}
          flow: {Line 3: {count: 42}}
          node: {count: {last: 95}}
`)
	wantStatus(t, r, flowtest.Pass)

	r = one(t, counter, `
tests:
  - name: under the limit, no alarm
    context: {global: {limit: 90}}
    inject: [{node: count, msg: {payload: 12}}]
    expect:
      - context: {global: {alarm: null}, flow: {t1: {count: 1}}}
`)
	wantStatus(t, r, flowtest.Pass)
}

// TestAWrongContextSaysWhatsThere: a failure names the scope, the key, and what
// it actually holds, because the usual answer is a typo.
func TestAWrongContextSaysWhatsThere(t *testing.T) {
	r := one(t, counter, `
tests:
  - name: wrong in three ways
    context: {global: {limit: 10}}
    inject: [{node: count, msg: {payload: 95}}]
    expect:
      - context:
          global: {alarm: null}
          flow: {Line 3: {count: 50, cuont: 1}}
`)
	wantStatus(t, r, flowtest.Fail)
	text := problems(r)
	for _, want := range []string{
		"expect 1: global context: alarm: wanted it unset, and it's true",
		`expect 1: flow context of "Line 3" (t1): count: wanted 50, got 1`,
		`expect 1: flow context of "Line 3" (t1): cuont: not set; it holds count`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("wanted %q in:\n%s", want, text)
		}
	}
}

// TestNoTestLeansOnAnother: the first test counts to one; the second, run after
// it in the same suite, starts from nothing and counts to one as well.
// Context is the easiest thing in a flow to leak between tests and the
// hardest leak to spot, because the second test only passes when the first
// ran first.
func TestNoTestLeansOnAnother(t *testing.T) {
	rep := run(t, counter, `
tests:
  - name: first
    inject: [{node: count, msg: {payload: 1}}]
    expect:
      - context: {flow: {Line 3: {count: 1}}, node: {count: {last: 1}}}
  - name: second, from nothing again
    inject: [{node: count, msg: {payload: 2}}]
    expect:
      - context: {flow: {Line 3: {count: 1}}, node: {count: {last: 2}}}
  - name: third, from its own fixture
    context: {flow: {Line 3: {count: 100}}}
    inject: [{node: count, msg: {payload: 3}}]
    expect:
      - context: {flow: {Line 3: {count: 101}}}
`)
	for _, r := range rep.Tests {
		wantStatus(t, r, flowtest.Pass)
	}
}

// TestCredentialFixtures: a test runs with the secrets it names and never the
// real ones. The password reaches the request the node would have sent,
// whether the node keeps it in the credential store or points at a mounted
// file, and a node that won't start without one starts.
func TestCredentialFixtures(t *testing.T) {
	const flows = `[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"ifx","type":"hotloop-flow-influxdb","name":"historian","url":"http://historian.plant.example:8086","org":"plant","bucket":"line3"},
  {"id":"go","type":"junction","z":"t1","x":1,"y":1,"wires":[["api","filed","hist"]]},
  {"id":"api","type":"http request","z":"t1","name":"api","url":"http://plc.plant.example/state","authType":"basic","user":"line3","x":2,"y":1,"wires":[[]]},
  {"id":"filed","type":"http request","z":"t1","name":"filed","url":"http://plc.plant.example/state","authType":"basic","user":"line3","ew_credentialFiles":{"password":"/run/secrets/plc/password"},"x":2,"y":2,"wires":[[]]},
  {"id":"hist","type":"influxdb out","z":"t1","name":"history","server":"ifx","measurement":"temperature","fields":[{"column":"value","value":"payload","valueType":"msg"}],"x":2,"y":3,"wires":[[]]}
]`
	basic := func(pw string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte("line3:"+pw))
	}
	r := one(t, flows, `
tests:
  - name: the test's own secrets
    credentials:
      api: {password: s3cret}
      filed: {password: from-the-test}
      historian: {token: 12345}
    inject: [{node: go, msg: {payload: 1}}]
    expect:
      - {node: api, sent: {headers: {authorization: "`+basic("s3cret")+`"}}}
      - {node: filed, sent: {headers: {authorization: "`+basic("from-the-test")+`"}}}
      - {node: history, sent: {line: temperature value=1}}
`)
	wantStatus(t, r, flowtest.Pass)

	// Without the token the InfluxDB configuration refuses to start, which
	// is what it would do on the box, and the test says so.
	r = one(t, flows, `
tests:
  - name: no token
    inject: [{node: go, msg: {payload: 1}}]
    expect: [{node: history, nothing: true}]
`)
	wantStatus(t, r, flowtest.Error)
	if !strings.Contains(problems(r), "a token is required for the 2.x API") {
		t.Errorf("the refusal:\n%s", problems(r))
	}
}

func TestFixturesNameRealThings(t *testing.T) {
	for name, tc := range map[string]struct{ suite, says string }{
		"a flow that isn't there": {`
tests:
  - name: x
    context: {flow: {Line 9: {count: 1}}}
    expect: [{node: out, nothing: true}]
`, `context: there's no flow with the id or label "Line 9"`},
		"credentials for nobody": {`
tests:
  - name: x
    credentials: {ghost: {password: x}}
    expect: [{node: out, nothing: true}]
`, `credentials: there's no node with the id or name "ghost"`},
		"a context check on a node that isn't there": {`
tests:
  - name: x
    expect: [{context: {node: {ghost: {last: 1}}}}]
`, `expect 1: there's no node with the id or name "ghost"`},
	} {
		t.Run(name, func(t *testing.T) {
			r := one(t, counter, tc.suite)
			wantStatus(t, r, flowtest.Error)
			if !strings.Contains(problems(r), tc.says) {
				t.Errorf("wanted %q in:\n%s", tc.says, problems(r))
			}
		})
	}

	for _, tc := range []struct{ suite, says string }{
		{"tests:\n  - name: a\n    expect: [{node: out, context: {global: {a: 1}}}]", "context checks what context holds at the end, on its own"},
		{"tests:\n  - name: a\n    expect: [{context: {}}]", "context names nothing to check"},
	} {
		if _, err := flowtest.Parse([]byte(tc.suite)); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("Parse(%q) = %v, want %q", tc.suite, err, tc.says)
		}
	}
}

// TestTheREADMEContextExample runs the README's context example word for word.
func TestTheREADMEContextExample(t *testing.T) {
	flows := strings.Replace(counter, `{"id":"out"`, `{"id":"ifx","type":"hotloop-flow-influxdb","name":"historian","url":"http://historian.plant.example:8086","org":"plant","bucket":"line3"},
  {"id":"plc","type":"http request","z":"t1","name":"plc api","url":"http://plc.plant.example/state","authType":"basic","user":"line3","x":3,"y":3,"wires":[[]]},
  {"id":"out"`, 1)
	r := one(t, flows, "tests:\n"+readmeBlock(t, "  - name: picks up where the line left off"))
	wantStatus(t, r, flowtest.Pass)
}
