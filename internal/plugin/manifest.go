// Package plugin loads WebAssembly plugins and turns each into a node type.
//
// Node-RED's community nodes are npm packages. They need Node.js on the box, and
// once installed they run in the runtime's own process with everything it can
// reach. Flow's answer is a plugin that is a WebAssembly module, which can only
// compute on what it is handed, plus a descriptor the editor draws its dialog
// from, plus a signature, and which gets host access only when its descriptor
// asks and the operator says yes.
//
// A plugin is a directory with three files:
//
//	plugin.wasm   the module, implementing the ABI in internal/wasmhost
//	plugin.json   the descriptor: name, dialog, capabilities, limits
//	plugin.sig    an ed25519 signature over both
//
// Nothing here is read at message time. Every check happens when the process
// starts or when a flow is deployed, so a plugin that is going to be refused is
// refused where somebody is looking.
package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Files a plugin directory holds.
const (
	ModuleFile     = "plugin.wasm"
	DescriptorFile = "plugin.json"
	SignatureFile  = "plugin.sig"
)

// FormatVersion is the descriptor and signature format this build reads.
const FormatVersion = 1

// TypePrefix is what every plugin's node type starts with. The rest is the
// plugin's name, so one plugin is one node type and the flow file says which.
const TypePrefix = "wasm-"

// What a plugin may use when it doesn't say, and the most it may be given.
const (
	DefaultMemoryMiB = 16
	DefaultTimeoutMs = 1000
)

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// Manifest is plugin.json.
type Manifest struct {
	Format  int    `json:"format"`
	Name    string `json:"name"`
	Version string `json:"version"`

	// Label is the palette label. Defaults to the name.
	Label string `json:"label,omitempty"`
	// Description is one line, for the palette and the plugin list.
	Description string `json:"description,omitempty"`
	// Help is Markdown for the editor's info sidebar.
	Help string `json:"help,omitempty"`

	// Inputs and Outputs default to one each. Zero inputs is a node that never
	// receives a message, which a guest with no way to start itself can't use,
	// so inputs must be 1.
	Inputs  *int `json:"inputs,omitempty"`
	Outputs *int `json:"outputs,omitempty"`

	// Props are the fields of the edit dialog. The editor draws them from this
	// list exactly as it does for a built-in node, and the values reach the
	// guest as the config of every message.
	Props []node.Prop `json:"props,omitempty"`

	// Capabilities are the host access the plugin asks for.
	Capabilities []string `json:"capabilities,omitempty"`

	// Limits are what the plugin says it needs. The operator's ceiling still
	// applies, and each node can set its own within it.
	Limits Limits `json:"limits,omitempty"`
}

// Limits are a plugin's requested defaults.
type Limits struct {
	MemoryMiB int `json:"memoryMiB,omitempty"`
	TimeoutMs int `json:"timeoutMs,omitempty"`
}

// The names a plugin's own props may not take: the node's name, which every
// node has, and the two limits the plugin node adds.
const (
	propName    = "name"
	propMemory  = "memoryMiB"
	propTimeout = "timeoutMs"
)

// ParseManifest reads plugin.json strictly. An unknown field is an error, so a
// misspelled "capabilites" is a refusal and not a plugin that quietly asked for
// nothing.
func ParseManifest(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", DescriptorFile, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s has more than one JSON document in it", DescriptorFile)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", DescriptorFile, err)
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if m.Format != FormatVersion {
		return fmt.Errorf("format is %d; this build reads format %d", m.Format, FormatVersion)
	}
	if !nameRE.MatchString(m.Name) {
		return fmt.Errorf("name %q must be lowercase letters, digits and hyphens, starting with a letter, at most 40 characters", m.Name)
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("version is empty")
	}
	if m.Inputs != nil && *m.Inputs != 1 {
		return fmt.Errorf("inputs is %d; a plugin node takes one input", *m.Inputs)
	}
	if m.Outputs != nil && (*m.Outputs < 0 || *m.Outputs > 16) {
		return fmt.Errorf("outputs is %d; it must be between 0 and 16", *m.Outputs)
	}
	if m.Limits.MemoryMiB < 0 || m.Limits.TimeoutMs < 0 {
		return fmt.Errorf("limits must not be negative")
	}
	seen := map[Capability]bool{}
	for _, c := range m.Capabilities {
		capability, err := ParseCapability(c)
		if err != nil {
			return err
		}
		if seen[capability] {
			return fmt.Errorf("capability %q is listed twice", c)
		}
		seen[capability] = true
	}
	return m.validateProps()
}

