package flowtest_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
)

// Every timing node in the palette, proven on the test's clock. Each flow ends
// in a Change node that stamps msg.at with the time (a date-typed value, which
// reads the node's clock), so a test can say exactly when, to the millisecond,
// a message came out. Every test also has to finish in well under a second of
// real time, however many minutes or hours it covers.

// six is the start of every test's clock here: 06:00 on 2026-10-05, in the
// process's own time zone, because that's the zone a crontab reads.
var six = time.Date(2026, 10, 5, 6, 0, 0, 0, time.Local)

// at is the millisecond stamp a message carries when it leaves at six plus d.
func at(d time.Duration) string { return fmt.Sprint(six.Add(d).UnixMilli()) }

// stamped wraps the nodes under test in a tab, with a stamp node and a Debug
// called "out" after them, and a Catch called "errors".
func stamped(nodes string) string {
	return `[
  {"id":"t1","type":"tab","label":"T"},
  ` + nodes + `,
  {"id":"stamp","type":"change","z":"t1","rules":[{"t":"set","p":"at","pt":"msg","to":"","tot":"date"}],"x":8,"y":1,"wires":[["out"]]},
  {"id":"out","type":"debug","z":"t1","name":"out","x":9,"y":1,"wires":[]},
  {"id":"errors","type":"catch","z":"t1","name":"errors","x":1,"y":9,"wires":[[]]}
]`
}

// clockSuite fills in the clock and the stamps.
func clockSuite(yaml string) string {
	yaml = strings.ReplaceAll(yaml, "CLOCK", six.Format(time.RFC3339))
	for _, d := range []string{"0s", "10s", "30s", "1m", "2m", "5m", "6m", "9m", "10m", "20m", "30m", "40m", "50m", "1h"} {
		dur, _ := time.ParseDuration(d)
		yaml = strings.ReplaceAll(yaml, "AT("+d+")", at(dur))
	}
	return yaml
}

func fast(t *testing.T, r flowtest.Result) {
	t.Helper()
	wantStatus(t, r, flowtest.Pass)
	if r.Seconds > 1 {
		t.Errorf("%q took %.2fs of real time; the clock is supposed to make that instant", r.Name, r.Seconds)
	}
}

// TestAFiveMinuteDelayRunsInMilliseconds is the headline: five minutes of
// Delay, checked to the millisecond, in no time at all.
func TestAFiveMinuteDelayRunsInMilliseconds(t *testing.T) {
	flows := stamped(`{"id":"wait","type":"delay","z":"t1","name":"wait","pauseType":"delay","timeout":"5","timeoutUnits":"minutes","x":2,"y":1,"wires":[["stamp"]]}`)
	r := one(t, flows, clockSuite(`
tests:
  - name: five minutes
    clock: CLOCK
    inject: [{node: wait, msg: {payload: 1}}]
    expect:
      - {node: out, msg: {payload: 1, at: AT(5m)}, within: 5m}
`))
	fast(t, r)

	r = one(t, flows, clockSuite(`
tests:
  - name: not in four
    clock: CLOCK
    timeout: 6m
    inject: [{node: wait, msg: {payload: 1}}]
    expect:
      - {node: out, within: 4m59s}
`))
	wantStatus(t, r, flowtest.Fail)
	if !strings.Contains(problems(r), "it matched, but 5m0s in, and within is 4m59s") {
		t.Errorf("unhelpful failure:\n%s", problems(r))
	}
}

