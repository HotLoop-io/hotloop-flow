package nodes

import (
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

func TestTheInterfaceAClientUsesComesFromItsOwnSettingThenTheTabThenTheConfig(t *testing.T) {
	svc := newTestServices()
	if got := networkInterfaceFor(svc, ""); got != "" {
		t.Fatalf("nothing set: %q", got)
	}
	svc.env[NetworkInterfaceEnv] = "enp2s0.14"
	if got := networkInterfaceFor(svc, ""); got != "enp2s0.14" {
		t.Fatalf("the tab's variable: %q", got)
	}
	if got := networkInterfaceFor(svc, " enp2s0.15 "); got != "enp2s0.15" {
		t.Fatalf("the node's own setting should win over the tab: %q", got)
	}
}

func TestANodeRefusesAnInterfaceNameLinuxCannotHave(t *testing.T) {
	svc := newConfigServices()
	bad := `"networkInterface":"a-name-that-is-too-long"`
	for typ, cfg := range map[string]string{
		"modbus-server": `{"hostname":"127.0.0.1","serverPort":"10502",` + bad + `}`,
		"modbus-getter": `{"server":"cl","dataType":"Coil","adr":"0","quantity":"1",` + bad + `}`,
		"postgresql":    `{"postgreSQLConfig":"pg","query":"SELECT 1",` + bad + `}`,
		"e-mail":        `{"server":"smtp.example.com","port":"25","authtype":"NONE","name":"a@example.com",` + bad + `}`,
		"tcp request":   `{"host":"10.0.0.1","port":502,"out":"time",` + bad + `}`,
		"udp out":       `{"addr":"10.0.0.1","port":9,` + bad + `}`,
		"http request":  `{"method":"GET","url":"http://10.0.0.1/",` + bad + `}`,
	} {
		err := buildErr(t, typ, cfg, svc)
		if err == nil || !strings.Contains(err.Error(), "characters") {
			t.Errorf("%s: %v", typ, err)
		}
	}
	for typ, cfg := range map[string]string{
		"modbus-client":       `{"clienttype":"tcp","tcpHost":"10.0.0.1","tcpPort":"502",` + bad + `}`,
		"postgreSQLConfig":    `{"host":"h","database":"d","user":"u",` + bad + `}`,
		"hotloop-flow-modbus": `{"host":"10.0.0.1",` + bad + `}`,
	} {
		reg := mustLookup(t, typ)
		_, err := reg.New(newDef(t, typ, cfg, svc))
		if err == nil || !strings.Contains(err.Error(), "characters") {
			t.Errorf("%s: %v", typ, err)
		}
	}
}

// An interface that is not there is said plainly, with the interface's name, and
// the connection is not made some other way.
func TestAReadOnAnInterfaceThatIsNotThereNamesIt(t *testing.T) {
	_, port := startDevice(t)
	svc := newConfigServices()
	nrClient(t, svc, port, "")
	n := build(t, "modbus-getter", `{"server":"cl","dataType":"HoldingRegister","adr":"0","quantity":"1","networkInterface":"flxnope0"}`, svc)
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	em := e.on(1)
	if len(em) != 1 {
		t.Fatalf("a read bound to an interface that does not exist went through: %v", e.on(0))
	}
	text := em[0].Data["error"].(map[string]any)["message"].(string)
	if !strings.Contains(text, "flxnope0") {
		t.Fatalf("the error does not name the interface: %s", text)
	}
}

func TestTheTabsInterfaceReachesANodeThroughItsServices(t *testing.T) {
	_, port := startDevice(t)
	svc := newConfigServices()
	svc.env[NetworkInterfaceEnv] = "flxnope1"
	nrClient(t, svc, port, "")
	n := build(t, "modbus-getter", `{"server":"cl","dataType":"HoldingRegister","adr":"0","quantity":"1"}`, svc)
	e, _ := send(t, n, msg(t, `{}`))
	if len(e.on(1)) != 1 || !strings.Contains(e.on(1)[0].Data["error"].(map[string]any)["message"].(string), "flxnope1") {
		t.Fatalf("the tab's interface was not used: %v", e.on(1))
	}
}

func mustLookup(t *testing.T, typ string) node.Registration {
	t.Helper()
	reg, ok := node.Default.Lookup(typ)
	if !ok {
		t.Fatalf("%s is not registered", typ)
	}
	return reg
}

func newDef(t *testing.T, typ, cfg string, svc node.Services) *node.Definition {
	t.Helper()
	raw := parseNodeConfig(t, typ, cfg).Raw
	for _, k := range []string{"x", "y", "wires", "z"} {
		delete(raw, k)
	}
	raw["id"] = "cfg"
	return &node.Definition{Node: &engine.Node{ID: "cfg", Type: typ, IsConfig: true, Raw: raw}, Services: svc}
}
