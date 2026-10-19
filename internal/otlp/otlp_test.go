package otlp_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
	_ "github.com/HotLoop-io/hotloop-flow/internal/nodes" // the real palette
	"github.com/HotLoop-io/hotloop-flow/internal/otlp"
)

// flow is a split, a join and a subflow on one tab, and on the same tab an
// Inject whose Function throws to a Catch, so the spans have a join with several
// parents in them and one with an error on it.
const flow = `[
  {"id":"t1","type":"tab","label":"Line 3"},
  {"id":"go","type":"inject","z":"t1","name":"go","props":[{"p":"payload","v":"[1,2,3]","vt":"json"}],"x":1,"y":1,"wires":[["split"]]},
  {"id":"split","type":"split","z":"t1","name":"split","x":2,"y":1,"wires":[["bump"]]},
  {"id":"bump","type":"change","z":"t1","name":"bump","rules":[{"t":"set","p":"seen","pt":"msg","to":"yes","tot":"str"}],"x":3,"y":1,"wires":[["join"]]},
  {"id":"join","type":"join","z":"t1","name":"join","mode":"custom","build":"array","count":"3","timeout":"30","x":4,"y":1,"wires":[["inst"]]},
  {"id":"sf","type":"subflow","name":"stamp","in":[{"x":0,"y":0,"wires":[{"id":"mark"}]}],"out":[{"x":0,"y":0,"wires":[{"id":"mark","port":0}]}]},
  {"id":"mark","type":"change","z":"sf","name":"mark","rules":[{"t":"set","p":"stamped","pt":"msg","to":"true","tot":"bool"}],"x":1,"y":1,"wires":[[]]},
  {"id":"inst","type":"subflow:sf","z":"t1","name":"stamper","x":5,"y":1,"wires":[["end"]]},
  {"id":"end","type":"debug","z":"t1","name":"end","x":6,"y":1,"wires":[]},
  {"id":"bad","type":"inject","z":"t1","name":"bad","props":[{"p":"payload","v":"-1","vt":"num"}],"x":1,"y":2,"wires":[["boom"]]},
  {"id":"boom","type":"function","z":"t1","name":"boom","func":"throw new Error('reading out of range');","outputs":1,"x":2,"y":2,"wires":[[]]},
  {"id":"errs","type":"catch","z":"t1","name":"errs","x":3,"y":2,"wires":[["err-out"]]},
  {"id":"err-out","type":"debug","z":"t1","name":"err out","x":4,"y":2,"wires":[]}
]`

// collected is one span as the collector's debug exporter printed it.
type collected struct {
	trace, parent, id, name, status, message string
	links                                    []string
	attrs                                    map[string]string
}

var attrLine = regexp.MustCompile(`^\s+-> ([\w.]+): (Str|Int)\((.*)\)$`)

// parseCollector reads the debug exporter's detailed output, which prints
// every span the collector accepted, into one record per span.
func parseCollector(log string) []collected {
	var out []collected
	var cur *collected
	inLinks := false
	field := func(line, name string) (string, bool) {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, name) {
			return "", false
		}
		_, v, ok := strings.Cut(t, ":")
		return strings.TrimSpace(v), ok
	}
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "Span #"):
			out = append(out, collected{attrs: map[string]string{}})
			cur = &out[len(out)-1]
			inLinks = false
		case cur == nil:
		case strings.HasPrefix(line, "Links:"):
			inLinks = true
		case strings.HasPrefix(line, "SpanLink #"):
		case inLinks && strings.Contains(line, "-> ID:"):
			v, _ := field(strings.TrimPrefix(strings.TrimSpace(line), "-> "), "ID")
			cur.links = append(cur.links, v)
		case strings.HasPrefix(line, "    Trace ID"):
			cur.trace, _ = field(line, "Trace ID")
		case strings.HasPrefix(line, "    Parent ID"):
			cur.parent, _ = field(line, "Parent ID")
		case strings.HasPrefix(line, "    ID "):
			cur.id, _ = field(line, "ID")
		case strings.HasPrefix(line, "    Name"):
			cur.name, _ = field(line, "Name")
		case strings.HasPrefix(line, "    Status code"):
			cur.status, _ = field(line, "Status code")
		case strings.HasPrefix(line, "    Status message"):
			cur.message, _ = field(line, "Status message")
		default:
			if m := attrLine.FindStringSubmatch(line); m != nil && !inLinks {
				cur.attrs[m[1]] = m[3]
			}
		}
	}
	return out
}

