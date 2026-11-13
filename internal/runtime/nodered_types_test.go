package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowhttp"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/nodes"
	_ "github.com/HotLoop-io/hotloop-flow/internal/nodes"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
	"github.com/simonvetter/modbus"
)

// The Node-RED node types a plant's flows use, run through the real runtime:
// the flow file in testdata/nodered/types.json has one of every type, in the
// shape Shepherd Boy Farms' captured flows carry them, with the plant's
// addresses and logins swapped for test ones.

func loadFlows(t *testing.T, doc []byte) *engine.Flows {
	t.Helper()
	flows, err := engine.ParseFlows(doc)
	if err != nil {
		t.Fatal(err)
	}
	return flows
}

func startFlows(t *testing.T, doc []byte) (*runtime.Runtime, []runtime.StartError) {
	t.Helper()
	rt := runtime.New(node.Default, loadFlows(t, doc), runtime.Options{})
	go func() {
		for range rt.Events() {
		}
	}()
	fails := rt.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		rt.Stop(ctx)
	})
	return rt, fails
}

func TestEveryNodeRedTypeInTheFixtureStarts(t *testing.T) {
	doc, err := os.ReadFile("../../testdata/nodered/types.json")
	if err != nil {
		t.Fatal(err)
	}
	// The one type of node that listens: keep it off the ports of a busy box.
	doc = rewriteJSON(t, doc, func(n map[string]any) {
		if n["type"] == "modbus-server" {
			n["serverPort"] = fmt.Sprint(freeTCPPort(t))
		}
	})
	rt, fails := startFlows(t, doc)
	for _, f := range fails {
		t.Errorf("%s %s did not start: %v", f.Type, f.NodeID, f.Err)
	}
	var arr []map[string]any
	_ = json.Unmarshal(doc, &arr)
	seen := map[string]bool{}
	for _, n := range arr {
		typ := n["type"].(string)
		if typ == "tab" || n["d"] == true {
			continue
		}
		id := n["id"].(string)
		if _, isConfig := n["x"]; isConfig {
			if !rt.Running(id) {
				t.Errorf("%s %s is not running", typ, id)
			}
		}
		seen[typ] = true
	}
	if len(seen) < 30 {
		t.Fatalf("the fixture covers only %d types", len(seen))
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// rewriteJSON changes every node of a flow file.
func rewriteJSON(t *testing.T, doc []byte, f func(map[string]any)) []byte {
	t.Helper()
	var arr []map[string]any
	if err := json.Unmarshal(doc, &arr); err != nil {
		t.Fatal(err)
	}
	for _, n := range arr {
		f(n)
	}
	out, err := json.Marshal(arr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// device is a Modbus TCP server holding a float32 the way a plant PLC does.
type device struct {
	mu      sync.Mutex
	holding [100]uint16
	coils   [100]bool
}

func (d *device) HandleCoils(r *modbus.CoilsRequest) ([]bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.IsWrite {
		copy(d.coils[r.Addr:], r.Args)
		return nil, nil
	}
	return append([]bool(nil), d.coils[r.Addr:int(r.Addr)+int(r.Quantity)]...), nil
}
func (d *device) HandleDiscreteInputs(*modbus.DiscreteInputsRequest) ([]bool, error) {
	return nil, modbus.ErrIllegalFunction
}
func (d *device) HandleHoldingRegisters(r *modbus.HoldingRegistersRequest) ([]uint16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.IsWrite {
		copy(d.holding[r.Addr:], r.Args)
		return nil, nil
	}
	return append([]uint16(nil), d.holding[r.Addr:int(r.Addr)+int(r.Quantity)]...), nil
}
func (d *device) HandleInputRegisters(*modbus.InputRegistersRequest) ([]uint16, error) {
	return nil, modbus.ErrIllegalFunction
}

func startDevice(t *testing.T) (*device, int) {
	t.Helper()
	d := &device{}
	port := freeTCPPort(t)
	srv, err := modbus.NewServer(&modbus.ServerConfiguration{URL: fmt.Sprintf("tcp://127.0.0.1:%d", port),
		Timeout: time.Minute, Logger: log.New(io.Discard, "", 0)}, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return d, port
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The whole path a plant relies on: a Modbus device, a getter, a function that
// builds a float from the bytes with Buffer, a gauge on the dashboard, and an
// operator's click on the page writing back to the device.
func TestAModbusReadReachesTheDashboardAndAClickWritesBack(t *testing.T) {
	dev, port := startDevice(t)
	dev.holding[10], dev.holding[11], dev.holding[12], dev.holding[13] = 0, 0, 17087, 13107 // float at 12: 95.6

	doc, err := os.ReadFile("../../testdata/nodered/types.json")
	if err != nil {
		t.Fatal(err)
	}
	doc = rewriteJSON(t, doc, func(n map[string]any) {
		switch n["type"] {
		case "modbus-client":
			n["tcpPort"] = fmt.Sprint(port)
		case "modbus-server":
			n["serverPort"] = fmt.Sprint(freeTCPPort(t))
		case "modbus-getter":
			n["adr"] = "10"
		}
	})
	// f1 reads the float at the 12th and 13th registers: bytes 4 and 5 of the response.
	rt, fails := startFlows(t, doc)
	if len(fails) != 0 {
		t.Fatalf("failures: %v", fails)
	}
	if err := rt.Inject("go", engine.NewMsgWithPayload("go")); err != nil {
		t.Fatal(err)
	}
	// The mux serves the dashboard through the flow routes, as the binary does.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, params, ok := nodes.Routes.Match(r.Method, r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, flowhttp.WithRouteParams(r, params))
	}))
	defer srv.Close()

	eventually(t, "the gauge to show the float the device holds", func() bool {
		_, body := get(t, srv.URL+"/ui/api/layout")
		return strings.Contains(body, `"id":"gauge"`)
	})
	var snapshot string
	eventually(t, "the dashboard state to carry the float (95.5999...)", func() bool {
		snapshot = readSnapshot(t, srv.URL+"/ui/api/events")
		return strings.Contains(snapshot, `"id":"gauge"`) && strings.Contains(snapshot, "95.59")
	})
	if !strings.Contains(snapshot, `"id":"num"`) || !strings.Contains(snapshot, `"id":"txt"`) {
		t.Fatalf("the numeric and the text did not get the register values: %s", snapshot)
	}

	// The operator presses Heater 1: the button sends 1 to a modbus-write on coil 1.
	post := func(body string) int {
		r, err := http.Post(srv.URL+"/ui/api/emit", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if c := post(`{"id":"btn","event":{"event":"click"}}`); c != 204 {
		t.Fatalf("click = %d", c)
	}
	eventually(t, "coil 1 to be written", func() bool { dev.mu.Lock(); defer dev.mu.Unlock(); return dev.coils[1] })

	// The operator sets a numeric: the function builds a flex write, and register 28 changes.
	if c := post(`{"id":"num","event":{"event":"change","value":321}}`); c != 204 {
		t.Fatalf("change = %d", c)
	}
	eventually(t, "register 28 to hold 321", func() bool { dev.mu.Lock(); defer dev.mu.Unlock(); return dev.holding[28] == 321 })
}

func readSnapshot(t *testing.T, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 1<<16)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if strings.Contains(string(buf), "\n\n") || err != nil {
			break
		}
	}
	return string(buf)
}

// A function that sets an interval in its setup code ticks while the flow runs
// and stops when it is redeployed.
func TestASetIntervalInAFlowTicksAndStopsWithTheFlow(t *testing.T) {
	doc := []byte(`[
	 {"id":"t","type":"tab","label":"T"},
	 {"id":"tick","type":"function","z":"t","name":"","func":"return null;","outputs":1,"timeout":0,"noerr":0,
	  "initialize":"let n = 0;\nsetInterval(function () { n++; node.send({ payload: n }); }, 20);","finalize":"","libs":[],"x":1,"y":1,"wires":[["dbg"]]},
	 {"id":"dbg","type":"debug","z":"t","name":"","active":true,"tosidebar":true,"console":false,"tostatus":false,"complete":"payload","x":2,"y":1,"wires":[]}
	]`)
	rt := runtime.New(node.Default, loadFlows(t, doc), runtime.Options{})
	var mu sync.Mutex
	debugs := 0
	go func() {
		for e := range rt.Events() {
			if e.Topic == runtime.TopicDebug {
				mu.Lock()
				debugs++
				mu.Unlock()
			}
		}
	}()
	ticks := func() int { mu.Lock(); defer mu.Unlock(); return debugs }
	if fails := rt.Start(context.Background()); len(fails) != 0 {
		t.Fatalf("failures: %v", fails)
	}
	eventually(t, "three ticks", func() bool { return ticks() >= 3 })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt.Stop(ctx)
	after := ticks()
	time.Sleep(150 * time.Millisecond)
	if ticks() != after {
		t.Fatalf("the interval kept ticking after the flow stopped: %d then %d", after, ticks())
	}
}

// The captured flows themselves. They hold the plant's addresses and logins,
// so they are not in the repository: point HOTLOOP_FLOW_SBF_FLOWS at the file.
// Every address is swapped for loopback before the flows start, so nothing here
// reaches a plant device.
//
//	HOTLOOP_FLOW_SBF_FLOWS=/path/merged-flows.json go test ./internal/runtime/ -run CapturedSBF -v
func TestTheCapturedSBFFlowsStartWithNoUnknownNodeTypesBesidesMongoDB(t *testing.T) {
	path := os.Getenv("HOTLOOP_FLOW_SBF_FLOWS")
	if path == "" {
		t.Skip("HOTLOOP_FLOW_SBF_FLOWS is not set")
	}
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	next := 41000
	doc = rewriteJSON(t, doc, func(n map[string]any) {
		switch n["type"] {
		case "modbus-client":
			if n["clienttype"] == "tcp" {
				n["tcpHost"], n["tcpPort"] = "127.0.0.1", "1"
			}
		case "modbus-server":
			next++
			n["hostname"], n["serverPort"] = "127.0.0.1", fmt.Sprint(next)
		case "postgreSQLConfig":
			n["host"], n["port"], n["hostFieldType"] = "127.0.0.1", 1, "str"
		case "e-mail":
			n["server"], n["port"] = "127.0.0.1", "1"
		}
	})
	flows := loadFlows(t, doc)
	rt := runtime.New(node.Default, flows, runtime.Options{})
	go func() {
		for range rt.Events() {
		}
	}()
	start := time.Now()
	fails := rt.Start(context.Background())
	t.Logf("started %d nodes in %s, %d failures", len(rt.RunningIDs()), time.Since(start).Round(time.Millisecond), len(fails))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		rt.Stop(ctx)
	})
	byKind := map[string]int{}
	byMongo := 0
	for _, f := range fails {
		msg := f.Error()
		switch {
		case strings.Contains(msg, "unknown node type") && (f.Type == "mongodb4" || f.Type == "mongodb4-client"):
			// Flow has no MongoDB node. The captured flows' MongoDB nodes are the one
			// known gap, and they fail by themselves with their type named.
			byMongo++
		case strings.Contains(msg, "unknown node type"):
			t.Errorf("unknown node type: %s %s", f.Type, f.NodeID)
		case strings.Contains(msg, "setInterval is not defined"):
			t.Errorf("setInterval is still not defined: %s", f.NodeID)
		default:
			byKind[f.Type+": "+msg]++
		}
	}
	for k, n := range byKind {
		t.Errorf("%d x %s", n, k)
	}
	t.Logf("%d MongoDB nodes did not start (Flow has no MongoDB node)", byMongo)
	if n := len(rt.RunningIDs()); n < 13000 {
		t.Errorf("only %d nodes are running", n)
	}
}
