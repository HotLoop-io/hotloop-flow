package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Against a real HotLoop.
//
// These run against the published HotLoop IoT image and the same Modbus server
// the Modbus tests use, or they skip. The HotLoop is started with an admin
// login, and the test makes everything else it needs through HotLoop's own API:
// a Modbus device pointing at the simulator, three tags (one armed with a
// range, one read-only, one coil), a script, a recipe and an operator account
// for Flow to sign in as. Then it checks both ends: what HotLoop said, and what
// the simulated PLC actually holds afterwards.
//
//	HOTLOOP_FLOW_TEST_HOTLOOP=http://localhost:18480 \
//	HOTLOOP_FLOW_TEST_HOTLOOP_ADMIN=admin \
//	HOTLOOP_FLOW_TEST_HOTLOOP_ADMIN_PASSWORD=... \
//	HOTLOOP_FLOW_TEST_MODBUS=localhost:5020 \
//	go test ./internal/nodes/ -run Hotloop -v
//
// HOTLOOP_FLOW_TEST_HOTLOOP_MODBUS is where HotLoop reaches the simulator when
// that isn't where the test does (a HotLoop in a container with its own network).
// HotLoop has to be started with HOTLOOP_ALLOW_WRITES=true.

// The simulated PLC's registers this file owns. The Modbus tests use addresses
// 0 to 43 of the holding registers and coils 0 to 6, and the server only has
// the ones testdata/plant/modbus_server.json lists, so these are the free ones.
const (
	hlSetpointAddr = 36 // holding register, the tag flowtest/sp, armed, 0 to 95
	hlLockedAddr   = 37 // holding register, the tag flowtest/locked, not armed
	hlPumpAddr     = 7  // coil, the tag flowtest/pump
)

// hotloopLive is a provisioned HotLoop and what the tests need to reach it.
type hotloopLive struct {
	base     string
	user     string
	password string
	svc      *credServices
}

