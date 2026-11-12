package runtime_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// One Flow, two tabs, two VLANs that carry the same subnet.
//
// testdata/vlan/lab.sh builds, from network namespaces, what the Shepherd Boy
// Farms box has on its port: a Modbus device at 192.168.1.5:5020 on each of two
// interfaces (flxv14 and flxv15), the same address on both, answering with
// different register values. A connection that is not bound to an interface
// reaches whichever the kernel likes. These tests bind each tab to its own and
// read each device's own values, many times over, then take a link down and up.
//
//	sudo testdata/vlan/lab.sh up 14 15
//	HOTLOOP_FLOW_TEST_VLAN_LAB=$PWD/testdata/vlan/lab.sh go test ./internal/runtime/ -run VLAN -v
//	sudo testdata/vlan/lab.sh down
func vlanLab(t *testing.T) string {
	t.Helper()
	lab := os.Getenv("HOTLOOP_FLOW_TEST_VLAN_LAB")
	if lab == "" {
		t.Skip("HOTLOOP_FLOW_TEST_VLAN_LAB is not set")
	}
	return lab
}

func flap(t *testing.T, lab, vlan, action string) {
	t.Helper()
	out, err := exec.Command(lab, "flap", vlan, action).CombinedOutput()
	if err != nil {
		t.Fatalf("lab.sh flap %s %s: %v %s", vlan, action, err, out)
	}
}

type debugLog struct {
	mu   sync.Mutex
	byID map[string][]string
}

func (d *debugLog) take(id string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.byID[id]
	d.byID[id] = nil
	return out
}

func (d *debugLog) wait(t *testing.T, id string, n int, within time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		got := len(d.byID[id])
		d.mu.Unlock()
		if got >= n {
			return d.take(id)
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	t.Fatalf("waited %s for %d debug message(s) on %s, got %v", within, n, id, d.byID[id])
	return nil
}

// vlanFlows is one flow file: a Modbus client shared by every tab, and a tab for
// each dryer that sets its own network interface once, in the tab's environment.
func vlanFlows(addr string) string {
	host, port, _ := strings.Cut(addr, ":")
	tab := func(id, label, iface string) string {
		env := ""
		if iface != "" {
			env = fmt.Sprintf(`,"env":[{"name":"FLOW_NETWORK_INTERFACE","value":%q,"type":"str"}]`, iface)
		}
		return fmt.Sprintf(`{"id":%q,"type":"tab","label":%q%s}`, id, label, env)
	}
	reader := func(id, z string, extra string) string {
		return fmt.Sprintf(`{"id":"in-%[1]s","type":"inject","z":%[2]q,"name":"read","props":[{"p":"payload","v":"go","vt":"str"}],"repeat":"","crontab":"","once":false,"x":1,"y":1,"wires":[["get-%[1]s"]]},
		{"id":"get-%[1]s","type":"modbus-getter","z":%[2]q,"name":"","server":"mb","dataType":"HoldingRegister","adr":"0","quantity":"1","unitid":""%[3]s,"showErrors":false,"x":2,"y":1,"wires":[["ok-%[1]s"],["err-%[1]s"]]},
		{"id":"ok-%[1]s","type":"debug","z":%[2]q,"name":"","active":true,"tosidebar":true,"complete":"payload","x":3,"y":1,"wires":[]},
		{"id":"err-%[1]s","type":"debug","z":%[2]q,"name":"","active":true,"tosidebar":true,"complete":"error.message","x":3,"y":2,"wires":[]}`, id, z, extra)
	}
	return "[" + strings.Join([]string{
		tab("t14", "dryer 14", "flxv14"),
		tab("t15", "dryer 15", "flxv15"),
		`{"id":"mb","type":"modbus-client","name":"dryer devices","clienttype":"tcp","tcpHost":"` + host + `","tcpPort":"` + port + `","tcpType":"DEFAULT","unit_id":1,"clientTimeout":1000}`,
		reader("a", "t14", ""),
		reader("b", "t15", ""),
		// A node on dryer 15's tab with its own interface: the node wins over the tab.
		reader("c", "t15", `,"networkInterface":"flxv14"`),
	}, ",\n") + "]"
}

func startVLANFlows(t *testing.T, doc string) (*runtime.Runtime, *debugLog) {
	t.Helper()
	rt := runtime.New(node.Default, loadFlows(t, []byte(doc)), runtime.Options{})
	log := &debugLog{byID: map[string][]string{}}
	go func() {
		for e := range rt.Events() {
			if e.Topic == runtime.TopicDebug {
				id, _ := e.Data["id"].(string)
				msg, _ := e.Data["msg"].(string)
				log.mu.Lock()
				log.byID[id] = append(log.byID[id], msg)
				log.mu.Unlock()
			}
		}
	}()
	if fails := rt.Start(context.Background()); len(fails) != 0 {
		t.Fatalf("failures: %v", fails)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		rt.Stop(ctx)
	})
	return rt, log
}

