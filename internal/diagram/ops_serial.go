package diagram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/goburrow/serial"
)

// A serial session is an open port that reads and writes bytes. It is a value on
// a wire like any other: the open node makes one, the read and write nodes take it.
//
// A port that goes away, an unplugged adapter or a pulled cable, is not the end of
// the session. The read that fails says so on its error wire, and the next read
// opens the port again, and counts that it did.

// SerialConfig is how a port is opened.
type SerialConfig struct {
	Device   string
	Baud     int
	DataBits int
	StopBits int
	Parity   string // N, E or O
}

type serialSession struct {
	cfg SerialConfig

	mu         sync.Mutex
	port       serial.Port
	buf        []byte
	losses     int  // times the port was lost
	reconnects int  // times it was opened again after a loss
	lost       bool // lost and not yet opened again
	closed     bool
}

// MarshalJSON is what a probe and the panel see of a session.
func (s *serialSession) MarshalJSON() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Marshal(map[string]any{"session": "serial", "device": s.cfg.Device, "baud": s.cfg.Baud, "open": s.port != nil, "losses": s.losses, "reconnects": s.reconnects})
}

// readTick is how long one read of the port waits, so a cancelled run is noticed.
const readTick = 100 * time.Millisecond

func openPort(c SerialConfig) (serial.Port, error) {
	return serial.Open(&serial.Config{
		Address: c.Device, BaudRate: c.Baud, DataBits: c.DataBits, StopBits: c.StopBits,
		Parity: c.Parity, Timeout: readTick,
	})
}

func (s *serialSession) ensure() error {
	if s.closed {
		return errors.New("the port is closed")
	}
	if s.port != nil {
		return nil
	}
	p, err := openPort(s.cfg)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", s.cfg.Device, err)
	}
	s.port = p
	if s.lost {
		s.lost = false
		s.reconnects++
	}
	return nil
}

// lose drops a port that failed and notes it, so the next call opens it again.
func (s *serialSession) lose() {
	s.drop()
	s.losses++
	s.lost = true
}

func (s *serialSession) drop() {
	if s.port != nil {
		_ = s.port.Close()
		s.port = nil
	}
	s.buf = nil
}

// Close ends the session.
func (s *serialSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.drop()
	return nil
}

// read returns the bytes up to a terminator, or n bytes, or what is there when the
// time runs out as an error. Bytes read past the terminator stay for the next call.
func (s *serialSession) read(ctx context.Context, term []byte, n int, timeout time.Duration) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensure(); err != nil {
		return nil, &Fault{Code: CodeDevice, Source: err.Error()}
	}
	deadline := time.Now().Add(timeout)
	chunk := make([]byte, 256)
	for {
		if line, ok := s.take(term, n); ok {
			return line, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, &Fault{Code: CodeTimeout, Source: fmt.Sprintf("nothing arrived on %s in %v", s.cfg.Device, timeout)}
		}
		got, err := s.port.Read(chunk)
		if got > 0 {
			s.buf = append(s.buf, chunk[:got]...)
		}
		if got == 0 && err == nil {
			// The port was ready to read and gave no bytes: it is a terminal whose
			// other end has gone, which is how a pulled cable or a closed
			// pseudo terminal shows. Waiting longer would wait for nothing.
			err = errors.New("the device closed the port")
		}
		if err != nil && !errors.Is(err, serial.ErrTimeout) {
			s.lose()
			return nil, &Fault{Code: CodeDevice, Source: fmt.Sprintf("lost %s: %v", s.cfg.Device, err)}
		}
	}
}

func (s *serialSession) take(term []byte, n int) ([]byte, bool) {
	if n > 0 {
		if len(s.buf) >= n {
			out := append([]byte(nil), s.buf[:n]...)
			s.buf = s.buf[n:]
			return out, true
		}
		return nil, false
	}
	if len(term) == 0 {
		if len(s.buf) > 0 {
			out := s.buf
			s.buf = nil
			return out, true
		}
		return nil, false
	}
	if i := bytes.Index(s.buf, term); i >= 0 {
		out := append([]byte(nil), s.buf[:i]...)
		s.buf = s.buf[i+len(term):]
		return out, true
	}
	return nil, false
}

func (s *serialSession) write(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensure(); err != nil {
		return &Fault{Code: CodeDevice, Source: err.Error()}
	}
	if _, err := s.port.Write(data); err != nil {
		s.lose()
		return &Fault{Code: CodeDevice, Source: fmt.Sprintf("lost %s: %v", s.cfg.Device, err)}
	}
	return nil
}

// Counts reports how many times the port was lost and how many times it was
// opened again.
func (s *serialSession) Counts() (losses, reconnects int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.losses, s.reconnects
}

