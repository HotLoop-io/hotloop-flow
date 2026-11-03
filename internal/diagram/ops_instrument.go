package diagram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/diagram/scpi"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Instrument sessions (I1, I2). An instrument is opened by an address string:
//
//	tcp://10.0.0.5:5025   or just 10.0.0.5:5025     a LAN instrument on a raw socket
//	serial:///dev/ttyUSB0?baud=19200&parity=E        a serial instrument
//	/dev/ttyUSB0, COM3                               a serial instrument, 9600 8N1
//
// The query node writes a command and reads the reply, to a terminator, to a
// count, or as a binary block. The parse node turns text into numbers, an
// identity or a status. The parsers are in the scpi package.

// link is what a query needs of a session, and what both transports give.
type link interface {
	read(ctx context.Context, term []byte, n int, timeout time.Duration) ([]byte, error)
	write(data []byte) error
}

// tcpSession is an open raw socket. A socket that drops is opened again by the
// next call, and the drop is counted, like a serial port.
type tcpSession struct {
	addr string

	mu         sync.Mutex
	conn       net.Conn
	buf        []byte
	losses     int
	reconnects int
	lost       bool
	closed     bool
}

func (s *tcpSession) MarshalJSON() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Marshal(map[string]any{"session": "tcp", "address": s.addr, "open": s.conn != nil, "losses": s.losses, "reconnects": s.reconnects})
}

func (s *tcpSession) ensure(timeout time.Duration) error {
	if s.closed {
		return errors.New("the socket is closed")
	}
	if s.conn != nil {
		return nil
	}
	c, err := net.DialTimeout("tcp", s.addr, timeout)
	if err != nil {
		return fmt.Errorf("cannot connect to %s: %w", s.addr, err)
	}
	s.conn = c
	if s.lost {
		s.lost = false
		s.reconnects++
	}
	return nil
}

func (s *tcpSession) drop() {
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	s.buf = nil
}

func (s *tcpSession) lose() {
	s.drop()
	s.losses++
	s.lost = true
}

func (s *tcpSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.drop()
	return nil
}

func (s *tcpSession) Counts() (losses, reconnects int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.losses, s.reconnects
}

func (s *tcpSession) read(ctx context.Context, term []byte, n int, timeout time.Duration) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensure(timeout); err != nil {
		return nil, &Fault{Code: CodeDevice, Source: err.Error()}
	}
	deadline := time.Now().Add(timeout)
	chunk := make([]byte, 4096)
	for {
		if out, ok := takeFrame(&s.buf, term, n); ok {
			return out, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, &Fault{Code: CodeTimeout, Source: fmt.Sprintf("nothing arrived from %s in %v", s.addr, timeout)}
		}
		// Wake often enough to see a cancelled run.
		wait := left
		if wait > readTick {
			wait = readTick
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(wait))
		got, err := s.conn.Read(chunk)
		if got > 0 {
			s.buf = append(s.buf, chunk[:got]...)
		}
		var ne net.Error
		if err != nil && !(errors.As(err, &ne) && ne.Timeout()) {
			s.lose()
			return nil, &Fault{Code: CodeDevice, Source: fmt.Sprintf("lost %s: %v", s.addr, err)}
		}
	}
}

func (s *tcpSession) write(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensure(2 * time.Second); err != nil {
		return &Fault{Code: CodeDevice, Source: err.Error()}
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := s.conn.Write(data); err != nil {
		s.lose()
		return &Fault{Code: CodeDevice, Source: fmt.Sprintf("lost %s: %v", s.addr, err)}
	}
	return nil
}

// takeFrame cuts one frame off the front of buf, the way the serial session does:
// n bytes, or up to a terminator, or everything there is when neither is set.
func takeFrame(buf *[]byte, term []byte, n int) ([]byte, bool) {
	b := *buf
	switch {
	case n > 0:
		if len(b) < n {
			return nil, false
		}
		*buf = b[n:]
		return append([]byte(nil), b[:n]...), true
	case len(term) == 0:
		if len(b) == 0 {
			return nil, false
		}
		*buf = nil
		return b, true
	}
	i := bytes.Index(b, term)
	if i < 0 {
		return nil, false
	}
	*buf = b[i+len(term):]
	return append([]byte(nil), b[:i]...), true
}

// parseAddress turns an address string into a session, not yet open.
func parseAddress(addr string) (link, error) {
	addr = strings.TrimSpace(addr)
	switch {
	case addr == "":
		return nil, errors.New("the instrument has no address")
	case strings.HasPrefix(addr, "serial://"):
		u, err := url.Parse(addr)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address: %v", addr, err)
		}
		dev := u.Host + u.Path
		if dev == "" {
			return nil, fmt.Errorf("%q has no port", addr)
		}
		q := u.Query()
		cfg := SerialConfig{Device: dev, Baud: intOr(q.Get("baud"), 9600), DataBits: intOr(q.Get("data"), 8), StopBits: intOr(q.Get("stop"), 1), Parity: "N"}
		switch strings.ToUpper(q.Get("parity")) {
		case "E", "EVEN":
			cfg.Parity = "E"
		case "O", "ODD":
			cfg.Parity = "O"
		}
		return &serialSession{cfg: cfg}, nil
	case strings.HasPrefix(addr, "/dev/") || strings.HasPrefix(strings.ToUpper(addr), "COM"):
		return &serialSession{cfg: SerialConfig{Device: addr, Baud: 9600, DataBits: 8, StopBits: 1, Parity: "N"}}, nil
	}
	addr = strings.TrimPrefix(addr, "tcp://")
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return nil, fmt.Errorf("%q is not host:port", addr)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("%q is not a port", port)
	}
	return &tcpSession{addr: addr}, nil
}

