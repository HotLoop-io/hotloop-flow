package nodes

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// The e-mail output node, with the property names node-red-node-email's flow
// entries use: server, port, authtype, secure, tls, name (the recipient, which
// is the node's "To"), dname (what the node is called) and the credentials
// userid and password. It sends over SMTP with the standard library. Nothing is
// ported from the community package.

func init() { registerNREmail() }

type nrEmailNode struct {
	server    string
	port      int
	authtype  string
	secure    bool
	verify    bool
	to        string
	fromName  string
	tokenPath string
	svc       node.Services
	timeout   time.Duration
}

func registerNREmail() {
	node.MustRegister(node.Descriptor{
		Type:         "e-mail",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "email",
		Inputs:       1,
		Outputs:      0,
		Align:        "right",
		PaletteLabel: "email",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Sends msg.payload as the body and msg.topic as the subject, to the node's To (the name " +
				"property) or, when that is empty, msg.to. msg.cc, msg.bcc, msg.from, msg.html and msg.attachments " +
				"([{filename, content, contentType}]) are honored, and a Buffer payload goes as an attachment. " +
				"secure is an encrypted connection from the first byte (port 465). Otherwise the node asks for " +
				"STARTTLS when the server offers it. tls means only connect to a server with a valid certificate. " +
				"Authentication is BASIC (PLAIN, or LOGIN as a fallback), XOAUTH2 with the access token read from " +
				"the message property in token, or NONE. A password is refused over a connection that is not " +
				"encrypted unless the server is this host. Sending only: the e-mail in node (reading a " +
				"mailbox) is not provided. msg.header and the saslformat switch are ignored.",
			UnsupportedProps: []string{"saslformat"},
		},
		Props: []node.Prop{
			{Name: "server", Kind: node.PropString, Label: "Server", Placeholder: "smtp.example.com"},
			{Name: "port", Kind: node.PropString, Label: "Port", Default: "465"},
			{Name: "authtype", Kind: node.PropSelect, Label: "Authentication", Default: "BASIC", Options: []node.Option{
				{Value: "BASIC", Label: "Basic"}, {Value: "XOAUTH2", Label: "XOAuth2"}, {Value: "NONE", Label: "None"}}},
			{Name: "saslformat", Kind: node.PropBool, Label: "SASL format"},
			{Name: "token", Kind: node.PropString, Label: "Token property", Default: "oauth2Response.access_token"},
			{Name: "secure", Kind: node.PropBool, Label: "Use a secure connection from the start", Default: true},
			{Name: "tls", Kind: node.PropBool, Label: "Only connect to servers with a valid certificate", Default: true},
			{Name: "name", Kind: node.PropString, Label: "To"},
			{Name: "dname", Kind: node.PropString, Label: "Name"},
			{Name: "userid", Kind: node.PropCredential, Label: "User"},
			{Name: "password", Kind: node.PropCredential, Label: "Password"},
		},
		Help: "Sends an e-mail. msg.payload is the body, msg.topic the subject, the node's To or msg.to the recipient.",
	}, newNREmail)
}

func newNREmail(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &nrEmailNode{
		server:    strings.TrimSpace(p.PropString("server", "")),
		authtype:  strings.ToUpper(p.PropString("authtype", "BASIC")),
		secure:    p.PropBool("secure", false),
		verify:    p.PropBool("tls", true),
		to:        strings.TrimSpace(p.PropString("name", "")),
		tokenPath: p.PropString("token", "oauth2Response.access_token"),
		svc:       def.Services,
		timeout:   30 * time.Second,
	}
	if n.server == "" {
		return nil, fmt.Errorf("server is required: the SMTP host that sends the mail")
	}
	port, err := strconv.Atoi(strings.TrimSpace(p.PropString("port", "")))
	if v, ok := p.Raw["port"].(float64); ok {
		port, err = int(v), nil
	}
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %v is not a number from 1 to 65535", p.Raw["port"])
	}
	n.port = port
	switch n.authtype {
	case "BASIC", "XOAUTH2", "NONE":
	default:
		return nil, fmt.Errorf("authtype %q is not BASIC, XOAUTH2 or NONE", n.authtype)
	}
	if n.to != "" {
		if _, err := mail.ParseAddressList(n.to); err != nil {
			return nil, fmt.Errorf("To %q is not an address: %v", n.to, err)
		}
	}
	return n, nil
}

func (n *nrEmailNode) credentials() (user, pass string) {
	user, _ = n.svc.Credential("userid")
	pass, _ = n.svc.Credential("password")
	return
}

// nrHeaderSafe refuses a line break in a header value, which is how a message
// field turns into a second header.
func nrHeaderSafe(what, v string) (string, error) {
	if strings.ContainsAny(v, "\r\n") {
		return "", fmt.Errorf("%s has a line break in it, which is not allowed in a header", what)
	}
	return v, nil
}