func unescape(s string) []byte {
	r := strings.NewReplacer(`\r`, "\r", `\n`, "\n", `\t`, "\t", `\0`, "\x00")
	return []byte(r.Replace(s))
}

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-serial-open", Color: "#bfd9c9", Icon: "serial", PaletteLabel: "serial port", LabelProp: "device",
			InputPorts: []string{"err?"}, OutputPorts: []string{"session", "err"},
			Props: []node.Prop{
				{Name: "device", Label: "Port", Kind: node.PropString, Required: true, Placeholder: "/dev/ttyUSB0 or COM3", Default: "/dev/ttyUSB0"},
				prop("baud", "Baud rate", node.PropNumber, 9600),
				choice("data", "Data bits", "8", "5", "6", "7", "8"),
				choice("parity", "Parity", "none", "none", "even", "odd"),
				choice("stop", "Stop bits", "1", "1", "2"),
			},
			Help: "Opens a serial port and hands on a session. The run closes it when it ends. If the cable is pulled the read says so on its error wire and opens the port again on the next read.",
		},
		New: func(raw map[string]any) (Op, error) {
			cfg := SerialConfig{
				Device: strProp(raw, "device", ""), Baud: intProp(raw, "baud", 9600),
				DataBits: intOr(strProp(raw, "data", "8"), 8), StopBits: intOr(strProp(raw, "stop", "1"), 1),
			}
			switch strProp(raw, "parity", "none") {
			case "even":
				cfg.Parity = "E"
			case "odd":
				cfg.Parity = "O"
			default:
				cfg.Parity = "N"
			}
			if cfg.Device == "" {
				return nil, fmt.Errorf("the serial port has no device")
			}
			return opFunc(func(_ context.Context, rc *Ctx, _ map[string]any) (map[string]any, error) {
				s := &serialSession{cfg: cfg}
				s.mu.Lock()
				err := s.ensure()
				s.mu.Unlock()
				if err != nil {
					return nil, &Fault{Code: CodeDevice, Source: err.Error()}
				}
				rc.Run.sessions.add(s)
				return map[string]any{"session": s}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-serial-read", Color: "#bfd9c9", Icon: "serial", PaletteLabel: "serial read", LabelProp: "terminator",
			InputPorts: []string{"session", "err?"}, OutputPorts: []string{"line", "err"},
			Props: []node.Prop{
				prop("terminator", "Read until", node.PropString, `\n`),
				prop("bytes", "Or read this many bytes (0 to use the terminator)", node.PropNumber, 0),
				prop("timeoutMs", "Give up after (ms)", node.PropNumber, 2000),
			},
			Help: "Reads one frame from a serial session: up to the terminator, or a count of bytes. A frame that does not arrive in time is an error on the error wire, with the port and the wait in its source.",
		},
		New: func(raw map[string]any) (Op, error) {
			term := unescape(strProp(raw, "terminator", `\n`))
			count := intProp(raw, "bytes", 0)
			timeout := time.Duration(floatProp(raw, "timeoutMs", 2000)) * time.Millisecond
			return opFunc(func(ctx context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				s, ok := in["session"].(*serialSession)
				if !ok {
					return nil, &Fault{Code: CodeBadInput, Source: "the session input is not a serial session"}
				}
				b, err := s.read(ctx, term, count, timeout)
				if err != nil {
					return nil, err
				}
				// A line ending that comes before the terminator is not part of the frame.
				if len(term) == 1 && term[0] == '\n' {
					b = bytes.TrimSuffix(b, []byte("\r"))
				}
				return map[string]any{"line": string(b)}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-serial-write", Color: "#bfd9c9", Icon: "serial", PaletteLabel: "serial write", LabelProp: "terminator",
			InputPorts: []string{"session", "data", "err?"}, OutputPorts: []string{"session", "err"},
			Props: []node.Prop{prop("terminator", "Add at the end", node.PropString, `\r\n`)},
			Help:  "Writes text to a serial session, then the terminator. Hands the session on, so a read can be wired after it.",
		},
		New: func(raw map[string]any) (Op, error) {
			term := unescape(strProp(raw, "terminator", `\r\n`))
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				s, ok := in["session"].(*serialSession)
				if !ok {
					return nil, &Fault{Code: CodeBadInput, Source: "the session input is not a serial session"}
				}
				text, ok := in["data"].(string)
				if !ok {
					return nil, &Fault{Code: CodeBadInput, Source: "the data input is not text"}
				}
				if err := s.write(append([]byte(text), term...)); err != nil {
					return nil, err
				}
				return map[string]any{"session": s}, nil
			}), nil
		},
	})
}
