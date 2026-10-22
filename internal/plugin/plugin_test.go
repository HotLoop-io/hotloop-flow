package plugin_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	_ "github.com/HotLoop-io/hotloop-flow/internal/nodes" // the real palette
	"github.com/HotLoop-io/hotloop-flow/internal/plugin"
	"github.com/HotLoop-io/hotloop-flow/internal/wasmhost/wasmasm"
)

const okResponse = `{"send":[[{"payload":"fine"}]]}`

// publisher is a throwaway key pair, made fresh for each test. No key is ever
// committed to this repository.
type publisher struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newPublisher(t *testing.T) publisher {
	t.Helper()
	privPEM, pubPEM, err := plugin.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := plugin.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := plugin.ParsePublicKey(string(pubPEM))
	if err != nil {
		t.Fatal(err)
	}
	return publisher{priv: priv, pub: pub}
}

func manifest(name string, extra string) string {
	m := `{"format":1,"name":"` + name + `","version":"1.0.0","description":"a test plugin"`
	if extra != "" {
		m += "," + extra
	}
	return m + "}"
}

// install writes a plugin directory. signer nil leaves it unsigned.
func install(t *testing.T, root, name string, module []byte, descriptor string, signer *publisher) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(file string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, file), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(plugin.ModuleFile, module)
	write(plugin.DescriptorFile, []byte(descriptor))
	if signer != nil {
		sig, err := plugin.Sign(signer.priv, module, []byte(descriptor))
		if err != nil {
			t.Fatal(err)
		}
		write(plugin.SignatureFile, sig)
	}
	return dir
}

func policy(trusted ...publisher) plugin.Policy {
	keys := make([]ed25519.PublicKey, len(trusted))
	for i, p := range trusted {
		keys[i] = p.pub
	}
	return plugin.Policy{Trust: plugin.NewTrust(keys...), MaxMemoryMiB: 64, MaxTimeout: 5 * time.Second}
}

func load(t *testing.T, root string, p plugin.Policy) *plugin.Host {
	t.Helper()
	h, err := plugin.Load(context.Background(), root, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h
}

func refusal(t *testing.T, h *plugin.Host, name string) string {
	t.Helper()
	for _, r := range h.Refusals() {
		if r.Name == name {
			return r.Reason
		}
	}
	t.Fatalf("plugin %q was not refused; loaded %v, refused %v", name, h.Loaded(), h.Refusals())
	return ""
}

func mustContain(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("%q does not contain %q", got, w)
		}
	}
}

// ---------------------------------------------------------------------------
// Format and signature
// ---------------------------------------------------------------------------

func TestASignedPluginLoadsAndGetsAnEditorDialog(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "scale", wasmasm.Echo(okResponse), manifest("scale", `
		"label":"scale","outputs":2,
		"props":[
			{"name":"factor","kind":"number","label":"Factor","default":2,"required":true},
			{"name":"unit","kind":"select","label":"Unit","options":[{"value":"kPa","label":"kPa"},{"value":"bar","label":"bar"}]}
		],
		"limits":{"memoryMiB":8,"timeoutMs":250}`), &pub)

	h := load(t, root, policy(pub))
	if got := h.Loaded(); len(got) != 1 || got[0] != "scale" {
		t.Fatalf("loaded %v, refused %v", got, h.Refusals())
	}

	reg := node.NewRegistry()
	if err := h.Register(reg); err != nil {
		t.Fatal(err)
	}
	reg2, ok := reg.Lookup("wasm-scale")
	if !ok {
		t.Fatal("the plugin did not become a node type")
	}
	d := reg2.Descriptor
	if d.Outputs != 2 || d.PaletteLabel != "scale" || d.Category != node.CategoryFunction {
		t.Errorf("descriptor = %+v", d)
	}

	// The dialog is the plugin's own props, then the two limits every plugin
	// node has, all in the shape the editor already renders.
	var names []string
	for _, p := range d.Props {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "name,factor,unit,memoryMiB,timeoutMs" {
		t.Errorf("props = %v", names)
	}
	mem, _ := d.PropByName("memoryMiB")
	if mem.Default != 8 {
		t.Errorf("memory default = %v, want the plugin's 8", mem.Default)
	}
	if _, err := json.Marshal(d); err != nil {
		t.Errorf("descriptor does not serialise for GET /nodes: %v", err)
	}
	mustContain(t, d.Help, "scale", "1.0.0", plugin.KeyID(pub.pub), "asks for no host access")
}

