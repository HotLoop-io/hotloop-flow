package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/wasmhost"
)

// Policy is what the operator decides: whose signatures to believe, what each
// plugin may use, and how much of the machine any one node can take.
type Policy struct {
	Trust *Trust

	// Grants maps a plugin's name to the capabilities the operator gives it.
	Grants map[string][]Capability

	// MaxMemoryMiB and MaxTimeout cap what a node may ask for.
	MaxMemoryMiB int
	MaxTimeout   time.Duration
}

// ParseGrants checks the configured grants, naming the plugin and the entry
// that is wrong.
func ParseGrants(in map[string][]string) (map[string][]Capability, error) {
	out := make(map[string][]Capability, len(in))
	for name, caps := range in {
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("plugins.grants: %q is not a plugin name", name)
		}
		for _, c := range caps {
			capability, err := ParseCapability(c)
			if err != nil {
				return nil, fmt.Errorf("plugins.grants.%s: %w", name, err)
			}
			out[name] = append(out[name], capability)
		}
	}
	return out, nil
}

// Plugin is a plugin that passed its signature check.
type Plugin struct {
	Manifest   Manifest
	Dir        string
	SignedBy   string
	Module     []byte
	Needs      []Capability
	Descriptor node.Descriptor
}

// Refused is a plugin directory that did not load, and why.
type Refused struct {
	Dir    string `json:"dir"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Host holds every plugin found at startup. It is read-only once built.
type Host struct {
	dir     string
	policy  Policy
	cache   wazero.CompilationCache
	plugins map[string]*Plugin // by name
	refused map[string]Refused // by name, which is the directory name
}

// Load reads every plugin directory under dir. A missing dir is not an error:
// most instances have no plugins.
//
// Each plugin ends up in exactly one of two places, loaded or refused with a
// reason, and nothing in between. A refused plugin is never compiled, so
// unsigned code is never even parsed past its descriptor.
func Load(ctx context.Context, dir string, policy Policy) (*Host, error) {
	h := &Host{
		dir: dir, policy: policy,
		cache:   wazero.NewCompilationCache(),
		plugins: map[string]*Plugin{}, refused: map[string]Refused{},
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the plugin directory: %w", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		// A ConfigMap or Secret volume lays a directory out as a symlink to the
		// kubelet's own tree, so what is a directory is asked of the target.
		if info, err := os.Stat(filepath.Join(dir, e.Name())); err != nil || !info.IsDir() {
			continue
		}
		p, err := h.loadOne(ctx, filepath.Join(dir, e.Name()))
		if err != nil {
			h.refused[e.Name()] = Refused{Dir: filepath.Join(dir, e.Name()), Name: e.Name(), Reason: err.Error()}
			continue
		}
		h.plugins[p.Manifest.Name] = p
	}
	return h, nil
}

// Close releases the shared compilation cache.
func (h *Host) Close(ctx context.Context) error { return h.cache.Close(ctx) }

func (h *Host) loadOne(ctx context.Context, dir string) (*Plugin, error) {
	descriptor, err := readFile(dir, DescriptorFile)
	if err != nil {
		return nil, err
	}
	module, err := readFile(dir, ModuleFile)
	if err != nil {
		return nil, err
	}
	sig, err := readOptional(dir, SignatureFile)
	if err != nil {
		return nil, err
	}

	// The signature is checked before the descriptor is looked at, so what a
	// person is told about an unsigned plugin is that it is unsigned, whatever
	// else is also wrong with it.
	keyID, err := h.policy.Trust.Verify(sig, module, descriptor)
	if err != nil {
		return nil, err
	}

	m, err := ParseManifest(descriptor)
	if err != nil {
		return nil, err
	}
	if m.Name != filepath.Base(dir) {
		return nil, fmt.Errorf("the directory is called %q and %s says the plugin is %q; they have to match, "+
			"because that name is what plugins.grants and the flow file use", filepath.Base(dir), DescriptorFile, m.Name)
	}

	needs := make([]Capability, 0, len(m.Capabilities))
	for _, c := range m.Capabilities {
		capability, _ := ParseCapability(c)
		needs = append(needs, capability)
	}

	p := &Plugin{Manifest: *m, Dir: dir, SignedBy: keyID, Module: module, Needs: needs}
	p.Descriptor = p.descriptor()
	if err := p.Descriptor.Validate(); err != nil {
		return nil, fmt.Errorf("the descriptor does not make a valid node: %w", err)
	}

	// Compiled once here with the most it could be given, so a module that is
	// not valid WebAssembly, lacks the entry points, or imports something the
	// host does not provide is refused at startup, and so the machine code is
	// in the cache when the first node asks for it.
	probe, err := wasmhost.CompileWith(ctx, m.Name, module,
		wasmhost.Limits{MaxMemoryBytes: uint64(h.policy.MaxMemoryMiB) << 20}, wasmhost.Options{Capabilities: hostCaps(needs), Cache: h.cache})
	if err != nil {
		return nil, fmt.Errorf("the module is not usable: %w", err)
	}
	_ = probe.Close(ctx)
	return p, nil
}

func readFile(dir, name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s is missing", name)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return data, nil
}

func readOptional(dir, name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return data, nil
}

func hostCaps(needs []Capability) wasmhost.Capabilities {
	var c wasmhost.Capabilities
	for _, n := range needs {
		switch n {
		case CapClock:
			c.Clock = true
		case CapRandom:
			c.Random = true
		case CapContextFlow:
			c.ContextFlow = true
		case CapContextGlobal:
			c.ContextGlobal = true
		}
	}
	return c
}

// Missing is what the plugin asks for that the operator has not granted.
func (h *Host) Missing(p *Plugin) []Capability {
	granted := map[Capability]bool{}
	for _, c := range h.policy.Grants[p.Manifest.Name] {
		granted[c] = true
	}
	var missing []Capability
	for _, n := range p.Needs {
		if !granted[n] {
			missing = append(missing, n)
		}
	}
	return missing
}

func capList(cs []Capability) string {
	s := make([]string, len(cs))
	for i, c := range cs {
		s[i] = string(c)
	}
	return strings.Join(s, ", ")
}

// Register puts every loaded plugin's node type into reg, which must happen
// before the runtime starts.
func (h *Host) Register(reg *node.Registry) error {
	for _, name := range h.names() {
		p := h.plugins[name]
		if err := reg.Register(p.Descriptor, func(def *node.Definition) (node.Node, error) {
			return h.newNode(p, def)
		}); err != nil {
			return fmt.Errorf("plugin %s: %w", name, err)
		}
	}
	return nil
}

func (h *Host) names() []string {
	out := make([]string, 0, len(h.plugins))
	for n := range h.plugins {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Problem is one node a deploy was refused for.
type Problem struct {
	NodeID string `json:"nodeId"`
	Node   string `json:"node,omitempty"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// DeployError is a deploy refused because of its plugins. Nothing was written.
type DeployError struct{ Problems []Problem }

func (e *DeployError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		who := p.NodeID
		if p.Node != "" {
			who = fmt.Sprintf("%s (%s)", p.Node, p.NodeID)
		}
		parts[i] = fmt.Sprintf("node %s is a %s: %s", who, p.Type, p.Reason)
	}
	return "the deploy was refused because of its plugins: " + strings.Join(parts, "; ")
}

