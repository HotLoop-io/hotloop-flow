package runtime_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	_ "github.com/HotLoop-io/hotloop-flow/internal/nodes"
	rt "github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

func freeLoopPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// A plant's Modbus getters were polled by injects every second, a great many of
// them through the same connection to a PLC that answers one request at a time,
// so more was asked than the device could answer and every getter's inbox grew
// until it was full, with senders blocking and dropping behind it. Here a device
// that takes 40 ms to answer is asked 100 times a second by one getter. It can
// answer 25, so the inbox must not grow: the polls it has not got to are merged
// into the newest, which is the one worth answering.
func TestAGetterPolledFasterThanItsDeviceAnswersDoesNotStackRequests(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("holds a device for 6 s")
	}
	port := freeLoopPort(t)
	doc := fmt.Sprintf(`[
{"id":"tab","type":"tab","label":"t"},
{"id":"dev","type":"modbus-server","z":"tab","hostname":"127.0.0.1","serverPort":"%d","responseDelay":"40","delayUnit":"ms","wires":[[],[],[],[],[]]},
{"id":"cl","type":"modbus-client","name":"plc","clienttype":"tcp","tcpHost":"127.0.0.1","tcpPort":"%d","tcpType":"DEFAULT","unit_id":1,"clientTimeout":3000},
{"id":"poll","type":"inject","z":"tab","name":"poll","repeat":"0.01","once":true,"onceDelay":"1","props":[{"p":"payload","v":"1","vt":"num"}],"wires":[["get"]]},
{"id":"get","type":"modbus-getter","z":"tab","server":"cl","dataType":"HoldingRegister","adr":"0","quantity":"2","wires":[["out"],[]]},
{"id":"out","type":"debug","z":"tab","active":false,"complete":"payload","wires":[]}
]`, port, port)
	flows, err := engine.ParseFlows([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	r := rt.New(node.Default, flows, rt.Options{InboxCapacity: 1024})
	go func() {
		for range r.Events() {
		}
	}()
	if fails := r.Start(context.Background()); len(fails) > 0 {
		t.Fatalf("start: %v", fails[0])
	}
	defer r.Stop(context.Background())

	time.Sleep(6 * time.Second)
	var depth, high int
	var recv, merged int64
	for _, s := range r.Snapshots() {
		if s.NodeID == "get" {
			depth, high, recv, merged = s.QueueLen, int(s.QueueHigh), s.Received, s.Coalesced
		}
	}
	t.Logf("POLL queued=%d high=%d answered=%d merged=%d", depth, high, recv, merged)
	if recv < 20 {
		t.Fatalf("the getter answered %d polls in 6 s, the device can answer about 25 a second", recv)
	}
	if depth > 40 {
		t.Fatalf("%d polls are stacked behind the device (high water %d)", depth, high)
	}
}