// A plugin's dialog is built from these kinds only. The rest either reach
// outside the process (a config node reference, a credential) or run code of
// their own in the editor's languages, and a plugin has no business with either.
func (m *Manifest) validateProps() error {
	var check func(props []node.Prop, path string) error
	check = func(props []node.Prop, path string) error {
		for _, p := range props {
			switch p.Kind {
			case node.PropString, node.PropText, node.PropNumber, node.PropBool, node.PropSelect, node.PropJSON:
			case node.PropList:
				if err := check(p.Fields, path+p.Name+"."); err != nil {
					return err
				}
			default:
				return fmt.Errorf("property %s%s has kind %q, which a plugin can't use; "+
					"the kinds are string, text, number, bool, select, json and list", path, p.Name, p.Kind)
			}
		}
		return nil
	}
	if err := check(m.Props, ""); err != nil {
		return err
	}
	for _, p := range m.Props {
		switch p.Name {
		case propName, propMemory, propTimeout:
			return fmt.Errorf("property %q is added to every plugin node by Flow; use another name", p.Name)
		}
		if strings.HasPrefix(p.Name, "ew_") {
			return fmt.Errorf("property %q starts with ew_, which Flow keeps for its own per-node settings", p.Name)
		}
	}
	return nil
}

// Type is the node type the plugin registers.
func (m *Manifest) Type() string { return TypePrefix + m.Name }

func (m *Manifest) outputs() int {
	if m.Outputs == nil {
		return 1
	}
	return *m.Outputs
}

func (m *Manifest) memoryMiB() int {
	if m.Limits.MemoryMiB > 0 {
		return m.Limits.MemoryMiB
	}
	return DefaultMemoryMiB
}

func (m *Manifest) timeoutMs() int {
	if m.Limits.TimeoutMs > 0 {
		return m.Limits.TimeoutMs
	}
	return DefaultTimeoutMs
}

// Capability is one thing a plugin may ask for and an operator may grant.
type Capability string

// The capabilities. Each is wired into the host only when it is granted, and a
// plugin that asks for one that isn't granted does not deploy.
const (
	// CapClock gives the plugin the real clock. Without it, time stands still.
	CapClock Capability = "clock"
	// CapRandom gives the plugin the operating system's random bytes. Without
	// it, its random numbers are the same sequence every time.
	CapRandom Capability = "random"
	// CapContextFlow lets the plugin read and write the flow context.
	CapContextFlow Capability = "context.flow"
	// CapContextGlobal lets the plugin read and write the global context.
	CapContextGlobal Capability = "context.global"
)

// Capabilities lists every capability with what it gives, for the plugin list
// and for the error a mistyped name gets.
var Capabilities = []struct {
	Name Capability
	What string
}{
	{CapClock, "the real wall clock and monotonic clock"},
	{CapRandom, "the operating system's random bytes"},
	{CapContextFlow, "read and write the flow context"},
	{CapContextGlobal, "read and write the global context"},
}

// ParseCapability checks a name against the list.
func ParseCapability(s string) (Capability, error) {
	for _, c := range Capabilities {
		if string(c.Name) == s {
			return c.Name, nil
		}
	}
	names := make([]string, len(Capabilities))
	for i, c := range Capabilities {
		names[i] = string(c.Name)
	}
	return "", fmt.Errorf("capability %q is not one Flow has; the capabilities are %s", s, strings.Join(names, ", "))
}
