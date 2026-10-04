package flowtest_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/nodes"
)

// theOutside is a broker, a database, a web service, a PLC and a UDP listener
// all at once: one TCP port and one UDP port on loopback, held by the test,
// counting every connection and every datagram. Every node in the flow below
// is pointed at them. A node that reaches out for real shows up in the count,
// and a node that tries to listen on them fails to start, because the test
// already has them.
type theOutside struct {
	tcp, udp       int
	dir            string
	accepts, grams atomic.Int64
}

func newOutside(t *testing.T) *theOutside {
	t.Helper()
	o := &theOutside{dir: t.TempDir()}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	o.tcp = ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			o.accepts.Add(1)
			c.Close()
		}
	}()

	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	o.udp = pc.LocalAddr().(*net.UDPAddr).Port
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
			o.grams.Add(1)
		}
	}()

	if err := os.WriteFile(filepath.Join(o.dir, "secret.txt"), []byte("the real contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	return o
}

// expand fills TCP, UDP and DIR into a template.
func (o *theOutside) expand(s string) string {
	return strings.NewReplacer("TCP", fmt.Sprint(o.tcp), "UDP", fmt.Sprint(o.udp), "DIR", filepath.ToSlash(o.dir)).Replace(s)
}

// everythingOutside is a flow with every node in the palette that reaches
// outside the process, each pointed at theOutside.
const everythingOutside = `[
  {"id":"t1","type":"tab","label":"Outside"},
  {"id":"brk","type":"mqtt-broker","broker":"127.0.0.1","port":TCP},
  {"id":"brk5","type":"mqtt-broker","broker":"127.0.0.1","port":TCP,"protocolVersion":"5"},
  {"id":"tlsc","type":"tls-config","cert":"/nowhere/cert.pem","key":"/nowhere/key.pem","ca":"/nowhere/ca.pem"},
  {"id":"brks","type":"mqtt-broker","broker":"127.0.0.1","port":TCP,"usetls":true,"tls":"tlsc"},
  {"id":"pgc","type":"hotloop-flow-postgres","host":"127.0.0.1","port":TCP,"database":"plant","user":"flow","sslmode":"disable"},
  {"id":"ifx","type":"hotloop-flow-influxdb","url":"http://127.0.0.1:TCP","apiVersion":"1","database":"plant"},
  {"id":"wsl","type":"websocket-listener","path":"/ws/line3"},
  {"id":"wsc","type":"websocket-client","path":"ws://127.0.0.1:TCP/feed"},

  {"id":"go","type":"junction","z":"t1","x":1,"y":1,"wires":[["pub","pub5","pubs","apis","tcrs","wsco","api","rec","look","ifo","tco","tcr","udo","ex","fo","fi","scn","ni","wso","hres"]]},
  {"id":"pub","type":"mqtt out","z":"t1","name":"publish","broker":"brk","topic":"line3/alarm","qos":"1","retain":true,"x":2,"y":1,"wires":[]},
  {"id":"pubs","type":"mqtt out","z":"t1","name":"publish tls","broker":"brks","topic":"line3/secure","x":2,"y":17,"wires":[]},
  {"id":"apis","type":"http request","z":"t1","name":"api tls","method":"GET","url":"https://127.0.0.1:TCP/secure","tls":"tlsc","x":2,"y":18,"wires":[[]]},
  {"id":"tcrs","type":"tcp request","z":"t1","name":"plc tls","host":"127.0.0.1","port":TCP,"out":"immed","tls":"tlsc","x":2,"y":19,"wires":[[]]},
  {"id":"pub5","type":"mqtt out","z":"t1","name":"publish v5","broker":"brk5","topic":"line3/state","contentType":"application/json","userProps":{"line":"3"},"x":2,"y":16,"wires":[]},
  {"id":"api","type":"http request","z":"t1","name":"api","method":"POST","url":"http://127.0.0.1:TCP/readings/{{topic}}","ret":"obj","x":2,"y":2,"wires":[["api-out"]]},
  {"id":"api-out","type":"debug","z":"t1","name":"api out","x":3,"y":2,"wires":[]},
  {"id":"rec","type":"postgres","z":"t1","name":"record","server":"pgc","mode":"insert","table":"readings","columns":[{"column":"line","value":"topic","valueType":"msg"},{"column":"value","value":"payload","valueType":"msg"}],"x":2,"y":3,"wires":[[]]},
  {"id":"look","type":"postgres","z":"t1","name":"lookup","server":"pgc","mode":"query","sql":"select limit_c from limits where line = $1","params":[{"value":"topic","valueType":"msg"}],"x":2,"y":4,"wires":[["look-out"]]},
  {"id":"look-out","type":"debug","z":"t1","name":"lookup out","x":3,"y":4,"wires":[]},
  {"id":"ifo","type":"influxdb out","z":"t1","name":"history","server":"ifx","measurement":"temperature","fields":[{"column":"value","value":"payload","valueType":"msg"}],"x":2,"y":5,"wires":[[]]},
  {"id":"tco","type":"tcp out","z":"t1","name":"printer","beserver":"client","host":"127.0.0.1","port":TCP,"x":2,"y":6,"wires":[]},
  {"id":"tcr","type":"tcp request","z":"t1","name":"plc","host":"127.0.0.1","port":TCP,"out":"sit","datatype":"utf8","x":2,"y":7,"wires":[["tcr-out"]]},
  {"id":"tcr-out","type":"debug","z":"t1","name":"plc out","x":3,"y":7,"wires":[]},
  {"id":"udo","type":"udp out","z":"t1","name":"beacon","addr":"127.0.0.1","port":UDP,"x":2,"y":8,"wires":[]},
  {"id":"ex","type":"exec","z":"t1","name":"touch","command":"touch","append":"DIR/exec-ran","addpay":false,"x":2,"y":9,"wires":[["ex-out"],[],[]]},
  {"id":"ex-out","type":"debug","z":"t1","name":"touch out","x":3,"y":9,"wires":[]},
  {"id":"fo","type":"file","z":"t1","name":"log","filename":"DIR/written.log","overwriteFile":"false","x":2,"y":10,"wires":[[]]},
  {"id":"fi","type":"file in","z":"t1","name":"read","filename":"DIR/secret.txt","format":"utf8","x":2,"y":11,"wires":[["fi-out"]]},
  {"id":"fi-out","type":"debug","z":"t1","name":"read out","x":3,"y":11,"wires":[]},
  {"id":"scn","type":"scan","z":"t1","name":"sweep","target":"127.0.0.1/32","targetType":"str","ports":"TCP","x":2,"y":12,"wires":[["scn-out"]]},
  {"id":"scn-out","type":"debug","z":"t1","name":"sweep out","x":3,"y":12,"wires":[]},
  {"id":"ni","type":"netinfo","z":"t1","name":"nics","x":2,"y":13,"wires":[["ni-out"]]},
  {"id":"ni-out","type":"debug","z":"t1","name":"nics out","x":3,"y":13,"wires":[]},
  {"id":"wso","type":"websocket out","z":"t1","name":"screens","server":"wsl","x":2,"y":14,"wires":[]},
  {"id":"hres","type":"http response","z":"t1","name":"reply","statusCode":"201","x":2,"y":15,"wires":[]},

  {"id":"min","type":"mqtt in","z":"t1","name":"readings","broker":"brk","topic":"line3/#","x":1,"y":20,"wires":[["min-out"]]},
  {"id":"min5","type":"mqtt in","z":"t1","name":"readings v5","broker":"brk5","topic":"line3/#","x":1,"y":27,"wires":[["min-out"]]},
  {"id":"min-out","type":"debug","z":"t1","name":"readings out","x":2,"y":20,"wires":[]},
  {"id":"tin","type":"tcp in","z":"t1","name":"listen","server":"server","host":"127.0.0.1","port":TCP,"x":1,"y":21,"wires":[[]]},
  {"id":"tic","type":"tcp in","z":"t1","name":"dial","server":"client","host":"127.0.0.1","port":TCP,"x":1,"y":22,"wires":[[]]},
  {"id":"uin","type":"udp in","z":"t1","name":"listen udp","port":UDP,"x":1,"y":23,"wires":[[]]},
  {"id":"hin","type":"http in","z":"t1","name":"hook","url":"/hook","method":"post","x":1,"y":24,"wires":[["hres"]]},
  {"id":"wat","type":"watch","z":"t1","name":"watch","files":"DIR","x":1,"y":25,"wires":[["wat-out"]]},
  {"id":"wat-out","type":"debug","z":"t1","name":"watch out","x":2,"y":25,"wires":[]},
  {"id":"wsi","type":"websocket in","z":"t1","name":"ws in","server":"wsl","x":1,"y":26,"wires":[[]]},
  {"id":"wsci","type":"websocket in","z":"t1","name":"ws client in","client":"wsc","x":1,"y":28,"wires":[[]]},
  {"id":"wsco","type":"websocket out","z":"t1","name":"ws client out","client":"wsc","x":2,"y":28,"wires":[]},
  {"id":"errors","type":"catch","z":"t1","name":"errors","x":1,"y":30,"wires":[[]]}
]`

// TestNothingReachesTheOutside is the promise behind flow tests: every node in
// the palette that talks to the world outside the process does its whole job
// in a test, and none of it leaves. What each would have sent is checked
// exactly, what came back is what the test scripted, and the ports, the files
// and the shell saw nothing at all.
func TestNothingReachesTheOutside(t *testing.T) {
	o := newOutside(t)
	flows := o.expand(everythingOutside)

	// The paths the HTTP In and the websocket listener would serve are taken
	// first, so either one serving under a test can't start, the same trap
	// the held ports set for TCP In and UDP In.
	for _, r := range []struct{ method, path string }{{http.MethodPost, "/hook"}, {http.MethodGet, "/ws/line3"}} {
		unbind, err := nodes.Routes.Register("held-by-the-test", r.method, r.path,
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(unbind)
	}

	r := one(t, flows, o.expand(`
tests:
  - name: one reading, everything outside
    inject:
      - node: go
        msg: {topic: "3", payload: 92.5, cookies: {session: abc}}
    replies:
      - {node: api, reply: {statusCode: 200, payload: {limit: 90}}}
      - {node: lookup, reply: {rows: [{limit_c: 90}]}}
      - {node: plc, reply: {payload: ACK}}
      - {node: touch, reply: {stdout: "done\n", code: 0}}
      - {node: read, reply: {payload: the scripted contents}}
      - {node: sweep, reply: {devices: [{address: 127.0.0.1, source: tcp}]}}
      - {node: nics, reply: {interfaces: [{name: plant0}]}}
    expect:
      - node: publish
        sent: {topic: line3/alarm, payload: "92.5", qos: 1, retain: true}
      - node: publish tls
        sent: {topic: line3/secure, payload: "92.5"}
      - node: api tls
        sent: {method: GET, url: "https://127.0.0.1:TCP/secure"}
      - node: plc tls
        sent: {host: 127.0.0.1, port: TCP, payload: "92.5"}
      - node: publish v5
        sent: {topic: line3/state, payload: "92.5", properties: {contentType: application/json, userProperties: {line: "3"}}}
      - node: api
        sent: {method: POST, url: "http://127.0.0.1:TCP/readings/3", payload: "92.5", headers: {content-type: application/json, cookie: session=abc}}
      - node: api out
        msg: {payload: {limit: 90}, statusCode: 200}
      - node: record
        sent: {params: ["3", 92.5]}
        assert: $contains(query, "INSERT INTO") and $contains(query, "readings")
      - node: lookup
        sent: {query: "select limit_c from limits where line = $1", params: ["3"]}
      - node: lookup out
        msg: {payload: [{limit_c: 90}], rowCount: 1}
      - node: history
        sent: {line: "temperature value=92.5"}
      - node: printer
        sent: {host: 127.0.0.1, port: TCP, payload: "92.5"}
      - node: plc
        sent: {host: 127.0.0.1, port: TCP, payload: "92.5"}
      - node: plc out
        msg: {payload: ACK}
      - node: beacon
        sent: {host: 127.0.0.1, port: UDP, payload: "92.5"}
      - node: touch
        sent: {command: touch, args: [DIR/exec-ran]}
      - node: touch out
        msg: {payload: "done\n", rc: {code: 0}}
      - node: log
        sent: {filename: DIR/written.log, action: append, payload: "92.5\n"}
      - node: read out
        msg: {payload: the scripted contents, filename: DIR/secret.txt}
      - node: sweep
        sent: {range: 127.0.0.1/32, ports: [TCP]}
      - node: sweep out
        msg: {payload: [{address: 127.0.0.1}], deviceCount: 1}
      - node: nics out
        msg: {payload: [{name: plant0}]}
      - node: screens
        sent: {payload: "92.5"}
      - node: ws client out
        sent: {payload: "92.5"}
      - node: reply
        sent: {statusCode: 201, payload: "92.5"}
        assert: $count(cookies) = 1 and $contains(cookies[0], "session=abc")
      - {node: readings out, nothing: true}
      - {node: watch out, nothing: true}
      - {node: errors, nothing: true}
`))
	wantStatus(t, r, flowtest.Pass)

	// Give anything that leaked a moment to arrive before counting.
	time.Sleep(200 * time.Millisecond)
	if n := o.accepts.Load(); n != 0 {
		t.Errorf("%d TCP connection(s) reached the outside", n)
	}
	if n := o.grams.Load(); n != 0 {
		t.Errorf("%d UDP datagram(s) reached the outside", n)
	}
	for _, name := range []string{"exec-ran", "written.log"} {
		if _, err := os.Stat(filepath.Join(o.dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists: the test touched the disk (%v)", name, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(o.dir, "secret.txt")); string(b) != "the real contents" {
		t.Errorf("secret.txt changed: %q", b)
	}
}

// TestAScriptedFailureIsTheRealFailure: "the historian is down" is a reply a
// test can script, and the flow handles it the way it would handle the real
// thing, here a Catch node.
func TestAScriptedFailureIsTheRealFailure(t *testing.T) {
	o := newOutside(t)
	r := one(t, o.expand(everythingOutside), `
tests:
  - name: the database is down
    inject: [{node: go, msg: {topic: "3", payload: 1}}]
    replies:
      - {node: record, error: connection refused}
    expect:
      - node: errors
        error: "insert into readings: connection refused"
`)
	wantStatus(t, r, flowtest.Pass)
	if n := o.accepts.Load(); n != 0 {
		t.Errorf("%d connection(s) reached the outside", n)
	}
}

// TestRepliesAnswerInOrder: the first reply answers the first call and the
// last keeps answering, so a test can script a device that answers, then
// stops.
func TestRepliesAnswerInOrder(t *testing.T) {
	const flows = `[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"api","type":"http request","z":"t1","name":"api","url":"http://192.0.2.1/state","ret":"txt","x":1,"y":1,"wires":[["out"]]},
  {"id":"out","type":"debug","z":"t1","name":"out","x":2,"y":1,"wires":[]}
]`
	r := one(t, flows, `
tests:
  - name: up, then down, then still down
    inject: [{node: api}, {node: api}, {node: api}]
    replies:
      - {node: api, reply: {payload: running}}
      - {node: api, reply: {statusCode: 503, payload: stopped}}
    expect:
      - {node: out, msg: {payload: running, statusCode: 200}}
      - {node: out, msg: {payload: stopped, statusCode: 503}}
      - {node: out, msg: {payload: stopped, statusCode: 503}}
      - {node: api, sent: {method: GET, url: "http://192.0.2.1/state"}}
`)
	wantStatus(t, r, flowtest.Pass)
}

// TestSentFailsAndSaysWhat: a wrong expectation on what went out says what did.
func TestSentFailsAndSaysWhat(t *testing.T) {
	o := newOutside(t)
	r := one(t, o.expand(everythingOutside), `
tests:
  - name: wrong topic
    timeout: 300ms
    inject: [{node: go, msg: {payload: 1}}]
    expect:
      - node: publish
        sent: {topic: line4/alarm}
`)
	wantStatus(t, r, flowtest.Fail)
	if !strings.Contains(problems(r), `sent topic: wanted "line4/alarm", got "line3/alarm"`) {
		t.Errorf("unhelpful failure:\n%s", problems(r))
	}
}

// TestRepliesAndSentNeedANodeThatReachesOut: scripting a reply for a Switch is a
// test that misunderstands the flow, and it says so instead of passing.
func TestRepliesAndSentNeedANodeThatReachesOut(t *testing.T) {
	r := one(t, line3, `
tests:
  - name: x
    inject: [{node: reading, msg: {payload: 1}}]
    replies: [{node: "over limit?", reply: {payload: 1}}]
    expect: [{node: "over limit?", sent: {payload: 1}}]
`)
	wantStatus(t, r, flowtest.Error)
	for _, want := range []string{"reply 1: switch", "nothing to reply to", "expect 1:", "nothing to check sent against"} {
		if !strings.Contains(problems(r), want) {
			t.Errorf("wanted %q in:\n%s", want, problems(r))
		}
	}
}

// TestANodeWithNoStandInIsRefused: a node that reaches outside and has no
// stand-in, from a registry other than the palette or one added without one,
// is refused by name rather than run for real.
func TestANodeWithNoStandInIsRefused(t *testing.T) {
	reg := node.NewRegistry()
	err := reg.Register(node.Descriptor{
		Type: "raw socket", Category: node.CategoryNetwork, Inputs: 1,
		Compatibility: node.Compatibility{Level: node.CompatOnly},
	}, func(*node.Definition) (node.Node, error) { return dialer{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	s, err := flowtest.Parse([]byte(`
tests:
  - name: x
    inject: [{node: sock, msg: {payload: 1}}]
    expect: [{node: sock, nothing: true}]
`))
	if err != nil {
		t.Fatal(err)
	}
	rep := flowtest.Run(context.Background(), []byte(`[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"sock","type":"raw socket","z":"t1","name":"sock","x":1,"y":1,"wires":[]}
]`), s, flowtest.Options{Registry: reg})
	r := rep.Tests[0]
	wantStatus(t, r, flowtest.Error)
	if !strings.Contains(problems(r), `no stand-in to talk to instead: raw socket "sock" (sock)`) {
		t.Errorf("the refusal:\n%s", problems(r))
	}
}

type dialer struct{}

func (dialer) Receive(context.Context, *engine.Msg, node.Emitter) error {
	return errors.New("this would have dialled")
}

// TestEveryNodeThatReachesOutHasAStandIn is the palette half of the promise: a
// node added to network, storage or discover, or exec, without a stand-in
// fails here, long before a test refuses to run it. So does a configuration
// node that starts on its own, since the runtime started those (#40): one that
// listens or dials by itself has to stay home under a test as well.
func TestEveryNodeThatReachesOutHasAStandIn(t *testing.T) {
	for _, d := range node.Default.Descriptors() {
		reaches := d.Type == "exec" || d.Category == node.CategoryNetwork ||
			d.Category == node.CategoryStorage || d.Category == node.CategoryDiscover
		if reaches && !d.StandIn {
			t.Errorf("%s reaches outside the process and has no stand-in for a flow test", d.Type)
		}
	}
	for _, typ := range startingConfigNodes(t) {
		reg, _ := node.Default.Lookup(typ)
		if !reg.Descriptor.StandIn {
			t.Errorf("%s is a configuration node that starts on its own and has no stand-in for a flow test", typ)
		}
	}
}

// startingConfigNodes builds every configuration node type in the palette from
// an empty entry and returns the ones that implement node.Starter. One whose
// factory refuses an empty entry is built again with the settings its
// descriptor marks required filled in with something plausible.
func startingConfigNodes(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, d := range node.Default.Descriptors() {
		if !d.IsConfig {
			continue
		}
		reg, _ := node.Default.Lookup(d.Type)
		raw := map[string]any{"id": "c1", "type": d.Type}
		for _, p := range d.Props {
			if p.Required {
				raw[p.Name] = plausible(p.Name)
			}
		}
		inst, err := reg.New(&node.Definition{Node: &engine.Node{ID: "c1", Type: d.Type, IsConfig: true, Raw: raw}, Services: noServices{}})
		if err != nil {
			// Refuses without settings a test can't guess, a token or an
			// org. Those don't start on their own; the check below that the
			// websocket ones were found keeps this honest.
			t.Logf("%s doesn't build from a bare entry: %v", d.Type, err)
			continue
		}
		if _, starts := inst.(node.Starter); starts {
			out = append(out, d.Type)
		}
	}
	if !slices.Contains(out, "websocket-listener") || !slices.Contains(out, "websocket-client") {
		t.Fatalf("found %v starting on their own; the websocket listener and client both do since #40", out)
	}
	return out
}

func plausible(name string) any {
	switch name {
	case "path":
		return "ws://127.0.0.1:1/x"
	case "url":
		return "http://127.0.0.1:1"
	case "port":
		return 1.0
	}
	return "x"
}

// noServices is node.Services with nothing behind it, for building a node to
// look at it.
type noServices struct{}

func (noServices) Context(node.ContextScope) node.Context { return nil }
func (noServices) Credential(string) (string, bool)       { return "", false }
func (noServices) ConfigNode(string) (node.Node, bool)    { return nil, false }
func (noServices) Env(string) (string, bool)              { return "", false }
func (noServices) Log(node.LogLevel, string, ...any)      {}

// TestTheREADMEExample runs the stand-in example from the README, word for
// word, against a flow it describes, so the example can't quietly stop being
// true.
func TestTheREADMEExample(t *testing.T) {
	const flows = `[
  {"id":"t1","type":"tab","label":"Line 3"},
  {"id":"brk","type":"mqtt-broker","broker":"broker.plant.example","port":1883},
  {"id":"pgc","type":"hotloop-flow-postgres","host":"db.plant.example","database":"plant","user":"flow"},
  {"id":"ifx","type":"hotloop-flow-influxdb","url":"http://historian.plant.example:8086","apiVersion":"1","database":"plant"},
  {"id":"reading","type":"inject","z":"t1","name":"reading","x":1,"y":1,"wires":[["keep","historian","api"]]},
  {"id":"keep","type":"change","z":"t1","rules":[{"t":"set","p":"reading","pt":"msg","to":"payload","tot":"msg"}],"x":2,"y":1,"wires":[["lookup"]]},
  {"id":"lookup","type":"postgres","z":"t1","name":"lookup limit","server":"pgc","mode":"query","sql":"select limit_c from limits where line = $1","params":[{"value":"topic","valueType":"msg"}],"x":3,"y":1,"wires":[["decide"]]},
  {"id":"decide","type":"function","z":"t1","name":"over the limit?","func":"if (msg.reading > msg.payload[0].limit_c) { msg.payload = 'HIGH'; return [msg, null]; }\nreturn [null, msg];","outputs":2,"x":4,"y":1,"wires":[["plant"],[]]},
  {"id":"plant","type":"mqtt out","z":"t1","name":"to the plant","broker":"brk","topic":"line3/alarm","qos":"1","retain":true,"x":5,"y":1,"wires":[]},
  {"id":"historian","type":"influxdb out","z":"t1","name":"historian","server":"ifx","measurement":"temperature","fields":[{"column":"value","value":"payload","valueType":"msg"}],"x":2,"y":2,"wires":[[]]},
  {"id":"api","type":"http request","z":"t1","name":"maintenance api","url":"http://maintenance.plant.example/line/{{topic}}","ret":"obj","x":2,"y":3,"wires":[[]]},
  {"id":"errors","type":"catch","z":"t1","name":"errors","x":1,"y":5,"wires":[[]]}
]`
	r := one(t, flows, "tests:\n"+readmeBlock(t, "  - name: the limit comes from the database and the alarm goes to the broker"))
	wantStatus(t, r, flowtest.Pass)
}

// readmeBlock returns the YAML in the README from the line that starts with
// first to the end of its code block.
func readmeBlock(t *testing.T, first string) string {
	t.Helper()
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(readme), "\r\n", "\n")
	start := strings.Index(text, first)
	if start < 0 {
		t.Fatalf("the example starting %q is gone from the README", first)
	}
	end := strings.Index(text[start:], "```")
	return text[start : start+end]
}
