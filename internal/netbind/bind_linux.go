//go:build linux

package netbind

import "syscall"

func bind(iface string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, iface)
	}); err != nil {
		return err
	}
	return serr
}
