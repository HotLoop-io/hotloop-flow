//go:build !linux

package netbind

import (
	"errors"
	"syscall"
)

func bind(string, syscall.RawConn) error {
	return errors.New("binding a connection to a network interface is only supported on Linux")
}
