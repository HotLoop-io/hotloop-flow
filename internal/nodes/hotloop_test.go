package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// These tests stand a small HTTP server in for HotLoop, answering with the
// shapes HotLoop's docs/API.md documents, so the parts that are Flow's own (the
// login, the token, the reconnect, the diffing of the feed, what an error says)
// run in every build. What HotLoop really does is checked in
// hotloop_integration_test.go against the published image.

// fakeHotloop records what it was asked and answers from handlers.
type fakeHotloop struct {
	srv *httptest.Server

	mu        sync.Mutex
	logins    int
	validTok  string
	requests  []string
	writes    []map[string]any
	handlers  map[string]func(w http.ResponseWriter, r *http.Request)
	loginFail bool
}

const fakeUser, fakePass = "flow", "correct-horse-battery"

func newFakeHotloop(t *testing.T) *fakeHotloop {
	t.Helper()
	f := &fakeHotloop{handlers: map[string]func(http.ResponseWriter, *http.Request){}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHotloop) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if r.URL.Path == "/api/auth/login" {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.loginFail || in["username"] != fakeUser || in["password"] != fakePass {
			writeJSON(w, 401, map[string]any{"error": "invalid username or password"})
			return
		}
		f.logins++
		f.validTok = fmt.Sprintf("tok-%d", f.logins)
		writeJSON(w, 200, map[string]any{"token": f.validTok, "username": fakeUser, "role": "operator"})
		return
	}
	f.mu.Lock()
	ok := r.Header.Get("Authorization") == "Bearer "+f.validTok && f.validTok != ""
	f.mu.Unlock()
	if !ok {
		writeJSON(w, 401, map[string]any{"error": "the session is invalid or has expired"})
		return
	}
	if r.URL.Path == "/api/write" {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.writes = append(f.writes, in)
		f.mu.Unlock()
	}
	f.mu.Lock()
	h := f.handlers[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if h == nil {
		writeJSON(w, 404, map[string]any{"error": "no such thing in the fake"})
		return
	}
	h(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeHotloop) handle(route string, h func(http.ResponseWriter, *http.Request)) {
	f.mu.Lock()
	f.handlers[route] = h
	f.mu.Unlock()
}

func (f *fakeHotloop) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

func (f *fakeHotloop) writeBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.writes...)
}

func (f *fakeHotloop) services(t *testing.T) *credServices {
	t.Helper()
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	svc.creds["password"] = fakePass
	svc.configs["hl"] = build(t, "hotloop-flow-hotloop", fmt.Sprintf(`{"url":%q,"username":%q,"timeout":5}`, f.srv.URL, fakeUser), svc)
	return svc
}

func TestHotloopConnectionRefusesWhatCannotWork(t *testing.T) {
	svc := newTestServices()
	for _, c := range []struct{ cfg, want string }{
		{`{}`, "url is required"},
		{`{"url":"ftp://x"}`, "scheme must be http or https"},
		{`{"url":"http://"}`, "has no host"},
		{`{"url":"http://u:p@host"}`, "no user, query or fragment"},
		{`{"url":"http://host?x=1"}`, "no user, query or fragment"},
		{`{"url":"http://host","timeout":0}`, "more than zero"},
	} {
		err := buildErr(t, "hotloop-flow-hotloop", c.cfg, svc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.cfg, err, c.want)
		}
	}
	// No username means no login at all, which is a HotLoop that runs without one.
	if err := buildErr(t, "hotloop-flow-hotloop", `{"url":"http://host"}`, svc); err != nil {
		t.Errorf("a connection with no username should build: %v", err)
	}
}

