package nodes

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uapolicy"
)

// ---------------------------------------------------------------------------
// No server needed
// ---------------------------------------------------------------------------

func TestOPCUAConfigRefusesWhatCannotWork(t *testing.T) {
	svc := newTestServices()
	svc.creds["password"] = "pw"
	for _, c := range []struct{ cfg, want string }{
		{`{}`, "endpoint is required"},
		{`{"endpoint":"http://plc:4840"}`, "opc.tcp"},
		{`{"endpoint":"plc:4840"}`, "opc.tcp"},
		{`{"endpoint":"opc.tcp://plc:4840","securityMode":"Encrypt"}`, "not None, Sign or SignAndEncrypt"},
		{`{"endpoint":"opc.tcp://plc:4840","securityMode":"Sign","securityPolicy":"Wishful"}`, "not one of"},
		{`{"endpoint":"opc.tcp://plc:4840","securityMode":"Sign","securityPolicy":"Aes256_Sha256_RsaPss"}`, "not one of"},
		{`{"endpoint":"opc.tcp://plc:4840","authType":"username"}`, "username is required"},
		{`{"endpoint":"opc.tcp://plc:4840","authType":"kerberos"}`, "anonymous or username"},
		{`{"endpoint":"opc.tcp://plc:4840","serverThumbprint":"abc"}`, "40 hex digits"},
		{`{"endpoint":"opc.tcp://plc:4840","timeout":0}`, "timeout"},
	} {
		err := buildErr(t, "hotloop-flow-opcua", c.cfg, svc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want %q", c.cfg, err, c.want)
		}
	}
	// A user with no password.
	nopw := newTestServices()
	if err := buildErr(t, "hotloop-flow-opcua", `{"endpoint":"opc.tcp://plc:4840","authType":"username","username":"u"}`, nopw); err == nil ||
		!strings.Contains(err.Error(), "password is required") {
		t.Errorf("a user with no password: %v", err)
	}

	for _, c := range []struct{ typ, cfg, want string }{
		{"opcua read", `{}`, "no server selected"},
		{"opcua read", `{"server":"s"}`, "node id is required"},
		{"opcua read", `{"server":"s","nodeId":"Tag1"}`, "isn't a node id"},
		{"opcua write", `{"server":"s","nodeId":"ns=2;s=A","dataType":"Decimal"}`, "data type"},
		{"opcua write", `{"nodeId":"ns=2;s=A"}`, "no server selected"},
		{"opcua subscribe", `{"server":"s"}`, "at least one node id"},
		{"opcua subscribe", `{"server":"s","nodes":[{"nodeId":"junk"}]}`, "isn't a node id"},
		{"opcua subscribe", `{"server":"s","nodes":[{"nodeId":"ns=2;s=A"}],"interval":1}`, "at least 10"},
	} {
		err := buildErr(t, c.typ, c.cfg, svc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: error = %v, want %q", c.typ, c.cfg, err, c.want)
		}
	}
}

func TestOPCUAWriteConvertsToTheTypeTheNodeWants(t *testing.T) {
	ok := []struct {
		typ  string
		in   any
		want any
	}{
		{"Int32", 42.0, int32(42)}, {"UInt16", 65535.0, uint16(65535)}, {"SByte", -128.0, int8(-128)},
		{"Float", 1.5, float32(1.5)}, {"Double", "2.5", 2.5}, {"Boolean", 1.0, true}, {"Boolean", "off", false},
		{"String", "hi", "hi"}, {"String", 12.0, "12"}, {"Int64", "-9223372036854775808", int64(-9223372036854775808)},
		{"UInt64", "18446744073709551615", uint64(18446744073709551615)},
	}
	for _, c := range ok {
		got, err := opcuaConvert(c.in, c.typ)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %#v = %#v, %v; want %#v", c.typ, c.in, got, err, c.want)
		}
	}
	bad := []struct {
		typ  string
		in   any
		want string
	}{
		{"Int32", 3.5, "whole number"}, {"UInt16", -1.0, "outside what UInt16 holds"}, {"Byte", 300.0, "outside"},
		{"Int32", "abc", "isn't a number"}, {"Boolean", 7.0, "on or off"}, {"Float", 1e300, "too big"},
		{"DateTime", "yesterday", "isn't a date"}, {"UInt64", 1e20, "Past 2^53"},
	}
	for _, c := range bad {
		_, err := opcuaConvert(c.in, c.typ)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %#v: error = %v, want %q", c.typ, c.in, err, c.want)
		}
	}
	v, err := opcuaVariant([]any{1.0, 2.0, 3.0}, "Int32")
	if err != nil || !reflect.DeepEqual(v.Value(), []int32{1, 2, 3}) {
		t.Errorf("a list should write an array: %#v, %v", v, err)
	}
	if _, err := opcuaVariant([]any{1.0, "x"}, "Int32"); err == nil || !strings.Contains(err.Error(), "value 1") {
		t.Errorf("the bad item in a list should be named: %v", err)
	}
}

