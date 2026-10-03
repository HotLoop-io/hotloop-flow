package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/history"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// setFlow is a change node that sets the payload to a value, wired to a debug
// node, so what a deploy does can be read off what comes out.
func setFlow(value string) string {
	return `[{"id":"t1","type":"tab","label":"Line 3"},` +
		`{"id":"c1","type":"change","z":"t1","name":"set","rules":[{"t":"set","p":"payload","pt":"msg","to":"` + value + `","tot":"str"}],"wires":[["d1"]]},` +
		`{"id":"d1","type":"debug","z":"t1","complete":"payload","wires":[]}]`
}

func (e *e2e) deployDoc(t *testing.T, token, doc string) int64 {
	t.Helper()
	code, out := e.call(t, "POST", "/flows", token, []byte(doc))
	if code != http.StatusOK {
		t.Fatalf("deploy: %d %s", code, out)
	}
	var res struct {
		Deployment int64 `json:"deployment"`
	}
	_ = json.Unmarshal(out, &res)
	return res.Deployment
}

func (e *e2e) rollbackTo(t *testing.T, token string, seq int64, body string, hdr ...string) (int, []byte) {
	t.Helper()
	return e.call(t, "POST", "/deployments/"+strconv.FormatInt(seq, 10)+"/rollback", token, []byte(body), hdr...)
}

// The roadmap's behavioural test: what comes out of the flow after a rollback
// is what came out before the deploy that's being undone.
func TestRollbackPutsTheOldBehaviourBack(t *testing.T) {
	app, rec := newApp(t)
	e := serveApp(t, app, map[string][]string{"dana": {"*"}})
	dana := e.login(t, "dana")

	first := e.deployDoc(t, dana, setFlow("one"))
	e.deployDoc(t, dana, setFlow("two"))

	rec.reset()
	injectPayload(t, app, "c1", "x")
	if got := rec.debugFrom(t, "d1"); got != "two" {
		t.Fatalf("before the rollback the flow says %q", got)
	}

	code, out := e.rollbackTo(t, dana, first, `{"note":"two broke the label printer"}`)
	if code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, out)
	}
	rec.reset()
	injectPayload(t, app, "c1", "x")
	if got := rec.debugFrom(t, "d1"); got != "one" {
		t.Fatalf("after the rollback the flow says %q, want one", got)
	}

	// Append-only: three records, the newest a rollback that says what it
	// undid and why, and the one it went back to untouched.
	l := app.history.List(0)
	if len(l) != 3 {
		t.Fatalf("%d records, want 3", len(l))
	}
	rb := l[0]
	if rb.Kind != history.KindRollback || rb.RollbackOf != first || rb.User != "dana" ||
		rb.Note != "rollback to deployment 1: two broke the label printer" {
		t.Fatalf("rollback record = %+v", rb)
	}
	if rb.ParentRev != l[1].Rev {
		t.Fatal("the rollback's parent isn't what it replaced")
	}
	// The flow file comes back byte for byte, so it's the same revision.
	if rb.Rev != l[2].Rev {
		t.Fatalf("rolled back to rev %s, the original was %s", rb.Rev, l[2].Rev)
	}
	orig, _ := app.history.Get(first)
	now, _ := os.ReadFile(app.cfg.FlowPath())
	if string(now) != string(orig.Flows) {
		t.Fatal("the flow file after a rollback is not the bytes that were recorded")
	}
}

// A node deleted since the record comes back with its password, because the
// record carried the credentials as they stood.
func TestRollbackBringsCredentialsBack(t *testing.T) {
	app, _ := newApp(t)
	e := serveApp(t, app, map[string][]string{"dana": {"*"}})
	dana := e.login(t, "dana")

	withBroker := `[{"id":"t1","type":"tab","label":"x"},` +
		`{"id":"b1","type":"vendor-broker","credentials":{"password":"hunter2-but-longer"}}]`
	first := e.deployDoc(t, dana, withBroker)
	e.deployDoc(t, dana, `[{"id":"t1","type":"tab","label":"x"}]`)
	if app.creds.Get("b1") != nil {
		t.Fatal("deleting the node didn't prune its credentials, so this test proves nothing")
	}

	if code, out := e.rollbackTo(t, dana, first, ""); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, out)
	}
	if got := app.creds.Get("b1")["password"]; got != "hunter2-but-longer" {
		t.Fatalf("after the rollback the password is %q", got)
	}
	// And it's on disk, encrypted, not only in memory.
	fresh := store.NewCredentialStore(app.cfg.CredentialsPath(), "a-secret-long-enough-to-count")
	if err := fresh.Load(); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Get("b1")["password"]; got != "hunter2-but-longer" {
		t.Fatalf("the credential file after a rollback holds %q", got)
	}
	raw, _ := os.ReadFile(app.cfg.CredentialsPath())
	if strings.Contains(string(raw), "hunter2") {
		t.Fatal("the credential file holds the password in plaintext")
	}
}