func TestHotloopUsernameWithNoPasswordSaysSoWhenItNeedsToSignIn(t *testing.T) {
	f := newFakeHotloop(t)
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	svc.configs["hl"] = build(t, "hotloop-flow-hotloop", fmt.Sprintf(`{"url":%q,"username":"flow"}`, f.srv.URL), svc)
	n := build(t, "hotloop state", `{"server":"hl","entity":"sensor.a"}`, svc)
	if _, err := send(t, n, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "no password") {
		t.Fatalf("got %v", err)
	}
	if got := f.loginCount(); got != 0 {
		t.Errorf("a login with no password was sent to HotLoop")
	}
}

func TestHotloopNodesRefuseWhatCannotWork(t *testing.T) {
	svc := newTestServices()
	for _, c := range []struct{ typ, cfg, want string }{
		{"hotloop in", `{"entities":[{"entity":"*"}]}`, "no HotLoop selected"},
		{"hotloop in", `{"server":"hl"}`, "at least one entity"},
		{"hotloop in", `{"server":"hl","entities":[{"entity":"sensor.[a"}]}`, "not valid"},
		{"hotloop state", `{"entity":"x.y"}`, "no HotLoop selected"},
		{"hotloop state", `{"server":"hl"}`, "an entity is required"},
		{"hotloop script", `{"server":"hl"}`, "a script is required"},
		{"hotloop script", `{"script":"a"}`, "no HotLoop selected"},
		{"hotloop recipe", `{"server":"hl"}`, "a recipe is required"},
		{"hotloop recipe", `{"server":"hl","recipe":"a","version":-1}`, "negative"},
		{"modbus write", `{"server":"s","hotloop":"hl"}`, "HotLoop tag is required"},
		{"opcua write", `{"server":"s","nodeId":"ns=2;s=A","hotloop":"hl"}`, "HotLoop tag is required"},
		{"s7 write", `{"server":"s","address":"DB1.DBW0","hotloop":"hl"}`, "HotLoop tag is required"},
		{"ethernetip write", `{"server":"s","tag":"A","hotloop":"hl"}`, "HotLoop tag is required"},
	} {
		err := buildErr(t, c.typ, c.cfg, svc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: got %v, want %q", c.typ, c.cfg, err, c.want)
		}
	}
}

func TestEveryEquipmentWriteNodeOffersTheGate(t *testing.T) {
	for _, typ := range []string{"modbus write", "opcua write", "s7 write", "ethernetip write"} {
		reg, ok := node.Default.Lookup(typ)
		if !ok {
			t.Fatalf("%s is not registered", typ)
		}
		for _, name := range []string{"hotloop", "hotloopTag"} {
			if _, ok := reg.Descriptor.PropByName(name); !ok {
				t.Errorf("%s has no %s property", typ, name)
			}
		}
		if p, _ := reg.Descriptor.PropByName("hotloop"); p.Required {
			t.Errorf("%s: the HotLoop has to be optional, or every existing flow stops deploying", typ)
		}
		if !strings.Contains(reg.Descriptor.Help, "Write through HotLoop") {
			t.Errorf("%s: the help doesn't say what the gate does", typ)
		}
	}
}

func TestHotloopSignsInOnceAndReusesTheToken(t *testing.T) {
	f := newFakeHotloop(t)
	f.handle("GET /api/states/sensor.a", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"entity_id": "sensor.a", "state": "12.5", "quality": "good",
			"attributes": map[string]any{"unit_of_measurement": "C"}, "last_changed": "2026-10-21T10:00:00Z"})
	})
	svc := f.services(t)
	n := build(t, "hotloop state", `{"server":"hl","entity":"sensor.a"}`, svc)
	for i := 0; i < 3; i++ {
		e, err := send(t, n, msg(t, `{}`))
		if err != nil {
			t.Fatal(err)
		}
		m := e.on(0)[0]
		if m.Payload() != "12.5" || m.Topic() != "sensor.a" {
			t.Fatalf("state msg = %v on %q", m.Payload(), m.Topic())
		}
		info, _ := m.Data["hotloop"].(map[string]any)
		if info["quality"] != "good" || info["server"] != f.srv.URL {
			t.Errorf("msg.hotloop = %#v", info)
		}
	}
	if got := f.loginCount(); got != 1 {
		t.Errorf("three reads signed in %d times, want once", got)
	}
}