func TestOPCUAValuesBecomeWhatAFlowCanCarry(t *testing.T) {
	cases := []struct {
		in   *ua.Variant
		want any
	}{
		{ua.MustVariant(int32(-5)), -5.0},
		{ua.MustVariant(uint16(7)), 7.0},
		{ua.MustVariant(float32(2.5)), 2.5},
		{ua.MustVariant("text"), "text"},
		{ua.MustVariant(true), true},
		{ua.MustVariant([]int32{1, 2}), []any{1.0, 2.0}},
		{ua.MustVariant(uint64(18446744073709551615)), "18446744073709551615"},
		{ua.MustVariant(int64(9007199254740993)), "9007199254740993"},
		{ua.MustVariant(int64(5)), 5.0},
		{ua.MustVariant(time.Date(2026, 10, 18, 14, 30, 0, 0, time.UTC)), "2026-10-18T14:30:00Z"},
		{ua.MustVariant(ua.NewLocalizedText("Läuft")), "Läuft"},
		{nil, nil},
	}
	for _, c := range cases {
		if got := opcuaVariantValue(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%#v became %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestOPCUAErrorsSayWhatToChange(t *testing.T) {
	for _, c := range []struct {
		sc   ua.StatusCode
		want string
	}{
		{ua.StatusBadNodeIDUnknown, "namespace index"},
		{ua.StatusBadNotWritable, "read-only"},
		{ua.StatusBadUserAccessDenied, "role"},
		{ua.StatusBadTypeMismatch, "data type"},
		{ua.StatusBadIdentityTokenRejected, "username and password"},
		{ua.StatusBadCertificateUntrusted, "trusted list"},
		{ua.StatusBadCertificateURIInvalid, "application URI"},
		{ua.StatusBadSecurityChecksFailed, "mode and policy"},
		{ua.StatusBadTooManySessions, "no sessions left"},
	} {
		err := explainOPCUA("opc.tcp://plc:4840", "reading ns=2;s=A", c.sc)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "opc.tcp://plc:4840") {
			t.Errorf("%v: %v, want it to mention %q", c.sc, err, c.want)
		}
	}
	if err := explainOPCUA("opc.tcp://plc:4840", "reading", context.DeadlineExceeded); err == nil ||
		!strings.Contains(err.Error(), "server is silent") || !strings.Contains(err.Error(), "next request opens a new one") {
		t.Errorf("a timeout: %v", err)
	}
}

func TestOPCUAOnlyAnErrorThatEndsTheSessionDropsIt(t *testing.T) {
	if opcuaDropsSession(ua.StatusBadNodeIDUnknown) || opcuaDropsSession(ua.StatusBadNotWritable) {
		t.Error("a bad status on one node is an answer. The session is fine")
	}
	if !opcuaDropsSession(ua.StatusBadSessionIDInvalid) || !opcuaDropsSession(context.DeadlineExceeded) || !opcuaDropsSession(io.EOF) {
		t.Error("a dead session or a silent server must not be reused")
	}
}

func TestOPCUAThumbprintPinning(t *testing.T) {
	der := []byte("a certificate")
	s1, s256 := sha1.Sum(der), sha256.Sum256(der)
	if err := checkOPCUAThumbprint(der, s1[:]); err != nil {
		t.Errorf("sha1: %v", err)
	}
	if err := checkOPCUAThumbprint(der, s256[:]); err != nil {
		t.Errorf("sha256: %v", err)
	}
	other := sha1.Sum([]byte("another"))
	if err := checkOPCUAThumbprint(der, other[:]); err == nil || !strings.Contains(err.Error(), "something else is answering") {
		t.Errorf("a different certificate: %v", err)
	}
	if err := checkOPCUAThumbprint(nil, s1[:]); err == nil {
		t.Error("a server that shows no certificate can't match a pin")
	}
}

func TestOPCUAReadAndWriteUnderAFlowTestNeverTouchTheNetwork(t *testing.T) {
	svc := newTestServices()
	si := &scriptedStandIn{reply: map[string]any{"values": []any{21.5, "ok"}}}
	svc.standIn = si
	n := build(t, "opcua read", `{"server":"srv","nodeId":"payload","nodeIdType":"msg"}`, svc)
	e, err := send(t, n, msg(t, `{"payload":["ns=2;s=Temp","ns=2;s=State"]}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := e.on(0)[0].Payload(); !reflect.DeepEqual(got, []any{21.5, "ok"}) {
		t.Errorf("payload = %#v", got)
	}
	sent := si.calls[0]["sent"].(map[string]any)
	if si.calls[0]["kind"] != "opcua" || sent["op"] != "read" || !reflect.DeepEqual(sent["nodeIds"], []any{"ns=2;s=Temp", "ns=2;s=State"}) {
		t.Errorf("the stand-in was told %#v", si.calls[0])
	}

	si.reply = nil
	w := build(t, "opcua write", `{"server":"srv","nodeId":"ns=2;s=Setpoint","dataType":"Int32","value":"payload","valueType":"msg"}`, svc)
	if _, err := send(t, w, msg(t, `{"payload":80}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	last := si.calls[len(si.calls)-1]["sent"].(map[string]any)
	if last["op"] != "write" || last["nodeId"] != "ns=2;s=Setpoint" || last["dataType"] != "Int32" || last["value"] != 80.0 {
		t.Errorf("the write was %#v", last)
	}
	// A value that can't be that type fails before it leaves.
	if _, err := send(t, w, msg(t, `{"payload":3.5}`)); err == nil || !strings.Contains(err.Error(), "whole number") {
		t.Errorf("3.5 into an Int32: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Against a real OPC UA server
// ---------------------------------------------------------------------------

// The server is Microsoft's OPC PLC (mcr.microsoft.com/iotedge/opc-plc), started
// with testdata/plant/opcplc_nodes.json and a login of flowuser/flowpass. The CI
// workflow has the exact command.
//
//	HOTLOOP_FLOW_TEST_OPCUA=localhost:50000 go test ./internal/nodes/ -run OPCUA -v

const (
	opcInt32  = "ns=3;s=Int32Tag"
	opcDouble = "ns=3;s=DoubleTag"
	opcString = "ns=3;s=StringTag"
	opcBool   = "ns=3;s=BoolTag"
	opcUInt   = "ns=3;s=UIntTag"
	opcRO     = "ns=3;s=ReadOnlyTag"
	opcSub    = "ns=3;s=SubTag"
	opcFast   = "ns=3;s=FastUInt1"
)

type opcOpts struct {
	mode, policy string
	user, pass   string
	extra        string
}

func opcLive(t *testing.T, addr string, o opcOpts) (*credServices, func()) {
	t.Helper()
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	cfg := `"endpoint":"opc.tcp://` + addr + `","timeout":4`
	if o.mode != "" {
		cfg += `,"securityMode":"` + o.mode + `","securityPolicy":"` + o.policy + `"`
	}
	if o.user != "" {
		svc.creds["password"] = o.pass
		cfg += `,"authType":"username","username":"` + o.user + `"`
	}
	cfg += o.extra
	srv := build(t, "hotloop-flow-opcua", `{`+cfg+`}`, svc)
	svc.configs["srv"] = srv
	return svc, func() { _ = srv.(node.Closer).Close(context.Background(), false) }
}

func opcRead(t *testing.T, svc node.Services, nodeID string) (any, map[string]any, error) {
	t.Helper()
	n := build(t, "opcua read", `{"server":"srv","nodeId":"`+nodeID+`"}`, svc)
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		return nil, nil, err
	}
	m := e.on(0)[0]
	info, _ := m.Data["opcua"].(map[string]any)
	return m.Payload(), info, nil
}

func opcWrite(t *testing.T, svc node.Services, nodeID, dataType, body string) error {
	t.Helper()
	n := build(t, "opcua write", `{"server":"srv","nodeId":"`+nodeID+`","dataType":"`+dataType+`"}`, svc)
	e, err := send(t, n, msg(t, body))
	if err == nil && len(e.on(0)) != 1 {
		t.Fatal("a write that worked should pass the message on")
	}
	return err
}

func TestOPCUAWriteIsReadBackFromTheRealServer(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_OPCUA")
	svc, done := opcLive(t, addr, opcOpts{})
	defer done()

	for _, c := range []struct {
		id, typ, body string
		want          any
	}{
		{opcInt32, "auto", `{"payload":424242}`, 424242.0},
		{opcInt32, "Int32", `{"payload":-7}`, -7.0},
		{opcDouble, "auto", `{"payload":-12.375}`, -12.375},
		{opcString, "auto", `{"payload":"Kessel 3 läuft"}`, "Kessel 3 läuft"},
		{opcBool, "auto", `{"payload":true}`, true},
		{opcUInt, "auto", `{"payload":65535}`, 65535.0},
	} {
		if err := opcWrite(t, svc, c.id, c.typ, c.body); err != nil {
			t.Errorf("write %s %s: %v", c.id, c.body, err)
			continue
		}
		got, info, err := opcRead(t, svc, c.id)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("read back %s = %#v, %v; want %#v", c.id, got, err, c.want)
			continue
		}
		t.Logf("wrote %s to %s, read back %#v (status %v, source time %v)", c.body, c.id, got, info["status"], info["sourceTimestamp"])
	}
}

func TestOPCUAReadSeveralNodesInOneRequest(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_OPCUA")
	svc, done := opcLive(t, addr, opcOpts{})
	defer done()
	if err := opcWrite(t, svc, opcInt32, "auto", `{"payload":11}`); err != nil {
		t.Fatal(err)
	}
	if err := opcWrite(t, svc, opcString, "auto", `{"payload":"eleven"}`); err != nil {
		t.Fatal(err)
	}
	n := build(t, "opcua read", `{"server":"srv","nodeId":"payload","nodeIdType":"msg"}`, svc)
	e, err := send(t, n, msg(t, `{"payload":["`+opcInt32+`","`+opcString+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := e.on(0)[0]
	if !reflect.DeepEqual(m.Payload(), []any{11.0, "eleven"}) {
		t.Errorf("payload = %#v", m.Payload())
	}
	if infos, _ := m.Data["opcua"].([]any); len(infos) != 2 {
		t.Errorf("msg.opcua should have one entry per node: %#v", m.Data["opcua"])
	}
}

func TestOPCUAServerSaysNoAndTheErrorSaysWhatToDo(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_OPCUA")
	svc, done := opcLive(t, addr, opcOpts{})
	defer done()

	_, _, err := opcRead(t, svc, "ns=3;s=NoSuchTag")
	if err == nil || !strings.Contains(err.Error(), "BadNodeIDUnknown") || !strings.Contains(err.Error(), "namespace index") {
		t.Errorf("unknown node: %v", err)
	} else {
		t.Logf("operator sees: %v", err)
	}

	err = opcWrite(t, svc, opcRO, "auto", `{"payload":1}`)
	if err == nil || !strings.Contains(err.Error(), "BadNotWritable") || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("read-only node: %v", err)
	} else {
		t.Logf("operator sees: %v", err)
	}

	// An explicit type the node doesn't have is the server's call.
	err = opcWrite(t, svc, opcInt32, "String", `{"payload":"nope"}`)
	if err == nil || !strings.Contains(err.Error(), "BadTypeMismatch") {
		t.Errorf("wrong type: %v", err)
	} else {
		t.Logf("operator sees: %v", err)
	}

	// None of that cost the session.
	if _, _, err := opcRead(t, svc, opcInt32); err != nil {
		t.Errorf("after the refusals the session should still work: %v", err)
	}
}

func TestOPCUASecurityModesPoliciesAndLogins(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_OPCUA")
	cases := []struct {
		name string
		o    opcOpts
	}{
		{"None, anonymous", opcOpts{}},
		{"None, user and password", opcOpts{user: "flowuser", pass: "flowpass"}},
		{"Sign Basic256Sha256, anonymous", opcOpts{mode: "Sign", policy: "Basic256Sha256"}},
		{"SignAndEncrypt Basic256Sha256, anonymous", opcOpts{mode: "SignAndEncrypt", policy: "Basic256Sha256"}},
		{"SignAndEncrypt Basic256Sha256, user and password", opcOpts{mode: "SignAndEncrypt", policy: "Basic256Sha256", user: "flowuser", pass: "flowpass"}},
		{"SignAndEncrypt Aes128_Sha256_RsaOaep, user and password", opcOpts{mode: "SignAndEncrypt", policy: "Aes128_Sha256_RsaOaep", user: "flowuser", pass: "flowpass"}},
		{"Sign Aes128_Sha256_RsaOaep, user and password", opcOpts{mode: "Sign", policy: "Aes128_Sha256_RsaOaep", user: "flowuser", pass: "flowpass"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, done := opcLive(t, addr, c.o)
			defer done()
			if err := opcWrite(t, svc, opcInt32, "auto", `{"payload":2718}`); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, _, err := opcRead(t, svc, opcInt32)
			if err != nil || got != 2718.0 {
				t.Fatalf("read back = %#v, %v", got, err)
			}
			t.Logf("%s: wrote 2718 and read back %v", c.name, got)
		})
	}
}

func TestOPCUALoginAndSecurityMistakesSayWhatToChange(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_OPCUA")

	svc, done := opcLive(t, addr, opcOpts{user: "flowuser", pass: "not-the-password"})
	_, _, err := opcRead(t, svc, opcInt32)
	done()
	if err == nil || !strings.Contains(err.Error(), "refused the login") || !strings.Contains(err.Error(), "Check the username and password") {
		t.Errorf("wrong password: %v", err)
	} else {
		t.Logf("wrong password, operator sees: %v", err)
	}

	// A server that doesn't offer what the config asks for: the real server's
	// own endpoint list, less the one asked for.
	eps, err := opcua.GetEndpoints(context.Background(), "opc.tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	var fewer []*ua.EndpointDescription
	for _, ep := range eps {
		if !(ep.SecurityPolicyURI == ua.FormatSecurityPolicyURI("Aes128_Sha256_RsaOaep") && ep.SecurityMode == ua.MessageSecurityModeSign) {
			fewer = append(fewer, ep)
		}
	}
	_, err = selectOPCUAEndpoint(fewer, "Aes128_Sha256_RsaOaep", ua.MessageSecurityModeSign)
	if err == nil || !strings.Contains(err.Error(), "it offers") || !strings.Contains(err.Error(), "Basic256Sha256/") ||
		strings.Contains(err.Error(), "Aes128_Sha256_RsaOaep/MessageSecurityModeSign,") || strings.Contains(err.Error(), "RsaPss") {
		t.Errorf("a policy the server doesn't offer should list the ones it does: %v", err)
	} else {
		t.Logf("unoffered policy, operator sees: %v", err)
	}
}

func TestOPCUAPinnedServerCertificate(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_OPCUA")
	eps, err := opcua.GetEndpoints(context.Background(), "opc.tcp://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	var der []byte
	for _, ep := range eps {
		if len(ep.ServerCertificate) > 0 {
			der = ep.ServerCertificate
			break
		}
	}
	sum := sha256.Sum256(der)
	right := hex.EncodeToString(sum[:])

	svc, done := opcLive(t, addr, opcOpts{mode: "SignAndEncrypt", policy: "Basic256Sha256", extra: `,"serverThumbprint":"` + right + `"`})
	if _, _, err := opcRead(t, svc, opcInt32); err != nil {
		t.Errorf("the right thumbprint should connect: %v", err)
	}
	done()

	wrong := strings.Repeat("ab", 32)
	svc, done = opcLive(t, addr, opcOpts{mode: "SignAndEncrypt", policy: "Basic256Sha256", extra: `,"serverThumbprint":"` + wrong + `"`})
	_, _, err = opcRead(t, svc, opcInt32)
	done()
	if err == nil || !strings.Contains(err.Error(), "isn't the one this config is pinned to") {
		t.Errorf("the wrong thumbprint should be refused: %v", err)
	} else {
		t.Logf("wrong pin, operator sees: %v", err)
	}
}

// pemClientCert makes a client certificate the way an engineer would hand one
// over: RSA, with an application URI in the subject alternative name or without.
func pemClientCert(t *testing.T, appURI string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "flow-test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment |
			x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames:    []string{"flow-test"},
	}
	if appURI != "" {
		u, _ := url.Parse(appURI)
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func TestOPCUAClientCertificateFromATLSConfig(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_OPCUA")

	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	cert, key := pemClientCert(t, "urn:flow-test:client")
	svc.creds["certdata"], svc.creds["keydata"] = cert, key
	svc.configs["tls"] = build(t, "tls-config", `{"certType":"files"}`, svc)
	srv := build(t, "hotloop-flow-opcua", `{"endpoint":"opc.tcp://`+addr+`","timeout":4,"securityMode":"SignAndEncrypt",
		"securityPolicy":"Basic256Sha256","tls":"tls"}`, svc)
	svc.configs["srv"] = srv
	defer srv.(node.Closer).Close(context.Background(), false)
	if err := opcWrite(t, svc, opcInt32, "auto", `{"payload":31415}`); err != nil {
		t.Fatalf("a connection made with the TLS config's own certificate: %v", err)
	}
	if got, _, err := opcRead(t, svc, opcInt32); err != nil || got != 31415.0 {
		t.Fatalf("read back = %#v, %v", got, err)
	}

	// A certificate with no application URI is refused at deploy, with the reason.
	svc2 := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	cert, key = pemClientCert(t, "")
	svc2.creds["certdata"], svc2.creds["keydata"] = cert, key
	svc2.configs["tls"] = build(t, "tls-config", `{"certType":"files"}`, svc2)
	err := buildErr(t, "hotloop-flow-opcua", `{"endpoint":"opc.tcp://`+addr+`","securityMode":"Sign","securityPolicy":"Basic256Sha256","tls":"tls"}`, svc2)
	if err == nil || !strings.Contains(err.Error(), "application URI") {
		t.Errorf("a certificate with no URI: %v", err)
	} else {
		t.Logf("operator sees at deploy: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Subscriptions, and what happens when the server goes silent and comes back
// ---------------------------------------------------------------------------

// flakyOPC is flakyLine with a third state. Silent swallows what the client
// sends and leaves the socket open, the way a hung server looks. Down closes
// every connection and refuses new ones, the way a server that is off does.
type flakyOPC struct {
	l        net.Listener
	upstream string
	silent   atomic.Bool
	down     atomic.Bool
	mu       sync.Mutex
	conns    []net.Conn
	accepted atomic.Int32
}

func newFlakyOPC(t *testing.T, upstream string) *flakyOPC {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	f := &flakyOPC{l: l, upstream: upstream}
	go f.serve()
	t.Cleanup(func() { l.Close(); f.dropAll() })
	return f
}

func (f *flakyOPC) addr() string { return f.l.Addr().String() }

func (f *flakyOPC) dropAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		c.Close()
	}
	f.conns = nil
}

func (f *flakyOPC) serve() {
	for {
		c, err := f.l.Accept()
		if err != nil {
			return
		}
		if f.down.Load() {
			c.Close()
			continue
		}
		f.accepted.Add(1)
		up, err := net.Dial("tcp", f.upstream)
		if err != nil {
			c.Close()
			continue
		}
		f.mu.Lock()
		f.conns = append(f.conns, c, up)
		f.mu.Unlock()
		go func() {
			buf := make([]byte, 64<<10)
			for {
				n, err := up.Read(buf)
				if err != nil {
					c.Close()
					return
				}
				if f.silent.Load() {
					continue
				}
				if _, err := c.Write(buf[:n]); err != nil {
					return
				}
			}
		}()
		go func() {
			buf := make([]byte, 64<<10)
			for {
				n, err := c.Read(buf)
				if err != nil {
					up.Close()
					return
				}
				if f.silent.Load() {
					continue
				}
				if _, err := up.Write(buf[:n]); err != nil {
					return
				}
			}
		}()
	}
}

func startSubscribe(t *testing.T, svc node.Services, cfg string) (*testEmitter, context.CancelFunc, node.Node) {
	t.Helper()
	sub := build(t, "opcua subscribe", cfg, svc)
	e := newTestEmitter()
	ctx, cancel := context.WithCancel(context.Background())
	if err := sub.(node.Starter).Start(ctx, e); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	return e, func() {
		cancel()
		_ = sub.(node.Closer).Close(context.Background(), false)
	}, sub
}

func waitUntilTrue(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("gave up waiting for %s after %s", what, within)
}

func lastStatus(e *testEmitter) node.Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.statuses) == 0 {
		return node.Status{}
	}
	return e.statuses[len(e.statuses)-1]
}

func TestOPCUASubscribeDeliversTheServersChanges(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_OPCUA")
	svc, done := opcLive(t, addr, opcOpts{mode: "SignAndEncrypt", policy: "Basic256Sha256", user: "flowuser", pass: "flowpass"})
	defer done()

	e, stop, _ := startSubscribe(t, svc, `{"server":"srv","interval":200,"nodes":[{"nodeId":"`+opcFast+`"},{"nodeId":"`+opcSub+`"}]}`)
	defer stop()

	// The simulator steps FastUInt1 once a second by itself.
	waitUntilTrue(t, "three changes of the simulator's own counter", 20*time.Second, func() bool {
		n := 0
		for _, m := range e.on(0) {
			if m.Topic() == opcFast {
				n++
			}
		}
		return n >= 3
	})
	var seen []float64
	for _, m := range e.on(0) {
		if m.Topic() == opcFast {
			seen = append(seen, m.Payload().(float64))
		}
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Errorf("the counter should only go up, got %v", seen)
		}
	}
	t.Logf("subscription delivered the simulator's counter: %v", seen)

	// A change another client makes arrives too. The write goes through the
	// write node, so the same two nodes talk to each other through the server.
	if err := opcWrite(t, svc, opcSub, "auto", `{"payload":9001}`); err != nil {
		t.Fatal(err)
	}
	waitUntilTrue(t, "the written value to come back through the subscription", 10*time.Second, func() bool {
		for _, m := range e.on(0) {
			if m.Topic() == opcSub && m.Payload() == 9001.0 {
				return true
			}
		}
		return false
	})
	for _, m := range e.on(0) {
		if m.Topic() == opcSub && m.Payload() == 9001.0 {
			info := m.Data["opcua"].(map[string]any)
			t.Logf("wrote 9001 to %s, the subscription delivered it: topic %s, status %v, source time %v",
				opcSub, m.Topic(), info["status"], info["sourceTimestamp"])
		}
	}
}

func TestOPCUASubscriptionSurvivesTheServerGoingSilentAndDownAndComingBack(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_OPCUA")
	line := newFlakyOPC(t, addr)
	svc, done := opcLive(t, line.addr(), opcOpts{extra: `,"timeout":2`})
	defer done()

	e, stop, _ := startSubscribe(t, svc, `{"server":"srv","interval":200,"nodes":[{"nodeId":"`+opcFast+`"}]}`)
	defer stop()

	count := func() int { return len(e.on(0)) }
	waitUntilTrue(t, "the first changes", 20*time.Second, func() bool { return count() >= 2 })
	if st := lastStatus(e); st.Fill != "green" {
		t.Errorf("a working subscription should show green, got %+v", st)
	}

	// The server hangs with the socket open.
	line.silent.Store(true)
	before := count()
	waitUntilTrue(t, "the node to notice the silence and say so", 40*time.Second, func() bool { return lastStatus(e).Fill == "red" })
	t.Logf("server silent, badge: %+v", lastStatus(e))
	e.mu.Lock()
	errs := append([]error(nil), e.errs...)
	e.mu.Unlock()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "silent") {
		t.Errorf("one error should have reached a Catch node, and say the server is silent: %v", errs)
	} else {
		t.Logf("operator sees: %v", errs[0])
	}

	// It stays gone for a while. The node keeps trying and doesn't flood anything.
	time.Sleep(4 * time.Second)
	e.mu.Lock()
	if len(e.errs) != 1 {
		t.Errorf("an outage is reported once, saw %d errors", len(e.errs))
	}
	e.mu.Unlock()

	// It comes back, and nobody redeploys anything.
	line.silent.Store(false)
	line.dropAll() // the old, hung connections are not coming back
	waitUntilTrue(t, "values to flow again", 60*time.Second, func() bool { return count() > before+1 && lastStatus(e).Fill == "green" })
	t.Logf("server back, badge: %+v, %d messages, %d connections made", lastStatus(e), count(), line.accepted.Load())

	// Now it is off, not hung: every connection closed and new ones refused.
	line.down.Store(true)
	line.dropAll()
	waitUntilTrue(t, "the node to see the server is gone", 40*time.Second, func() bool { return lastStatus(e).Fill == "red" })
	e.mu.Lock()
	n := len(e.errs)
	e.mu.Unlock()
	if n != 2 {
		t.Errorf("the second outage is a new report, so two errors in all, saw %d", n)
	}
	mid := count()
	line.down.Store(false)
	waitUntilTrue(t, "values to flow after the server was switched back on", 60*time.Second, func() bool { return count() > mid+1 && lastStatus(e).Fill == "green" })
	t.Logf("server switched back on, badge: %+v, %d messages, %d connections made", lastStatus(e), count(), line.accepted.Load())
}

func TestOPCUAReadDuringSilenceTimesOutAndRecovers(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_OPCUA")
	line := newFlakyOPC(t, addr)
	svc, done := opcLive(t, line.addr(), opcOpts{extra: `,"timeout":1.5`})
	defer done()

	if _, _, err := opcRead(t, svc, opcInt32); err != nil {
		t.Fatalf("before: %v", err)
	}
	line.silent.Store(true)
	start := time.Now()
	_, _, err := opcRead(t, svc, opcInt32)
	if err == nil || !strings.Contains(err.Error(), "silent") {
		t.Fatalf("a silent server: %v", err)
	}
	if took := time.Since(start); took > 6*time.Second {
		t.Errorf("gave up after %s on a 1.5s timeout", took)
	}
	t.Logf("server silent, operator sees: %v", err)

	line.silent.Store(false)
	line.dropAll()
	got, _, err := opcRead(t, svc, opcInt32)
	if err != nil {
		t.Fatalf("after it came back: %v", err)
	}
	t.Logf("server back, read %v after %d connections", got, line.accepted.Load())
}

func TestOPCUAUnknownHostSaysSo(t *testing.T) {
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	cfg := build(t, "hotloop-flow-opcua", `{"endpoint":"opc.tcp://no-such-plc.invalid:4840","timeout":1}`, svc)
	svc.configs["srv"] = cfg
	defer cfg.(node.Closer).Close(context.Background(), false)
	_, _, err := opcRead(t, svc, "ns=2;s=A")
	if err == nil || !strings.Contains(err.Error(), "couldn't find a host called") {
		t.Errorf("error = %v", err)
	}
}

// A server that accepts the connection and never says a word. Endpoint
// discovery against it takes the whole timeout plus five seconds, and the
// connect used to hold the server config's lock for all of it, so Close, and
// every other node on the same server, queued behind one hung dial.
func TestOPCUAHungDiscoveryDoesNotLockOutTheOtherNodes(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	var held []net.Conn
	var heldMu sync.Mutex
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			heldMu.Lock()
			held = append(held, c)
			heldMu.Unlock()
		}
	}()
	defer func() {
		l.Close()
		heldMu.Lock()
		for _, c := range held {
			c.Close()
		}
		heldMu.Unlock()
	}()

	svc, done := opcLive(t, l.Addr().String(), opcOpts{extra: `,"timeout":4`})
	srv := svc.configs["srv"].(*opcuaServer)

	first := make(chan error, 1)
	go func() {
		_, err := srv.client(context.Background())
		first <- err
	}()
	time.Sleep(300 * time.Millisecond)

	// A second node whose flow gives up after half a second leaves after half a
	// second, not after the first node's nine.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	_, err = srv.client(ctx)
	cancel()
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("a second caller waited %s and got %v", time.Since(start), err)
	}

	// Closing the config while the dial hangs returns at once, and the caller
	// still waiting is told why instead of sitting out the timeout.
	start = time.Now()
	if err := srv.Close(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("Close took %s behind a hung dial", took)
	}
	select {
	case err := <-first:
		if err == nil {
			t.Error("the hung connect reported success")
		}
		t.Logf("the first caller was told: %v", err)
	case <-time.After(3 * time.Second):
		t.Error("the first caller was still waiting 3s after Close")
	}
	_ = done
}

// Five nodes asking at once is one discovery and one session, not five.
func TestOPCUAConcurrentNodesShareOneSession(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_OPCUA")
	line := newFlakyOPC(t, addr)
	svc, done := opcLive(t, line.addr(), opcOpts{})
	defer done()
	srv := svc.configs["srv"].(*opcuaServer)

	var wg sync.WaitGroup
	clients := make([]*opcua.Client, 5)
	errs := make([]error, 5)
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			clients[i], errs[i] = srv.client(context.Background())
		}()
	}
	wg.Wait()
	for i := range clients {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if clients[i] != clients[0] {
			t.Errorf("caller %d got a different session", i)
		}
	}
	// One connection for discovery, one for the session.
	if n := line.accepted.Load(); n != 2 {
		t.Errorf("%d connections to the server for five callers, want 2 (discovery and the session)", n)
	}
	t.Logf("five callers, %d connections", line.accepted.Load())
}

