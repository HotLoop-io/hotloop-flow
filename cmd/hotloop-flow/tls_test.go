package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// TLS through the real runtime: the tls-config's PEM arrives as credentials,
// gets split out of the flow file into the encrypted store like any password,
// and comes back to the node through the credential lookup. Then a partial
// deploy that swaps the trusted CA restarts the TCP node using it, and the
// server it trusted a moment ago is refused.

// selfSignedCA makes a CA and a server certificate for 127.0.0.1 it signed.
func selfSignedCA(t *testing.T, name string) (caPEM string, server tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "plc"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:   time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// echoOverTLS answers every line with "echo:<line>".
func echoOverTLS(t *testing.T, cert tls.Certificate) int {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				line, err := bufio.NewReader(c).ReadString('\n')
				if err == nil {
					_, _ = c.Write([]byte("echo:" + line))
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func tlsFlow(t *testing.T, port int, caPEM string) string {
	t.Helper()
	ca, _ := json.Marshal(caPEM)
	return `[{"id":"t1","type":"tab","label":"Line 3"},
	{"id":"tls1","type":"tls-config","name":"PLC","verifyservercert":true,"credentials":{"cadata":` + string(ca) + `}},
	{"id":"req","type":"tcp request","z":"t1","host":"127.0.0.1","port":"` + strconv.Itoa(port) + `","out":"char",
	 "splitc":"\\n","datatype":"utf8","tls":"tls1","x":100,"y":100,"wires":[["ok"]]},
	{"id":"ok","type":"debug","z":"t1","complete":"payload","x":300,"y":100,"wires":[]},
	{"id":"c1","type":"catch","z":"t1","x":100,"y":200,"wires":[["bad"]]},
	{"id":"bad","type":"debug","z":"t1","complete":"error.message","x":300,"y":200,"wires":[]}]`
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTLSConfigThroughTheRuntimeAndAPartialDeploy(t *testing.T) {
	app, rec := newApp(t)
	trusted, cert := selfSignedCA(t, "Line 3 CA")
	stranger, _ := selfSignedCA(t, "Somebody else's CA")
	port := echoOverTLS(t, cert)

	deployJSON(t, app, tlsFlow(t, port, trusted), runtime.DeployFull)
	if err := app.currentRuntime().Inject("req", engine.WrapMsg(map[string]any{"payload": "ping\n"})); err != nil {
		t.Fatal(err)
	}
	if got := rec.debugFrom(t, "ok"); got != "echo:ping" {
		t.Fatalf("the TLS request came back as %q", got)
	}

	// The PEM is a credential: it is not in the flow file on disk.
	if strings.Contains(string(mustRead(t, app.cfg.FlowPath())), "BEGIN CERTIFICATE") {
		t.Fatal("the CA certificate was written into the flow file")
	}

	// Swap the CA for one that never signed the server. Only the credentials
	// changed, and that still restarts the config and the node that uses it.
	rec.reset()
	up := deployJSON(t, app, tlsFlow(t, port, stranger), runtime.DeployNodes)
	slices.Sort(up.Restarted)
	if !slices.Equal(up.Restarted, []string{"req", "tls1"}) {
		t.Fatalf("a CA change restarted %v, want the tls-config and the TCP request", up.Restarted)
	}
	if err := app.currentRuntime().Inject("req", engine.WrapMsg(map[string]any{"payload": "ping\n"})); err != nil {
		t.Fatal(err)
	}
	if got := rec.debugFrom(t, "bad"); !strings.Contains(got, "certificate") {
		t.Fatalf("the server signed by the old CA was not refused: %q", got)
	}
	if _, ok := rec.lastDebug("ok"); ok {
		t.Fatal("a reply came back through a CA that should not trust the server")
	}
}