func TestHotloopSignsInAgainWhenTheTokenIsRefused(t *testing.T) {
	f := newFakeHotloop(t)
	f.handle("GET /api/states/sensor.a", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"entity_id": "sensor.a", "state": "on"})
	})
	svc := f.services(t)
	n := build(t, "hotloop state", `{"server":"hl","entity":"sensor.a"}`, svc)
	if _, err := send(t, n, msg(t, `{}`)); err != nil {
		t.Fatal(err)
	}
	// HotLoop restarts, or the session runs out: the next token it knows is a new one.
	f.mu.Lock()
	f.validTok = "a-token-from-a-session-that-ended"
	f.mu.Unlock()
	if _, err := send(t, n, msg(t, `{}`)); err != nil {
		t.Fatalf("an expired session should be replaced by signing in again: %v", err)
	}
	if got := f.loginCount(); got != 2 {
		t.Errorf("signed in %d times, want 2", got)
	}
}

func TestHotloopRefusedLoginSaysSoAndNeverRepeatsThePassword(t *testing.T) {
	f := newFakeHotloop(t)
	svc := newTestServices()
	svc.creds["password"] = "a-secret-that-must-not-leak"
	cfg := build(t, "hotloop-flow-hotloop", fmt.Sprintf(`{"url":%q,"username":%q}`, f.srv.URL, fakeUser), svc)
	svc2 := &credServices{testServices: svc, configs: map[string]node.Node{"hl": cfg}}
	n := build(t, "hotloop state", `{"server":"hl","entity":"sensor.a"}`, svc2)
	_, err := send(t, n, msg(t, `{}`))
	if err == nil || !strings.Contains(err.Error(), "refused the login") || !strings.Contains(err.Error(), "invalid username or password") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "a-secret-that-must-not-leak") {
		t.Fatalf("the error repeats the password: %v", err)
	}
}

func TestHotloopWithNoLoginExplainsAWallOfAuth(t *testing.T) {
	f := newFakeHotloop(t)
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	svc.configs["hl"] = build(t, "hotloop-flow-hotloop", fmt.Sprintf(`{"url":%q}`, f.srv.URL), svc)
	n := build(t, "hotloop state", `{"server":"hl","entity":"sensor.a"}`, svc)
	_, err := send(t, n, msg(t, `{}`))
	if err == nil || !strings.Contains(err.Error(), "wants a login") || !strings.Contains(err.Error(), "Set a username and password") {
		t.Fatalf("got %v", err)
	}
}

func TestHotloopGateSendsOneScalarAndTheReason(t *testing.T) {
	f := newFakeHotloop(t)
	f.handle("POST /api/write", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"tag": "plc-1/Setpoint", "written": "72.5"})
	})
	svc := f.services(t)
	for _, c := range []struct {
		typ, cfg string
	}{
		{"modbus write", `"server":"srv","hotloopTag":"plc-1/Setpoint"`},
		{"opcua write", `"server":"srv","nodeId":"ns=2;s=A","hotloopTag":"plc-1/Setpoint"`},
		{"s7 write", `"server":"srv","address":"DB1.DBW0","hotloopTag":"plc-1/Setpoint"`},
		{"ethernetip write", `"server":"srv","tag":"Speed","hotloopTag":"plc-1/Setpoint"`},
	} {
		n := build(t, c.typ, `{`+c.cfg+`,"hotloop":"hl","value":"payload","valueType":"msg"}`, svc)
		e, err := send(t, n, msg(t, `{"payload":72.5}`))
		if err != nil {
			t.Fatalf("%s: %v", c.typ, err)
		}
		if len(e.on(0)) != 1 {
			t.Fatalf("%s: a write the gate allowed should pass the message on", c.typ)
		}
	}
	writes := f.writeBodies()
	if len(writes) != 4 {
		t.Fatalf("HotLoop got %d writes, want 4", len(writes))
	}
	for _, w := range writes {
		if w["tag"] != "plc-1/Setpoint" || w["value"] != 72.5 || w["reason"] != "flow:test-node" {
			t.Errorf("write body = %#v", w)
		}
	}
	// The equipment connection ("srv") isn't configured at all in these tests,
	// so a write that touched it instead would have failed above.
}

