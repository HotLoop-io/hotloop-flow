package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/plugin"
	"github.com/HotLoop-io/hotloop-flow/internal/wasmhost/wasmasm"
)

const pluginOK = `{"send":[[{"payload":"fine"}]]}`

// pluginDir writes one plugin directory, signed when priv is not nil.
func pluginDir(t *testing.T, root, name string, module []byte, extra string, privPEM []byte) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	desc := `{"format":1,"name":"` + name + `","version":"1.0.0"` + extra + `}`
	for file, data := range map[string][]byte{plugin.ModuleFile: module, plugin.DescriptorFile: []byte(desc)} {
		if err := os.WriteFile(filepath.Join(dir, file), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if privPEM == nil {
		return
	}
	priv, err := plugin.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := plugin.Sign(priv, module, []byte(desc))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, plugin.SignatureFile), sig, 0o644); err != nil {
		t.Fatal(err)
	}
}

// pluginApp is the real application with plugins loaded the way cmdServe loads
// them, into a copy of the palette so tests don't leave types in the default.
func pluginApp(t *testing.T, root string, trustedPub string, grants map[string][]string) (*application, *recorder, *e2e, string) {
	t.Helper()
	app, rec := newApp(t)
	app.cfg.Plugins.Dir = root
	app.cfg.Plugins.TrustedKeys = []string{trustedPub}
	app.cfg.Plugins.Grants = grants

	reg := node.NewRegistry()
	for _, d := range node.Default.Descriptors() {
		r, _ := node.Default.Lookup(d.Type)
		if err := reg.Register(d, r.New); err != nil {
			t.Fatal(err)
		}
	}
	host, err := loadPlugins(context.Background(), pluginSetup(app.cfg), reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	app.registry = reg
	app.plugins = host

	e := serveAppWith(t, app, map[string][]string{"dana": {"*"}}, func(d *api.Deps) {
		d.Registry = reg
		d.Plugins = func() any { return host.Report() }
	})
	return app, rec, e, e.login(t, "dana")
}

func TestPluginsOverTheAdminAPI(t *testing.T) {
	privPEM, pubPEM, err := plugin.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := plugin.ParsePublicKey(string(pubPEM))
	root := t.TempDir()
	pluginDir(t, root, "good", wasmasm.Echo(pluginOK), `,"description":"answers fine","props":[{"name":"factor","kind":"number","default":2}]`, privPEM)
	pluginDir(t, root, "needy", wasmasm.Echo(pluginOK), `,"capabilities":["clock"]`, privPEM)
	pluginDir(t, root, "loose", wasmasm.Echo(pluginOK), ``, nil)
	pluginDir(t, root, "hog", wasmasm.Hog(), ``, privPEM)

	app, rec, e, dana := pluginApp(t, root, plugin.PublicKeyText(pub), nil)

	// The editor's palette has the plugin, with the dialog its descriptor declares.
	code, out := e.call(t, "GET", "/nodes", dana, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /nodes: %d %s", code, out)
	}
	var descriptors []node.Descriptor
	if err := json.Unmarshal(out, &descriptors); err != nil {
		t.Fatal(err)
	}
	var found *node.Descriptor
	for i := range descriptors {
		if descriptors[i].Type == "wasm-good" {
			found = &descriptors[i]
		}
		if descriptors[i].Type == "wasm-loose" {
			t.Error("an unsigned plugin is in the palette")
		}
	}
	if found == nil {
		t.Fatal("wasm-good is not in GET /nodes")
	}
	if _, ok := found.PropByName("factor"); !ok {
		t.Errorf("the plugin's own field is not in its dialog: %+v", found.Props)
	}

	// The list says what loaded, what asks for what, and why one was refused.
	code, out = e.call(t, "GET", "/plugins", dana, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /plugins: %d %s", code, out)
	}
	t.Logf("GET /plugins: %s", out)
	var rep plugin.Report
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Plugins) != 3 || len(rep.Refused) != 1 || rep.Refused[0].Name != "loose" ||
		!strings.Contains(rep.Refused[0].Reason, "unsigned") {
		t.Errorf("report = %+v", rep)
	}

	// An ungranted capability fails the deploy over HTTP, and writes nothing.
	revBefore := app.flowStore.Rev()
	refused := func(doc string, want ...string) {
		t.Helper()
		code, out := e.call(t, "POST", "/flows", dana, []byte(doc))
		t.Logf("POST /flows -> %d %s", code, out)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("deploy = %d %s, want 422", code, out)
		}
		for _, w := range want {
			if !strings.Contains(string(out), w) {
				t.Errorf("the refusal does not say %q: %s", w, out)
			}
		}
		if app.flowStore.Rev() != revBefore {
			t.Error("a refused deploy changed the flow file")
		}
		if n := len(app.history.List(0)); n != 0 {
			t.Errorf("a refused deploy was recorded: %d deployments", n)
		}
	}
	refused(`[{"id":"t1","type":"tab","label":"p"},{"id":"n1","type":"wasm-needy","z":"t1","name":"stamp","wires":[[]]}]`,
		"clock", "plugins.grants.needy", "stamp")
	refused(`[{"id":"t1","type":"tab","label":"p"},{"id":"n1","type":"wasm-loose","z":"t1","wires":[[]]}]`,
		"did not load", "unsigned")

	// The refusals are in the audit trail.
	code, out = e.call(t, "GET", "/audit", dana, nil)
	if code != http.StatusOK || !strings.Contains(string(out), "plugins") {
		t.Errorf("audit: %d %s", code, out)
	}

	// A plugin that is allowed deploys, runs, and a message through a plugin
	// that traps comes back as an error to a Catch node while the others work.
	doc := `[{"id":"t1","type":"tab","label":"p"},
	{"id":"good","type":"wasm-good","z":"t1","name":"good","wires":[["good-out"]]},
	{"id":"good-out","type":"debug","z":"t1","complete":"payload","wires":[]},
	{"id":"hog","type":"wasm-hog","z":"t1","name":"hog","memoryMiB":32,"wires":[["hog-out"]]},
	{"id":"hog-out","type":"debug","z":"t1","complete":"payload","wires":[]},
	{"id":"catch","type":"catch","z":"t1","wires":[["caught"]]},
	{"id":"caught","type":"debug","z":"t1","complete":"error.message","wires":[]}]`
	code, out = e.call(t, "POST", "/flows", dana, []byte(doc))
	if code != http.StatusOK {
		t.Fatalf("a good plugin was refused: %d %s", code, out)
	}
	injectPayload(t, app, "hog", "x")
	msg := rec.debugFrom(t, "caught")
	t.Logf("the Catch node got: %s", msg)
	if !strings.Contains(msg, "exceeded its memory limit") || !strings.Contains(msg, "hog") {
		t.Errorf("the error does not say what happened: %q", msg)
	}
	injectPayload(t, app, "good", "x")
	if got := rec.debugFrom(t, "good-out"); got != "fine" {
		t.Errorf("the healthy plugin said %q after the other trapped", got)
	}
}

