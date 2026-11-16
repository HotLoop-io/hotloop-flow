//go:build linux

package diagram

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// applyFlow sets the flow control of an open port: none, hardware (RTS and CTS)
// or software (XON and XOFF). The serial library this package opens ports with
// has no flow control, so the setting is made on the terminal itself. Terminal
// settings belong to the device and not to a file descriptor, so a second open of
// the same device sets what the first one reads and writes with.
func applyFlow(device, flow string) error {
	// None is a setting too: a port another program left with flow control on is
	// opened without it when the diagram says none.
	if flow == "" {
		flow = "none"
	}
	if flow != "none" && flow != "rtscts" && flow != "xonxoff" {
		return fmt.Errorf("flow control %q is not none, rtscts or xonxoff", flow)
	}
	f, err := os.OpenFile(device, os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("cannot set flow control on %s: %w", device, err)
	}
	defer f.Close()
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return fmt.Errorf("cannot read the settings of %s: %w", device, err)
	}
	t.Cflag &^= unix.CRTSCTS
	t.Iflag &^= unix.IXON | unix.IXOFF | unix.IXANY
	switch flow {
	case "rtscts":
		t.Cflag |= unix.CRTSCTS
	case "xonxoff":
		t.Iflag |= unix.IXON | unix.IXOFF
		t.Cc[unix.VSTART], t.Cc[unix.VSTOP] = 0x11, 0x13
	}
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		return fmt.Errorf("cannot set flow control on %s: %w", device, err)
	}
	return nil
}