func TestHotloopGateTakesTheTagFromTheMessage(t *testing.T) {
	f := newFakeHotloop(t)
	f.handle("POST /api/write", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"written": "1"})
	})
	svc := f.services(t)
	n := build(t, "modbus write", `{"server":"srv","hotloop":"hl","hotloopTag":"tag","hotloopTagType":"msg","value":"payload","valueType":"msg"}`, svc)
	if _, err := send(t, n, msg(t, `{"payload":true,"tag":"press-1/Run"}`)); err != nil {
		t.Fatal(err)
	}
	w := f.writeBodies()[0]
	if w["tag"] != "press-1/Run" || w["value"] != true {
		t.Errorf("write body = %#v: a bool has to go as a bool", w)
	}
	if _, err := send(t, n, msg(t, `{"payload":1}`)); err == nil || !strings.Contains(err.Error(), "resolved to nothing") {
		t.Errorf("a tag that resolves to nothing should say so, got %v", err)
	}
	if _, err := send(t, n, msg(t, `{"payload":1,"tag":"no-slash"}`)); err == nil || !strings.Contains(err.Error(), "device/tag") {
		t.Errorf("a tag with no device should say so, got %v", err)
	}
}

func TestHotloopGateRefusesWhatTheGateCannotTake(t *testing.T) {
	f := newFakeHotloop(t)
	f.handle("POST /api/write", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{}) })
	svc := f.services(t)
	n := build(t, "modbus write", `{"server":"srv","hotloop":"hl","hotloopTag":"a/b","value":"payload","valueType":"msg"}`, svc)
	for _, c := range []struct{ body, want string }{
		{`{"payload":[1,2]}`, "a list of 2"},
		{`{"payload":{"x":1}}`, "can't go through HotLoop's gate"},
	} {
		if _, err := send(t, n, msg(t, c.body)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.body, err, c.want)
		}
	}
	if got := len(f.writeBodies()); got != 0 {
		t.Errorf("a value the gate can't take still reached HotLoop %d times", got)
	}
	// A list of one is that one.
	if _, err := send(t, n, msg(t, `{"payload":[7]}`)); err != nil {
		t.Errorf("a list of one should go through as that value: %v", err)
	}
}

func TestHotloopGateReportsWhoSaidNo(t *testing.T) {
	f := newFakeHotloop(t)
	status := 422
	text := "value is outside the tag's configured range: 120 is outside 0..95"
	f.handle("POST /api/write", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, status, map[string]any{"error": text}) })
	svc := f.services(t)
	n := build(t, "modbus write", `{"server":"srv","hotloop":"hl","hotloopTag":"a/b","value":"payload","valueType":"msg"}`, svc)

	e, err := send(t, n, msg(t, `{"payload":120}`))
	if err == nil || !strings.Contains(err.Error(), "gate refused the write to a/b") || !strings.Contains(err.Error(), text) || !strings.Contains(err.Error(), "HTTP 422") {
		t.Fatalf("got %v", err)
	}
	if len(e.on(0)) != 0 {
		t.Errorf("a refused write must not pass the message on")
	}
	status, text = 503, "the device is not connected"
	if _, err := send(t, n, msg(t, `{"payload":1}`)); err == nil || !strings.Contains(err.Error(), "gate failed the write") {
		t.Errorf("a device that isn't connected is a failure and not a refusal: %v", err)
	}
	status, text = 403, "a/b: this tag is not marked writable"
	if _, err := send(t, n, msg(t, `{"payload":1}`)); err == nil || !strings.Contains(err.Error(), "gate refused") {
		t.Errorf("a tag that isn't armed is a refusal: %v", err)
	}
}