func TestDelayModesOnTheClock(t *testing.T) {
	t.Run("rate limit", func(t *testing.T) {
		// Three at once through one-a-minute: the first straight away, the
		// others a minute apart.
		flows := stamped(`{"id":"limit","type":"delay","z":"t1","name":"limit","pauseType":"rate","rate":"1","nbRateUnits":"1","rateUnits":"minute","x":2,"y":1,"wires":[["stamp"]]}`)
		fast(t, one(t, flows, clockSuite(`
tests:
  - name: one a minute
    clock: CLOCK
    timeout: 5m
    inject:
      - {node: limit, msg: {payload: a}}
      - {node: limit, msg: {payload: b}}
      - {node: limit, msg: {payload: c}}
    expect:
      - {node: out, msg: {payload: a, at: AT(0s)}}
      - {node: out, msg: {payload: b, at: AT(1m)}}
      - {node: out, msg: {payload: c, at: AT(2m)}}
      - {node: out, count: 3}
`)))
	})
	t.Run("rate limit after a quiet spell", func(t *testing.T) {
		// Nothing for two minutes, so the next one isn't held for a window
		// that's long gone.
		flows := stamped(`{"id":"limit","type":"delay","z":"t1","name":"limit","pauseType":"rate","rate":"1","nbRateUnits":"1","rateUnits":"minute","x":2,"y":1,"wires":[["stamp"]]}`)
		fast(t, one(t, flows, clockSuite(`
tests:
  - name: straight through
    clock: CLOCK
    timeout: 5m
    inject:
      - {node: limit, msg: {payload: a}}
      - {node: limit, msg: {payload: b}, at: 2m}
    expect:
      - {node: out, msg: {payload: a, at: AT(0s)}}
      - {node: out, msg: {payload: b, at: AT(2m)}}
`)))
	})
	t.Run("held messages leave in the order they came", func(t *testing.T) {
		// Two messages a Delay lets go at the same instant, and one it lets go
		// before the test's next injection: each leaves when it should, in
		// the order it should.
		flows := stamped(`{"id":"wait","type":"delay","z":"t1","name":"wait","pauseType":"delay","timeout":"1","timeoutUnits":"minutes","x":2,"y":1,"wires":[["stamp"]]}`)
		fast(t, one(t, flows, clockSuite(`
tests:
  - name: first in, first out
    clock: CLOCK
    timeout: 10m
    inject:
      - {node: wait, msg: {payload: a}}
      - {node: wait, msg: {payload: b}}
      - {node: wait, msg: {payload: c}, at: 5m}
    expect:
      - {node: out, msg: {payload: a, at: AT(1m)}}
      - {node: out, msg: {payload: b, at: AT(1m)}}
      - {node: out, msg: {payload: c, at: AT(6m)}}
      - {node: out, count: 3}
`)))
	})
	t.Run("timed release", func(t *testing.T) {
		flows := stamped(`{"id":"batch","type":"delay","z":"t1","name":"release","pauseType":"timed","rate":"1","nbRateUnits":"10","rateUnits":"minute","x":2,"y":1,"wires":[["stamp"]]}`)
		fast(t, one(t, flows, clockSuite(`
tests:
  - name: everything at ten past
    clock: CLOCK
    timeout: 15m
    inject:
      - {node: release, msg: {payload: a}, at: 1m}
      - {node: release, msg: {payload: b}, at: 2m}
    expect:
      - {node: out, msg: {payload: a, at: AT(10m)}}
      - {node: out, msg: {payload: b, at: AT(10m)}}
`)))
	})
	t.Run("random", func(t *testing.T) {
		flows := stamped(`{"id":"wait","type":"delay","z":"t1","name":"wait","pauseType":"random","randomFirst":"1","randomLast":"2","randomUnits":"minutes","x":2,"y":1,"wires":[["stamp"]]}`)
		r := one(t, flows, clockSuite(`
tests:
  - name: between one and two minutes
    clock: CLOCK
    inject: [{node: wait, msg: {payload: 1}}]
    expect:
      - node: out
        within: 2m
        assert: at >= AT(1m) and at <= AT(2m)
`))
		fast(t, r)
	})
}

// TestTriggerOnTheClock: a watchdog that says "off" five minutes after the last
// "on", and a second message at four minutes that pushes it out to nine.
func TestTriggerOnTheClock(t *testing.T) {
	flows := stamped(`{"id":"dog","type":"trigger","z":"t1","name":"watchdog","op1":"on","op1type":"str","op2":"off","op2type":"str","duration":"5","units":"min","extend":true,"x":2,"y":1,"wires":[["stamp"]]}`)
	fast(t, one(t, flows, clockSuite(`
tests:
  - name: extended by a second reading
    clock: CLOCK
    timeout: 20m
    inject:
      - {node: watchdog, msg: {payload: reading}}
      - {node: watchdog, msg: {payload: reading}, at: 4m}
    expect:
      - {node: out, msg: {payload: "on", at: AT(0s)}}
      - {node: out, msg: {payload: "off", at: AT(9m)}}
      - {node: out, count: 2}
`)))
}