// adminCall makes one request to HotLoop as the admin, from the test's own
// client and not from the code under test.
func adminCall(t *testing.T, base, token, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func newHotloopLive(t *testing.T) *hotloopLive {
	t.Helper()
	base := strings.TrimRight(requireEnv(t, "HOTLOOP_FLOW_TEST_HOTLOOP"), "/")
	admin := requireEnv(t, "HOTLOOP_FLOW_TEST_HOTLOOP_ADMIN")
	adminPass := requireEnv(t, "HOTLOOP_FLOW_TEST_HOTLOOP_ADMIN_PASSWORD")
	plc := requireEnv(t, "HOTLOOP_FLOW_TEST_MODBUS")
	if v := os.Getenv("HOTLOOP_FLOW_TEST_HOTLOOP_MODBUS"); v != "" {
		plc = v
	}

	st, b := adminCall(t, base, "", "POST", "/api/auth/login", map[string]string{"username": admin, "password": adminPass})
	if st != 200 {
		t.Fatalf("the admin login was refused: HTTP %d: %s", st, b)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &login); err != nil || login.Token == "" {
		t.Fatalf("the admin login answered with no token")
	}
	tok := login.Token
	must := func(method, path string, body any) {
		t.Helper()
		if st, b := adminCall(t, base, tok, method, path, body); st/100 != 2 {
			t.Fatalf("%s %s: HTTP %d: %s", method, path, st, b)
		}
	}

	must("POST", "/api/devices", map[string]any{"id": "flowtest", "name": "Flow test PLC", "protocol": "modbus",
		"address": plc, "enabled": true, "pollIntervalMs": 250, "config": map[string]any{}})
	must("POST", "/api/tags", map[string]any{"deviceId": "flowtest", "name": "sp", "address": fmt.Sprintf("holding:%d", hlSetpointAddr),
		"dataType": "int", "enabled": true, "writable": true, "hasRange": true, "minValue": 0, "maxValue": 95})
	must("POST", "/api/tags", map[string]any{"deviceId": "flowtest", "name": "locked", "address": fmt.Sprintf("holding:%d", hlLockedAddr),
		"dataType": "int", "enabled": true, "writable": false})
	must("POST", "/api/tags", map[string]any{"deviceId": "flowtest", "name": "pump", "address": fmt.Sprintf("coil:%d", hlPumpAddr),
		"dataType": "bool", "enabled": true, "writable": true})
	// The script and the recipe are made once and left alone after that, so a
	// second run against the same HotLoop doesn't pile up recipe versions.
	mustOrExists := func(path string, body any) {
		t.Helper()
		if st, b := adminCall(t, base, tok, "POST", path, body); st/100 != 2 && st != 409 {
			t.Fatalf("POST %s: HTTP %d: %s", path, st, b)
		}
	}
	mustOrExists("/api/scripts", map[string]any{"id": "flowtest_set_level", "name": "Flow test set level", "mode": "skip",
		"fields": []any{map[string]any{"name": "level", "type": "number", "min": 0, "max": 95, "required": true}},
		"sequence": []any{map[string]any{"type": "call_service", "params": map[string]any{
			"entityId": "number.flowtest_sp", "service": "number.set_value",
			"dataExpressions": map[string]any{"value": "vars.level"}}}}})
	mustOrExists("/api/recipes", map[string]any{"id": "flowtest_shift", "name": "Flow test shift", "verify_sec": 5,
		"params": []any{map[string]any{"name": "level", "type": "number", "min": 0, "max": 95, "required": true}},
		"targets": []any{
			map[string]any{"entity_id": "number.flowtest_sp", "service": "set_value", "data_expressions": map[string]any{"value": "vars.level"}},
			map[string]any{"entity_id": "switch.flowtest_pump", "service": "turn_on"},
		}})

	// The account Flow signs in as: an operator, which is what a plant would give it.
	flowUser, flowPass := "flowtest", "flowtest-"+fmt.Sprint(time.Now().UnixNano())
	if st, _ := adminCall(t, base, tok, "POST", "/api/users", map[string]string{"username": flowUser, "password": flowPass, "role": "operator"}); st == 409 {
		must("POST", "/api/users/"+flowUser+"/password", map[string]string{"password": flowPass})
	} else if st/100 != 2 {
		t.Fatalf("creating the operator account: HTTP %d", st)
	}

	live := &hotloopLive{base: base, user: flowUser, password: flowPass}
	svc, done := modbusLive(t, requireEnv(t, "HOTLOOP_FLOW_TEST_MODBUS"), "")
	t.Cleanup(done)
	live.svc = svc
	live.svc.creds["password"] = flowPass
	cfg := build(t, "hotloop-flow-hotloop", fmt.Sprintf(`{"url":%q,"username":%q,"timeout":20}`, base, flowUser), svc)
	svc.configs["hl"] = cfg

	// The entities appear when the tags are saved, and a value only shows once
	// HotLoop has polled the simulator.
	live.waitState(t, "number.flowtest_sp", func(s string) bool { return s != "" && s != "unknown" && s != "unavailable" })
	return live
}

// state reads an entity's state straight from HotLoop, as the test and not as a node.
func (l *hotloopLive) state(t *testing.T, id string) string {
	t.Helper()
	tok := l.token(t)
	st, b := adminCall(t, l.base, tok, "GET", "/api/states/"+url.PathEscape(id), nil)
	if st != 200 {
		return ""
	}
	var s struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(b, &s)
	return s.State
}

func (l *hotloopLive) token(t *testing.T) string {
	t.Helper()
	st, b := adminCall(t, l.base, "", "POST", "/api/auth/login", map[string]string{"username": l.user, "password": l.password})
	if st != 200 {
		t.Fatalf("the Flow account could not sign in: HTTP %d", st)
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(b, &out)
	return out.Token
}

func (l *hotloopLive) waitState(t *testing.T, id string, ok func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if s := l.state(t, id); ok(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached the state the test waited for (last %q)", id, l.state(t, id))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// plc reads a register or coil from the simulator itself with Flow's own Modbus
// read node, which is the answer that doesn't depend on anything HotLoop says.
func (l *hotloopLive) plc(t *testing.T, area string, addr int) any {
	t.Helper()
	got, _, err := readOnce(t, l.svc, fmt.Sprintf(`"area":%q,"address":"%d","dataType":"uint16"`, area, addr))
	if err != nil {
		t.Fatalf("reading the simulator: %v", err)
	}
	return got
}

// gatedWrite builds a Modbus write node that goes through HotLoop and sends it one value.
func (l *hotloopLive) gatedWrite(t *testing.T, tag string, value any, extra string) (*testEmitter, error) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"payload": value})
	n := build(t, "modbus write", `{"server":"srv","area":"holding","address":"0","dataType":"uint16",
		"value":"payload","valueType":"msg","hotloop":"hl","hotloopTag":"`+tag+`"`+extra+`}`, l.svc)
	return send(t, n, msg(t, string(body)))
}

func TestHotloopGatedWriteGoesThroughTheRealGate(t *testing.T) {
	l := newHotloopLive(t)

	// A value inside the range goes through, and the PLC holds it.
	e, err := l.gatedWrite(t, "flowtest/sp", 42, "")
	if err != nil {
		t.Fatalf("a write inside the range was refused: %v", err)
	}
	if len(e.on(0)) != 1 {
		t.Fatalf("a write that went through should pass the message on, got %d", len(e.on(0)))
	}
	info, _ := e.on(0)[0].Data["hotloop"].(map[string]any)
	if info["tag"] != "flowtest/sp" || info["reason"] != "flow:test-node" {
		t.Errorf("msg.hotloop = %#v", info)
	}
	if got := l.plc(t, "holding", hlSetpointAddr); got != 42.0 {
		t.Errorf("the PLC holds %v after a write of 42 through the gate", got)
	}

	// Outside the range: HotLoop's gate says no, in its own words, and the PLC does not move.
	_, err = l.gatedWrite(t, "flowtest/sp", 120, "")
	if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "120 is outside 0..95") {
		t.Fatalf("a write outside the range should be refused with the gate's reason, got %v", err)
	}
	t.Logf("refused: %v", err)
	if got := l.plc(t, "holding", hlSetpointAddr); got != 42.0 {
		t.Errorf("the PLC holds %v after a refused write, want 42", got)
	}

	// A tag that is not armed.
	before := l.plc(t, "holding", hlLockedAddr)
	_, err = l.gatedWrite(t, "flowtest/locked", 9, "")
	if err == nil || !strings.Contains(err.Error(), "not marked writable") {
		t.Fatalf("a write to a tag that isn't armed should be refused, got %v", err)
	}
	t.Logf("refused: %v", err)
	if got := l.plc(t, "holding", hlLockedAddr); got != before {
		t.Errorf("the PLC's locked register moved from %v to %v", before, got)
	}

	// A tag HotLoop doesn't have.
	_, err = l.gatedWrite(t, "flowtest/nope", 1, "")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("a write to an unknown tag should say so, got %v", err)
	}

	// A bool goes as a bool, to a coil.
	if _, err := l.gatedWrite(t, "flowtest/pump", true, ""); err != nil {
		t.Fatalf("writing the pump coil through the gate: %v", err)
	}
	if got := l.plc(t, "coil", hlPumpAddr); got != true {
		t.Errorf("the pump coil is %v after a write of true", got)
	}

	// HotLoop's own record: every attempt is in the write audit, refused ones too,
	// under the account Flow signed in as, and the reason carries the node id.
	tok := l.token(t)
	st, b := adminCall(t, l.base, tok, "GET", "/api/writes?tag=flowtest/sp&limit=10", nil)
	if st != 200 {
		t.Fatalf("GET /api/writes: HTTP %d", st)
	}
	var audit []struct {
		Value     string `json:"value"`
		Actor     string `json:"actor"`
		Succeeded bool   `json:"succeeded"`
		Error     string `json:"error"`
	}
	_ = json.Unmarshal(b, &audit)
	var sawOK, sawRefused bool
	for _, a := range audit {
		if a.Actor != l.user {
			continue
		}
		if a.Value == "42" && a.Succeeded {
			sawOK = true
		}
		if a.Value == "120" && !a.Succeeded && strings.Contains(a.Error, "outside") {
			sawRefused = true
		}
	}
	if !sawOK || !sawRefused {
		t.Errorf("the write audit should hold the written 42 and the refused 120 as %s: %s", l.user, b)
	}
	st, b = adminCall(t, l.base, tok, "GET", "/api/logbook?kinds=event&limit=200", nil)
	if st != 200 || !strings.Contains(string(b), "reason: flow:test-node") {
		t.Errorf("the logbook should carry the reason flow:test-node, HTTP %d", st)
	}
}

