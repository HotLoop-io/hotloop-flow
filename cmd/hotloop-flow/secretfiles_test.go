package main

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/filescope"
	"github.com/HotLoop-io/hotloop-flow/internal/nodes"
)

// The roadmap claim, end to end against a broker that refuses anonymous
// clients: the broker password lives in a file laid out the way the kubelet
// mounts a Secret, the flow names the file, the subscriber logs in with it, and
// the password is nowhere the flow goes: not the flow file, not the credential
// store, not the deployment log.
func TestIntegrationPasswordFromAMountedSecret(t *testing.T) {
	addr := os.Getenv("HOTLOOP_FLOW_TEST_MQTT_AUTH")
	if addr == "" {
		t.Skip("set HOTLOOP_FLOW_TEST_MQTT_AUTH, HOTLOOP_FLOW_TEST_MQTT_USER and HOTLOOP_FLOW_TEST_MQTT_PASSWORD to run this against a real broker")
	}
	user, pass := os.Getenv("HOTLOOP_FLOW_TEST_MQTT_USER"), os.Getenv("HOTLOOP_FLOW_TEST_MQTT_PASSWORD")
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}

	// A Secret volume: password -> ..data/password -> ..<timestamp>/password,
	// with the trailing newline kubectl gives a value created from a file.
	mount := t.TempDir()
	stamp := filepath.Join(mount, "..2026_10_03_12_00_00.000000001")
	if err := os.Mkdir(stamp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stamp, "password"), []byte(pass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(stamp), filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "password"), filepath.Join(mount, "password")); err != nil {
		t.Fatal(err)
	}

	app, rec := newApp(t)
	scope, err := filescope.NewSecretScope(app.cfg.Data.Dir, []string{mount})
	if err != nil {
		t.Fatal(err)
	}
	prev := nodes.Secrets
	nodes.Secrets = scope
	t.Cleanup(func() { nodes.Secrets = prev })

	e := serveApp(t, app, map[string][]string{"dana": {"*"}})
	dana := e.login(t, "dana")

	topic := "hotloop-flow/test/secret-file/" + engine.GenerateID()
	doc := `[{"id":"t1","type":"tab","label":"Line 3"},` +
		`{"id":"b1","type":"mqtt-broker","broker":"` + host + `","port":` + port + `,"user":"` + user + `",` +
		`"clientid":"flw-secret-` + engine.GenerateID() + `",` +
		`"ew_credentialFiles":{"password":"` + filepath.ToSlash(filepath.Join(mount, "password")) + `"}},` +
		`{"id":"m1","type":"mqtt in","z":"t1","broker":"b1","topic":"` + topic + `","qos":"1","datatype":"utf8","wires":[["d1"]]},` +
		`{"id":"d1","type":"debug","z":"t1","complete":"payload","wires":[]}]`
	e.deployDoc(t, dana, doc)

	pub := mqtt.NewClient(mqtt.NewClientOptions().AddBroker("tcp://" + addr).SetClientID("flw-pub-" + engine.GenerateID()).
		SetUsername(user).SetPassword(pass).SetConnectTimeout(5 * time.Second))
	if tok := pub.Connect(); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("publisher could not connect: %v", tok.Error())
	}
	defer pub.Disconnect(100)

	deadline := time.Now().Add(20 * time.Second)
	for {
		pub.Publish(topic, 1, false, "logged in from a file").WaitTimeout(5 * time.Second)
		if got, ok := rec.lastDebug("d1"); ok && got == "logged in from a file" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the flow's subscriber never logged in with the password from the file")
		}
		time.Sleep(250 * time.Millisecond)
	}

	// Nothing the flow writes down holds the password: walk every file in
	// the data directory, flows, credentials and deployment log included.
	err = filepath.WalkDir(app.cfg.Data.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), pass) {
			t.Errorf("%s holds the broker password", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := app.creds.Get("b1"); c["password"] != "" {
		t.Error("the password went into the credential store")
	}
	flowFile, _ := os.ReadFile(app.cfg.FlowPath())
	if !strings.Contains(string(flowFile), "ew_credentialFiles") {
		t.Error("the flow file lost the reference")
	}
}

// The production reader is the secret scope's. A flow naming a file outside it,
// here the service account token every pod has, fails that node's deploy and
// says which setting would allow it. Read through any wider reader, the token
// would be on its way to whatever broker the flow points at.
func TestCredentialFileOutsideTheSecretScopeFailsTheNode(t *testing.T) {
	app, _ := newApp(t)
	scope, err := filescope.NewSecretScope(app.cfg.Data.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	prev := nodes.Secrets
	nodes.Secrets = scope
	t.Cleanup(func() { nodes.Secrets = prev })

	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("eyJhbGciOi.the-pod-identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := `[{"id":"t1","type":"tab","label":"Line 3"},` +
		`{"id":"b1","type":"mqtt-broker","broker":"192.0.2.1","port":1883,"user":"line3",` +
		`"ew_credentialFiles":{"password":"` + filepath.ToSlash(token) + `"}}]`
	res, err := app.deploy(t.Context(), api.DeployRequest{Flows: parse(t, doc)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failures) != 1 || res.Failures[0].NodeID != "b1" ||
		!strings.Contains(res.Failures[0].Err.Error(), "secrets.allowedPaths") {
		t.Fatalf("failures %v, want b1 refused for being outside the secret scope", res.Failures)
	}
}