// Aes256_Sha256_RsaPss is not in the policy list because gopcua gets the RSA-OAEP
// SHA-256 padding wrong. This test stands guard: it passes while that is still
// true, and the day a gopcua update fixes it, it fails and says what to do.
func TestOPCUAAes256RsaPssWaitsOnGopcua(t *testing.T) {
	const wrong = (2 * 64) + 2
	const right = (2 * 32) + 2 // RFC 8017 section 7.1.1: 2 * hLen + 2, and SHA-256's hLen is 32
	switch uapolicy.RSAOAEPMinPaddingSHA256 {
	case wrong:
		for _, p := range opcuaPolicies {
			if p == "Aes256_Sha256_RsaPss" {
				t.Fatalf("Aes256_Sha256_RsaPss is on the list while gopcua still pads RSA-OAEP-SHA256 with %d bytes instead of %d, "+
					"which Microsoft's OPC PLC refuses with BadSecurityChecksFailed", wrong, right)
			}
		}
	case right:
		t.Fatalf("gopcua now pads RSA-OAEP-SHA256 with %d bytes. Add Aes256_Sha256_RsaPss back to opcuaPolicies and the dropdown, "+
			"add it to TestOPCUASecurityModesPoliciesAndLogins, and delete this test", right)
	default:
		t.Fatalf("gopcua's RSAOAEPMinPaddingSHA256 is %d, which is neither the old %d nor the right %d. Look at it before trusting the policy", uapolicy.RSAOAEPMinPaddingSHA256, wrong, right)
	}
}