func TestHotloopGatedWriteNeverFallsBackToTheEquipment(t *testing.T) {
	l := newHotloopLive(t)
	// A HotLoop that isn't there: the write must fail, and the PLC must not move,
	// because a fallback would be a write the gate never saw.
	if _, err := l.gatedWrite(t, "flowtest/sp", 10, ""); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	dead := build(t, "hotloop-flow-hotloop", `{"url":"http://127.0.0.1:1","timeout":2}`, l.svc)
	l.svc.configs["dead"] = dead
	n := build(t, "modbus write", `{"server":"srv","area":"holding","address":"`+fmt.Sprint(hlSetpointAddr)+`","dataType":"uint16",
		"value":"payload","valueType":"msg","hotloop":"dead","hotloopTag":"flowtest/sp"}`, l.svc)
	_, err := send(t, n, msg(t, `{"payload":77}`))
	if err == nil || !strings.Contains(err.Error(), "could not reach HotLoop") {
		t.Fatalf("a write with HotLoop unreachable should fail, got %v", err)
	}
	t.Logf("unreachable: %v", err)
	if got := l.plc(t, "holding", hlSetpointAddr); got != 10.0 {
		t.Errorf("the PLC holds %v after a gated write that could not reach the gate, want 10", got)
	}

	// With no HotLoop selected the same node writes straight to the PLC, as it always did.
	direct := build(t, "modbus write", `{"server":"srv","area":"holding","address":"`+fmt.Sprint(hlSetpointAddr)+`","dataType":"uint16",
		"value":"payload","valueType":"msg"}`, l.svc)
	if _, err := send(t, direct, msg(t, `{"payload":11}`)); err != nil {
		t.Fatalf("a node with no HotLoop selected writes straight to the equipment: %v", err)
	}
	if got := l.plc(t, "holding", hlSetpointAddr); got != 11.0 {
		t.Errorf("the direct write left %v in the PLC, want 11", got)
	}
}

