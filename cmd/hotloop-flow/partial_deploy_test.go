package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowhttp"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/nodes"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// eventHub stands where the editor's websocket hub does and keeps what the
// runtime published.
type eventHub struct{ ch chan runtime.Event }

func (h *eventHub) Broadcast(e runtime.Event) {
	select {
	case h.ch <- e:
	default:
	}
}

// next waits for the first event matching want, failing on one matching bad.
func (h *eventHub) next(t *testing.T, within time.Duration, want, bad func(runtime.Event) bool) bool {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case e := <-h.ch:
			if bad != nil && bad(e) {
				t.Fatalf("unexpected event: %s %v", e.Topic, e.Data)
			}
			if want(e) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// newTestApp builds the application the way cmdServe does, on a scratch data
// directory, and starts it on an empty flow set.
func newTestApp(t *testing.T) *application {
	t.Helper()
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	nodes.Routes = flowhttp.NewRouter(cfg.Server.HTTPRoot, nil)

	creds := store.NewCredentialStore(cfg.CredentialsPath(), "partial-deploy-test-secret")
	if err := creds.Load(); err != nil {
		t.Fatal(err)
	}
	app := &application{
		cfg:       cfg,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		flowStore: store.NewFlowStore(cfg.FlowPath()),
		creds:     creds,
		registry:  node.Default,
		contexts:  store.NewScopedContexts(),
		hub:       &eventHub{ch: make(chan runtime.Event, 4096)},
	}
	empty, err := engine.ParseFlows([]byte(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	app.start(context.Background(), empty)
	t.Cleanup(func() { app.stop(context.Background()) })
	return app
}

func deployJSON(t *testing.T, app *application, js string, mode runtime.DeployMode) runtime.UpdateResult {
	t.Helper()
	flows, err := engine.ParseFlows([]byte(js))
	if err != nil {
		t.Fatalf("parsing the test flow: %v", err)
	}
	res, err := app.deploy(context.Background(), api.DeployRequest{Flows: flows, Mode: mode})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(res.Failures) > 0 {
		t.Fatalf("deploy failures: %v", res.Failures)
	}
	return res.Update
}

// counter collects what an outside MQTT client sees on one topic.
type counter struct {
	mu   sync.Mutex
	msgs []string
}

func (c *counter) add(_ mqtt.Client, m mqtt.Message) {
	c.mu.Lock()
	c.msgs = append(c.msgs, string(m.Payload()))
	c.mu.Unlock()
}

func (c *counter) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}

func eventually(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// The claim on the roadmap: a save that touches nothing MQTT-related does not
// drop the MQTT session. Proven against a real broker, through the same deploy
// path the API calls, by counting birth messages: the broker config publishes
// one every time it connects, so a reconnect cannot hide.
//
// Messages are flowing through the flow the whole time a partial deploy lands,
// and every one of them has to come out the other side.
func TestPartialDeployKeepsTheMQTTSessionUp(t *testing.T) {
	addr := os.Getenv("HOTLOOP_FLOW_TEST_MQTT")
	if addr == "" {
		t.Skip("set HOTLOOP_FLOW_TEST_MQTT=host:port to run this against a real broker")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("HOTLOOP_FLOW_TEST_MQTT=%q: %v", addr, err)
	}

	run := engine.GenerateID()
	birthTopic := "hotloop-flow/test/" + run + "/birth"
	inTopic := "hotloop-flow/test/" + run + "/in"
	outTopic := "hotloop-flow/test/" + run + "/out"

	// The outside world: watches births and echoes, and publishes into the flow.
	births, echoes := &counter{}, &counter{}
	outside := mqtt.NewClient(mqtt.NewClientOptions().
		AddBroker("tcp://" + addr).SetClientID("hotloop-flow-test-" + run).SetOrderMatters(true))
	if tok := outside.Connect(); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("connecting the test client to %s: %v", addr, tok.Error())
	}
	defer outside.Disconnect(100)
	for topic, c := range map[string]*counter{birthTopic: births, outTopic: echoes} {
		if tok := outside.Subscribe(topic, 1, c.add); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
			t.Fatalf("subscribing to %s: %v", topic, tok.Error())
		}
	}

	flow := func(keepalive int, note string) string {
		return fmt.Sprintf(`[
        {"id":"t1","type":"tab","label":"Line 1"},
        {"id":"brk","type":"mqtt-broker","name":"plant broker","broker":%q,"port":%s,
         "clientid":"hotloop-flow-%s","cleansession":true,"keepalive":%d,
         "birthTopic":%q,"birthPayload":"up"},
        {"id":"in","type":"mqtt in","z":"t1","x":100,"y":100,"broker":"brk","topic":%q,
         "qos":"1","datatype":"utf8","wires":[["out"]]},
        {"id":"out","type":"mqtt out","z":"t1","x":300,"y":100,"broker":"brk","topic":%q,
         "qos":"1","wires":[]},
        {"id":"note","type":"comment","z":"t1","x":100,"y":200,"name":%q,"wires":[]}
    ]`, host, port, run, keepalive, birthTopic, inTopic, outTopic, note)
	}

	app := newTestApp(t)
	deployJSON(t, app, flow(30, "v1"), runtime.DeployNodes)

	eventually(t, "the broker config to connect and publish its birth", 15*time.Second, func() bool {
		return len(births.all()) == 1
	})
	ping := func(payload string) {
		if tok := outside.Publish(inTopic, 1, false, payload); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
			t.Fatalf("publishing %s: %v", payload, tok.Error())
		}
	}
	eventually(t, "the flow to echo", 15*time.Second, func() bool {
		ping("warmup")
		return slices.Contains(echoes.all(), "warmup")
	})

	// Keep traffic moving through the flow while a deploy lands that changes
	// only the comment.
	const n = 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range n {
			ping("seq-" + strconv.Itoa(i))
			time.Sleep(5 * time.Millisecond)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	up := deployJSON(t, app, flow(30, "v2"), runtime.DeployNodes)
	<-done

	if !slices.Equal(up.Restarted, []string{"note"}) || up.Unchanged != 3 {
		t.Errorf("a comment change restarted %v and left %d alone; want only the comment restarted",
			up.Restarted, up.Unchanged)
	}
	eventually(t, "every message sent during the deploy to come back", 15*time.Second, func() bool {
		got := echoes.all()
		for i := range n {
			if !slices.Contains(got, "seq-"+strconv.Itoa(i)) {
				return false
			}
		}
		return true
	})
	time.Sleep(500 * time.Millisecond)
	if b := len(births.all()); b != 1 {
		t.Fatalf("the broker connection restarted on a deploy that only changed a comment: %d births", b)
	}

	// The control. A full deploy of the same flow does reconnect, which proves
	// the birth count above would have seen it.
	deployJSON(t, app, flow(30, "v2"), runtime.DeployFull)
	eventually(t, "a full deploy to reconnect", 15*time.Second, func() bool { return len(births.all()) == 2 })

	// And a partial deploy that does change the broker restarts it and the two
	// nodes using it, and still not the comment.
	up = deployJSON(t, app, flow(31, "v2"), runtime.DeployNodes)
	slices.Sort(up.Restarted)
	if !slices.Equal(up.Restarted, []string{"brk", "in", "out"}) || up.Unchanged != 1 {
		t.Errorf("a broker change restarted %v and left %d alone; want the broker and both MQTT nodes",
			up.Restarted, up.Unchanged)
	}
	eventually(t, "the changed broker to reconnect", 15*time.Second, func() bool { return len(births.all()) == 3 })
	eventually(t, "the flow to echo on the new connection", 15*time.Second, func() bool {
		ping("after")
		return slices.Contains(echoes.all(), "after")
	})
}

// A Link In replaced by a partial deploy must be reachable from a Link Out that
// was not touched, and a Link In that was deleted must not be: the Link Out
// says the target is not running instead of delivering into a runner that has
// gone.
func TestPartialDeployKeepsLinksResolvable(t *testing.T) {
	app := newTestApp(t)
	nodes.Links.Reset()

	flow := func(inName string, withIn bool) string {
		in := ""
		if withIn {
			in = fmt.Sprintf(`,{"id":"lin","type":"link in","z":"t2","x":1,"y":1,"name":%q,"wires":[["dbg"]]}`, inName)
		}
		return `[
        {"id":"t1","type":"tab","label":"A"},
        {"id":"t2","type":"tab","label":"B"},
        {"id":"inj","type":"inject","z":"t1","x":1,"y":1,"wires":[["lout"]]},
        {"id":"lout","type":"link out","z":"t1","x":2,"y":1,"links":["lin"],"wires":[]},
        {"id":"dbg","type":"debug","z":"t2","x":2,"y":1,"active":true,"tosidebar":true,"wires":[]}` + in + `
    ]`
	}

	hub := app.hub.(*eventHub)
	isDebug := func(e runtime.Event) bool { return e.Topic == runtime.TopicDebug }

	deployJSON(t, app, flow("one", true), runtime.DeployNodes)
	rt := app.currentRuntime()
	if err := rt.Inject("inj", engine.NewMsg()); err != nil {
		t.Fatal(err)
	}
	if !hub.next(t, 5*time.Second, isDebug, nil) {
		t.Fatal("the message never crossed the link before any partial deploy")
	}

	up := deployJSON(t, app, flow("two", true), runtime.DeployNodes)
	if !slices.Equal(up.Restarted, []string{"lin"}) {
		t.Fatalf("restarted %v, want only the link in", up.Restarted)
	}
	if err := rt.Inject("inj", engine.NewMsg()); err != nil {
		t.Fatal(err)
	}
	if !hub.next(t, 5*time.Second, isDebug, nil) {
		t.Fatal("an untouched link out could not reach the link in that replaced its target")
	}

	deployJSON(t, app, flow("two", false), runtime.DeployNodes)
	if err := rt.Inject("inj", engine.NewMsg()); err != nil {
		t.Fatal(err)
	}
	linkErr := func(e runtime.Event) bool { return e.Topic == runtime.TopicError && e.Data["nodeId"] == "lout" }
	if !hub.next(t, 5*time.Second, linkErr, isDebug) {
		t.Fatal("a link out naming a deleted link in raised no error")
	}
}
