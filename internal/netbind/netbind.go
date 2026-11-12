// Package netbind opens network connections bound to one network interface.
//
// A box with one port that carries many VLANs has a subinterface for each, and
// the same subnet on all of them: the plant's devices at 192.168.1.5 to .10 sit
// on every dryer's VLAN, twelve times over. To the kernel those are twelve
// routes to one destination. A socket bound to enp2s0.14 with SO_BINDTODEVICE
// routes and resolves addresses on that interface and no other, so the same
// address reaches the right device.
//
// The binding is per socket, set before connect. Everything that connects out on
// a plant network goes through Dialer (or Control, for a library that builds its
// own sockets), and an empty interface name means the kernel chooses, as it
// always did.
//
// Linux only. On the kernels Flow ships on (5.7 and later) an unprivileged
// process may bind a socket that is not yet bound to a device. Before 5.7 it
// takes CAP_NET_RAW, and the chart adds that capability when asked to.
package netbind

import (
	"context"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"
)

// MaxNameLen is the longest interface name Linux allows (IFNAMSIZ less the
// terminating NUL).
const MaxNameLen = 15

// Validate checks an interface name for the mistakes that are plain from the
// text: too long, or characters the kernel never allows in one. Whether the
// interface exists is only known when a socket is opened.
func Validate(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("network interface %q is %d characters, and Linux allows %d at most", name, len(name), MaxNameLen)
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/ \t\r\n\x00") {
		return fmt.Errorf("network interface %q is not a name Linux allows (no slash, no spaces)", name)
	}
	return nil
}

// Control returns a net.Dialer Control function that binds each socket to the
// named interface, or nil when the name is empty.
func Control(iface string) func(network, address string, c syscall.RawConn) error {
	if iface == "" {
		return nil
	}
	return func(network, address string, c syscall.RawConn) error {
		return bind(iface, c)
	}
}

// Dialer returns a dialer whose sockets are bound to the interface.
func Dialer(iface string, timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: Control(iface)}
}

// DialTimeout is net.DialTimeout on one interface.
func DialTimeout(iface, network, address string, timeout time.Duration) (net.Conn, error) {
	conn, err := Dialer(iface, timeout).Dial(network, address)
	return conn, Explain(iface, err)
}

// DialContext is a context-aware dial on one interface.
func DialContext(ctx context.Context, iface, network, address string) (net.Conn, error) {
	conn, err := Dialer(iface, 0).DialContext(ctx, network, address)
	return conn, Explain(iface, err)
}

// ListenUDP opens a UDP socket bound to an interface.
func ListenUDP(iface string, local *net.UDPAddr) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: Control(iface)}
	pc, err := lc.ListenPacket(context.Background(), "udp4", local.String())
	if err != nil {
		return nil, Explain(iface, err)
	}
	return pc.(*net.UDPConn), nil
}

// Explain says what a failure on a bound socket means to the person reading it.
// With no interface it hands the error back untouched.
func Explain(iface string, err error) error {
	if err == nil || iface == "" {
		return err
	}
	switch {
	case isErrno(err, syscall.ENODEV):
		return fmt.Errorf("network interface %s does not exist on this host (or is not up yet): %w", iface, err)
	case isErrno(err, syscall.EPERM), isErrno(err, syscall.EACCES):
		return fmt.Errorf("not allowed to bind to network interface %s. A kernel before 5.7 needs CAP_NET_RAW for this, "+
			"and a container needs the capability added and the host's network: %w", iface, err)
	case isErrno(err, syscall.ENETUNREACH), isErrno(err, syscall.EHOSTUNREACH):
		return fmt.Errorf("nothing on network interface %s reaches that address. Check the link is up, the "+
			"interface has an address in the device's subnet, and the cable: %w", iface, err)
	}
	return fmt.Errorf("on network interface %s: %w", iface, err)
}

func isErrno(err error, want syscall.Errno) bool {
	for err != nil {
		if e, ok := err.(syscall.Errno); ok {
			return e == want
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