// TestSpansReachARealCollector sends the hops of real traces to an
// OpenTelemetry collector and reads back what the collector says it received.
// The collector is the stock otel/opentelemetry-collector image, not a stand-in
// for one: the point is that a collector accepts what we send, and a test
// against a fake would only prove we agree with ourselves.
//
// HOTLOOP_FLOW_TEST_OTLP is the collector's OTLP/HTTP address, and
// HOTLOOP_FLOW_TEST_OTLP_LOG a file its output is written to. CI starts the
// collector and says so; without them the test skips, and CI checks that it
// didn't.
func TestSpansReachARealCollector(t *testing.T) {
	addr, logPath := os.Getenv("HOTLOOP_FLOW_TEST_OTLP"), os.Getenv("HOTLOOP_FLOW_TEST_OTLP_LOG")
	if addr == "" || logPath == "" {
		t.Skip("HOTLOOP_FLOW_TEST_OTLP and HOTLOOP_FLOW_TEST_OTLP_LOG name a running collector and its log; none given")
	}

	tracer := engine.NewTracer(engine.TraceLimits{})
	service := fmt.Sprintf("hotloop-flow-test-%d", time.Now().UnixNano())
	exp, err := otlp.New(otlp.Config{Endpoint: "http://" + addr, ServiceName: service, ServiceVersion: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tracer.OnFinish(exp.Add)

	suite := &flowtest.Suite{Tests: []flowtest.Case{
		{Name: "fan", Inject: []flowtest.Injection{{Node: "go", Msg: map[string]any{"payload": []any{1.0, 2.0, 3.0}}}}},
		{Name: "error", Inject: []flowtest.Injection{{Node: "bad", Msg: map[string]any{"payload": -1.0}}}},
	}}
	rep := flowtest.Run(context.Background(), []byte(flow), suite, flowtest.Options{Tracer: tracer})
	if rep.Passed != 2 {
		t.Fatalf("the runs didn't pass: %+v", rep)
	}
	if err := exp.Flush(context.Background()); err != nil {
		t.Fatalf("the collector refused the spans: %v", err)
	}

	type want struct {
		trace string
		hop   engine.Hop
		tr    *engine.Trace
	}
	var wants []want
	for _, tr := range tracer.Traces() {
		s := tr.Snapshot()
		for _, h := range s.Hops {
			wants = append(wants, want{trace: s.ID, hop: h, tr: tr})
		}
	}
	// 1 + 1 + 3 + 3 + 1 + 1 + 1 for the fan, 1 + 1 + 1 + 1 for the error.
	if len(wants) < 14 {
		t.Fatalf("only %d hops were written down", len(wants))
	}

	var got map[string]collected
	deadline := time.Now().Add(15 * time.Second)
	for {
		raw, _ := os.ReadFile(logPath)
		got = map[string]collected{}
		for _, c := range parseCollector(string(raw)) {
			got[c.trace+"/"+c.id] = c
		}
		missing := 0
		for _, w := range wants {
			if _, ok := got[w.trace+"/"+w.tr.SpanID(w.hop.ID)]; !ok {
				missing++
			}
		}
		if missing == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d spans never showed up in the collector's output (%d spans seen in all)", missing, len(wants), len(got))
		}
		time.Sleep(200 * time.Millisecond)
	}

	if raw, _ := os.ReadFile(logPath); !strings.Contains(string(raw), "service.name: Str("+service+")") {
		t.Errorf("the collector never saw service.name %s on a resource", service)
	}

	joinParents := 0
	sawError := false
	for _, w := range wants {
		c := got[w.trace+"/"+w.tr.SpanID(w.hop.ID)]
		if c.name != w.hop.Type+" "+w.hop.Node {
			t.Errorf("hop %d: the collector has the span named %q, want %q", w.hop.ID, c.name, w.hop.Type+" "+w.hop.Node)
		}
		wantParent := ""
		if len(w.hop.Parents) > 0 {
			wantParent = w.tr.SpanID(w.hop.Parents[0])
		}
		if c.parent != wantParent {
			t.Errorf("hop %d (%s): parent %q, want %q", w.hop.ID, w.hop.Node, c.parent, wantParent)
		}
		if len(w.hop.Parents) > 1 {
			if len(c.links) != len(w.hop.Parents)-1 {
				t.Errorf("hop %d (%s): %d links, want %d", w.hop.ID, w.hop.Node, len(c.links), len(w.hop.Parents)-1)
			}
			joinParents = max(joinParents, len(w.hop.Parents))
		}
		if c.attrs["flow.node.id"] != w.hop.Node || c.attrs["flow.node.type"] != w.hop.Type || c.attrs["flow.message.id"] != w.hop.MsgID {
			t.Errorf("hop %d (%s): attributes %v", w.hop.ID, w.hop.Node, c.attrs)
		}
		if w.hop.Error != "" {
			sawError = true
			if c.status != "Error" || !strings.Contains(c.message, "reading out of range") {
				t.Errorf("the failing hop arrived as status %q %q", c.status, c.message)
			}
		}
	}
	if joinParents < 3 {
		t.Errorf("no span with a join's three parents reached the collector (most parents seen: %d)", joinParents)
	}
	if !sawError {
		t.Error("the failing hop wasn't in the trace")
	}
	if st := exp.Stats(); st.Exported != int64(len(wants)) || st.Failed != 0 || st.Dropped != 0 {
		t.Errorf("the exporter says %+v for %d spans", st, len(wants))
	}
	t.Logf("%d spans from %d traces reached the collector, service %s", len(wants), len(tracer.Traces()), service)
}

// TestAnUnreachableCollectorCostsTheFlowNothing: a collector that is down must
// not make a flow slower, fail a deploy or grow the process. The spans are
// counted as failed and the flow carries on.
func TestAnUnreachableCollectorCostsTheFlowNothing(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing is listening there now

	exp, err := otlp.New(otlp.Config{Endpoint: "http://" + addr, Timeout: time.Second, QueueSize: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tracer := engine.NewTracer(engine.TraceLimits{})
	tracer.OnFinish(exp.Add)

	suite := &flowtest.Suite{Tests: []flowtest.Case{{Name: "fan",
		Inject: []flowtest.Injection{{Node: "go", Msg: map[string]any{"payload": []any{1.0, 2.0, 3.0}}}}}}}
	rep := flowtest.Run(context.Background(), []byte(flow), suite, flowtest.Options{Tracer: tracer})
	if rep.Passed != 1 {
		t.Fatalf("the flow didn't pass with the collector down: %+v", rep)
	}
	if err := exp.Flush(context.Background()); err == nil {
		t.Error("flushing to nothing reported success")
	}
	st := exp.Stats()
	if st.Dropped == 0 || st.Failed == 0 || st.Exported != 0 {
		t.Errorf("stats with the collector down: %+v", st)
	}
}

func TestTheEndpointHasToBeAnHTTPAddress(t *testing.T) {
	for _, bad := range []string{"", "collector:4318", "ftp://collector", "http://"} {
		if _, err := otlp.New(otlp.Config{Endpoint: bad}, nil); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if _, err := otlp.New(otlp.Config{Endpoint: "https://collector.example:4318/"}, nil); err != nil {
		t.Errorf("a good address was refused: %v", err)
	}
}