// readBlock reads a binary block off the link: "#", the digit count, the length,
// then that many bytes, then the terminator the instrument adds.
func readBlock(ctx context.Context, l link, term []byte, timeout time.Duration) ([]byte, error) {
	head, err := l.read(ctx, nil, 2, timeout)
	if err != nil {
		return nil, err
	}
	if head[0] != '#' {
		return nil, &Fault{Code: CodeParse, Source: fmt.Sprintf("a block reply starts with #, this started with %q", head)}
	}
	digits := int(head[1] - '0')
	if head[1] < '0' || head[1] > '9' {
		return nil, &Fault{Code: CodeParse, Source: fmt.Sprintf("%q is not a block header", head)}
	}
	if digits == 0 {
		rest, err := l.read(ctx, []byte("\n"), 0, timeout)
		if err != nil {
			return nil, err
		}
		return rest, nil
	}
	lenb, err := l.read(ctx, nil, digits, timeout)
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(string(lenb))
	if err != nil || n < 0 {
		return nil, &Fault{Code: CodeParse, Source: fmt.Sprintf("%q is not a block length", lenb)}
	}
	payload := []byte{}
	if n > 0 {
		if payload, err = l.read(ctx, nil, n, timeout); err != nil {
			return nil, err
		}
	}
	if len(term) > 0 {
		// The terminator after a block is not part of it. If it does not come, the
		// samples are still good, so a late or missing one is not an error.
		_, _ = l.read(ctx, nil, len(term), 50*time.Millisecond)
	}
	return payload, nil
}