func TestAnUnsignedPluginIsRefusedAndSaysSo(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "loose", wasmasm.Echo(okResponse), manifest("loose", ""), nil)

	h := load(t, root, policy(pub))
	if len(h.Loaded()) != 0 {
		t.Fatal("an unsigned plugin loaded")
	}
	reason := refusal(t, h, "loose")
	t.Logf("refusal: %s", reason)
	mustContain(t, reason, "unsigned", "plugin.sig is missing")
}

func TestATamperedModuleIsRefusedAndSaysSo(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	dir := install(t, root, "tamper", wasmasm.Echo(okResponse), manifest("tamper", ""), &pub)

	// Somebody swaps the module after it was signed.
	if err := os.WriteFile(filepath.Join(dir, plugin.ModuleFile), wasmasm.Echo(`{"send":[[{"payload":"evil"}]]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := load(t, root, policy(pub))
	if len(h.Loaded()) != 0 {
		t.Fatal("a tampered plugin loaded")
	}
	reason := refusal(t, h, "tamper")
	t.Logf("refusal: %s", reason)
	mustContain(t, reason, "signature does not match", "plugin.wasm", "changed after it was signed", plugin.KeyID(pub.pub))
}

func TestAddingACapabilityToASignedDescriptorIsRefused(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	dir := install(t, root, "sneaky", wasmasm.Echo(okResponse), manifest("sneaky", ""), &pub)

	// The module is untouched. The descriptor now asks for the clock.
	if err := os.WriteFile(filepath.Join(dir, plugin.DescriptorFile), []byte(manifest("sneaky", `"capabilities":["clock"]`)), 0o644); err != nil {
		t.Fatal(err)
	}
	h := load(t, root, policy(pub))
	reason := refusal(t, h, "sneaky")
	t.Logf("refusal: %s", reason)
	mustContain(t, reason, "signature does not match", "plugin.json")
}

func TestASignatureFromAnUntrustedKeyIsRefusedByKeyID(t *testing.T) {
	root := t.TempDir()
	mine, theirs := newPublisher(t), newPublisher(t)
	install(t, root, "foreign", wasmasm.Echo(okResponse), manifest("foreign", ""), &theirs)

	h := load(t, root, policy(mine))
	reason := refusal(t, h, "foreign")
	t.Logf("refusal: %s", reason)
	mustContain(t, reason, "not trusted", plugin.KeyID(theirs.pub), "plugins.trustedKeys")
}

func TestWithNoTrustedKeysNothingLoads(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "orphan", wasmasm.Echo(okResponse), manifest("orphan", ""), &pub)

	h := load(t, root, plugin.Policy{MaxMemoryMiB: 64, MaxTimeout: time.Second})
	reason := refusal(t, h, "orphan")
	t.Logf("refusal: %s", reason)
	mustContain(t, reason, "no trusted keys are configured")
}

func TestAGarbledSignatureFileIsRefused(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	dir := install(t, root, "garbled", wasmasm.Echo(okResponse), manifest("garbled", ""), &pub)
	if err := os.WriteFile(filepath.Join(dir, plugin.SignatureFile), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := load(t, root, policy(pub))
	mustContain(t, refusal(t, h, "garbled"), "malformed")
}

func TestADescriptorWithAMisspeltFieldIsRefused(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "typo", wasmasm.Echo(okResponse), manifest("typo", `"capabilites":["clock"]`), &pub)
	h := load(t, root, policy(pub))
	mustContain(t, refusal(t, h, "typo"), "capabilites")
}

func TestDescriptorRefusals(t *testing.T) {
	pub := newPublisher(t)
	cases := []struct{ name, extra, want string }{
		{"badkind", `"props":[{"name":"k","kind":"credential"}]`, `kind "credential"`},
		{"badcap", `"capabilities":["network"]`, `"network" is not one Flow has`},
		{"dupcap", `"capabilities":["clock","clock"]`, "listed twice"},
		{"reserved", `"props":[{"name":"memoryMiB","kind":"number"}]`, "added to every plugin node"},
		{"twoin", `"inputs":2`, "one input"},
		{"nofmt", ``, "format is 0"},
	}
	root := t.TempDir()
	for _, c := range cases {
		d := manifest(c.name, c.extra)
		if c.name == "nofmt" {
			d = `{"name":"nofmt","version":"1"}`
		}
		install(t, root, c.name, wasmasm.Echo(okResponse), d, &pub)
	}
	h := load(t, root, policy(pub))
	for _, c := range cases {
		mustContain(t, refusal(t, h, c.name), c.want)
	}
}

func TestADirectoryNameThatIsNotThePluginNameIsRefused(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "dirname", wasmasm.Echo(okResponse), manifest("othername", ""), &pub)
	h := load(t, root, policy(pub))
	mustContain(t, refusal(t, h, "dirname"), "othername", "have to match")
}

func TestAModuleThatImportsWhatTheHostDoesNotProvideIsRefused(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "reach", wasmasm.ImportsNothingItShould(), manifest("reach", ""), &pub)
	h := load(t, root, policy(pub))
	reason := refusal(t, h, "reach")
	t.Logf("refusal: %s", reason)
	mustContain(t, reason, "env.reach_out")
}

func TestAModuleThatUsesContextWithoutAskingIsRefused(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "unasked", wasmasm.ContextSet(0, "k", "1", okResponse), manifest("unasked", ""), &pub)
	h := load(t, root, policy(pub))
	reason := refusal(t, h, "unasked")
	t.Logf("refusal: %s", reason)
	mustContain(t, reason, "context_set", "context capability")
}

func TestAModuleThatIsNotWebAssemblyIsRefused(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "junk", []byte("this is not wasm"), manifest("junk", ""), &pub)
	h := load(t, root, policy(pub))
	mustContain(t, refusal(t, h, "junk"), "not usable")
}

func TestAMissingPluginDirectoryIsNotAnError(t *testing.T) {
	h := load(t, filepath.Join(t.TempDir(), "nope"), policy())
	if len(h.Loaded()) != 0 || len(h.Refusals()) != 0 {
		t.Error("something loaded from a directory that isn't there")
	}
}

func TestKeysRoundTripThroughBothTextForms(t *testing.T) {
	pub := newPublisher(t)
	line := plugin.PublicKeyText(pub.pub)
	back, err := plugin.ParsePublicKey(line)
	if err != nil || !back.Equal(pub.pub) {
		t.Fatalf("ed25519: form: %v", err)
	}
	trust, err := plugin.LoadTrust([]string{line})
	if err != nil || trust.Len() != 1 {
		t.Fatalf("LoadTrust: %v", err)
	}
	file := filepath.Join(t.TempDir(), "publisher.pub")
	_, pubPEM, _ := plugin.GenerateKey()
	if err := os.WriteFile(file, pubPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if trust, err = plugin.LoadTrust([]string{file}); err != nil || trust.Len() != 1 {
		t.Fatalf("a key in a file: %v", err)
	}
	if _, err := plugin.LoadTrust([]string{"ed25519:AAAA"}); err == nil {
		t.Error("a short key was accepted")
	}
	if _, err := plugin.LoadTrust([]string{filepath.Join(t.TempDir(), "missing.pub")}); err == nil {
		t.Error("a missing key file was accepted")
	}
}

// ---------------------------------------------------------------------------
// Capabilities
// ---------------------------------------------------------------------------

func flowsWith(nodes ...string) *engine.Flows {
	doc := `[{"id":"t1","type":"tab","label":"t"},` + strings.Join(nodes, ",") + `]`
	f, err := engine.ParseFlows([]byte(doc))
	if err != nil {
		panic(err)
	}
	return f
}

func TestAskingForSomethingNotGrantedFailsTheDeploy(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "stamper", wasmasm.ContextSet(0, "k", "1", okResponse),
		manifest("stamper", `"capabilities":["context.flow","clock"]`), &pub)
	flows := flowsWith(`{"id":"n1","type":"wasm-stamper","z":"t1","name":"stamp","wires":[[]]}`)

	// Signed, trusted, and loaded, but nothing granted.
	h := load(t, root, policy(pub))
	if len(h.Loaded()) != 1 {
		t.Fatalf("it did not load: %v", h.Refusals())
	}
	err := h.Preflight(flows)
	var de *plugin.DeployError
	if !errors.As(err, &de) {
		t.Fatalf("Preflight = %v, want a DeployError", err)
	}
	t.Logf("deploy refusal: %v", err)
	mustContain(t, err.Error(), "stamp", "n1", "context.flow", "clock", "plugins.grants.stamper")

	// Granting one of two is still a refusal, and names only the other.
	p := policy(pub)
	p.Grants = map[string][]plugin.Capability{"stamper": {plugin.CapContextFlow}}
	h = load(t, root, p)
	err = h.Preflight(flows)
	if err == nil {
		t.Fatal("a half-granted plugin deployed")
	}
	if strings.Contains(err.Error(), "context.flow") || !strings.Contains(err.Error(), "not granted: clock.") {
		t.Errorf("the refusal names the wrong capability: %v", err)
	}

	// Granting both lets it through.
	p.Grants = map[string][]plugin.Capability{"stamper": {plugin.CapContextFlow, plugin.CapClock}}
	h = load(t, root, p)
	if err := h.Preflight(flows); err != nil {
		t.Errorf("a fully granted plugin was refused: %v", err)
	}

	// And a grant to a different plugin is no grant at all.
	p.Grants = map[string][]plugin.Capability{"somebody-else": {plugin.CapContextFlow, plugin.CapClock}}
	h = load(t, root, p)
	if err := h.Preflight(flows); err == nil {
		t.Error("a grant to another plugin let this one through")
	}
}

func TestADisabledNodeDoesNotRefuseTheDeploy(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "needy", wasmasm.Echo(okResponse), manifest("needy", `"capabilities":["clock"]`), &pub)
	h := load(t, root, policy(pub))
	flows := flowsWith(`{"id":"n1","type":"wasm-needy","z":"t1","d":true,"wires":[[]]}`)
	if err := h.Preflight(flows); err != nil {
		t.Errorf("a disabled node refused the deploy: %v", err)
	}
}

func TestADeployUsingARefusedPluginSaysWhyItWasRefused(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "loose", wasmasm.Echo(okResponse), manifest("loose", ""), nil)
	h := load(t, root, policy(pub))
	err := h.Preflight(flowsWith(`{"id":"n1","type":"wasm-loose","z":"t1","wires":[[]]}`))
	if err == nil {
		t.Fatal("a flow using an unsigned plugin deployed")
	}
	t.Logf("deploy refusal: %v", err)
	mustContain(t, err.Error(), "did not load", "unsigned")
}

func TestANodeAskingForMoreThanTheHostAllowsFailsTheDeploy(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "greedy", wasmasm.Echo(okResponse), manifest("greedy", ""), &pub)
	h := load(t, root, policy(pub))

	err := h.Preflight(flowsWith(`{"id":"n1","type":"wasm-greedy","z":"t1","memoryMiB":512,"wires":[[]]}`))
	if err == nil {
		t.Fatal("512 MiB was allowed against a 64 MiB ceiling")
	}
	mustContain(t, err.Error(), "512 MiB", "at most 64", "plugins.maxMemoryMiB")

	err = h.Preflight(flowsWith(`{"id":"n1","type":"wasm-greedy","z":"t1","timeoutMs":60000,"wires":[[]]}`))
	if err == nil {
		t.Fatal("a minute was allowed against a 5 s ceiling")
	}
	mustContain(t, err.Error(), "60000 ms", "plugins.maxTimeout")
}

func TestGrantsAreChecked(t *testing.T) {
	if _, err := plugin.ParseGrants(map[string][]string{"scale": {"clock", "network"}}); err == nil ||
		!strings.Contains(err.Error(), "plugins.grants.scale") {
		t.Errorf("ParseGrants = %v", err)
	}
	if _, err := plugin.ParseGrants(map[string][]string{"Bad Name": {"clock"}}); err == nil {
		t.Error("a bad plugin name was accepted")
	}
	got, err := plugin.ParseGrants(map[string][]string{"scale": {"clock", "random"}})
	if err != nil || len(got["scale"]) != 2 {
		t.Errorf("ParseGrants = %v, %v", got, err)
	}
}

// ---------------------------------------------------------------------------
// Running in a flow
// ---------------------------------------------------------------------------

// registryWith is the real palette plus this host's plugin types.
func registryWith(t *testing.T, h *plugin.Host) *node.Registry {
	t.Helper()
	reg := node.NewRegistry()
	for _, d := range node.Default.Descriptors() {
		r, _ := node.Default.Lookup(d.Type)
		if err := reg.Register(d, r.New); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Register(reg); err != nil {
		t.Fatal(err)
	}
	return reg
}

func runSuite(t *testing.T, reg *node.Registry, flows, suite string) flowtest.Report {
	t.Helper()
	s, err := flowtest.Parse([]byte(suite))
	if err != nil {
		t.Fatal(err)
	}
	return flowtest.Run(context.Background(), []byte(flows), s, flowtest.Options{Registry: reg})
}

func requirePass(t *testing.T, r flowtest.Report) {
	t.Helper()
	for _, res := range r.Tests {
		if res.Status != flowtest.Pass {
			var b strings.Builder
			for _, p := range res.Problems {
				b.WriteString(p.String() + "\n")
			}
			for _, l := range res.Log {
				b.WriteString("log: " + l + "\n")
			}
			t.Errorf("%q came out %s\n%s", res.Name, res.Status, b.String())
		}
	}
	if len(r.Tests) == 0 {
		t.Fatal("no tests ran")
	}
}

// guestFlow has a plugin node, a healthy plugin node and a built-in chain, all
// in one tab with one Catch node, so what one does to the others is visible.
func guestFlow(bad string, badProps string) string {
	return `[
  {"id":"t1","type":"tab","label":"plant"},
  {"id":"in-bad","type":"inject","z":"t1","name":"in bad","wires":[["bad"]]},
  {"id":"bad","type":"` + bad + `","z":"t1","name":"bad",` + badProps + `,"wires":[["bad-out"]]},
  {"id":"bad-out","type":"debug","z":"t1","name":"bad out","wires":[]},
  {"id":"in-good","type":"inject","z":"t1","name":"in good","wires":[["good"]]},
  {"id":"good","type":"wasm-good","z":"t1","name":"good","wires":[["good-out"]]},
  {"id":"good-out","type":"debug","z":"t1","name":"good out","wires":[]},
  {"id":"in-plain","type":"inject","z":"t1","name":"in plain","wires":[["plain"]]},
  {"id":"plain","type":"change","z":"t1","name":"plain","rules":[{"t":"set","p":"payload","pt":"msg","to":"still here","tot":"str"}],"wires":[["plain-out"]]},
  {"id":"plain-out","type":"debug","z":"t1","name":"plain out","wires":[]},
  {"id":"errors","type":"catch","z":"t1","name":"errors","wires":[["errors-out"]]},
  {"id":"errors-out","type":"debug","z":"t1","name":"errors out","wires":[]}
]`
}

func installTrapPlugins(t *testing.T) *plugin.Host {
	t.Helper()
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "good", wasmasm.Echo(okResponse), manifest("good", ""), &pub)
	install(t, root, "hog", wasmasm.Hog(), manifest("hog", ""), &pub)
	install(t, root, "spin", wasmasm.Spin(), manifest("spin", ""), &pub)
	return load(t, root, policy(pub))
}

func TestAGuestPastItsMemoryCeilingGoesToCatchAndNothingElseNoticesInAFlow(t *testing.T) {
	h := installTrapPlugins(t)
	if len(h.Loaded()) != 3 {
		t.Fatalf("loaded %v, refused %v", h.Loaded(), h.Refusals())
	}
	r := runSuite(t, registryWith(t, h), guestFlow("wasm-hog", `"memoryMiB":32`), `
tests:
  - name: the hog traps, and the rest of the plant carries on
    timeout: 20s
    inject:
      - node: in bad
      - node: in good
      - node: in plain
    expect:
      - node: errors
        error: exceeded its memory limit
      - node: errors out
        msg: {error: {source: {name: bad}}}
      - node: good out
        msg: {payload: fine}
      - node: plain out
        msg: {payload: still here}
      - node: bad out
        nothing: true
`)
	requirePass(t, r)
	for _, res := range r.Tests {
		t.Logf("flow test %q: %s", res.Name, res.Status)
	}
}

func TestAGuestThatLoopsPastItsTimeoutGoesToCatchAndNothingElseNoticesInAFlow(t *testing.T) {
	h := installTrapPlugins(t)
	r := runSuite(t, registryWith(t, h), guestFlow("wasm-spin", `"timeoutMs":150`), `
tests:
  - name: the spinner is stopped, and the rest of the plant carries on
    timeout: 20s
    inject:
      - node: in bad
      - node: in good
      - node: in plain
    expect:
      - node: errors
        error: timed out
      - node: good out
        msg: {payload: fine}
      - node: plain out
        msg: {payload: still here}
      - node: bad out
        nothing: true
`)
	requirePass(t, r)
}

func TestAPluginRunsInAFlowWithItsConfigAndContext(t *testing.T) {
	root := t.TempDir()
	pub := newPublisher(t)
	install(t, root, "remember", wasmasm.ContextSet(0, "last", `{"seen":true}`, okResponse),
		manifest("remember", `"capabilities":["context.flow"]`), &pub)
	p := policy(pub)
	p.Grants = map[string][]plugin.Capability{"remember": {plugin.CapContextFlow}}
	h := load(t, root, p)

	flows := `[
  {"id":"t1","type":"tab","label":"plant"},
  {"id":"in","type":"inject","z":"t1","name":"in","wires":[["rem"]]},
  {"id":"rem","type":"wasm-remember","z":"t1","name":"rem","wires":[["out"]]},
  {"id":"out","type":"debug","z":"t1","name":"out","wires":[]}
]`
	r := runSuite(t, registryWith(t, h), flows, `
tests:
  - name: a granted plugin writes the flow context
    inject:
      - node: in
    expect:
      - node: out
        msg: {payload: fine}
      - context:
          flow:
            plant: {last: {seen: true}}
`)
	requirePass(t, r)

	// The same flow with the grant taken away does not deploy, and so does not start.
	flowsDoc, err := engine.ParseFlows([]byte(flows))
	if err != nil {
		t.Fatal(err)
	}
	h2 := load(t, root, policy(pub))
	if err := h2.Preflight(flowsDoc); err == nil {
		t.Error("the ungranted plugin was allowed to deploy")
	}
	// Belt and braces: even if a flow reached the runtime some other way, the
	// node's own factory refuses it.
	r2 := runSuite(t, registryWith(t, h2), flows, `
tests:
  - name: ungranted
    inject:
      - node: in
    expect:
      - node: out
        nothing: true
`)
	if len(r2.Tests) != 1 || r2.Tests[0].Status != flowtest.Error {
		t.Fatalf("the ungranted node started: %+v", r2.Tests)
	}
	t.Logf("with no grant, the node did not start: %s, %v", r2.Tests[0].Status, r2.Tests[0].Problems)
}