// Preflight refuses a flow set that uses a plugin that isn't allowed to run:
// one that was refused at startup, one asking for something the operator has
// not granted, or a node asking for more than the host will give. It runs
// before the flow file is written, so a refused deploy changes nothing.
//
// Disabled nodes are skipped, because they never start.
func (h *Host) Preflight(flows *engine.Flows) error {
	var problems []Problem
	ids := make([]string, 0, len(flows.Nodes))
	for id := range flows.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := flows.Nodes[id]
		if n.Disabled || !strings.HasPrefix(n.Type, TypePrefix) {
			continue
		}
		name := strings.TrimPrefix(n.Type, TypePrefix)
		if r, ok := h.refused[name]; ok {
			problems = append(problems, Problem{NodeID: id, Node: n.Name, Type: n.Type,
				Reason: fmt.Sprintf("the plugin %q did not load: %s", name, r.Reason)})
			continue
		}
		p, ok := h.plugins[name]
		if !ok {
			continue // unknown type: the runtime reports it the way it reports any
		}
		if err := h.admit(p, n); err != nil {
			problems = append(problems, Problem{NodeID: id, Node: n.Name, Type: n.Type, Reason: err.Error()})
		}
	}
	if len(problems) > 0 {
		return &DeployError{Problems: problems}
	}
	return nil
}