const instrColor = "#bfd0d9"

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-instr-open", Color: instrColor, Icon: "serial", PaletteLabel: "instrument", LabelProp: "address",
			InputPorts: []string{"err?"}, OutputPorts: []string{"session", "err"},
			Props: []node.Prop{
				{Name: "address", Label: "Address", Kind: node.PropString, Required: true, Placeholder: "10.0.0.5:5025 or serial:///dev/ttyUSB0?baud=19200", Default: "127.0.0.1:5025"},
			},
			Help: "Opens an instrument by its address: host:port for a LAN instrument on a raw socket, or serial:///dev/ttyUSB0?baud=19200&parity=E for a serial one. A COM or /dev path alone is 9600 8N1. The run closes it when it ends. If the link drops, the next query opens it again and the drop is counted.",
		},
		New: func(raw map[string]any) (Op, error) {
			addr := strProp(raw, "address", "")
			if _, err := parseAddress(addr); err != nil {
				return nil, err
			}
			return opFunc(func(_ context.Context, rc *Ctx, _ map[string]any) (map[string]any, error) {
				l, _ := parseAddress(addr)
				switch s := l.(type) {
				case *serialSession:
					s.mu.Lock()
					err := s.ensure()
					s.mu.Unlock()
					if err != nil {
						return nil, &Fault{Code: CodeDevice, Source: err.Error()}
					}
					rc.Run.sessions.add(s)
				case *tcpSession:
					s.mu.Lock()
					err := s.ensure(3 * time.Second)
					s.mu.Unlock()
					if err != nil {
						return nil, &Fault{Code: CodeDevice, Source: err.Error()}
					}
					rc.Run.sessions.add(s)
				}
				return map[string]any{"session": l}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-instr-query", Color: instrColor, Icon: "serial", PaletteLabel: "instrument query", LabelProp: "command",
			InputPorts: []string{"session", "command?", "err?"}, OutputPorts: []string{"session", "reply", "samples", "err"},
			Props: []node.Prop{
				prop("command", "Command (or wire one in)", node.PropString, "*IDN?"),
				prop("terminator", "Terminator", node.PropString, `\n`),
				choice("reply", "Reply is", "text", "text", "none", "block"),
				choice("format", "Block samples are", "int16", "int8", "uint8", "int16", "uint16", "int32", "float32", "float64"),
				choice("order", "Block byte order", "big", "big", "little"),
				prop("timeoutMs", "Give up after (ms)", node.PropNumber, 2000),
			},
			Help: "Writes a command and the terminator, then reads the reply: a line of text, nothing (a plain write), or a binary block that comes out as an array of samples. A reply that does not arrive in time is an error on the error wire, with the address and the wait in its source. Hands the session on.",
		},
		New: func(raw map[string]any) (Op, error) {
			term := unescape(strProp(raw, "terminator", `\n`))
			mode := strProp(raw, "reply", "text")
			format := strProp(raw, "format", "int16")
			little := strProp(raw, "order", "big") == "little"
			timeout := time.Duration(floatProp(raw, "timeoutMs", 2000)) * time.Millisecond
			fixed := strProp(raw, "command", "")
			return opFunc(func(ctx context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				l, ok := in["session"].(link)
				if !ok {
					return nil, &Fault{Code: CodeBadInput, Source: "the session input is not an instrument session"}
				}
				cmd := fixed
				if v, wired := in["command"]; wired {
					if cmd, ok = v.(string); !ok {
						return nil, &Fault{Code: CodeBadInput, Source: "the command input is not text"}
					}
				}
				if cmd == "" {
					return nil, &Fault{Code: CodeBadInput, Source: "there is no command to send"}
				}
				if err := l.write(append([]byte(cmd), term...)); err != nil {
					return nil, err
				}
				out := map[string]any{"session": l}
				switch mode {
				case "none":
				case "block":
					p, err := readBlock(ctx, l, term, timeout)
					if err != nil {
						return nil, err
					}
					s, err := scpi.Samples(p, format, little)
					if err != nil {
						return nil, &Fault{Code: CodeParse, Source: err.Error()}
					}
					out["samples"] = toTokens(s)
				default:
					b, err := l.read(ctx, term, 0, timeout)
					if err != nil {
						return nil, err
					}
					out["reply"] = strings.TrimSpace(string(b))
				}
				return out, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-scpi-parse", Color: instrColor, Icon: "serial", PaletteLabel: "parse reply", LabelProp: "as",
			InputPorts: []string{"reply", "err?"}, OutputPorts: []string{"value", "err"},
			Props: []node.Prop{
				choice("as", "Read the reply as", "numbers", "numbers", "number", "identity", "status byte", "event status"),
			},
			Help: "Turns an instrument's text reply into data: a comma list into an array of numbers, one number, an identification reply into manufacturer, model, serial and firmware, or a status byte into the names of its set bits. A reply that does not fit is an error on the error wire.",
		},
		New: func(raw map[string]any) (Op, error) {
			as := strProp(raw, "as", "numbers")
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				text, ok := in["reply"].(string)
				if !ok {
					return nil, &Fault{Code: CodeBadInput, Source: "the reply input is not text"}
				}
				switch as {
				case "identity":
					id, err := scpi.ParseIdentity(text)
					if err != nil {
						return nil, &Fault{Code: CodeParse, Source: err.Error()}
					}
					return map[string]any{"value": map[string]any{"manufacturer": id.Manufacturer, "model": id.Model, "serial": id.Serial, "firmware": id.Firmware}}, nil
				case "status byte", "event status":
					n, err := strconv.Atoi(strings.TrimSpace(text))
					if err != nil || n < 0 || n > 255 {
						return nil, &Fault{Code: CodeParse, Source: fmt.Sprintf("%q is not a status byte", text)}
					}
					names := scpi.STB
					if as == "event status" {
						names = scpi.ESR
					}
					set := []any{}
					for _, s := range scpi.StatusBits(n, names) {
						set = append(set, s)
					}
					return map[string]any{"value": map[string]any{"byte": float64(n), "set": set}}, nil
				}
				v, err := scpi.Numbers(text)
				if err != nil {
					return nil, &Fault{Code: CodeParse, Source: err.Error()}
				}
				if as == "number" {
					if len(v) != 1 {
						return nil, &Fault{Code: CodeParse, Source: fmt.Sprintf("the reply has %d numbers, not 1", len(v))}
					}
					return map[string]any{"value": v[0]}, nil
				}
				return map[string]any{"value": toTokens(v)}, nil
			}), nil
		},
	})
}
