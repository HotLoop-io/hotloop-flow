//go:build linux

// Command scalesim is a stand-in for a serial scale: a program that opens a
// pseudo terminal and writes the frames a blender scale writes, on a real
// terminal device, at a steady rate. The logger opens the other end of the
// terminal as its serial port. It is a real byte stream on a real serial device
// node, not a physical scale.
//
//	scalesim -link /tmp/scale-tty -hz 5
//
// Signals play the cable: SIGUSR1 pulls it (the terminal closes), SIGUSR2 puts it
// back (a new terminal, and the link points at it). SIGHUP sends one frame of
// garbage.
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type pty struct {
	master *os.File
	slave  string
}

func open() (*pty, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	fd := int(m.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		m.Close()
		return nil, err
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		m.Close()
		return nil, err
	}
	return &pty{master: m, slave: fmt.Sprintf("/dev/pts/%d", n)}, nil
}

func point(link, target string) error {
	tmp := link + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

func main() {
	link := flag.String("link", "/tmp/scale-tty", "path of the symlink to the terminal the logger opens")
	hz := flag.Float64("hz", 5, "frames per second")
	flag.Parse()

	var mu sync.Mutex
	cur, err := open()
	if err != nil {
		log.Fatal(err)
	}
	if err := point(*link, cur.slave); err != nil {
		log.Fatal(err)
	}
	log.Printf("scale on %s, link %s, %.1f frames per second", cur.slave, *link, *hz)

	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	garbage := false
	go func() {
		for s := range sig {
			mu.Lock()
			switch s {
			case syscall.SIGUSR1:
				if cur != nil {
					cur.master.Close()
					cur = nil
					log.Print("cable pulled")
				}
			case syscall.SIGUSR2:
				if cur == nil {
					if p, err := open(); err == nil {
						cur = p
						_ = point(*link, p.slave)
						log.Printf("cable back, on %s", p.slave)
					}
				}
			case syscall.SIGHUP:
				garbage = true
			default:
				_ = os.Remove(*link)
				os.Exit(0)
			}
			mu.Unlock()
		}
	}()

	tick := time.NewTicker(time.Duration(float64(time.Second) / *hz))
	target, shown, n := 12.34, 0.0, 0
	for range tick.C {
		mu.Lock()
		p := cur
		g := garbage
		garbage = false
		mu.Unlock()
		if p == nil {
			continue
		}
		// Ten frames while the load settles, then a long steady stretch.
		status := "ST"
		switch {
		case n%40 < 10:
			shown = target + (rand.Float64()-0.5)*4*float64(10-n%40)/10
			status = "US"
		default:
			shown = target
		}
		if n%40 == 39 {
			target = 5 + rand.Float64()*40
		}
		n++
		line := fmt.Sprintf("%s,GS,%7.2f kg\r\n", status, shown)
		if g {
			line = "\x00\xffgarbage\x01\r\n"
		}
		_, _ = p.master.WriteString(line)
	}
}