func TestGrantedPluginDeploysAndARollbackToARevokedOneIsRefused(t *testing.T) {
	privPEM, pubPEM, _ := plugin.GenerateKey()
	pub, _ := plugin.ParsePublicKey(string(pubPEM))
	root := t.TempDir()
	pluginDir(t, root, "needy", wasmasm.Echo(pluginOK), `,"capabilities":["clock"]`, privPEM)
	doc := `[{"id":"t1","type":"tab","label":"p"},{"id":"n1","type":"wasm-needy","z":"t1","wires":[[]]}]`

	app, _, e, dana := pluginApp(t, root, plugin.PublicKeyText(pub), map[string][]string{"needy": {"clock"}})
	code, out := e.call(t, "POST", "/flows", dana, []byte(doc))
	if code != http.StatusOK {
		t.Fatalf("a granted plugin was refused: %d %s", code, out)
	}
	seq := app.history.List(0)[0].Seq

	// The operator takes the grant away and restarts. Rolling back to the
	// deployment that used it is a deploy like any other, and is refused.
	revoked, err := loadPlugins(context.Background(), PluginSetup{Dir: root, Plugins: config.Plugins{
		TrustedKeys: []string{plugin.PublicKeyText(pub)}, MaxMemoryMiB: 64, MaxTimeout: time.Second}},
		node.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = revoked.Close(context.Background()) })
	app.plugins = revoked
	code, out = e.rollbackTo(t, dana, seq, `{}`)
	t.Logf("rollback with the grant gone -> %d %s", code, out)
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(out), "plugins.grants.needy") {
		t.Errorf("rollback = %d %s", code, out)
	}
}