func TestHotloopStateScriptAndRecipeAgainstTheRealHotloop(t *testing.T) {
	l := newHotloopLive(t)

	// Script: HotLoop runs it, the write goes through its gate, the PLC holds it.
	script := build(t, "hotloop script", `{"server":"hl","script":"flowtest_set_level","fields":"{\"level\":50}","fieldsType":"json","wait":true,"reason":"shift start"}`, l.svc)
	e, err := send(t, script, msg(t, `{}`))
	if err != nil {
		t.Fatalf("running the script: %v", err)
	}
	run, _ := e.on(0)[0].Payload().(map[string]any)
	if run["status"] != "succeeded" || run["actor"] != l.user {
		t.Errorf("the run = %#v", run)
	}
	if got := l.plc(t, "holding", hlSetpointAddr); got != 50.0 {
		t.Errorf("the PLC holds %v after the script set 50", got)
	}

	// A field out of range is HotLoop's refusal, and nothing runs.
	bad := build(t, "hotloop script", `{"server":"hl","script":"flowtest_set_level","fields":"{\"level\":500}","fieldsType":"json"}`, l.svc)
	if _, err := send(t, bad, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "above its maximum 95") {
		t.Fatalf("a field out of range should be refused with HotLoop's words, got %v", err)
	} else {
		t.Logf("refused: %v", err)
	}
	if got := l.plc(t, "holding", hlSetpointAddr); got != 50.0 {
		t.Errorf("the PLC moved to %v on a refused run", got)
	}

	// A script that doesn't exist.
	none := build(t, "hotloop script", `{"server":"hl","script":"no_such_script"}`, l.svc)
	if _, err := send(t, none, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("an unknown script should be a 404, got %v", err)
	}

	// Recipe: both targets written and read back.
	recipe := build(t, "hotloop recipe", `{"server":"hl","recipe":"flowtest_shift","params":"{\"level\":60}","paramsType":"json"}`, l.svc)
	e, err = send(t, recipe, msg(t, `{}`))
	if err != nil {
		t.Fatalf("applying the recipe: %v", err)
	}
	app, _ := e.on(0)[0].Payload().(map[string]any)
	if app["status"] != "applied" || app["actor"] != l.user || app["reason"] != "flow:test-node" {
		t.Errorf("the application = %#v", app)
	}
	if got := l.plc(t, "holding", hlSetpointAddr); got != 60.0 {
		t.Errorf("the PLC holds %v after the recipe set 60", got)
	}
	// A parameter out of range refuses the whole recipe: nothing is written.
	badRecipe := build(t, "hotloop recipe", `{"server":"hl","recipe":"flowtest_shift","params":"{\"level\":600}","paramsType":"json"}`, l.svc)
	if _, err := send(t, badRecipe, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "above its maximum 95") {
		t.Fatalf("a recipe parameter out of range should be refused, got %v", err)
	}
	if got := l.plc(t, "holding", hlSetpointAddr); got != 60.0 {
		t.Errorf("the PLC moved to %v on a refused recipe", got)
	}

	// State: what HotLoop says the entity is now, as text and as a number.
	l.waitState(t, "number.flowtest_sp", func(s string) bool { return s == "60" })
	st := build(t, "hotloop state", `{"server":"hl","entity":"number.flowtest_sp"}`, l.svc)
	e, err = send(t, st, msg(t, `{}`))
	if err != nil {
		t.Fatalf("reading the state: %v", err)
	}
	m := e.on(0)[0]
	if m.Payload() != "60" || m.Topic() != "number.flowtest_sp" {
		t.Errorf("state msg = %v on %q", m.Payload(), m.Topic())
	}
	num := build(t, "hotloop state", `{"server":"hl","entity":"number.flowtest_sp","numeric":true}`, l.svc)
	e, _ = send(t, num, msg(t, `{}`))
	if got := e.on(0)[0].Payload(); got != 60.0 {
		t.Errorf("numeric state = %#v", got)
	}
	missing := build(t, "hotloop state", `{"server":"hl","entity":"number.nope"}`, l.svc)
	if _, err := send(t, missing, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("an unknown entity should say so, got %v", err)
	}
}

