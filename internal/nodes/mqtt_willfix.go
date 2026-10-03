package nodes

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"net/url"
	"sync"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/packets"
)

// paho.golang 0.23.0 packs a will's properties with the rules for the CONNECT
// packet's own properties, and those leave out content type, response topic,
// correlation data, message expiry and payload format, which only a PUBLISH
// gets. A will configured with any of them reaches the broker without them,
// and the line's will arrives at whoever is watching without the content type
// that says how to read it. User properties and the will delay come through,
// because CONNECT has those too. Proved against Mosquitto: a bare paho client
// sends the same will and the properties are gone.
//
// Until that is fixed upstream, a connection whose will needs any of those
// five is dialled through willFixConn, which holds back the CONNECT packet paho
// writes, decodes it, and writes it out again with the will properties encoded
// in full. Everything else in the packet goes out exactly as paho built it.

// willNeedsFix reports whether a will carries a property paho would drop.
func willNeedsFix(w *mqttLWT) bool {
	if w == nil || w.Props == nil {
		return false
	}
	p := w.Props
	return p.ContentType != "" || p.ResponseTopic != "" || p.CorrelationData != nil ||
		p.MessageExpiry != nil || p.PayloadFormat != nil
}

// willFixDialer returns autopaho's AttemptConnection for a will that needs the
// fix: the usual TCP or TLS dial, wrapped.
func willFixDialer(w *mqttLWT, tlsCfg *tls.Config) func(context.Context, autopaho.ClientConfig, *url.URL) (net.Conn, error) {
	return func(ctx context.Context, _ autopaho.ClientConfig, u *url.URL) (net.Conn, error) {
		var conn net.Conn
		var err error
		switch u.Scheme {
		case "tls", "ssl", "mqtts":
			d := tls.Dialer{Config: tlsCfg}
			conn, err = d.DialContext(ctx, "tcp", u.Host)
		default:
			var d net.Dialer
			conn, err = d.DialContext(ctx, "tcp", u.Host)
		}
		if err != nil {
			return nil, err
		}
		return packets.NewThreadSafeConn(&willFixConn{Conn: conn, will: w}), nil
	}
}

type willFixConn struct {
	net.Conn
	will *mqttLWT

	mu   sync.Mutex
	sent bool
	buf  []byte
}

func (c *willFixConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sent {
		return c.Conn.Write(p)
	}
	c.buf = append(c.buf, p...)
	total, ok := mqttPacketLen(c.buf)
	if !ok || len(c.buf) < total {
		return len(p), nil
	}
	fixed, err := rewriteConnect(c.buf[:total], c.will)
	if err != nil {
		return 0, err
	}
	out := append(fixed, c.buf[total:]...)
	c.sent, c.buf = true, nil
	if _, err := c.Conn.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// mqttPacketLen reads a packet's fixed header and returns the whole packet's
// length, or false when not enough of it has arrived to tell.
func mqttPacketLen(b []byte) (int, bool) {
	if len(b) < 2 {
		return 0, false
	}
	n, mult := 0, 1
	for i := 1; i < len(b) && i <= 4; i++ {
		n += int(b[i]&0x7f) * mult
		if b[i]&0x80 == 0 {
			return 1 + i + n, true
		}
		mult *= 128
	}
	return 0, false
}

// rewriteConnect re-encodes a CONNECT packet with the will properties written
// out in full.
func rewriteConnect(raw []byte, w *mqttLWT) ([]byte, error) {
	cp, err := packets.ReadPacket(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("re-reading the CONNECT packet: %w", err)
	}
	c, ok := cp.Content.(*packets.Connect)
	if !ok || !c.WillFlag {
		return raw, nil
	}

	var body bytes.Buffer
	putString(&body, c.ProtocolName)
	body.WriteByte(c.ProtocolVersion)
	body.WriteByte(c.PackFlags())
	putUint16(&body, c.KeepAlive)
	props := c.Properties.Pack(packets.CONNECT)
	putVBI(&body, len(props))
	body.Write(props)

	putString(&body, c.ClientID)
	willProps := encodeWillProps(w)
	putVBI(&body, len(willProps))
	body.Write(willProps)
	putString(&body, c.WillTopic)
	putBinary(&body, c.WillMessage)
	if c.UsernameFlag {
		putString(&body, c.Username)
	}
	if c.PasswordFlag {
		putBinary(&body, c.Password)
	}

	var out bytes.Buffer
	out.WriteByte(packets.CONNECT << 4)
	putVBI(&out, body.Len())
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

// MQTT 5 property identifiers for a will, from section 3.1.3.2 of the
// specification.
const (
	propPayloadFormat     = 0x01
	propMessageExpiry     = 0x02
	propContentType       = 0x03
	propResponseTopic     = 0x08
	propCorrelationData   = 0x09
	propWillDelayInterval = 0x18
	propUserProperty      = 0x26
)

func encodeWillProps(w *mqttLWT) []byte {
	var b bytes.Buffer
	if w.Delay != nil {
		b.WriteByte(propWillDelayInterval)
		putUint32(&b, *w.Delay)
	}
	if p := w.Props; p != nil {
		if p.PayloadFormat != nil {
			b.WriteByte(propPayloadFormat)
			if *p.PayloadFormat {
				b.WriteByte(1)
			} else {
				b.WriteByte(0)
			}
		}
		if p.MessageExpiry != nil {
			b.WriteByte(propMessageExpiry)
			putUint32(&b, *p.MessageExpiry)
		}
		if p.ContentType != "" {
			b.WriteByte(propContentType)
			putString(&b, p.ContentType)
		}
		if p.ResponseTopic != "" {
			b.WriteByte(propResponseTopic)
			putString(&b, p.ResponseTopic)
		}
		if p.CorrelationData != nil {
			b.WriteByte(propCorrelationData)
			putBinary(&b, p.CorrelationData)
		}
		for _, kv := range p.User {
			b.WriteByte(propUserProperty)
			putString(&b, kv[0])
			putString(&b, kv[1])
		}
	}
	return b.Bytes()
}

func putUint16(b *bytes.Buffer, v uint16) {
	var x [2]byte
	binary.BigEndian.PutUint16(x[:], v)
	b.Write(x[:])
}

func putUint32(b *bytes.Buffer, v uint32) {
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], v)
	b.Write(x[:])
}

func putString(b *bytes.Buffer, s string) { putBinary(b, []byte(s)) }

func putBinary(b *bytes.Buffer, v []byte) {
	putUint16(b, uint16(len(v)))
	b.Write(v)
}

func putVBI(b *bytes.Buffer, n int) {
	for {
		digit := byte(n % 128)
		n /= 128
		if n > 0 {
			digit |= 0x80
		}
		b.WriteByte(digit)
		if n == 0 {
			return
		}
	}
}
