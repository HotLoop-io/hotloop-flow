package netbind

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	for _, ok := range []string{"", "enp2s0.14", "eth0", "flxv14", "br-lan", "123456789012345"} {
		if err := Validate(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"1234567890123456", "a b", "a/b", "..", "x\x00y"} {
		if err := Validate(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNoInterfaceIsAnOrdinaryDial(t *testing.T) {
	if Control("") != nil {
		t.Fatal("an empty interface should not install a control function")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	defer ln.Close()
	c, err := DialTimeout("", "tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestAnInterfaceThatIsNotThereIsSaidPlainly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	defer ln.Close()
	_, err = DialTimeout("flxnope0", "tcp", ln.Addr().String(), time.Second)
	if err == nil {
		t.Skip("this kernel let a socket bind to an interface that does not exist")
	}
	if !strings.Contains(err.Error(), "flxnope0") {
		t.Fatalf("the error does not name the interface: %v", err)
	}
}

func TestExplainKeepsTheCauseAndNamesTheInterface(t *testing.T) {
	for _, e := range []syscall.Errno{syscall.ENODEV, syscall.EPERM, syscall.ENETUNREACH} {
		got := Explain("enp2s0.14", &net.OpError{Op: "dial", Err: e})
		if !errors.Is(got, e) || !strings.Contains(got.Error(), "enp2s0.14") {
			t.Errorf("%v: %v", e, got)
		}
	}
	if Explain("", syscall.ENODEV) != syscall.ENODEV {
		t.Error("with no interface the error should pass through")
	}
	if Explain("x", nil) != nil {
		t.Error("nil stays nil")
	}
}

func TestBindingToTheLoopbackInterfaceStillConnects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	defer ln.Close()
	c, err := DialTimeout("lo", "tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Skipf("this host does not allow binding a socket to lo here: %v", err)
	}
	c.Close()
}