func nrAddrs(what, v string) ([]*mail.Address, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	if _, err := nrHeaderSafe(what, v); err != nil {
		return nil, err
	}
	l, err := mail.ParseAddressList(v)
	if err != nil {
		return nil, fmt.Errorf("%s %q is not an address: %v", what, v, err)
	}
	return l, nil
}

func msgString(m *engine.Msg, key string) string {
	if v, ok := m.Data[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

type nrAttachment struct {
	name, ctype string
	data        []byte
}

func emailBytes(v any) ([]byte, bool) {
	switch t := v.(type) {
	case []byte:
		return t, true
	case engine.ImmutableBytes:
		return []byte(t), true
	case string:
		return []byte(t), true
	}
	return nil, false
}

func (n *nrEmailNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	user, pass := n.credentials()
	from := msgString(m, "from")
	if from == "" {
		from = user
	}
	to := n.to
	if to == "" {
		to = msgString(m, "to")
	}
	if to == "" {
		return fmt.Errorf("there is no recipient: set To in the node or msg.to")
	}
	toList, err := nrAddrs("To", to)
	if err != nil {
		return err
	}
	ccList, err := nrAddrs("msg.cc", msgString(m, "cc"))
	if err != nil {
		return err
	}
	bccList, err := nrAddrs("msg.bcc", msgString(m, "bcc"))
	if err != nil {
		return err
	}
	fromList, err := nrAddrs("from", from)
	if err != nil {
		return err
	}
	if len(fromList) != 1 {
		return fmt.Errorf("there is no sender: the node's user name has to be an address, or set msg.from")
	}
	subject := msgString(m, "topic")
	if subject == "" {
		subject = msgString(m, "title")
	}
	if subject == "" {
		subject = "Message from Flow"
	}
	if _, err := nrHeaderSafe("the subject (msg.topic)", subject); err != nil {
		return err
	}

	var body string
	var attachments []nrAttachment
	switch t := m.Payload().(type) {
	case nil:
	case string:
		body = t
	case []byte, engine.ImmutableBytes:
		b, _ := emailBytes(t)
		attachments = append(attachments, nrAttachment{name: "attachment", ctype: "application/octet-stream", data: b})
	default:
		body = fmt.Sprint(t)
	}
	if l, ok := m.Data["attachments"].([]any); ok {
		for i, it := range l {
			a, ok := it.(map[string]any)
			if !ok {
				return fmt.Errorf("msg.attachments[%d] has to be an object like {filename, content}", i)
			}
			data, ok := emailBytes(a["content"])
			if !ok {
				return fmt.Errorf("msg.attachments[%d].content has to be text or a Buffer", i)
			}
			name, _ := a["filename"].(string)
			if name == "" {
				name = fmt.Sprintf("attachment%d", i+1)
			}
			ct, _ := a["contentType"].(string)
			if ct == "" {
				ct = mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
			}
			if ct == "" || strings.ContainsAny(ct, "\r\n") {
				ct = "application/octet-stream"
			}
			attachments = append(attachments, nrAttachment{name: name, ctype: ct, data: data})
		}
	}
	html := msgString(m, "html")

	var rcpt []string
	for _, l := range [][]*mail.Address{toList, ccList, bccList} {
		for _, a := range l {
			rcpt = append(rcpt, a.Address)
		}
	}
	raw, err := nrBuildMessage(fromList[0], toList, ccList, subject, body, html, attachments)
	if err != nil {
		return err
	}

	if si := node.StandInOf(n.svc); si != nil {
		_, err := si.Call("email", map[string]any{"from": fromList[0].Address, "to": stringsToAny(rcpt),
			"subject": subject, "body": body, "attachments": float64(len(attachments))})
		if err != nil {
			return fmt.Errorf("sending: %w", err)
		}
		out.Status(node.Status{Fill: "green", Shape: "dot", Text: "sent"})
		return nil
	}

	var token string
	if n.authtype == "XOAUTH2" {
		v, ok, _ := m.Get(n.tokenPath)
		token, _ = v.(string)
		if !ok || token == "" {
			return fmt.Errorf("XOAUTH2 needs an access token in msg.%s, and there is none", n.tokenPath)
		}
	}
	if err := n.send(ctx, fromList[0].Address, rcpt, raw, user, pass, token); err != nil {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: truncate(err.Error(), 32)})
		return fmt.Errorf("sending to %s: %w", n.server, err)
	}
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: "sent"})
	return nil
}

func stringsToAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// xoauth2 is the SASL XOAUTH2 mechanism, the one Microsoft 365 and Google ask for.
type xoauth2 struct{ user, token string }

func (a xoauth2) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "XOAUTH2", []byte("user=" + a.user + "\x01auth=Bearer " + a.token + "\x01\x01"), nil
}
func (a xoauth2) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		// The server sent an error document. An empty reply ends the exchange
		// and the server then sends the real failure.
		return []byte{}, nil
	}
	return nil, nil
}

// loginAuth is AUTH LOGIN, which some servers offer instead of PLAIN.
type loginAuth struct{ user, pass string }