func TestHotloopCallsUnderAFlowTestGoToTheStandIn(t *testing.T) {
	si := &recordingStandIn{}
	svc := newTestServices()
	svc.standIn = si
	svc2 := &credServices{testServices: svc, configs: map[string]node.Node{}}

	g := build(t, "modbus write", `{"server":"srv","hotloop":"hl","hotloopTag":"a/b","value":"payload","valueType":"msg"}`, svc2)
	if _, err := send(t, g, msg(t, `{"payload":5}`)); err != nil {
		t.Fatal(err)
	}
	r := build(t, "hotloop recipe", `{"server":"hl","recipe":"grade_a","params":"{\"speed\":250}","paramsType":"json","version":2,"reason":"2pm"}`, svc2)
	if _, err := send(t, r, msg(t, `{}`)); err != nil {
		t.Fatal(err)
	}
	s := build(t, "hotloop script", `{"server":"hl","script":"cip","fields":"{\"minutes\":20}","fieldsType":"json"}`, svc2)
	if _, err := send(t, s, msg(t, `{}`)); err != nil {
		t.Fatal(err)
	}
	if len(si.calls) != 3 {
		t.Fatalf("the stand-in saw %d calls, want 3", len(si.calls))
	}
	want := []struct{ op, field string }{{"write", "tag"}, {"recipe", "recipe"}, {"script", "script"}}
	for i, w := range want {
		c := si.calls[i]
		sent, _ := c["sent"].(map[string]any)
		if c["kind"] != "hotloop" || sent["op"] != w.op || sent[w.field] == nil {
			t.Errorf("call %d = %#v", i, c)
		}
		if reason, _ := sent["reason"].(string); !strings.HasPrefix(reason, "flow:test-node") {
			t.Errorf("call %d reason = %q", i, reason)
		}
	}
	if sent := si.calls[1]["sent"].(map[string]any); sent["version"] != 2.0 || sent["reason"] != "flow:test-node: 2pm" {
		t.Errorf("recipe call = %#v", sent)
	}
}

func TestHotloopScriptOutcomes(t *testing.T) {
	f := newFakeHotloop(t)
	var body atomic.Value
	run := func(w http.ResponseWriter, status int, r map[string]any) {
		writeJSON(w, status, map[string]any{"run": r})
	}
	f.handle("POST /api/scripts/ok/run", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		run(w, 200, map[string]any{"id": 7, "scriptId": "ok", "status": "succeeded", "steps": []any{}})
	})
	f.handle("POST /api/scripts/slow/run", func(w http.ResponseWriter, r *http.Request) {
		run(w, 202, map[string]any{"id": 8, "scriptId": "slow", "status": "running"})
	})
	f.handle("POST /api/scripts/broken/run", func(w http.ResponseWriter, r *http.Request) {
		run(w, 200, map[string]any{"id": 9, "scriptId": "broken", "status": "failed", "steps": []any{
			map[string]any{"index": 1, "type": "call_service", "error": "cip/pump: this tag is not marked writable"}}})
	})
	f.handle("POST /api/scripts/busy/run", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 409, map[string]any{"error": "script busy: script busy was not started: it is already running, and its mode is skip"})
	})
	svc := f.services(t)

	n := build(t, "hotloop script", `{"server":"hl","script":"ok","fields":"{\"minutes\":20}","fieldsType":"json","reason":"end of shift"}`, svc)
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(body.Load().(string)), &sent)
	if sent["reason"] != "flow:test-node: end of shift" || sent["wait"] != true || sent["fields"].(map[string]any)["minutes"] != 20.0 {
		t.Errorf("what HotLoop was asked = %#v", sent)
	}
	if p, _ := e.on(0)[0].Payload().(map[string]any); p["status"] != "succeeded" {
		t.Errorf("payload = %#v", e.on(0)[0].Payload())
	}

	// Not waiting: the run has only started, and that is fine.
	n = build(t, "hotloop script", `{"server":"hl","script":"slow","wait":false}`, svc)
	e, err = send(t, n, msg(t, `{}`))
	if err != nil || e.on(0)[0].Payload().(map[string]any)["status"] != "running" {
		t.Errorf("a run that was only asked to start: %v %v", err, e.on(0))
	}
	// Waiting for one that is still going is not a success.
	n = build(t, "hotloop script", `{"server":"hl","script":"slow"}`, svc)
	if _, err := send(t, n, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "ended running") {
		t.Errorf("got %v", err)
	}
	// A run that failed says which step and why.
	n = build(t, "hotloop script", `{"server":"hl","script":"broken"}`, svc)
	if _, err := send(t, n, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "ended failed") || !strings.Contains(err.Error(), "not marked writable") {
		t.Errorf("got %v", err)
	}
	n = build(t, "hotloop script", `{"server":"hl","script":"busy"}`, svc)
	if _, err := send(t, n, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "already running") || !strings.Contains(err.Error(), "HTTP 409") {
		t.Errorf("got %v", err)
	}
	// Fields have to be an object.
	n = build(t, "hotloop script", `{"server":"hl","script":"ok","fields":"payload","fieldsType":"msg"}`, svc)
	if _, err := send(t, n, msg(t, `{"payload":"nope"}`)); err == nil || !strings.Contains(err.Error(), "has to be an object") {
		t.Errorf("got %v", err)
	}
}

