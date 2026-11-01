package ni

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxMessage is the largest gRPC message the client accepts back. A read of a
// few million samples fits; a server that claims more is refused before the
// client allocates for it.
const maxMessage = 256 << 20

// Config says how to reach NI's gRPC Device Server.
type Config struct {
	// Address is host:port. The server listens on localhost:31763 unless its
	// configuration file says otherwise.
	Address string
	// TLS is how to secure the connection. A zero value means a plain, unsecured
	// connection, which is only for a server on the same machine or a bench.
	TLS TLS
	// CallTimeout bounds each call to the server. Zero means 10 seconds.
	CallTimeout time.Duration
}

// TLS is the secure-connection settings. Set RootCAFile to check the server's
// certificate against your own authority, and CertFile with KeyFile to also
// present a certificate of your own, which is what the server asks for when it
// runs mutual TLS.
type TLS struct {
	// Enabled turns TLS on. It is also on whenever any other field is set.
	Enabled bool
	// RootCAFile is a PEM file with the authority that signed the server's
	// certificate. Empty means the system's authorities.
	RootCAFile string
	// CertFile and KeyFile are the client's own certificate and key, PEM files.
	CertFile string
	KeyFile  string
	// ServerName is the name to check in the server's certificate. Empty means
	// the host part of Address.
	ServerName string
}

func (t TLS) on() bool {
	return t.Enabled || t.RootCAFile != "" || t.CertFile != "" || t.KeyFile != "" || t.ServerName != ""
}

// Client talks to one NI gRPC Device Server. It is safe for concurrent use.
type Client struct {
	hc      *http.Client
	base    string
	timeout time.Duration
	tr      *http.Transport
}

// Dial builds a client. It does not connect: the first call does.
func Dial(cfg Config) (*Client, error) {
	addr := strings.TrimSpace(cfg.Address)
	if addr == "" {
		return nil, errors.New("ni: no server address")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("ni: the address %q is not host:port: %v", addr, err)
	}
	if (cfg.TLS.CertFile == "") != (cfg.TLS.KeyFile == "") {
		return nil, errors.New("ni: a client certificate needs both its file and its key file")
	}
	timeout := cfg.CallTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     time.Minute,
		Protocols:           new(http.Protocols),
	}
	scheme := "http"
	if cfg.TLS.on() {
		scheme = "https"
		tc, err := cfg.TLS.config(addr)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = tc
		tr.Protocols.SetHTTP2(true)
	} else {
		tr.Protocols.SetUnencryptedHTTP2(true)
	}
	return &Client{hc: &http.Client{Transport: tr}, base: scheme + "://" + addr, timeout: timeout, tr: tr}, nil
}