func press(t *testing.T, rt *runtime.Runtime, id string) {
	t.Helper()
	if err := rt.Inject(id, engine.NewMsgWithPayload("go")); err != nil {
		t.Fatal(err)
	}
}

func TestEachTabReadsItsOwnVLANWithTheSameAddressOnBoth(t *testing.T) {
	vlanLab(t)
	addr := envOr("HOTLOOP_FLOW_TEST_VLAN_ADDR", "192.168.1.5:5020")
	rt, log := startVLANFlows(t, vlanFlows(addr))

	// 14 holds 1114 and 15 holds 1115. Alternate between the tabs many times:
	// every read has to be its own VLAN's, with no crosstalk.
	for i := 0; i < 30; i++ {
		press(t, rt, "in-a")
		if got := log.wait(t, "ok-a", 1, 5*time.Second); got[0] != "[1114]" {
			t.Fatalf("read %d, dryer 14's tab read %v, want [1114]", i, got)
		}
		press(t, rt, "in-b")
		if got := log.wait(t, "ok-b", 1, 5*time.Second); got[0] != "[1115]" {
			t.Fatalf("read %d, dryer 15's tab read %v, want [1115]", i, got)
		}
		// The node with its own interface on dryer 15's tab reads dryer 14.
		press(t, rt, "in-c")
		if got := log.wait(t, "ok-c", 1, 5*time.Second); got[0] != "[1114]" {
			t.Fatalf("read %d, the node bound to flxv14 read %v, want [1114]", i, got)
		}
	}
}

func TestTheSameReadsWithNoInterfaceReachOnlyOneOfThem(t *testing.T) {
	vlanLab(t)
	addr := envOr("HOTLOOP_FLOW_TEST_VLAN_ADDR", "192.168.1.5:5020")
	// The control: the same flow with no interface set anywhere. The host has the
	// same subnet on two interfaces, so the kernel sends every connection down
	// one of them, and the tab that wanted the other reads the wrong device.
	doc := strings.ReplaceAll(vlanFlows(addr), `,"env":[{"name":"FLOW_NETWORK_INTERFACE","value":"flxv14","type":"str"}]`, "")
	doc = strings.ReplaceAll(doc, `,"env":[{"name":"FLOW_NETWORK_INTERFACE","value":"flxv15","type":"str"}]`, "")
	doc = strings.ReplaceAll(doc, `,"networkInterface":"flxv14"`, "")
	rt, log := startVLANFlows(t, doc)
	press(t, rt, "in-a")
	a := log.wait(t, "ok-a", 1, 5*time.Second)[0]
	press(t, rt, "in-b")
	b := log.wait(t, "ok-b", 1, 5*time.Second)[0]
	t.Logf("unbound: dryer 14's tab read %s, dryer 15's tab read %s", a, b)
	if a != b {
		t.Fatalf("with no interface set the two tabs read different devices (%s and %s): the lab does not share one subnet, so it proves nothing", a, b)
	}
}

func TestALinkThatGoesDownAndComesBackIsReadAgain(t *testing.T) {
	lab := vlanLab(t)
	addr := envOr("HOTLOOP_FLOW_TEST_VLAN_ADDR", "192.168.1.5:5020")
	rt, log := startVLANFlows(t, vlanFlows(addr))

	press(t, rt, "in-a")
	log.wait(t, "ok-a", 1, 5*time.Second)
	press(t, rt, "in-b")
	log.wait(t, "ok-b", 1, 5*time.Second)

	flap(t, lab, "14", "down")
	t.Cleanup(func() { _ = exec.Command(lab, "flap", "14", "up").Run() })

	// While 14 is down its tab fails, the error is plain, and 15 is not touched.
	press(t, rt, "in-a")
	errs := log.wait(t, "err-a", 1, 8*time.Second)
	t.Logf("dryer 14's read while its link is down: %s", errs[0])
	if !strings.Contains(errs[0], "flxv14") && !strings.Contains(errs[0], "192.168.1.5") {
		t.Errorf("the error does not say where it was: %s", errs[0])
	}
	for i := 0; i < 5; i++ {
		press(t, rt, "in-b")
		if got := log.wait(t, "ok-b", 1, 5*time.Second); got[0] != "[1115]" {
			t.Fatalf("dryer 15 read %v while 14 was down", got)
		}
	}

	flap(t, lab, "14", "up")

	// Back up, it reads its own device again, with nothing done to the flow.
	deadline := time.Now().Add(15 * time.Second)
	for {
		press(t, rt, "in-a")
		select {
		case <-time.After(1500 * time.Millisecond):
		}
		if got := log.take("ok-a"); len(got) > 0 {
			if got[0] != "[1114]" {
				t.Fatalf("after the link came back, dryer 14 read %v, want [1114]", got)
			}
			break
		}
		log.take("err-a")
		if time.Now().After(deadline) {
			t.Fatal("dryer 14 never read again after its link came back")
		}
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
