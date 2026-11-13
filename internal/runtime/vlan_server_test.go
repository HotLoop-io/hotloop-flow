package runtime_test

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// Two modbus-server nodes on one address and port, one on each VLAN.
//
// At Shepherd Boy Farms a tab for each dryer runs a modbus-server at
// 192.168.1.9:10502, as each dryer's own Node-RED box did. In one Flow they
// would collide on the one address, unless each listening socket is bound to its
// VLAN's interface first. The lab (testdata/vlan/lab.sh) gives the host
// 192.168.1.9 on both flxv14 and flxv15, and a client in each VLAN's namespace
// reads what the server on its own VLAN holds.
//
//	sudo testdata/vlan/lab.sh up 14 15
//	HOTLOOP_FLOW_TEST_VLAN_LAB=$PWD/testdata/vlan/lab.sh go test ./internal/runtime/ -run VLANServer -v
func serverFlows(iface14, iface15 string) string {
	tab := func(id, label, iface string) string {
		env := ""
		if iface != "" {
			env = fmt.Sprintf(`,"env":[{"name":"FLOW_NETWORK_INTERFACE","value":%q,"type":"str"}]`, iface)
		}
		return fmt.Sprintf(`{"id":%q,"type":"tab","label":%q%s}`, id, label, env)
	}
	// Each tab's server holds its own VLAN's number in register 0: 2114 and 2115.
	server := func(id, z string, value int) string {
		return fmt.Sprintf(`{"id":"srv-%[1]s","type":"modbus-server","z":%[2]q,"name":"","logEnabled":false,"hostname":"192.168.1.9","serverPort":"10502","responseDelay":0,"delayUnit":"ms","coilsBufferSize":"100","holdingBufferSize":"100","inputBufferSize":"100","discreteBufferSize":"100","showErrors":false,"showStatusActivities":false,"x":1,"y":1,"wires":[[],[],[],[],[]]},
		{"id":"set-%[1]s","type":"inject","z":%[2]q,"name":"set","props":[{"p":"payload","v":"{\"register\":\"holding\",\"address\":0,\"value\":%[3]d}","vt":"json"}],"repeat":"","crontab":"","once":false,"x":1,"y":2,"wires":[["srv-%[1]s"]]}`, id, z, value)
	}
	return "[" + strings.Join([]string{
		tab("t14", "dryer 14", iface14), tab("t15", "dryer 15", iface15),
		server("a", "t14", 2114), server("b", "t15", 2115),
	}, ",\n") + "]"
}

func labRead(t *testing.T, lab, vlan string) string {
	t.Helper()
	out, err := exec.Command(lab, "read", vlan, "192.168.1.9", "10502", "0", "1").CombinedOutput()
	if err != nil {
		t.Fatalf("a client on VLAN %s could not read 192.168.1.9:10502: %v %s", vlan, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestTwoModbusServersOnOneAddressAndPortEachListenOnTheirOwnVLAN(t *testing.T) {
	lab := vlanLab(t)
	rt := runtime.New(node.Default, loadFlows(t, []byte(serverFlows("flxv14", "flxv15"))), runtime.Options{})
	go func() {
		for range rt.Events() {
		}
	}()
	if fails := rt.Start(context.Background()); len(fails) != 0 {
		t.Fatalf("with an interface for each, both servers should listen. failures: %v", fails)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		rt.Stop(ctx)
	})
	for _, id := range []string{"set-a", "set-b"} {
		if err := rt.Inject(id, engine.NewMsg()); err != nil {
			t.Fatal(err)
		}
	}
	// Every client reads its own VLAN's server, over and over, with no crosstalk.
	deadline := time.Now().Add(10 * time.Second)
	for labRead(t, lab, "14") != "2114" {
		if time.Now().After(deadline) {
			t.Fatal("the server on VLAN 14 never held 2114")
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		if got := labRead(t, lab, "14"); got != "2114" {
			t.Fatalf("round %d: the client on VLAN 14 read %q, want 2114", i, got)
		}
		if got := labRead(t, lab, "15"); got != "2115" {
			t.Fatalf("round %d: the client on VLAN 15 read %q, want 2115", i, got)
		}
	}
}

// The revert proof: the same two servers with no interface set collide, as they
// do in one process today.
func TestTwoUnboundModbusServersOnOneAddressAndPortStillCollide(t *testing.T) {
	vlanLab(t)
	rt := runtime.New(node.Default, loadFlows(t, []byte(serverFlows("", ""))), runtime.Options{})
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
	if len(fails) != 1 {
		t.Fatalf("two unbound servers on one address and port should leave exactly one failing, got %d: %v", len(fails), fails)
	}
	msg := fails[0].Error()
	t.Logf("the second server: %s", msg)
	if fails[0].Type != "modbus-server" || !strings.Contains(msg, "address already in use") {
		t.Fatalf("the failure should be the second server's bind, got %s: %s", fails[0].Type, msg)
	}
	if !strings.Contains(msg, "need a network interface each") {
		t.Errorf("the error does not say what to do: %s", msg)
	}
}