func (t TLS) config(addr string) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}}
	host, _, _ := net.SplitHostPort(addr)
	tc.ServerName = host
	if t.ServerName != "" {
		tc.ServerName = t.ServerName
	}
	if t.RootCAFile != "" {
		pem, err := os.ReadFile(t.RootCAFile)
		if err != nil {
			return nil, fmt.Errorf("ni: reading the root certificate: %v", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ni: %s holds no PEM certificate", t.RootCAFile)
		}
		tc.RootCAs = pool
	}
	if t.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("ni: reading the client certificate: %v", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}

// Close drops the client's idle connections.
func (c *Client) Close() error {
	c.tr.CloseIdleConnections()
	return nil
}

// Error is what a call returns when the server, or the way to it, said no.
type Error struct {
	// Method is the NI service method, such as "ReadAnalogF64".
	Method string
	// Code is NI's error code from the "ni-error" trailer: negative for a driver
	// error, such as -200077. It is zero when the failure did not come from the
	// driver (a refused connection, a timeout, a bad argument).
	Code int32
	// GRPC is the gRPC status code; 2 (unknown) is how the server reports a
	// driver error. It is -1 when no gRPC status came back at all.
	GRPC int
	// Message is the driver's description, or the transport's.
	Message string
	err     error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("ni " + e.Method + ": ")
	if e.Code != 0 {
		b.WriteString("error " + strconv.Itoa(int(e.Code)) + ": ")
	}
	b.WriteString(e.Message)
	return b.String()
}

func (e *Error) Unwrap() error { return e.err }

// Timeout reports whether the call ran out of time.
func (e *Error) Timeout() bool {
	return errors.Is(e.err, context.DeadlineExceeded) || e.GRPC == 4
}

const service = "/nidaqmx_grpc.NiDAQmx/"

// call sends one unary request and returns the reply message. Every call is
// bounded by ctx and by the client's per call timeout plus extra, whichever is
// shorter.
func (c *Client) call(ctx context.Context, method string, payload []byte, extra time.Duration) ([]byte, error) {
	fail := func(err error, grpc int, code int32, msg string) ([]byte, error) {
		return nil, &Error{Method: method, Code: code, GRPC: grpc, Message: msg, err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout+extra)
	defer cancel()
	frame := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+service+method, bytes.NewReader(frame))
	if err != nil {
		return fail(err, -1, 0, err.Error())
	}
	hr.Header.Set("Content-Type", "application/grpc")
	hr.Header.Set("Te", "trailers")
	hr.Header.Set("Grpc-Accept-Encoding", "identity")
	hr.Header.Set("User-Agent", "hotloop-flow-ni/1")
	if dl, ok := ctx.Deadline(); ok {
		if ms := time.Until(dl).Milliseconds(); ms > 0 {
			hr.Header.Set("Grpc-Timeout", strconv.FormatInt(ms, 10)+"m")
		}
	}
	hres, err := c.hc.Do(hr)
	if err != nil {
		return fail(err, -1, 0, transportText(ctx, err))
	}
	defer hres.Body.Close()
	if hres.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(hres.Body, 4096))
		return fail(nil, -1, 0, "the server answered HTTP "+hres.Status+", which is not a gRPC server's answer")
	}
	if ct := hres.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/grpc") {
		return fail(nil, -1, 0, "the server answered with content type "+strconv.Quote(ct)+", which is not gRPC")
	}
	var msg []byte
	var hdr [5]byte
	got := false
	for {
		if _, err := io.ReadFull(hres.Body, hdr[:]); err != nil {
			if err == io.EOF {
				break
			}
			return fail(err, -1, 0, transportText(ctx, err))
		}
		if hdr[0] != 0 {
			return fail(nil, -1, 0, "the server compressed its answer, which this client did not ask for")
		}
		n := binary.BigEndian.Uint32(hdr[1:])
		if n > maxMessage {
			return fail(nil, -1, 0, "the server's answer is "+strconv.FormatUint(uint64(n), 10)+" bytes, over the limit")
		}
		msg = make([]byte, n)
		if _, err := io.ReadFull(hres.Body, msg); err != nil {
			return fail(err, -1, 0, transportText(ctx, err))
		}
		got = true
	}
	// A reply with no message at all carries its status in the headers instead.
	tr := hres.Trailer
	if tr.Get("Grpc-Status") == "" {
		tr = hres.Header
	}
	st := tr.Get("Grpc-Status")
	if st == "" {
		return fail(nil, -1, 0, "the server's answer ended with no gRPC status")
	}
	code, err := strconv.Atoi(st)
	if err != nil {
		return fail(nil, -1, 0, "the gRPC status "+strconv.Quote(st)+" is not a number")
	}
	if code != 0 {
		ni, _ := strconv.ParseInt(strings.TrimSpace(tr.Get("Ni-Error")), 10, 32)
		text := tr.Get("Grpc-Message")
		if text == "" {
			text = "gRPC status " + st
		}
		return fail(nil, code, int32(ni), decodeGRPCMessage(text))
	}
	if !got {
		return fail(nil, 0, 0, "the server's answer had no message")
	}
	return msg, nil
}

func transportText(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "no answer in time (" + ctx.Err().Error() + ")"
	}
	return err.Error()
}

// decodeGRPCMessage undoes the percent encoding gRPC puts on grpc-message.
func decodeGRPCMessage(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