func TestHotloopRecipeOutcomes(t *testing.T) {
	f := newFakeHotloop(t)
	var req atomic.Value
	f.handle("POST /api/recipes/good/apply", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req.Store(string(b))
		writeJSON(w, 200, map[string]any{"application": map[string]any{"id": 1, "recipe_id": "good", "status": "applied",
			"targets": []any{map[string]any{"status": "written", "verification": "verified"}}}})
	})
	f.handle("POST /api/recipes/half/apply", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 502, map[string]any{"application": map[string]any{"id": 2, "recipe_id": "half", "status": "partial",
			"targets": []any{map[string]any{"status": "written"}, map[string]any{"status": "failed"}, map[string]any{"status": "skipped"}}}})
	})
	f.handle("POST /api/recipes/refused/apply", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 409, map[string]any{"error": "target 2 (set_value number.line_02_speed): line-02/Speed: this tag is not marked writable",
			"application": map[string]any{"id": 3, "status": "refused", "targets": []any{}}})
	})
	svc := f.services(t)

	n := build(t, "hotloop recipe", `{"server":"hl","recipe":"good","params":"{\"speed\":250}","paramsType":"json","version":3}`, svc)
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(req.Load().(string)), &sent)
	if sent["version"] != 3.0 || sent["reason"] != "flow:test-node" || sent["params"].(map[string]any)["speed"] != 250.0 {
		t.Errorf("what HotLoop was asked = %#v", sent)
	}
	if p := e.on(0)[0].Payload().(map[string]any); p["status"] != "applied" {
		t.Errorf("payload = %#v", p)
	}

	n = build(t, "hotloop recipe", `{"server":"hl","recipe":"half"}`, svc)
	e, err = send(t, n, msg(t, `{}`))
	if err == nil || !strings.Contains(err.Error(), "ended partial") || !strings.Contains(err.Error(), "1 of 3 targets were written") {
		t.Errorf("got %v", err)
	}
	if len(e.on(0)) != 0 {
		t.Errorf("a recipe that stopped halfway must not pass the message on")
	}
	n = build(t, "hotloop recipe", `{"server":"hl","recipe":"refused"}`, svc)
	if _, err := send(t, n, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "target 2") || !strings.Contains(err.Error(), "HTTP 409") {
		t.Errorf("a refusal should say HotLoop's words: %v", err)
	}
}