func TestRollbackRefusals(t *testing.T) {
	app, _ := newApp(t)
	e := serveApp(t, app, map[string][]string{"dana": {"*"}, "viewer": {"flows.read"}})
	dana, viewer := e.login(t, "dana"), e.login(t, "viewer")
	first := e.deployDoc(t, dana, setFlow("one"))
	e.deployDoc(t, dana, setFlow("two"))
	before := app.flowStore.Rev()

	if code, _ := e.rollbackTo(t, viewer, first, ""); code != http.StatusForbidden {
		t.Errorf("rollback with flows.read only: %d, want 403", code)
	}
	if code, _ := e.rollbackTo(t, dana, 99, ""); code != http.StatusNotFound {
		t.Errorf("rollback to a deployment that doesn't exist: %d, want 404", code)
	}
	// Somebody deployed since this editor last looked.
	staleRev := app.history.List(0)[1].Rev
	if code, _ := e.rollbackTo(t, dana, first, "", "HotLoop-Flow-Deployment-Rev", staleRev); code != http.StatusConflict {
		t.Errorf("rollback on a stale revision: %d, want 409", code)
	}
	if code, _ := e.rollbackTo(t, dana, first, "{not json"); code != http.StatusBadRequest {
		t.Errorf("rollback with a garbage body: %d, want 400", code)
	}
	if app.flowStore.Rev() != before || len(app.history.List(0)) != 2 {
		t.Fatal("a refused rollback changed something")
	}

	// The current revision is fine.
	if code, out := e.rollbackTo(t, dana, first, "", "HotLoop-Flow-Deployment-Rev", before); code != http.StatusOK {
		t.Fatalf("rollback on the current revision: %d %s", code, out)
	}
}

// A record whose credentials were encrypted under some other secret is
// refused before anything is written, because putting back flows that can't
// log in to anything is a rollback that only looks like it worked.
func TestRollbackRefusesCredentialsItCantRead(t *testing.T) {
	app, _ := newApp(t)
	e := serveApp(t, app, map[string][]string{"dana": {"*"}})
	dana := e.login(t, "dana")
	e.deployDoc(t, dana, setFlow("one"))
	before := app.flowStore.Rev()

	other := store.NewCredentialStore(filepath.Join(t.TempDir(), "c.json"), "somebody-elses-secret-entirely")
	other.Set("b1", map[string]string{"password": "x"})
	snap, err := other.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	alien, err := app.history.Append(history.Record{Kind: history.KindDeploy, Rev: "alien", Flows: []byte(setFlow("alien")), Credentials: snap})
	if err != nil {
		t.Fatal(err)
	}

	code, out := e.rollbackTo(t, dana, alien.Seq, "")
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(out), "credential secret") {
		t.Fatalf("rollback to unreadable credentials: %d %s", code, out)
	}
	if app.flowStore.Rev() != before {
		t.Fatal("a refused rollback wrote the flow file")
	}
}