func TestPluginKeygenSignAndVerifyCommands(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := cmdPlugin([]string{"keygen", "-out", filepath.Join(dir, "acme")}, &out); err != nil {
		t.Fatal(err)
	}
	t.Logf("keygen:\n%s", out.String())
	info, err := os.Stat(filepath.Join(dir, "acme.key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the private key: %v, %v", err, info)
	}
	if err := cmdPlugin([]string{"keygen", "-out", filepath.Join(dir, "acme")}, io.Discard); err == nil {
		t.Error("keygen overwrote an existing key")
	}

	root := filepath.Join(dir, "plugins")
	pluginDir(t, root, "demo", wasmasm.Echo(pluginOK), ``, nil)
	plug := filepath.Join(root, "demo")

	out.Reset()
	if err := cmdPlugin([]string{"verify", "-trust", filepath.Join(dir, "acme.pub"), plug}, &out); err == nil {
		t.Fatal("an unsigned plugin verified")
	}
	t.Logf("verify, unsigned: %s", strings.TrimSpace(out.String()))
	if !strings.Contains(out.String(), "REFUSED") || !strings.Contains(out.String(), "unsigned") {
		t.Errorf("output = %q", out.String())
	}

	out.Reset()
	if err := cmdPlugin([]string{"sign", "-key", filepath.Join(dir, "acme.key"), plug}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := cmdPlugin([]string{"verify", "-trust", filepath.Join(dir, "acme.pub"), plug}, &out); err != nil {
		t.Fatalf("a signed plugin did not verify: %v %s", err, out.String())
	}
	t.Logf("verify, signed: %s", strings.TrimSpace(out.String()))

	// A different publisher's key does not.
	if err := cmdPlugin([]string{"keygen", "-out", filepath.Join(dir, "other")}, io.Discard); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := cmdPlugin([]string{"verify", "-trust", filepath.Join(dir, "other.pub"), plug}, &out); err == nil ||
		!strings.Contains(out.String(), "not trusted") {
		t.Errorf("a plugin verified against the wrong key: %v %s", err, out.String())
	}

	// And a plugin edited after signing does not.
	if err := os.WriteFile(filepath.Join(plug, plugin.DescriptorFile),
		[]byte(`{"format":1,"name":"demo","version":"1.0.0","capabilities":["clock"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := cmdPlugin([]string{"verify", "-trust", filepath.Join(dir, "acme.pub"), plug}, &out); err == nil ||
		!strings.Contains(out.String(), "changed after it was signed") {
		t.Errorf("a tampered plugin verified: %v %s", err, out.String())
	}
}

// The deploy gate and the flow tests run in a child that has no config file.
// It is handed the plugin settings, so a flow that uses a plugin is tested
// with the plugin.
func TestFlowTestsRunPluginsFromTheBundle(t *testing.T) {
	privPEM, pubPEM, _ := plugin.GenerateKey()
	pub, _ := plugin.ParsePublicKey(string(pubPEM))
	root := t.TempDir()
	pluginDir(t, root, "bundled", wasmasm.Echo(pluginOK), ``, privPEM)

	cfg := config.Default()
	cfg.Plugins.Dir = root
	cfg.Plugins.TrustedKeys = []string{plugin.PublicKeyText(pub)}
	setup := pluginSetup(cfg)

	bundle, _ := json.Marshal(TestBundle{
		Flows: json.RawMessage(`[{"id":"t1","type":"tab","label":"p"},
			{"id":"in","type":"inject","z":"t1","name":"in","wires":[["b"]]},
			{"id":"b","type":"wasm-bundled","z":"t1","name":"b","wires":[["out"]]},
			{"id":"out","type":"debug","z":"t1","name":"out","wires":[]}]`),
		Tests:   "tests:\n  - name: the plugin runs\n    inject:\n      - node: in\n    expect:\n      - node: out\n        msg: {payload: fine}\n",
		Plugins: &setup,
	})
	var out bytes.Buffer
	if err := testBundle(bytes.NewReader(bundle), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"status":"pass"`) && !strings.Contains(out.String(), `"Status":"pass"`) {
		t.Errorf("the bundle did not pass: %s", out.String())
	}
	t.Logf("bundle report: %.300s", out.String())
}