// sseFake serves /api/stream with a script of connections: each one is the list
// of events it sends before it ends (or, when the last one, stays open).
type sseFake struct {
	f     *fakeHotloop
	mu    sync.Mutex
	conns [][]string
	n     int
	live  chan string
}

func newSSEFake(t *testing.T, conns ...[]string) *sseFake {
	s := &sseFake{f: newFakeHotloop(t), conns: conns, live: make(chan string, 16)}
	s.f.handle("GET /api/stream", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		i := s.n
		s.n++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "retry: 3000\n\n")
		fl.Flush()
		if i >= len(s.conns) {
			return
		}
		for _, ev := range s.conns[i] {
			fmt.Fprint(w, ev)
			fl.Flush()
		}
		if i < len(s.conns)-1 {
			return // this connection ends; the node has to come back
		}
		for {
			select {
			case ev := <-s.live:
				fmt.Fprint(w, ev)
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	return s
}

func entitiesEvent(states ...string) string {
	return "event: entities\ndata: {\"changed\":[" + strings.Join(states, ",") + "],\"removed\":[]}\n\n"
}

func hotloopStateJSON(id, state, changed string, attrs string) string {
	return fmt.Sprintf(`{"entity_id":%q,"state":%q,"attributes":%s,"quality":"good","last_changed":%q,"last_updated":%q}`,
		id, state, attrs, changed, changed)
}

func startIn(t *testing.T, svc node.Services, cfg string) (*testEmitter, func()) {
	t.Helper()
	n := build(t, "hotloop in", cfg, svc)
	e := newTestEmitter()
	ctx, cancel := context.WithCancel(context.Background())
	if err := n.(node.Starter).Start(ctx, e); err != nil {
		t.Fatal(err)
	}
	return e, func() { cancel(); _ = n.(node.Closer).Close(context.Background(), false) }
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// connected is whether the node's badge says it is on the feed.
func connected(e *testEmitter) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.statuses) > 0 && e.statuses[len(e.statuses)-1].Text == "connected"
}

func raised(e *testEmitter) []error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]error(nil), e.errs...)
}

func hotloopSeenOn(e *testEmitter) []string {
	var out []string
	for _, m := range e.on(0) {
		out = append(out, fmt.Sprintf("%s=%v", m.Topic(), m.Payload()))
	}
	return out
}

func TestHotloopInSendsChangesAndNotThePicture(t *testing.T) {
	a := `{"unit_of_measurement":"C"}`
	s := newSSEFake(t, []string{
		entitiesEvent(hotloopStateJSON("sensor.tank_1", "10", "t1", a), hotloopStateJSON("sensor.tank_2", "20", "t1", a), hotloopStateJSON("switch.pump", "off", "t1", "{}")),
	})
	svc := s.f.services(t)
	e, stop := startIn(t, svc, `{"server":"hl","entities":[{"entity":"sensor.tank_*"}]}`)
	defer stop()

	eventually(t, "the node to connect", func() bool { return connected(e) })
	time.Sleep(300 * time.Millisecond)
	if got := e.total(); got != 0 {
		t.Fatalf("the picture HotLoop sends on connect was passed on: %v", hotloopSeenOn(e))
	}

	// A change, an attribute-only update (not a change of state), and an entity the pattern doesn't follow.
	s.live <- entitiesEvent(hotloopStateJSON("sensor.tank_1", "11", "t2", a), hotloopStateJSON("switch.pump", "on", "t2", "{}"))
	s.live <- entitiesEvent(hotloopStateJSON("sensor.tank_1", "11", "t2", `{"unit_of_measurement":"F"}`))
	s.live <- entitiesEvent(hotloopStateJSON("sensor.tank_2", "21", "t3", a))
	eventually(t, "both changes", func() bool { return e.total() == 2 })
	time.Sleep(200 * time.Millisecond)
	got := hotloopSeenOn(e)
	if len(got) != 2 || got[0] != "sensor.tank_1=11" || got[1] != "sensor.tank_2=21" {
		t.Fatalf("got %v", got)
	}
	info, _ := e.on(0)[0].Data["hotloop"].(map[string]any)
	if info["previous"] != "10" || info["quality"] != "good" || info["last_changed"] != "t2" {
		t.Errorf("msg.hotloop = %#v", info)
	}
}

