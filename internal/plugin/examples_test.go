package plugin_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/plugin"
)

// The worked examples are built in CI, one in Rust and one in TinyGo, and this
// is what proves the builds are plugins: signed with a key made for the run,
// loaded the way a server loads them, and run.
//
// HOTLOOP_PLUGIN_EXAMPLES names a directory holding <name>.wasm for each
// example. Unset, the tests skip, and CI fails the build if they did.
func exampleDir(t *testing.T, name string, signer *publisher) string {
	t.Helper()
	built := os.Getenv("HOTLOOP_PLUGIN_EXAMPLES")
	if built == "" {
		t.Skip("HOTLOOP_PLUGIN_EXAMPLES is not set; CI builds the examples and sets it")
	}
	module, err := os.ReadFile(filepath.Join(built, name+".wasm"))
	if err != nil {
		t.Fatalf("the example was not built: %v", err)
	}
	descriptor, err := os.ReadFile(filepath.Join("..", "..", "examples", "plugins", name, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	install(t, root, name, module, string(descriptor), signer)
	return root
}

type stubServices struct{}

func (stubServices) Context(node.ContextScope) node.Context { return nil }
func (stubServices) Credential(string) (string, bool)       { return "", false }
func (stubServices) ConfigNode(string) (node.Node, bool)    { return nil, false }
func (stubServices) Env(string) (string, bool)              { return "", false }
func (stubServices) Log(node.LogLevel, string, ...any)      {}

type capture struct {
	mu   sync.Mutex
	sent []*engine.Msg
}

func (c *capture) Send(_ int, m *engine.Msg) { c.mu.Lock(); c.sent = append(c.sent, m); c.mu.Unlock() }
func (c *capture) SendAll(by [][]*engine.Msg) {
	for p, ms := range by {
		for _, m := range ms {
			c.Send(p, m)
		}
	}
}
func (c *capture) Status(node.Status)                {}
func (c *capture) Error(error, *engine.Msg)          {}
func (c *capture) Done(*engine.Msg, error)           {}
func (c *capture) Publish(string, map[string]any)    {}
func (c *capture) Log(node.LogLevel, string, ...any) {}

func buildNode(t *testing.T, h *plugin.Host, typ, props string) node.Node {
	t.Helper()
	reg := node.NewRegistry()
	if err := h.Register(reg); err != nil {
		t.Fatal(err)
	}
	r, ok := reg.Lookup(typ)
	if !ok {
		t.Fatalf("%s is not registered", typ)
	}
	flows, err := engine.ParseFlows([]byte(`[{"id":"t1","type":"tab"},{"id":"n1","type":"` + typ + `","z":"t1",` + props + `"wires":[[]]}]`))
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.New(&node.Definition{Node: flows.Nodes["n1"], Services: stubServices{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := n.(node.Closer); ok {
			_ = c.Close(context.Background(), true)
		}
	})
	return n
}

func TestTheRustExampleAveragesAWindowOfReadings(t *testing.T) {
	pub := newPublisher(t)
	root := exampleDir(t, "moving-average", &pub)
	h := load(t, root, policy(pub))
	if len(h.Loaded()) != 1 {
		t.Fatalf("the Rust example did not load: %v", h.Refusals())
	}
	n := buildNode(t, h, "wasm-moving-average", `"window":3,`)

	out := &capture{}
	for _, v := range []float64{10, 20, 30, 40} {
		if err := n.Receive(context.Background(), engine.WrapMsg(map[string]any{"payload": v, "topic": "line3/temp"}), out); err != nil {
			t.Fatal(err)
		}
	}
	// 10, then (10+20)/2, then (10+20+30)/3, then the window slides to 20,30,40.
	want := []float64{10, 15, 20, 30}
	if len(out.sent) != len(want) {
		t.Fatalf("%d messages out, want %d", len(out.sent), len(want))
	}
	for i, m := range out.sent {
		if m.Payload() != want[i] {
			t.Errorf("message %d payload = %v, want %v", i, m.Payload(), want[i])
		}
		if m.Topic() != "line3/temp" {
			t.Errorf("the topic did not come through: %q", m.Topic())
		}
	}
	if s := out.sent[3].Data["samples"]; s != 3.0 {
		t.Errorf("samples = %v", s)
	}

	// A message with nothing to average is an error, not a silent zero.
	err := n.Receive(context.Background(), engine.WrapMsg(map[string]any{"payload": "hot"}), out)
	if err == nil || !strings.Contains(err.Error(), "msg.payload is not a number") {
		t.Errorf("err = %v", err)
	}
}

func TestTheTinyGoExampleNeedsTheClockAndStampsRealTime(t *testing.T) {
	pub := newPublisher(t)
	root := exampleDir(t, "stamp", &pub)

	// Not granted: it loads, because it is signed, and a flow using it does not deploy.
	h := load(t, root, policy(pub))
	if len(h.Loaded()) != 1 {
		t.Fatalf("the TinyGo example did not load: %v", h.Refusals())
	}
	flows, _ := engine.ParseFlows([]byte(`[{"id":"t1","type":"tab"},{"id":"n1","type":"wasm-stamp","z":"t1","name":"stamp","wires":[[]]}]`))
	err := h.Preflight(flows)
	if err == nil {
		t.Fatal("a plugin asking for the clock deployed with no grant")
	}
	t.Logf("deploy with no grant: %v", err)
	if !strings.Contains(err.Error(), "clock") || !strings.Contains(err.Error(), "plugins.grants.stamp") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}

	// Granted: it stamps the real time.
	p := policy(pub)
	p.Grants = map[string][]plugin.Capability{"stamp": {plugin.CapClock}}
	h = load(t, root, p)
	if err := h.Preflight(flows); err != nil {
		t.Fatalf("a granted plugin was refused: %v", err)
	}
	n := buildNode(t, h, "wasm-stamp", `"field":"at",`)
	out := &capture{}
	if err := n.Receive(context.Background(), engine.WrapMsg(map[string]any{"payload": 1.0}), out); err != nil {
		t.Fatal(err)
	}
	m := out.sent[0]
	at, _ := m.Data["at"].(string)
	stamped, perr := time.Parse(time.RFC3339Nano, at)
	if perr != nil {
		t.Fatalf("msg.at = %q: %v", at, perr)
	}
	if d := time.Since(stamped); d < -time.Second || d > 10*time.Second {
		t.Errorf("the stamp %s is %v from now", at, d)
	}
	if m.Payload() != 1.0 {
		t.Errorf("the payload did not come through: %v", m.Payload())
	}
	t.Logf("stamped %s, %v ago", at, time.Since(stamped).Round(time.Millisecond))
}