func TestInjectOnTheClock(t *testing.T) {
	t.Run("every ten minutes for an hour", func(t *testing.T) {
		flows := stamped(`{"id":"tick","type":"inject","z":"t1","name":"tick","repeat":"600","props":[{"p":"payload","v":"tick","vt":"str"}],"x":1,"y":1,"wires":[["stamp"]]}`)
		fast(t, one(t, flows, clockSuite(`
tests:
  - name: six ticks
    clock: CLOCK
    timeout: 1h
    expect:
      - {node: out, msg: {at: AT(10m)}}
      - {node: out, msg: {at: AT(20m)}}
      - {node: out, msg: {at: AT(30m)}}
      - {node: out, msg: {at: AT(40m)}}
      - {node: out, msg: {at: AT(50m)}}
      - {node: out, msg: {at: AT(1h)}}
      - {node: out, count: 6}
`)))
	})
	t.Run("once at startup", func(t *testing.T) {
		flows := stamped(`{"id":"boot","type":"inject","z":"t1","name":"boot","once":true,"onceDelay":"30","props":[{"p":"payload","v":"hello","vt":"str"}],"x":1,"y":1,"wires":[["stamp"]]}`)
		fast(t, one(t, flows, clockSuite(`
tests:
  - name: thirty seconds in
    clock: CLOCK
    timeout: 5m
    expect:
      - {node: out, msg: {payload: hello, at: AT(30s)}}
      - {node: out, count: 1}
`)))
	})
	t.Run("a crontab", func(t *testing.T) {
		flows := stamped(`{"id":"shift","type":"inject","z":"t1","name":"shift change","crontab":"30 6 * * *","props":[{"p":"payload","v":"shift","vt":"str"}],"x":1,"y":1,"wires":[["stamp"]]}`)
		fast(t, one(t, flows, clockSuite(`
tests:
  - name: half past six, exactly once
    clock: CLOCK
    timeout: 2h
    expect:
      - {node: out, msg: {payload: shift, at: AT(30m)}}
      - {node: out, count: 1}
`)))
	})
}

func TestJoinBatchAndLinkCallOnTheClock(t *testing.T) {
	t.Run("join gives up thirty seconds after the first part", func(t *testing.T) {
		flows := stamped(`{"id":"join","type":"join","z":"t1","name":"join","mode":"custom","build":"array","count":"3","timeout":"30","x":2,"y":1,"wires":[["stamp"]]}`)
		fast(t, one(t, flows, clockSuite(`
tests:
  - name: two of three, then the timeout
    clock: CLOCK
    timeout: 5m
    inject:
      - {node: join, msg: {payload: a}}
      - {node: join, msg: {payload: b}, at: 10s}
    expect:
      - {node: out, msg: {payload: [a, b], at: AT(30s)}}
`)))
	})
	t.Run("batch by time", func(t *testing.T) {
		flows := stamped(`{"id":"batch","type":"batch","z":"t1","name":"batch","mode":"interval","interval":"60","x":2,"y":1,"wires":[["stamp"]]}`)
		fast(t, one(t, flows, clockSuite(`
tests:
  - name: a minute's worth
    clock: CLOCK
    timeout: 90s
    inject:
      - {node: batch, msg: {payload: a}}
      - {node: batch, msg: {payload: b}, at: 10s}
      - {node: batch, msg: {payload: c}, at: 30s}
    expect:
      - {node: out, msg: {payload: a, at: AT(1m), parts: {index: 0, count: 3}}}
      - {node: out, msg: {payload: b, at: AT(1m), parts: {index: 1, count: 3}}}
      - {node: out, msg: {payload: c, at: AT(1m), parts: {index: 2, count: 3}}}
`)))
	})
	t.Run("link call times out", func(t *testing.T) {
		flows := stamped(`{"id":"call","type":"link call","z":"t1","name":"call","links":["deadend"],"timeout":"10","x":2,"y":1,"wires":[["stamp"]]},
  {"id":"deadend","type":"link in","z":"t1","name":"dead end","x":3,"y":3,"wires":[[]]}`)
		fast(t, one(t, flows, clockSuite(`
tests:
  - name: no return in ten seconds
    clock: CLOCK
    timeout: 1m
    inject: [{node: call, msg: {payload: 1}}]
    expect:
      - node: errors
        error: link call timed out after 10s
        assert: error.source.id = "call"
      - {node: out, nothing: true}
`)))
	})
}