func TestHotloopInCanSendTheCurrentStateAndNumbers(t *testing.T) {
	s := newSSEFake(t, []string{
		entitiesEvent(hotloopStateJSON("sensor.a", "12.5", "t1", "{}"), hotloopStateJSON("binary_sensor.b", "on", "t1", "{}")),
	})
	svc := s.f.services(t)
	e, stop := startIn(t, svc, `{"server":"hl","entities":[{"entity":"*"}],"initial":true,"numeric":true}`)
	defer stop()
	eventually(t, "the picture", func() bool { return e.total() == 2 })
	got := hotloopSeenOn(e)
	if got[0] != "sensor.a=12.5" || got[1] != "binary_sensor.b=on" {
		t.Errorf("got %v", got)
	}
	if v := e.on(0)[0].Payload(); v != 12.5 {
		t.Errorf("a numeric state should be a number, got %#v", v)
	}
}

func TestHotloopInComesBackAndSendsWhatChangedWhileItWasGone(t *testing.T) {
	a := "{}"
	s := newSSEFake(t,
		[]string{entitiesEvent(hotloopStateJSON("sensor.a", "1", "t1", a), hotloopStateJSON("sensor.b", "5", "t1", a))},
		// The link dropped. When it is back, a changed and b did not, and c is new.
		[]string{entitiesEvent(hotloopStateJSON("sensor.a", "2", "t2", a), hotloopStateJSON("sensor.b", "5", "t1", a), hotloopStateJSON("sensor.c", "9", "t2", a))},
	)
	svc := s.f.services(t)
	e, stop := startIn(t, svc, `{"server":"hl","entities":[{"entity":"sensor.*"}]}`)
	defer stop()
	eventually(t, "what changed while the link was down", func() bool { return e.total() == 2 })
	time.Sleep(300 * time.Millisecond)
	got := hotloopSeenOn(e)
	if len(got) != 2 || got[0] != "sensor.a=2" || got[1] != "sensor.c=9" {
		t.Fatalf("got %v", got)
	}
	if info, _ := e.on(0)[0].Data["hotloop"].(map[string]any); info["previous"] != "1" {
		t.Errorf("the state it had before the link dropped should be there: %#v", info)
	}
	// The outage was reported once, to a Catch node, and not on every retry.
	if errs := raised(e); len(errs) != 1 || !strings.Contains(errs[0].Error(), "closed its live feed") {
		t.Errorf("errors = %v", errs)
	}
}

func TestHotloopInSignsInAgainWhenTheFeedIsRefused(t *testing.T) {
	s := newSSEFake(t, []string{entitiesEvent(hotloopStateJSON("sensor.a", "1", "t1", "{}"))})
	svc := s.f.services(t)
	// A token from a session HotLoop no longer knows: the feed is refused once,
	// and the node signs in again and gets it.
	conn := svc.configs["hl"].(*hotloopConn)
	conn.mu.Lock()
	conn.token = "a-token-from-a-session-that-ended"
	conn.mu.Unlock()
	e, stop := startIn(t, svc, `{"server":"hl","entities":[{"entity":"*"}]}`)
	defer stop()
	eventually(t, "the node to connect after signing in", func() bool { return connected(e) })
	if got := s.f.loginCount(); got != 1 {
		t.Errorf("signed in %d times, want 1", got)
	}
	if errs := raised(e); len(errs) != 0 {
		t.Errorf("a refused token that was replaced is not an outage: %v", errs)
	}
}