// The bug the rollback work turned up: a deploy refused for a stale revision
// still merged and pruned credentials in memory before the store said no. A
// stale editor that had deleted a node took that node's password away from the
// flows still running it.
func TestRefusedDeployLeavesCredentialsAlone(t *testing.T) {
	app, _ := newApp(t)
	withBroker := parse(t, `[{"id":"t1","type":"tab","label":"x"},`+
		`{"id":"b1","type":"vendor-broker","credentials":{"password":"hunter2-but-longer"}}]`)
	res, err := app.deploy(context.Background(), api.DeployRequest{Flows: withBroker})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.deploy(context.Background(), api.DeployRequest{
		Flows: parse(t, `[{"id":"t1","type":"tab","label":"y"},{"id":"b1","type":"vendor-broker"}]`), ExpectedRev: res.Rev,
	}); err != nil {
		t.Fatal(err)
	}

	// A stale editor deletes the broker and changes another node's secret.
	_, err = app.deploy(context.Background(), api.DeployRequest{
		Flows:       parse(t, `[{"id":"t1","type":"tab","label":"z"},{"id":"n2","type":"vendor-node","credentials":{"token":"new"}}]`),
		ExpectedRev: res.Rev,
	})
	if !errors.Is(err, store.ErrRevisionConflict) {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if got := app.creds.Get("b1")["password"]; got != "hunter2-but-longer" {
		t.Fatalf("a refused deploy pruned a running node's password: now %q", got)
	}
	if app.creds.Get("n2") != nil {
		t.Fatal("a refused deploy merged its credentials")
	}
}

// Against a real broker that refuses anonymous clients: delete the MQTT
// nodes, deploy, roll back, and the subscriber logs in again with its password
// and messages flow. Set HOTLOOP_FLOW_TEST_MQTT_AUTH to host:port and
// HOTLOOP_FLOW_TEST_MQTT_USER and HOTLOOP_FLOW_TEST_MQTT_PASSWORD to an
// account on it. CI starts Mosquitto with a password file for exactly this.
func TestIntegrationRollbackLogsBackInToARealBroker(t *testing.T) {
	addr := os.Getenv("HOTLOOP_FLOW_TEST_MQTT_AUTH")
	if addr == "" {
		t.Skip("set HOTLOOP_FLOW_TEST_MQTT_AUTH, HOTLOOP_FLOW_TEST_MQTT_USER and HOTLOOP_FLOW_TEST_MQTT_PASSWORD to run this against a real broker")
	}
	user, pass := os.Getenv("HOTLOOP_FLOW_TEST_MQTT_USER"), os.Getenv("HOTLOOP_FLOW_TEST_MQTT_PASSWORD")
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}

	// First prove the broker means it: no password, refused for exactly that
	// reason. Any other failure (a typo in the address, DNS) would make this
	// whole test pass for the wrong reason.
	anon := mqtt.NewClient(mqtt.NewClientOptions().AddBroker("tcp://" + addr).SetClientID("flw-anon-" + engine.GenerateID()).
		SetConnectTimeout(5 * time.Second).SetConnectRetry(false).SetAutoReconnect(false))
	tok := anon.Connect()
	if !tok.WaitTimeout(10 * time.Second) {
		t.Fatal("the broker didn't answer an anonymous connect within 10s")
	}
	if tok.Error() == nil {
		anon.Disconnect(0)
		t.Fatal("the broker let an anonymous client in, so it can't prove a password came back")
	}
	if msg := strings.ToLower(tok.Error().Error()); !strings.Contains(msg, "not authori") {
		t.Fatalf("an anonymous connect failed, but not because of auth: %v", tok.Error())
	}

	app, rec := newApp(t)
	e := serveApp(t, app, map[string][]string{"dana": {"*"}})
	dana := e.login(t, "dana")

	topic := "hotloop-flow/test/rollback/" + engine.GenerateID()
	withMQTT := `[{"id":"t1","type":"tab","label":"Line 3"},` +
		`{"id":"b1","type":"mqtt-broker","broker":"` + host + `","port":` + port + `,"user":"` + user + `","clientid":"flw-rollback-` + engine.GenerateID() + `","credentials":{"password":"` + pass + `"}},` +
		`{"id":"m1","type":"mqtt in","z":"t1","broker":"b1","topic":"` + topic + `","qos":"1","datatype":"utf8","wires":[["d1"]]},` +
		`{"id":"d1","type":"debug","z":"t1","complete":"payload","wires":[]}]`
	first := e.deployDoc(t, dana, withMQTT)

	pub := mqtt.NewClient(mqtt.NewClientOptions().AddBroker("tcp://" + addr).SetClientID("flw-pub-" + engine.GenerateID()).
		SetUsername(user).SetPassword(pass).SetConnectTimeout(5 * time.Second))
	if tok := pub.Connect(); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("publisher could not connect: %v", tok.Error())
	}
	defer pub.Disconnect(100)

	// Publish until the flow's subscriber is up and answers.
	expect := func(payload string) {
		t.Helper()
		rec.reset()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			pub.Publish(topic, 1, false, payload).WaitTimeout(5 * time.Second)
			if got, ok := rec.lastDebug("d1"); ok && got == payload {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("no %q from the flow's subscriber within 20s", payload)
	}
	expect("before")

	e.deployDoc(t, dana, `[{"id":"t1","type":"tab","label":"Line 3"}]`)
	if app.creds.Get("b1") != nil {
		t.Fatal("deleting the broker didn't prune its password")
	}

	if code, out := e.rollbackTo(t, dana, first, `{"note":"put the subscriber back"}`); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, out)
	}
	expect("after")
}

// A rollback restarts everything by default, and can be asked to restart only
// what it changes. Either way the old behaviour is what comes out.
func TestRollbackHonoursTheDeploymentType(t *testing.T) {
	app, rec := newApp(t)
	e := serveApp(t, app, map[string][]string{"dana": {"*"}})
	dana := e.login(t, "dana")
	first := e.deployDoc(t, dana, setFlow("one"))
	e.deployDoc(t, dana, setFlow("two"))

	if code, _ := e.rollbackTo(t, dana, first, "", "HotLoop-Flow-Deployment-Type", "sideways"); code != http.StatusBadRequest {
		t.Fatalf("an unknown deployment type: %d, want 400", code)
	}

	code, out := e.rollbackTo(t, dana, first, "", "HotLoop-Flow-Deployment-Type", "nodes")
	if code != http.StatusOK {
		t.Fatalf("partial rollback: %d %s", code, out)
	}
	var res struct {
		Type      string   `json:"type"`
		Restarted []string `json:"restarted"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Type != "nodes" || len(res.Restarted) != 1 || res.Restarted[0] != "c1" {
		t.Fatalf("a partial rollback reported %s", out)
	}
	rec.reset()
	injectPayload(t, app, "c1", "x")
	if got := rec.debugFrom(t, "d1"); got != "one" {
		t.Fatalf("after a partial rollback the flow says %q", got)
	}

	// The default is a full restart.
	code, out = e.rollbackTo(t, dana, first+1, "")
	if code != http.StatusOK || !strings.Contains(string(out), `"type":"full"`) {
		t.Fatalf("default rollback: %d %s", code, out)
	}
}