// TestTimeInsideNodesIsTheTestsTime: a Function node's Date and an Inject's
// date-typed payload read the same clock the Delay waited on, so a flow that
// stamps or compares times agrees with itself under a test.
func TestTimeInsideNodesIsTheTestsTime(t *testing.T) {
	flows := stamped(`{"id":"wait","type":"delay","z":"t1","name":"wait","pauseType":"delay","timeout":"2","timeoutUnits":"minutes","x":2,"y":1,"wires":[["fn"]]},
  {"id":"fn","type":"function","z":"t1","name":"fn","func":"msg.js = Date.now(); msg.iso = new Date().toISOString(); return msg;","outputs":1,"x":3,"y":1,"wires":[["stamp"]]},
  {"id":"go","type":"inject","z":"t1","name":"go","props":[{"p":"payload","v":"","vt":"date"}],"x":1,"y":1,"wires":[["wait"]]}`)
	fast(t, one(t, flows, clockSuite(`
tests:
  - name: every clock agrees
    clock: CLOCK
    inject: [{node: go}]
    expect:
      - {node: out, msg: {payload: AT(0s), js: AT(2m), at: AT(2m)}, within: 2m}
`)))
}

func TestClockAndAtAreChecked(t *testing.T) {
	for _, tc := range []struct{ suite, says string }{
		{"tests:\n  - name: a\n    clock: tuesday\n    expect: [{node: x}]", `"tuesday" is not an RFC 3339 time`},
		{"tests:\n  - name: a\n    inject: [{node: x, at: soon}]\n    expect: [{node: x}]", `at "soon" is not a time from the start of the test`},
	} {
		if _, err := flowtest.Parse([]byte(tc.suite)); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("Parse(%q) = %v, want %q", tc.suite, err, tc.says)
		}
	}
}

// TestTheREADMEClockExample runs the clock example from the README, word for
// word, against a watchdog that extends.
func TestTheREADMEClockExample(t *testing.T) {
	const flows = `[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"dog","type":"trigger","z":"t1","name":"watchdog","op1":"on","op1type":"str","op2":"off","op2type":"str","duration":"5","units":"min","extend":true,"x":1,"y":1,"wires":[[]]}
]`
	rep := run(t, flows, "tests:\n"+readmeBlock(t, "  - name: a reading at 4m pushes the watchdog's off out to 9m"))
	if len(rep.Tests) != 2 {
		t.Fatalf("the example has %d tests, want 2", len(rep.Tests))
	}
	for _, r := range rep.Tests {
		fast(t, r)
	}
}

// TestACookieExpiresOnTheTestsClock: a cookie with a maxAge gets its Expires
// from the time the flow keeps, so a test can say exactly what the client
// would have been told.
func TestACookieExpiresOnTheTestsClock(t *testing.T) {
	const flows = `[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"reply","type":"http response","z":"t1","name":"reply","x":1,"y":1,"wires":[]}
]`
	expires := six.Add(time.Minute).UTC().Format(http.TimeFormat)
	fast(t, one(t, flows, clockSuite(`
tests:
  - name: a minute from six
    clock: CLOCK
    inject: [{node: reply, msg: {payload: ok, cookies: {session: {value: abc, maxAge: 60000}}}}]
    expect:
      - node: reply
        sent: {statusCode: 200}
        assert: $contains(cookies[0], "Expires=`+expires+`")
`)))
}
