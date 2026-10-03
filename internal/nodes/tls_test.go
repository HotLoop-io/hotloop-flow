package nodes

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/filescope"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/youmark/pkcs8"
	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// Every test here runs real TLS over real sockets: a Go TLS server or client on
// the other end, certificates from a CA made for the test. Nothing is mocked,
// because the claim being tested is "this connection is encrypted and checked",
// and only a real handshake can say whether it is.

// testPKI is a CA plus whatever it issues.
type testPKI struct {
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey
	caPEM []byte
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Line 3 test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	return &testPKI{ca: ca, caKey: key, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issued is one leaf: its PEM, its parsed form and its key.
type issued struct {
	certPEM, keyPEM []byte
	cert            *x509.Certificate
	key             *ecdsa.PrivateKey
}

func (i issued) pair(t *testing.T) tls.Certificate {
	t.Helper()
	c, err := tls.X509KeyPair(i.certPEM, i.keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var serial int64 = 100

// issue makes a leaf for the given names. 127.0.0.1 goes in only when asked,
// so a test can make a certificate the dialled address does not match.
func (p *testPKI) issue(t *testing.T, cn string, dns []string, withLoopback bool) issued {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dns,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	if withLoopback {
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return issued{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		cert:    cert,
		key:     key,
	}
}

func (p *testPKI) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	return pool
}

// useSecretScope points the secret scope at the given trees for one test.
func useSecretScope(t *testing.T, extra ...string) {
	t.Helper()
	s, err := filescope.NewSecretScope(t.TempDir(), extra)
	if err != nil {
		t.Fatal(err)
	}
	prev := Secrets
	Secrets = s
	t.Cleanup(func() { Secrets = prev })
}

func writeSecret(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// buildTLS builds a tls-config the way the runtime does: properties from the
// flow file, secrets from the credential store, variables from the env.
func buildTLS(t *testing.T, props map[string]any, creds, env map[string]string) *tlsConfigNode {
	t.Helper()
	n, err := tryBuildTLS(t, props, creds, env)
	if err != nil {
		t.Fatalf("building the tls-config: %v", err)
	}
	return n
}

func tryBuildTLS(t *testing.T, props map[string]any, creds, env map[string]string) (*tlsConfigNode, error) {
	t.Helper()
	svc := newTestServices()
	for k, v := range creds {
		svc.creds[k] = v
	}
	for k, v := range env {
		svc.env[k] = v
	}
	cfg, err := jsonConfig(props)
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := node.Default.Lookup("tls-config")
	n, err := reg.New(&node.Definition{Node: parseNodeConfig(t, "tls-config", cfg), Services: svc})
	if err != nil {
		return nil, err
	}
	return n.(*tlsConfigNode), nil
}

// withTLS is the services a TCP or HTTP node is built with: one tls-config
// under the id "tls1".
func withTLS(cfg *tlsConfigNode) *servicesWithConfig {
	return &servicesWithConfig{testServices: newTestServices(), configs: map[string]node.Node{"tls1": cfg}}
}

// tlsLineServer accepts TLS connections and answers each line with
// "echo:<line>", reporting the line, the client certificate's name and the
// SNI the client sent.
type tlsLineServer struct {
	port  int
	lines chan string
	peers chan string
	sni   chan string
}

func startTLSLineServer(t *testing.T, cert tls.Certificate, clientCAs *x509.CertPool) *tlsLineServer {
	t.Helper()
	s := &tlsLineServer{lines: make(chan string, 16), peers: make(chan string, 16), sni: make(chan string, 16)}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			s.sni <- hello.ServerName
			return nil, nil
		},
	}
	if clientCAs != nil {
		cfg.ClientCAs = clientCAs
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s.port = ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				tc := c.(*tls.Conn)
				_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
				if err := tc.Handshake(); err != nil {
					return
				}
				if pc := tc.ConnectionState().PeerCertificates; len(pc) > 0 {
					s.peers <- pc[0].Subject.CommonName
				}
				r := bufio.NewReader(tc)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSuffix(line, "\n")
					s.lines <- line
					if _, err := tc.Write([]byte("echo:" + line + "\n")); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return s
}

func recv(t *testing.T, ch chan string, what string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("no %s within 10s", what)
		return ""
	}
}

// tcpRequestThrough builds a TCP Request on the line protocol against a port,
// with the tls-config as tls1.
func tcpRequestThrough(t *testing.T, cfg *tlsConfigNode, port int) node.Node {
	t.Helper()
	c, err := jsonConfig(map[string]any{
		"host": "127.0.0.1", "port": port, "out": "char", "splitc": `\n`,
		"datatype": "utf8", "tls": "tls1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return build(t, "tcp request", c, withTLS(cfg))
}

// ---------------------------------------------------------------------------
// TCP Request: the client side, and every way the server can be checked
// ---------------------------------------------------------------------------

func TestTLSTCPRequestTrustsTheConfiguredCA(t *testing.T) {
	pki := newTestPKI(t)
	srv := startTLSLineServer(t, pki.issue(t, "plc", nil, true).pair(t), nil)

	dir := t.TempDir()
	useSecretScope(t, dir)
	cfg := buildTLS(t, map[string]any{"ca": writeSecret(t, dir, "ca.pem", pki.caPEM)}, nil, nil)

	e, err := send(t, tcpRequestThrough(t, cfg, srv.port), msg(t, `{"payload":"status?\n"}`))
	if err != nil {
		t.Fatalf("the request failed: %v", err)
	}
	if got := e.on(0)[0].Payload(); got != "echo:status?" {
		t.Fatalf("the reply was %v", got)
	}
	if line := recv(t, srv.lines, "line at the server"); line != "status?" {
		t.Fatalf("the server read %q", line)
	}
}

// The test this node exists to pass and Node-RED's fails: a server signed by
// nobody the config trusts is refused. Node-RED's TCP Request builds its
// TLSSocket with rejectUnauthorized false and never looks again.
func TestTLSTCPRequestRefusesAServerItDoesNotTrust(t *testing.T) {
	pki := newTestPKI(t)
	srv := startTLSLineServer(t, pki.issue(t, "plc", nil, true).pair(t), nil)

	// No CA, so the system roots, which have never heard of this test's CA.
	strict := buildTLS(t, map[string]any{}, nil, nil)
	_, err := send(t, tcpRequestThrough(t, strict, srv.port), msg(t, `{"payload":"status?\n"}`))
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("an untrusted server was accepted, err = %v", err)
	}

	// Unticking verify is the user's call, and then it connects.
	lax := buildTLS(t, map[string]any{"verifyservercert": false}, nil, nil)
	e, err := send(t, tcpRequestThrough(t, lax, srv.port), msg(t, `{"payload":"status?\n"}`))
	if err != nil {
		t.Fatalf("with verification off: %v", err)
	}
	if got := e.on(0)[0].Payload(); got != "echo:status?" {
		t.Fatalf("the reply was %v", got)
	}
}

// The server name is both the SNI and the name the certificate is checked
// against, as in Node.js. A PLC's certificate names the PLC, not the address
// the flow dials.
func TestTLSServerNameIsSentAndChecked(t *testing.T) {
	pki := newTestPKI(t)
	srv := startTLSLineServer(t, pki.issue(t, "plc", []string{"plc.line3.local"}, false).pair(t), nil)
	creds := map[string]string{"cadata": string(pki.caPEM)}

	byAddress := buildTLS(t, map[string]any{}, creds, nil)
	if _, err := send(t, tcpRequestThrough(t, byAddress, srv.port), msg(t, `{"payload":"a\n"}`)); err == nil ||
		!strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("a certificate for plc.line3.local passed for 127.0.0.1, err = %v", err)
	}
	recv(t, srv.sni, "client hello")

	byName := buildTLS(t, map[string]any{"servername": "plc.line3.local"}, creds, nil)
	if _, err := send(t, tcpRequestThrough(t, byName, srv.port), msg(t, `{"payload":"a\n"}`)); err != nil {
		t.Fatalf("with the server name set: %v", err)
	}
	if sni := recv(t, srv.sni, "client hello"); sni != "plc.line3.local" {
		t.Fatalf("the server saw SNI %q", sni)
	}
}

// ---------------------------------------------------------------------------
// TCP Out: a client certificate the server demands
// ---------------------------------------------------------------------------

func TestTLSTCPOutPresentsItsClientCertificate(t *testing.T) {
	pki := newTestPKI(t)
	srv := startTLSLineServer(t, pki.issue(t, "plc", nil, true).pair(t), pki.pool())
	client := pki.issue(t, "line3-flow", nil, false)

	cfg := buildTLS(t, map[string]any{}, map[string]string{
		"certdata": string(client.certPEM), "keydata": string(client.keyPEM), "cadata": string(pki.caPEM),
	}, nil)
	c, err := jsonConfig(map[string]any{"beserver": "client", "host": "127.0.0.1", "port": srv.port, "tls": "tls1"})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "tcp out", c, withTLS(cfg))
	defer n.(*tcpOutNode).Close(t.Context(), false)

	if _, err := send(t, n, msg(t, `{"payload":"start line 3\n"}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if who := recv(t, srv.peers, "client certificate"); who != "line3-flow" {
		t.Fatalf("the server saw a certificate for %q", who)
	}
	if line := recv(t, srv.lines, "line at the server"); line != "start line 3" {
		t.Fatalf("the server read %q", line)
	}
}

// ---------------------------------------------------------------------------
// TCP In: a TLS listener, and a TLS client
// ---------------------------------------------------------------------------

func TestTLSTCPInListensWithTheConfigsCertificate(t *testing.T) {
	pki := newTestPKI(t)
	server := pki.issue(t, "flow", nil, true)
	cfg := buildTLS(t, map[string]any{}, map[string]string{
		"certdata": string(server.certPEM), "keydata": string(server.keyPEM),
	}, nil)

	port := freePort(t)
	c, err := jsonConfig(map[string]any{
		"server": "server", "port": port, "datamode": "stream", "datatype": "utf8",
		"newline": `\n`, "tls": "tls1",
	})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "tcp in", c, withTLS(cfg))
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	// A plaintext peer gets nothing through: what it sends is not a TLS
	// record, so no message comes out of it.
	plain := dialWithRetry(t, port)
	_, _ = plain.Write([]byte("plaintext\n"))
	plain.Close()

	conn, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{RootCAs: pki.pool()})
	if err != nil {
		t.Fatalf("a client trusting the CA could not connect: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("count=42\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the message off the TLS connection", func() bool { return e.total() >= 1 })
	time.Sleep(100 * time.Millisecond)
	sent := e.on(0)
	if len(sent) != 1 || sent[0].Payload() != "count=42" {
		var got []any
		for _, m := range sent {
			got = append(got, m.Payload())
		}
		t.Fatalf("messages %v, want only count=42", got)
	}
}

func TestTLSTCPInConnectsOutOverTLS(t *testing.T) {
	pki := newTestPKI(t)
	cert := pki.issue(t, "gateway", nil, true).pair(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("temp=71.5\n"))
		time.Sleep(2 * time.Second)
	}()

	cfg := buildTLS(t, map[string]any{}, map[string]string{"cadata": string(pki.caPEM)}, nil)
	c, err := jsonConfig(map[string]any{
		"server": "client", "host": "127.0.0.1", "port": ln.Addr().(*net.TCPAddr).Port,
		"datamode": "stream", "datatype": "utf8", "newline": `\n`, "tls": "tls1",
	})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "tcp in", c, withTLS(cfg))
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	waitFor(t, 10*time.Second, "the reading from the TLS server", func() bool { return e.total() == 1 })
	if got := e.on(0)[0].Payload(); got != "temp=71.5" {
		t.Fatalf("payload %v", got)
	}
}

// ---------------------------------------------------------------------------
// Where the certificate and key come from
// ---------------------------------------------------------------------------

// Each source is proved the same way: the config is used as a client
// certificate against a server that demands one, and the server reports whose
// certificate it got. A key that failed to load cannot pass that.
func TestTLSConfigLoadsEverySource(t *testing.T) {
	pki := newTestPKI(t)
	srv := startTLSLineServer(t, pki.issue(t, "plc", nil, true).pair(t), pki.pool())
	client := pki.issue(t, "line3-flow", nil, false)
	ca := string(pki.caPEM)

	dir := t.TempDir()
	useSecretScope(t, dir)
	certFile := writeSecret(t, dir, "tls.crt", client.certPEM)
	keyFile := writeSecret(t, dir, "tls.key", client.keyPEM)
	caFile := writeSecret(t, dir, "ca.crt", pki.caPEM)

	modern, err := pkcs12.Modern.Encode(client.key, client.cert, []*x509.Certificate{pki.ca}, "pfx-pass")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := pkcs12.Legacy.Encode(client.key, client.cert, []*x509.Certificate{pki.ca}, "pfx-pass")
	if err != nil {
		t.Fatal(err)
	}
	pfxFile := writeSecret(t, dir, "bundle.p12", modern)

	encDER, err := pkcs8.MarshalPrivateKey(client.key, []byte("key-pass"), nil)
	if err != nil {
		t.Fatal(err)
	}
	encPKCS8 := string(pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: encDER}))

	ecDER, err := x509.MarshalECPrivateKey(client.key)
	if err != nil {
		t.Fatal(err)
	}
	oldBlock, err := x509.EncryptPEMBlock(rand.Reader, "EC PRIVATE KEY", ecDER, []byte("key-pass"), x509.PEMCipherAES256)
	if err != nil {
		t.Fatal(err)
	}
	oldEncrypted := string(pem.EncodeToMemory(oldBlock))

	cases := []struct {
		name  string
		props map[string]any
		creds map[string]string
		env   map[string]string
	}{
		{"uploaded PEM", nil, map[string]string{
			"certdata": string(client.certPEM), "keydata": string(client.keyPEM), "cadata": ca}, nil},
		{"files", map[string]any{"cert": certFile, "key": keyFile, "ca": caFile}, nil, nil},
		{"environment variables by name", map[string]any{
			"certType": "env", "certEnv": "LINE3_CERT", "keyEnv": "LINE3_KEY", "caEnv": "LINE3_CA"}, nil,
			map[string]string{"LINE3_CERT": string(client.certPEM), "LINE3_KEY": string(client.keyPEM), "LINE3_CA": ca}},
		{"environment variables as ${NAME}", map[string]any{
			"certType": "env", "certEnv": "${LINE3_CERT}", "keyEnv": "${LINE3_KEY}", "caEnv": "${LINE3_CA}"}, nil,
			map[string]string{"LINE3_CERT": string(client.certPEM), "LINE3_KEY": string(client.keyPEM), "LINE3_CA": ca}},
		{"a PKCS#12 file, AES", map[string]any{"certType": "pfx", "p12": pfxFile},
			map[string]string{"passphrase": "pfx-pass"}, nil},
		{"an uploaded PKCS#12, 3DES and RC2", map[string]any{"certType": "pfx"},
			map[string]string{"p12data": base64.StdEncoding.EncodeToString(legacy), "passphrase": "pfx-pass"}, nil},
		{"an encrypted PKCS#8 key", nil, map[string]string{
			"certdata": string(client.certPEM), "keydata": encPKCS8, "cadata": ca, "passphrase": "key-pass"}, nil},
		{"an old-style encrypted PEM key", nil, map[string]string{
			"certdata": string(client.certPEM), "keydata": oldEncrypted, "cadata": ca, "passphrase": "key-pass"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			props := tc.props
			if props == nil {
				props = map[string]any{}
			}
			cfg := buildTLS(t, props, tc.creds, tc.env)
			if _, err := send(t, tcpRequestThrough(t, cfg, srv.port), msg(t, `{"payload":"hi\n"}`)); err != nil {
				t.Fatalf("the request failed: %v", err)
			}
			if who := recv(t, srv.peers, "client certificate"); who != "line3-flow" {
				t.Fatalf("the server saw %q", who)
			}
			recv(t, srv.lines, "line at the server")
		})
	}
}

// What openssl writes today, read back. Encrypting with the same library that
// decrypts proves only that the library agrees with itself; this is the file a
// site's IT department actually hands over.
func TestTLSConfigReadsWhatOpenSSLWrites(t *testing.T) {
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl is not on the PATH")
	}
	pki := newTestPKI(t)
	srv := startTLSLineServer(t, pki.issue(t, "plc", nil, true).pair(t), pki.pool())
	client := pki.issue(t, "line3-flow", nil, false)

	dir := t.TempDir()
	useSecretScope(t, dir)
	certFile := writeSecret(t, dir, "tls.crt", client.certPEM)
	keyFile := writeSecret(t, dir, "tls.key", client.keyPEM)
	caFile := writeSecret(t, dir, "ca.crt", pki.caPEM)

	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command(openssl, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("openssl %v: %v\n%s", args, err, out)
		}
	}
	pfxFile := filepath.Join(dir, "bundle.p12")
	run("pkcs12", "-export", "-in", certFile, "-inkey", keyFile, "-certfile", caFile,
		"-out", pfxFile, "-passout", "pass:pfx-pass")
	encKey := filepath.Join(dir, "tls-enc.key")
	run("pkcs8", "-topk8", "-v2", "aes-256-cbc", "-in", keyFile, "-out", encKey, "-passout", "pass:key-pass")

	for name, props := range map[string]map[string]any{
		"PKCS#12":              {"certType": "pfx", "p12": pfxFile},
		"encrypted PKCS#8 key": {"cert": certFile, "key": encKey, "ca": caFile},
	} {
		t.Run(name, func(t *testing.T) {
			pass := "pfx-pass"
			if props["key"] != nil {
				pass = "key-pass"
			}
			cfg := buildTLS(t, props, map[string]string{"passphrase": pass}, nil)
			if _, err := send(t, tcpRequestThrough(t, cfg, srv.port), msg(t, `{"payload":"hi\n"}`)); err != nil {
				t.Fatalf("the request failed: %v", err)
			}
			if who := recv(t, srv.peers, "client certificate"); who != "line3-flow" {
				t.Fatalf("the server saw %q", who)
			}
			recv(t, srv.lines, "line at the server")
		})
	}
}

// A broken config fails to deploy with a sentence that says what is wrong.
// Node-RED logs these and carries on without the certificate.
func TestTLSConfigRefusesWhatWouldNotWork(t *testing.T) {
	pki := newTestPKI(t)
	client := pki.issue(t, "line3-flow", nil, false)
	encDER, err := pkcs8.MarshalPrivateKey(client.key, []byte("key-pass"), nil)
	if err != nil {
		t.Fatal(err)
	}
	encPKCS8 := string(pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: encDER}))
	modern, err := pkcs12.Modern.Encode(client.key, client.cert, nil, "pfx-pass")
	if err != nil {
		t.Fatal(err)
	}

	inScope := t.TempDir()
	useSecretScope(t, inScope)
	outside := writeSecret(t, t.TempDir(), "tls.crt", client.certPEM)
	key := writeSecret(t, inScope, "tls.key", client.keyPEM)

	cases := []struct {
		name  string
		props map[string]any
		creds map[string]string
		env   map[string]string
		want  string
	}{
		{"a file outside the secret scope", map[string]any{"cert": outside, "key": key}, nil, nil,
			"secrets.allowedPaths"},
		{"a certificate path without a key", map[string]any{"cert": key}, nil, nil, "needs its private key"},
		{"an uploaded certificate without a key", nil, map[string]string{"certdata": string(client.certPEM)}, nil,
			"needs its private key"},
		{"a file that is not there", map[string]any{"cert": filepath.Join(inScope, "nope.crt"), "key": key}, nil, nil,
			"nope.crt"},
		{"an encrypted key with no passphrase", nil, map[string]string{
			"certdata": string(client.certPEM), "keydata": encPKCS8}, nil, "no passphrase"},
		{"an encrypted key with the wrong passphrase", nil, map[string]string{
			"certdata": string(client.certPEM), "keydata": encPKCS8, "passphrase": "guess"}, nil,
			"did not decrypt"},
		{"a bundle with the wrong passphrase", map[string]any{"certType": "pfx"}, map[string]string{
			"p12data": base64.StdEncoding.EncodeToString(modern), "passphrase": "guess"}, nil, "passphrase"},
		{"an environment variable that is not set", map[string]any{"certType": "env", "certEnv": "NOPE",
			"keyEnv": "ALSO_NOPE"}, nil, nil, "NOPE"},
		{"a CA that is not PEM", nil, map[string]string{"cadata": "not a certificate"}, nil, "no PEM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			props := tc.props
			if props == nil {
				props = map[string]any{}
			}
			_, err := tryBuildTLS(t, props, tc.creds, tc.env)
			if err == nil {
				t.Fatal("the config was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A node pointing at a TLS config that is not there must not quietly connect
// in plaintext, and a listener needs a certificate to present.
func TestTLSNodesRefuseAMissingOrUnusableConfig(t *testing.T) {
	c, _ := jsonConfig(map[string]any{"host": "127.0.0.1", "port": 1, "out": "immed", "tls": "missing"})
	if err := buildErr(t, "tcp request", c, withTLS(nil)); err == nil || !strings.Contains(err.Error(), "not deployed") {
		t.Fatalf("a missing TLS config was accepted: %v", err)
	}

	pki := newTestPKI(t)
	caOnly := buildTLS(t, map[string]any{}, map[string]string{"cadata": string(pki.caPEM)}, nil)
	c, _ = jsonConfig(map[string]any{"server": "server", "port": freePort(t), "datamode": "stream",
		"newline": `\n`, "tls": "tls1"})
	if err := buildErr(t, "tcp in", c, withTLS(caOnly)); err == nil || !strings.Contains(err.Error(), "needs a certificate") {
		t.Fatalf("a TLS listener with no certificate was accepted: %v", err)
	}
}

// ---------------------------------------------------------------------------
// HTTP Request
// ---------------------------------------------------------------------------

func TestTLSHTTPRequestUsesItsTLSConfig(t *testing.T) {
	pki := newTestPKI(t)
	serverCert := pki.issue(t, "historian", nil, true).pair(t)
	client := pki.issue(t, "line3-flow", nil, false)

	seen := make(chan string, 4)
	srv := httptestTLSServer(t, serverCert, pki.pool(), func(cn string) { seen <- cn })

	cfg := buildTLS(t, map[string]any{}, map[string]string{
		"certdata": string(client.certPEM), "keydata": string(client.keyPEM), "cadata": string(pki.caPEM),
	}, nil)
	c, _ := jsonConfig(map[string]any{"method": "GET", "url": srv, "ret": "txt", "tls": "tls1"})
	n := build(t, "http request", c, withTLS(cfg))
	defer n.(*httpRequestNode).Close(t.Context(), false)

	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("the request failed: %v", err)
	}
	if got := e.on(0)[0].Payload(); got != "hello line3-flow" {
		t.Fatalf("the body was %v", got)
	}
	if cn := recv(t, seen, "client certificate"); cn != "line3-flow" {
		t.Fatalf("the server saw %q", cn)
	}
}

// Without a tls-config the system roots are used, and msg.rejectUnauthorized
// false is the per-message way past them, as a boolean or as the string Node-RED
// also accepts. Anything else is ignored and the check stays on.
func TestTLSHTTPRequestRejectUnauthorized(t *testing.T) {
	pki := newTestPKI(t)
	srv := httptestTLSServer(t, pki.issue(t, "historian", nil, true).pair(t), nil, nil)
	c, _ := jsonConfig(map[string]any{"method": "GET", "url": srv, "ret": "txt"})
	n := build(t, "http request", c, newTestServices())
	defer n.(*httpRequestNode).Close(t.Context(), false)

	for _, m := range []string{`{}`, `{"rejectUnauthorized":true}`, `{"rejectUnauthorized":"maybe"}`} {
		if _, err := send(t, n, msg(t, m)); err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("%s reached an untrusted server, err = %v", m, err)
		}
	}
	for _, m := range []string{`{"rejectUnauthorized":false}`, `{"rejectUnauthorized":"FALSE"}`} {
		e, err := send(t, n, msg(t, m))
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if got := e.on(0)[0].Payload(); got != "hello anonymous" {
			t.Fatalf("%s: body %v", m, got)
		}
	}
	// One message turning the check off leaves the next one checked.
	if _, err := send(t, n, msg(t, `{}`)); err == nil {
		t.Fatal("the check stayed off after the message that turned it off")
	}
}

// httptestTLSServer serves "hello <client certificate's name>" over TLS with
// the given certificate, demanding a client certificate when clientCAs is set,
// and returns its URL.
func httptestTLSServer(t *testing.T, cert tls.Certificate, clientCAs *x509.CertPool, seen func(string)) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := "anonymous"
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			who = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		if seen != nil {
			seen(who)
		}
		_, _ = w.Write([]byte("hello " + who))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	if clientCAs != nil {
		srv.TLS.ClientCAs = clientCAs
		srv.TLS.ClientAuth = tls.RequireAndVerifyClientCert
	}
	// The server's own error log would print every refused handshake, which
	// is the outcome half these tests want.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL
}

// ---------------------------------------------------------------------------
// MQTT
// ---------------------------------------------------------------------------

// tlsTerminator is a TLS front for the plain test broker: it demands a client
// certificate from the CA, then pipes the bytes through. The broker is real;
// the TLS in front of it is too, and it records whose certificate it got.
func tlsTerminator(t *testing.T, cert tls.Certificate, clientCAs *x509.CertPool, target string) (int, chan string) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert}, ClientCAs: clientCAs, ClientAuth: tls.RequireAndVerifyClientCert,
	})
	if err != nil {
		t.Fatal(err)
	}
	peers := make(chan string, 16)
	var mu sync.Mutex
	var open []net.Conn
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range open {
			c.Close()
		}
		mu.Unlock()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				tc := c.(*tls.Conn)
				if err := tc.Handshake(); err != nil {
					tc.Close()
					return
				}
				peers <- tc.ConnectionState().PeerCertificates[0].Subject.CommonName
				up, err := net.Dial("tcp", target)
				if err != nil {
					tc.Close()
					return
				}
				mu.Lock()
				open = append(open, tc, up)
				mu.Unlock()
				go func() { _, _ = io.Copy(up, tc); up.Close() }()
				_, _ = io.Copy(tc, up)
				tc.Close()
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, peers
}

// An MQTT broker config written the way Node-RED writes it, usetls and a
// tls-config id, connects over TLS with the config's client certificate, on
// both protocol versions. Before this, the id was read as a boolean, came out
// false, and the connection went out in plaintext to a TLS port.
func TestMQTTOverTLSWithATLSConfig(t *testing.T) {
	addr, _, _ := mqttAddr(t)
	pki := newTestPKI(t)
	client := pki.issue(t, "line3-flow", nil, false)
	port, peers := tlsTerminator(t, pki.issue(t, "broker", nil, true).pair(t), pki.pool(), addr)
	cfg := buildTLS(t, map[string]any{}, map[string]string{
		"certdata": string(client.certPEM), "keydata": string(client.keyPEM), "cadata": string(pki.caPEM),
	}, nil)

	for _, version := range []string{"4", "5"} {
		t.Run("protocol "+version, func(t *testing.T) {
			svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{"tls1": cfg}}
			raw, _ := json.Marshal(map[string]any{
				"broker": "127.0.0.1", "port": port, "protocolVersion": version,
				"usetls": true, "tls": "tls1", "clientid": "flowgaps-tls-" + engine.GenerateID(),
			})
			b := build(t, "mqtt-broker", string(raw), svc)
			defer b.(node.Closer).Close(context.Background(), false)
			svc.configs["brk"] = b

			topic := "flowgaps/tls/" + engine.GenerateID()
			in := build(t, "mqtt in", `{"broker":"brk","topic":"`+topic+`","qos":"1","datatype":"utf8"}`, svc)
			e := newTestEmitter()
			_, cancel := startNode(t, in, e)
			defer cancel()
			waitConnected(t, b)
			if who := recv(t, peers, "client certificate"); who != "line3-flow" {
				t.Fatalf("the TLS front saw %q", who)
			}
			time.Sleep(300 * time.Millisecond)

			out := build(t, "mqtt out", `{"broker":"brk","topic":"`+topic+`","qos":"1"}`, svc)
			if _, err := send(t, out, msg(t, `{"payload":"over tls"}`)); err != nil {
				t.Fatalf("publish: %v", err)
			}
			waitFor(t, 10*time.Second, "the message back through the broker", func() bool { return e.total() == 1 })
			if got := e.on(0)[0].Payload(); got != "over tls" {
				t.Fatalf("payload %v", got)
			}
		})
	}
}

// A broker pointing at a tls-config that is not there fails to build rather
// than connecting without the certificate it was given.
func TestMQTTBrokerRefusesAMissingTLSConfig(t *testing.T) {
	err := buildErr(t, "mqtt-broker", `{"broker":"127.0.0.1","port":8883,"usetls":true,"tls":"gone"}`,
		&credServices{testServices: newTestServices(), configs: map[string]node.Node{}})
	if err == nil || !strings.Contains(err.Error(), "not deployed") {
		t.Fatalf("a missing tls-config was accepted: %v", err)
	}
}