// admit is every check on one node that does not need the module running.
func (h *Host) admit(p *Plugin, n *engine.Node) error {
	if missing := h.Missing(p); len(missing) > 0 {
		return fmt.Errorf("the plugin asks for host access the operator has not granted: %s. "+
			"Add it to plugins.grants.%s, or remove the capability from the plugin", capList(missing), p.Manifest.Name)
	}
	mem, timeout := nodeLimits(p, n)
	if mem > h.policy.MaxMemoryMiB {
		return fmt.Errorf("it asks for %d MiB of memory and this host allows at most %d (plugins.maxMemoryMiB)", mem, h.policy.MaxMemoryMiB)
	}
	if time.Duration(timeout)*time.Millisecond > h.policy.MaxTimeout {
		return fmt.Errorf("it asks for a %d ms timeout and this host allows at most %s (plugins.maxTimeout)", timeout, h.policy.MaxTimeout)
	}
	if mem < 1 || timeout < 1 {
		return fmt.Errorf("the memory limit and the timeout must each be at least 1")
	}
	return nil
}

func nodeLimits(p *Plugin, n *engine.Node) (memMiB, timeoutMs int) {
	return n.PropInt(propMemory, p.Manifest.memoryMiB()), n.PropInt(propTimeout, p.Manifest.timeoutMs())
}

// Report is what GET /plugins says.
type Report struct {
	Dir          string             `json:"dir"`
	TrustedKeys  []string           `json:"trustedKeys"`
	Plugins      []ReportPlugin     `json:"plugins"`
	Refused      []Refused          `json:"refused"`
	Capabilities []ReportCapability `json:"capabilities"`
}

// ReportPlugin is one loaded plugin.
type ReportPlugin struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Type         string   `json:"type"`
	Description  string   `json:"description,omitempty"`
	SignedBy     string   `json:"signedBy"`
	Capabilities []string `json:"capabilities"`
	Granted      []string `json:"granted"`
	// NotGranted is what it asks for and has not been given. A flow using the
	// plugin does not deploy while this is not empty.
	NotGranted []string `json:"notGranted"`
}

// ReportCapability describes one capability.
type ReportCapability struct {
	Name string `json:"name"`
	What string `json:"what"`
}

// Report lists what loaded and what was refused, with the reasons.
func (h *Host) Report() Report {
	r := Report{Dir: h.dir, TrustedKeys: h.policy.Trust.IDs(), Plugins: []ReportPlugin{}, Refused: []Refused{}}
	for _, name := range h.names() {
		p := h.plugins[name]
		rp := ReportPlugin{
			Name: name, Version: p.Manifest.Version, Type: p.Manifest.Type(),
			Description: p.Manifest.Description, SignedBy: p.SignedBy,
			Capabilities: []string{}, Granted: []string{}, NotGranted: []string{},
		}
		for _, c := range p.Needs {
			rp.Capabilities = append(rp.Capabilities, string(c))
		}
		for _, c := range h.policy.Grants[name] {
			rp.Granted = append(rp.Granted, string(c))
		}
		for _, c := range h.Missing(p) {
			rp.NotGranted = append(rp.NotGranted, string(c))
		}
		r.Plugins = append(r.Plugins, rp)
	}
	names := make([]string, 0, len(h.refused))
	for n := range h.refused {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r.Refused = append(r.Refused, h.refused[n])
	}
	for _, c := range Capabilities {
		r.Capabilities = append(r.Capabilities, ReportCapability{Name: string(c.Name), What: c.What})
	}
	return r
}

// Refusals returns the refused plugins, for the startup log.
func (h *Host) Refusals() []Refused { return h.Report().Refused }

// Loaded returns the loaded plugins' names.
func (h *Host) Loaded() []string { return h.names() }

// Inspect runs every load-time check on one plugin directory and returns the
// plugin or the reason it would be refused. It is what `hotloop-flow plugin
// verify` runs, so a publisher can see the refusal an operator would get.
func Inspect(ctx context.Context, dir string, policy Policy) (*Plugin, error) {
	if policy.MaxMemoryMiB < 1 {
		policy.MaxMemoryMiB = 64
	}
	h := &Host{dir: filepath.Dir(dir), policy: policy, cache: wazero.NewCompilationCache(),
		plugins: map[string]*Plugin{}, refused: map[string]Refused{}}
	defer h.Close(ctx)
	return h.loadOne(ctx, dir)
}

// NotGranted lists what a plugin asks for and a policy has not granted.
func NotGranted(p *Plugin, policy Policy) []Capability {
	h := &Host{policy: policy}
	return h.Missing(p)
}