func (a loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }
func (a loginAuth) Next(from []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(from))) {
	case "username:":
		return []byte(a.user), nil
	case "password:":
		return []byte(a.pass), nil
	}
	return nil, fmt.Errorf("the server asked for %q", from)
}

func (n *nrEmailNode) send(ctx context.Context, from string, rcpt []string, raw []byte, user, pass, token string) error {
	addr := net.JoinHostPort(n.server, strconv.Itoa(n.port))
	d := net.Dialer{Timeout: n.timeout}
	cfg := &tls.Config{ServerName: n.server, InsecureSkipVerify: !n.verify, MinVersion: tls.VersionTLS12} //nolint:gosec // tls=false is the flow author's switch, as in Node-RED
	var conn net.Conn
	var err error
	if n.secure {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, cfg)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("cannot connect to %s: %w. Check the server, the port and that this host can reach it", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(n.timeout))
	c, err := smtp.NewClient(conn, n.server)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("%s did not answer as an SMTP server: %w. Port 465 wants the secure connection switch on, 587 wants it off", addr, err)
	}
	defer c.Close()
	if !n.secure {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(cfg); err != nil {
				return fmt.Errorf("starting TLS with %s: %w", n.server, err)
			}
		}
	}
	switch {
	case n.authtype == "XOAUTH2":
		if err := c.Auth(xoauth2{user: user, token: token}); err != nil {
			return fmt.Errorf("signing in with the access token: %w", err)
		}
	case n.authtype == "BASIC" && (user != "" || pass != ""):
		if ok, mechs := c.Extension("AUTH"); ok {
			var a smtp.Auth = smtp.PlainAuth("", user, pass, n.server)
			if !strings.Contains(strings.ToUpper(mechs), "PLAIN") && strings.Contains(strings.ToUpper(mechs), "LOGIN") {
				a = loginAuth{user, pass}
			}
			if err := c.Auth(a); err != nil {
				return fmt.Errorf("signing in as %s: %w", user, err)
			}
		} else {
			return fmt.Errorf("%s does not offer sign in, and a user name and password are set. Set authentication to None, or check the port", n.server)
		}
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("the server refused the sender %s: %w", from, err)
	}
	for _, r := range rcpt {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("the server refused the recipient %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("the server did not accept the message: %w", err)
	}
	return c.Quit()
}

func nrBuildMessage(from *mail.Address, to, cc []*mail.Address, subject, body, html string, atts []nrAttachment) ([]byte, error) {
	var b bytes.Buffer
	join := func(l []*mail.Address) string {
		parts := make([]string, len(l))
		for i, a := range l {
			parts[i] = a.String()
		}
		return strings.Join(parts, ", ")
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	host := "flow.local"
	if i := strings.LastIndex(from.Address, "@"); i >= 0 {
		host = from.Address[i+1:]
	}
	fmt.Fprintf(&b, "From: %s\r\n", from.String())
	fmt.Fprintf(&b, "To: %s\r\n", join(to))
	if len(cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", join(cc))
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id), host)
	b.WriteString("MIME-Version: 1.0\r\n")

	textPart := func(w *multipart.Writer, ctype, content string) error {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", ctype+"; charset=utf-8")
		h.Set("Content-Transfer-Encoding", "base64")
		pw, err := w.CreatePart(h)
		if err != nil {
			return err
		}
		return writeBase64(pw, []byte(content))
	}
	if len(atts) == 0 && html == "" {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n")
		if err := writeBase64(&b, []byte(body)); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	mw := multipart.NewWriter(&b)
	kind := "mixed"
	if len(atts) == 0 {
		kind = "alternative"
	}
	fmt.Fprintf(&b, "Content-Type: multipart/%s; boundary=%s\r\n\r\n", kind, mw.Boundary())
	if kind == "alternative" {
		if err := textPart(mw, "text/plain", body); err != nil {
			return nil, err
		}
		if err := textPart(mw, "text/html", html); err != nil {
			return nil, err
		}
	} else {
		if html != "" {
			if err := textPart(mw, "text/html", html); err != nil {
				return nil, err
			}
		} else if body != "" {
			if err := textPart(mw, "text/plain", body); err != nil {
				return nil, err
			}
		}
		for _, a := range atts {
			h := textproto.MIMEHeader{}
			h.Set("Content-Type", a.ctype)
			h.Set("Content-Transfer-Encoding", "base64")
			h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.name}))
			pw, err := mw.CreatePart(h)
			if err != nil {
				return nil, err
			}
			if err := writeBase64(pw, a.data); err != nil {
				return nil, err
			}
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// writeBase64 writes data as base64 in lines of 76, which is what a mail
// server and a mail client both expect, and which keeps a line under the limit
// whatever the body holds.
func writeBase64(w interface{ Write([]byte) (int, error) }, data []byte) error {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		if _, err := w.Write([]byte(enc[:76] + "\r\n")); err != nil {
			return err
		}
		enc = enc[76:]
	}
	_, err := w.Write([]byte(enc + "\r\n"))
	return err
}
