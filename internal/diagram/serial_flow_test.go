//go:build linux

package diagram

import (
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// I3: a serial port has flow control, none, hardware or software, and a diagram's
// choice reaches the terminal. A pseudo terminal shares its settings between its two
// ends, so reading them on the master is reading what the port is opened with. What
// the RTS and CTS lines do on a real adapter is not shown by a pty.

func flowOf(t *testing.T, flow string, before func(fd int)) (cflag, iflag uint32) {
	t.Helper()
	m, slave := ptyPair(t)
	if before != nil {
		before(int(m.Fd()))
	}
	s := &serialSession{cfg: SerialConfig{Device: slave, Baud: 9600, DataBits: 8, StopBits: 1, Parity: "N", Flow: flow}}
	t.Cleanup(func() { _ = s.Close() })
	s.mu.Lock()
	err := s.ensure()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	tio, err := unix.IoctlGetTermios(int(m.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return uint32(tio.Cflag), uint32(tio.Iflag)
}

func TestHardwareFlowControlReachesThePort(t *testing.T) {
	c, i := flowOf(t, "rtscts", nil)
	if c&unix.CRTSCTS == 0 {
		t.Fatalf("CRTSCTS not set, cflag %#x", c)
	}
	if i&(unix.IXON|unix.IXOFF) != 0 {
		t.Fatalf("software flow control is on with hardware chosen, iflag %#x", i)
	}
}

func TestSoftwareFlowControlReachesThePort(t *testing.T) {
	c, i := flowOf(t, "xonxoff", nil)
	if i&unix.IXON == 0 || i&unix.IXOFF == 0 {
		t.Fatalf("XON and XOFF not both set, iflag %#x", i)
	}
	if c&unix.CRTSCTS != 0 {
		t.Fatalf("hardware flow control is on with software chosen, cflag %#x", c)
	}
}

// None clears what another program left on the port.
func TestNoFlowControlClearsWhatWasLeftOnThePort(t *testing.T) {
	c, i := flowOf(t, "none", func(fd int) {
		tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		tio.Cflag |= unix.CRTSCTS
		tio.Iflag |= unix.IXON | unix.IXOFF
		if err := unix.IoctlSetTermios(fd, unix.TCSETS, tio); err != nil {
			t.Fatal(err)
		}
	})
	if c&unix.CRTSCTS != 0 || i&(unix.IXON|unix.IXOFF) != 0 {
		t.Fatalf("flow control left on: cflag %#x iflag %#x", c, i)
	}
}

func TestAnInstrumentAddressTakesFlowControl(t *testing.T) {
	s, err := parseAddress("serial:///dev/ttyUSB0?baud=19200&flow=rtscts")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.(*serialSession).cfg.Flow; got != "rtscts" {
		t.Fatalf("flow %q", got)
	}
	if _, err := parseAddress("serial:///dev/ttyUSB0?flow=dtr"); err == nil || !strings.Contains(err.Error(), "none, rtscts or xonxoff") {
		t.Fatalf("a flow control that is not one was accepted: %v", err)
	}
}
