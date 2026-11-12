package nodes

import (
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// NetworkInterfaceEnv names the variable that gives a tab's client nodes their
// network interface. Set it in the tab's environment variables (the editor's tab
// dialog), in a subflow's, or in the process environment, and every node on the
// tab that connects out on a plant network binds its sockets to that interface
// with SO_BINDTODEVICE, unless the node names one of its own.
//
// It is the answer for a box whose one port carries a VLAN for each machine,
// every VLAN with the same subnet and the same device addresses: a tab
// for dryer 14 sets FLOW_NETWORK_INTERFACE to enp2s0.14 and reaches only dryer
// 14's devices.
const NetworkInterfaceEnv = "FLOW_NETWORK_INTERFACE"

// networkInterfaceFor resolves the interface a node's connections use: its own
// setting, else the tab's (or subflow's, or the process's) variable. A config
// node's own setting is the next fallback and is applied by the config node, so
// "" means neither of these named one.
func networkInterfaceFor(svc node.Services, own string) string {
	if own = strings.TrimSpace(own); own != "" {
		return own
	}
	if v, ok := svc.Env(NetworkInterfaceEnv); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// networkInterfaceProp is the editor field every network client carries.
func networkInterfaceProp(help string) node.Prop {
	return node.Prop{Name: "networkInterface", Kind: node.PropString, Label: "Network interface",
		Placeholder: "enp2s0.14", Help: help}
}
