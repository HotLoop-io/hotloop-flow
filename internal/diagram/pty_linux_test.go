//go:build linux

package diagram

import (
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ptyPair opens a pseudo terminal and returns the master, which the test writes
// to as the device would, and the path of the slave, which the diagram opens as
// its serial port. It is a real terminal device driven by a real byte stream; no
// physical scale is on the wire.
func ptyPair(t *testing.T) (*os.File, string) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx here: %v", err)
	}
	fd := int(m.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, fmt.Sprintf("/dev/pts/%d", n)
}

// waitOpened blocks until the diagram has opened the slave and put it in raw
// mode, which shows on the master as the terminal no longer being line buffered.
// A device that writes before the port is configured is not what is under test.
func waitOpened(t *testing.T, m *os.File) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tio, err := unix.IoctlGetTermios(int(m.Fd()), unix.TCGETS)
		if err == nil && tio.Lflag&unix.ICANON == 0 && tio.Iflag&unix.ICRNL == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the diagram never opened the port")
}
