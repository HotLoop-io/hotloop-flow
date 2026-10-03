package main

import (
	"context"
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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/flowhttp"
	"github.com/HotLoop-io/hotloop-flow/internal/nodes"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// WebSocket nodes through the real runtime, the real deploy path and real
// sockets. Before this, the runtime never started a configuration node, so
// both of these failed in every deployment while the node tests, which start
// the config node by hand, passed.

func withFlowRoutes(t *testing.T, app *application) string {
	t.Helper()
	prev := nodes.Routes
	nodes.Routes = flowhttp.NewRouter(app.cfg.Server.HTTPRoot, nil)
	t.Cleanup(func() { nodes.Routes = prev })
	e := serveAppWith(t, app, map[string][]string{"dana": {"*"}}, func(d *api.Deps) {
		d.FlowRoutes = nodes.Routes
	})
	return e.URL
}

func TestWebSocketListenerServesThroughTheRuntime(t *testing.T) {
	app, rec := newApp(t)
	base := withFlowRoutes(t, app)
	deployJSON(t, app, `[{"id":"t1","type":"tab","label":"Line 3"},
	  {"id":"wsl","type":"websocket-listener","path":"/feed","wholemsg":"false"},
	  {"id":"in","type":"websocket in","z":"t1","server":"wsl","client":"","x":100,"y":100,"wires":[["d1"]]},
	  {"id":"d1","type":"debug","z":"t1","complete":"payload","x":300,"y":100,"wires":[]}]`, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/feed", nil)
	if err != nil {
		t.Fatalf("the listener's path is not served: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	if err := conn.Write(ctx, websocket.MessageText, []byte("count=42")); err != nil {
		t.Fatal(err)
	}
	if got := rec.debugFrom(t, "d1"); got != "count=42" {
		t.Fatalf("the flow got %q", got)
	}
}

// A websocket-client dials out over TLS to a server only the tls-config's CA
// trusts, and its messages reach the flow.
func TestWebSocketClientDialsOutOverTLS(t *testing.T) {
	caPEM, cert := wsTestCA(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		_ = c.Write(r.Context(), websocket.MessageText, []byte("temp=71.5"))
		<-r.Context().Done()
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	app, rec := newApp(t)
	ca, _ := json.Marshal(caPEM)
	deployJSON(t, app, `[{"id":"t1","type":"tab","label":"Line 3"},
	  {"id":"tls1","type":"tls-config","credentials":{"cadata":`+string(ca)+`}},
	  {"id":"wsc","type":"websocket-client","path":"wss`+strings.TrimPrefix(srv.URL, "https")+`/","tls":"tls1","wholemsg":"false"},
	  {"id":"in","type":"websocket in","z":"t1","server":"","client":"wsc","x":100,"y":100,"wires":[["d1"]]},
	  {"id":"d1","type":"debug","z":"t1","complete":"payload","x":300,"y":100,"wires":[]}]`, "")

	if got := rec.debugFrom(t, "d1"); got != "temp=71.5" {
		t.Fatalf("the flow got %q", got)
	}

	// The control: without the tls-config the system roots refuse the
	// server, nothing reaches the flow, and the refusal is in the log
	// against the client's id instead of retried in silence.
	rec.reset()
	deployJSON(t, app, `[{"id":"t1","type":"tab","label":"Line 3"},
	  {"id":"wsc","type":"websocket-client","path":"wss`+strings.TrimPrefix(srv.URL, "https")+`/","wholemsg":"false"},
	  {"id":"in","type":"websocket in","z":"t1","server":"","client":"wsc","x":100,"y":100,"wires":[["d1"]]},
	  {"id":"d1","type":"debug","z":"t1","complete":"payload","x":300,"y":100,"wires":[]}]`, "")
	deadline := time.Now().Add(10 * time.Second)
	for !rec.logged("wsc", "certificate") {
		if time.Now().After(deadline) {
			t.Fatal("the refused certificate was never logged against the websocket-client")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := rec.lastDebug("d1"); ok {
		t.Fatal("a message came through a server nobody trusts")
	}
}

// logged reports whether a log event against the node mentions the text.
func (r *recorder) logged(nodeID, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Topic == runtime.TopicLog && e.Data["nodeId"] == nodeID {
			if m, _ := e.Data["message"].(string); strings.Contains(m, text) {
				return true
			}
		}
	}
	return false
}

// wsTestCA makes a CA and a server certificate for 127.0.0.1 signed by it.
func wsTestCA(t *testing.T) (string, tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Line 3 CA"},
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
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "gateway"},
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
