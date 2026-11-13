package nodes

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A small SMTP server on loopback that keeps what it was given.

type smtpCapture struct {
	mu       sync.Mutex
	from     string
	rcpt     []string
	data     string
	auth     string
	startTLS bool
	tlsSeen  bool
}

type fakeSMTP struct {
	ln       net.Listener
	cert     *tls.Certificate
	advAuth  string // mechanisms advertised, empty for none
	offerTLS bool
	implicit bool
	cap      smtpCapture
}

func selfSigned(t *testing.T) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func startSMTP(t *testing.T, s *fakeSMTP) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if s.implicit {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*s.cert}})
	}
	s.ln = ln
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func (s *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := func(line string) { _, _ = io.WriteString(c, line+"\r\n") }
	w("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			w("250-fake")
			if s.offerTLS && !s.cap.tlsSeen {
				w("250-STARTTLS")
			}
			if s.advAuth != "" {
				w("250-AUTH " + s.advAuth)
			}
			w("250 8BITMIME")
		case up == "STARTTLS":
			w("220 go ahead")
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*s.cert}})
			if err := tc.Handshake(); err != nil {
				return
			}
			c = tc
			r = bufio.NewReader(tc)
			w = func(line string) { _, _ = io.WriteString(tc, line+"\r\n") }
			s.cap.mu.Lock()
			s.cap.tlsSeen, s.cap.startTLS = true, true
			s.cap.mu.Unlock()
		case strings.HasPrefix(up, "AUTH"):
			s.cap.mu.Lock()
			s.cap.auth = line
			s.cap.mu.Unlock()
			if strings.HasPrefix(up, "AUTH LOGIN") {
				w("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
				u, _ := r.ReadString('\n')
				w("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
				p, _ := r.ReadString('\n')
				s.cap.mu.Lock()
				s.cap.auth = "LOGIN " + strings.TrimSpace(u) + " " + strings.TrimSpace(p)
				s.cap.mu.Unlock()
			}
			w("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			s.cap.mu.Lock()
			s.cap.from = angle(line[10:])
			s.cap.mu.Unlock()
			w("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			s.cap.mu.Lock()
			s.cap.rcpt = append(s.cap.rcpt, angle(line[8:]))
			s.cap.mu.Unlock()
			w("250 ok")
		case up == "DATA":
			w("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.cap.mu.Lock()
			s.cap.data = b.String()
			s.cap.mu.Unlock()
			w("250 queued")
		case up == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

func parseMail(t *testing.T, raw string) (*mail.Message, string) {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("what the server got is not a message: %v\n%s", err, raw)
	}
	mt, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	body, _ := io.ReadAll(m.Body)
	if strings.HasPrefix(mt, "multipart/") {
		return m, string(body) + "|" + params["boundary"]
	}
	dec, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.ReplaceAll(string(body), "\r\n", ""), "\n", ""))
	if err != nil {
		t.Fatalf("body is not base64: %v", err)
	}
	return m, string(dec)
}

func TestEmailSendsOverPlainSMTPWithABasicLogin(t *testing.T) {
	s := &fakeSMTP{advAuth: "PLAIN LOGIN"}
	port := startSMTP(t, s)
	svc := newConfigServices()
	svc.creds["userid"], svc.creds["password"] = "plant@example.com", "pw"
	n := build(t, "e-mail", `{"server":"127.0.0.1","port":"`+strconv.Itoa(port)+`","authtype":"BASIC","secure":false,"tls":false,"name":"ops@example.com","dname":"alarms"}`, svc)
	e, err := send(t, n, msg(t, `{"payload":"Dryer 3 is over temperature","topic":"Alarm: dryer 3"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.statuses) == 0 || e.statuses[len(e.statuses)-1].Text != "sent" {
		t.Fatalf("status = %v", e.statuses)
	}
	s.cap.mu.Lock()
	defer s.cap.mu.Unlock()
	if s.cap.from != "plant@example.com" || len(s.cap.rcpt) != 1 || s.cap.rcpt[0] != "ops@example.com" {
		t.Fatalf("envelope = %q %v", s.cap.from, s.cap.rcpt)
	}
	if !strings.HasPrefix(s.cap.auth, "AUTH PLAIN") {
		t.Fatalf("auth = %q", s.cap.auth)
	}
	m, body := parseMail(t, s.cap.data)
	if body != "Dryer 3 is over temperature" || m.Header.Get("Subject") != "Alarm: dryer 3" || m.Header.Get("To") == "" {
		t.Fatalf("message = %q / %q / %q", body, m.Header.Get("Subject"), m.Header.Get("To"))
	}
}

func TestEmailFallsBackToLoginAuthWhenPlainIsNotOffered(t *testing.T) {
	s := &fakeSMTP{advAuth: "LOGIN"}
	port := startSMTP(t, s)
	svc := newConfigServices()
	svc.creds["userid"], svc.creds["password"] = "plant@example.com", "pw"
	n := build(t, "e-mail", `{"server":"127.0.0.1","port":`+strconv.Itoa(port)+`,"authtype":"BASIC","name":"ops@example.com"}`, svc)
	if _, err := send(t, n, msg(t, `{"payload":"x"}`)); err != nil {
		t.Fatal(err)
	}
	s.cap.mu.Lock()
	defer s.cap.mu.Unlock()
	want := "LOGIN " + base64.StdEncoding.EncodeToString([]byte("plant@example.com")) + " " + base64.StdEncoding.EncodeToString([]byte("pw"))
	if s.cap.auth != want {
		t.Fatalf("auth = %q", s.cap.auth)
	}
}

func TestEmailStartTLSAndTheCertificateSwitch(t *testing.T) {
	s := &fakeSMTP{offerTLS: true, cert: selfSigned(t)}
	port := startSMTP(t, s)
	svc := newConfigServices()
	n := build(t, "e-mail", `{"server":"127.0.0.1","port":"`+strconv.Itoa(port)+`","authtype":"NONE","secure":false,"tls":false,"name":"a@example.com"}`, svc)
	if _, err := send(t, n, msg(t, `{"payload":"hello","topic":"t","from":"me@example.com"}`)); err != nil {
		t.Fatal(err)
	}
	s.cap.mu.Lock()
	upgraded := s.cap.startTLS
	s.cap.mu.Unlock()
	if !upgraded {
		t.Fatal("the node did not ask for STARTTLS when the server offered it")
	}
	strict := build(t, "e-mail", `{"server":"127.0.0.1","port":"`+strconv.Itoa(port)+`","authtype":"NONE","secure":false,"tls":true,"name":"a@example.com"}`, svc)
	s.cap.mu.Lock()
	s.cap.tlsSeen = false
	s.cap.mu.Unlock()
	_, err := send(t, strict, msg(t, `{"payload":"hello","from":"me@example.com"}`))
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("tls=true against a self-signed server should fail on the certificate, got %v", err)
	}
}

func TestEmailImplicitTLS(t *testing.T) {
	s := &fakeSMTP{implicit: true, cert: selfSigned(t)}
	port := startSMTP(t, s)
	n := build(t, "e-mail", `{"server":"127.0.0.1","port":"`+strconv.Itoa(port)+`","authtype":"NONE","secure":true,"tls":false,"name":"a@example.com"}`, newConfigServices())
	if _, err := send(t, n, msg(t, `{"payload":"over a secure connection","from":"me@example.com"}`)); err != nil {
		t.Fatal(err)
	}
	s.cap.mu.Lock()
	defer s.cap.mu.Unlock()
	if !strings.Contains(s.cap.data, "Subject: Message from Flow") {
		t.Fatalf("the default subject is missing: %q", s.cap.data)
	}
}

func TestEmailXOAuth2SendsTheTokenFromTheMessage(t *testing.T) {
	s := &fakeSMTP{advAuth: "XOAUTH2"}
	port := startSMTP(t, s)
	svc := newConfigServices()
	svc.creds["userid"] = "eric@example.com"
	n := build(t, "e-mail", `{"server":"127.0.0.1","port":"`+strconv.Itoa(port)+`","authtype":"XOAUTH2","token":"oauth2Response.access_token","name":"a@example.com"}`, svc)
	if _, err := send(t, n, msg(t, `{"payload":"x","oauth2Response":{"access_token":"tok123"}}`)); err != nil {
		t.Fatal(err)
	}
	s.cap.mu.Lock()
	got := s.cap.auth
	s.cap.mu.Unlock()
	want := "AUTH XOAUTH2 " + base64.StdEncoding.EncodeToString([]byte("user=eric@example.com\x01auth=Bearer tok123\x01\x01"))
	if got != want {
		t.Fatalf("auth = %q", got)
	}
	if _, err := send(t, n, msg(t, `{"payload":"x"}`)); err == nil || !strings.Contains(err.Error(), "access token") {
		t.Fatalf("a missing token should say so, got %v", err)
	}
}

func TestEmailAttachmentsAndBufferPayload(t *testing.T) {
	s := &fakeSMTP{}
	port := startSMTP(t, s)
	svc := newConfigServices()
	n := build(t, "e-mail", `{"server":"127.0.0.1","port":"`+strconv.Itoa(port)+`","authtype":"NONE","name":"a@example.com"}`, svc)
	m := msg(t, `{"payload":"see attached","from":"me@example.com","attachments":[{"filename":"readings.csv","content":"a,b\n1,2\n"}]}`)
	if _, err := send(t, n, m); err != nil {
		t.Fatal(err)
	}
	s.cap.mu.Lock()
	raw := s.cap.data
	s.cap.mu.Unlock()
	msgv, rest := parseMail(t, raw)
	mt, params, _ := mime.ParseMediaType(msgv.Header.Get("Content-Type"))
	if mt != "multipart/mixed" {
		t.Fatalf("content type = %s", mt)
	}
	mr := multipart.NewReader(strings.NewReader(strings.SplitN(rest, "|", 2)[0]), params["boundary"])
	var names []string
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(p)
		dec, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(b), "\r\n", ""))
		if fn := p.FileName(); fn != "" {
			names = append(names, fn)
			if string(dec) != "a,b\n1,2\n" {
				t.Errorf("attachment body = %q", dec)
			}
		} else if string(dec) != "see attached" {
			t.Errorf("text part = %q", dec)
		}
	}
	if len(names) != 1 || names[0] != "readings.csv" {
		t.Fatalf("attachments = %v", names)
	}
}

func TestEmailRefusesHeaderInjectionAndMissingRecipients(t *testing.T) {
	svc := newConfigServices()
	svc.standIn = &recordingStandIn{}
	n := build(t, "e-mail", `{"server":"127.0.0.1","port":"25","authtype":"NONE","name":""}`, svc)
	if _, err := send(t, n, msg(t, `{"payload":"x","from":"a@example.com"}`)); err == nil || !strings.Contains(err.Error(), "recipient") {
		t.Fatalf("no recipient: %v", err)
	}
	if _, err := send(t, n, msg(t, `{"payload":"x","from":"a@example.com","to":"b@example.com","topic":"hi\r\nBcc: evil@example.com"}`)); err == nil || !strings.Contains(err.Error(), "line break") {
		t.Fatalf("subject injection: %v", err)
	}
	if _, err := send(t, n, msg(t, `{"payload":"x","from":"a@example.com","to":"b@example.com\r\nBcc: evil@example.com"}`)); err == nil {
		t.Fatal("recipient injection accepted")
	}
	// msg.to is used when the node has no To, and the node's To wins when it has one.
	if _, err := send(t, n, msg(t, `{"payload":"x","from":"a@example.com","to":"b@example.com"}`)); err != nil {
		t.Fatalf("msg.to: %v", err)
	}
}

func TestEmailAcceptsTheShapeTheFlowsCarry(t *testing.T) {
	svc := newConfigServices()
	for _, cfg := range []string{
		`{"server":"smtp.office365.com","port":"587","authtype":"BASIC","saslformat":true,"token":"oauth2Response.access_token","secure":false,"tls":true,"name":"ops@example.com","dname":"test"}`,
		`{"server":"smtp.mailgun.org","port":"587","authtype":"BASIC","saslformat":true,"token":"oauth2Response.access_token","secure":false,"tls":true,"name":"ops@example.com","dname":"test"}`,
	} {
		build(t, "e-mail", cfg, svc)
	}
	if err := buildErr(t, "e-mail", `{"server":"","port":"587","authtype":"BASIC"}`, svc); err == nil {
		t.Fatal("an empty server was accepted")
	}
}

// angle is the address inside <...>, without the parameters after it.
func angle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ">"); i >= 0 {
		s = s[:i]
	}
	return strings.Trim(s, "<> ")
}