func TestHotloopSignsInAgainWhenTheSessionIsGone(t *testing.T) {
	l := newHotloopLive(t)
	cfg := l.svc.configs["hl"].(*hotloopConn)
	st := build(t, "hotloop state", `{"server":"hl","entity":"number.flowtest_sp"}`, l.svc)
	if _, err := send(t, st, msg(t, `{}`)); err != nil {
		t.Fatalf("first read: %v", err)
	}
	cfg.mu.Lock()
	cfg.token = "a-token-hotloop-never-issued"
	cfg.mu.Unlock()
	if _, err := send(t, st, msg(t, `{}`)); err != nil {
		t.Fatalf("a refused token should be replaced by signing in again: %v", err)
	}

	// And a wrong password is HotLoop's refusal, said as such.
	l.svc.creds["password"] = "not-the-password"
	bad := build(t, "hotloop-flow-hotloop", fmt.Sprintf(`{"url":%q,"username":%q}`, l.base, l.user), l.svc)
	l.svc.configs["bad"] = bad
	n := build(t, "hotloop state", `{"server":"bad","entity":"number.flowtest_sp"}`, l.svc)
	if _, err := send(t, n, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "refused the login") {
		t.Fatalf("a wrong password should be reported as a refused login, got %v", err)
	}
}

func TestHotloopInSendsEachChangeFromTheRealFeed(t *testing.T) {
	l := newHotloopLive(t)
	// Start from a known value, before the node is listening.
	if _, err := l.gatedWrite(t, "flowtest/sp", 20, ""); err != nil {
		t.Fatal(err)
	}
	l.waitState(t, "number.flowtest_sp", func(s string) bool { return s == "20" })

	n := build(t, "hotloop in", `{"server":"hl","entities":[{"entity":"number.flowtest_*"}],"numeric":true}`, l.svc)
	e := newTestEmitter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := n.(node.Starter).Start(ctx, e); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.(node.Closer).Close(context.Background(), false) }()

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitFor("the node to connect", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, s := range e.statuses {
			if s.Text == "connected" {
				return true
			}
		}
		return false
	})
	time.Sleep(1200 * time.Millisecond)
	if got := e.total(); got != 0 {
		t.Fatalf("with Send the current state off, connecting sent %d messages", got)
	}

	if _, err := l.gatedWrite(t, "flowtest/sp", 31, ""); err != nil {
		t.Fatal(err)
	}
	waitFor("the change to 31", func() bool {
		for _, m := range e.on(0) {
			if m.Payload() == 31.0 {
				return true
			}
		}
		return false
	})
	var got map[string]any
	for _, m := range e.on(0) {
		if m.Payload() == 31.0 {
			if m.Topic() != "number.flowtest_sp" {
				t.Errorf("topic = %q", m.Topic())
			}
			got, _ = m.Data["hotloop"].(map[string]any)
		}
	}
	if got["entity_id"] != "number.flowtest_sp" || got["previous"] != "20" || got["quality"] != "good" {
		t.Errorf("msg.hotloop = %#v", got)
	}
	for _, m := range e.on(0) {
		if !strings.HasPrefix(m.Topic(), "number.flowtest_") {
			t.Errorf("the pattern number.flowtest_* let %q through", m.Topic())
		}
	}
	if len(e.errs) > 0 {
		t.Errorf("errors raised: %v", e.errs)
	}
}
