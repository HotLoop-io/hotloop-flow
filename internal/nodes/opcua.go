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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
)

// OPC UA.
//
// Built on github.com/gopcua/opcua (MIT). The library speaks the protocol. What
// is here is what an operator meets around it: the connection that survives a
// device going away, the security settings in words a plant understands, and
// the error that says which setting to change.

func init() {
	registerOPCUAConfig()
	registerOPCUARead()
	registerOPCUAWrite()
	registerOPCUASubscribe()
}

// opcuaTarget is how the action nodes reach their server config node.
type opcuaTarget interface {
	// client returns a connected client, connecting first if there isn't one.
	client(ctx context.Context) (*opcua.Client, error)
	// lost throws a client away after a failure that says it can't be trusted,
	// so the next request starts from a new session.
	lost(cl *opcua.Client)
	// requestTimeout is how long one request waits for the server.
	requestTimeout() time.Duration
	// describe is the endpoint, for messages.
	describe() string
}

type opcuaServer struct {
	endpoint      string
	mode          ua.MessageSecurityMode
	policy        string // short name, "None" when the mode is None
	user          string
	password      string
	useUser       bool
	thumbprint    []byte
	timeout       time.Duration
	tlsID         string
	svc           node.Services
	appURI        string
	clientCertDER []byte
	clientKey     *rsa.PrivateKey

	mu sync.Mutex
	cl *opcua.Client
}

// opcuaPolicies are the policies this node offers: the two that open a channel
// against a real server in this repository's tests. gopcua also names
// Aes256_Sha256_RsaPss, Basic256 and Basic128Rsa15. The first fails against
// Microsoft's OPC PLC with BadSecurityChecksFailed on the client's signature,
// and the other two are deprecated by the OPC Foundation and the test server
// doesn't offer them, so a setting nobody could check isn't in the list.
var opcuaPolicies = []string{"Basic256Sha256", "Aes128_Sha256_RsaOaep"}

func registerOPCUAConfig() {
	node.MustRegister(node.Descriptor{
		Type:      "hotloop-flow-opcua",
		Category:  node.CategoryConfig,
		Color:     colorNetwork,
		Icon:      "network",
		IsConfig:  true,
		LabelProp: "name",
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own OPC UA connection. It connects on first use, notices when the " +
				"server goes away, and opens a new session on the next request or, for a " +
				"subscription, by itself with a growing wait between tries.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "endpoint", Kind: node.PropString, Label: "Endpoint", Required: true,
				Placeholder: "opc.tcp://10.20.1.20:4840"},
			{Name: "securityMode", Kind: node.PropSelect, Label: "Security mode", Default: "None",
				Options: []node.Option{
					{Value: "None", Label: "None, nothing signed or encrypted"},
					{Value: "Sign", Label: "Sign, tamper-proof but readable"},
					{Value: "SignAndEncrypt", Label: "Sign and encrypt"},
				}},
			{Name: "securityPolicy", Kind: node.PropSelect, Label: "Security policy", Default: "Basic256Sha256",
				Options: []node.Option{
					{Value: "Basic256Sha256", Label: "Basic256Sha256"},
					{Value: "Aes128_Sha256_RsaOaep", Label: "Aes128_Sha256_RsaOaep"},
				},
				Help: "Ignored when the security mode is None. Both have to be an endpoint the server offers."},
			{Name: "authType", Kind: node.PropSelect, Label: "Log in as", Default: "anonymous",
				Options: []node.Option{
					{Value: "anonymous", Label: "Anonymous"},
					{Value: "username", Label: "A user with a password"},
				}},
			{Name: "username", Kind: node.PropString, Label: "Username"},
			{Name: "password", Kind: node.PropCredential, Label: "Password"},
			{Name: "tls", Kind: node.PropConfigRef, Label: "Client certificate", ConfigType: "tls-config",
				Help: "A TLS config with an RSA certificate and key, for Sign and Sign and encrypt. Put the " +
					"application URI the server should expect in the certificate's subject alternative name " +
					"as a URI. Leave it empty and a certificate is made fresh each time the flow starts, " +
					"which the server then has to be told to trust every time."},
			{Name: "serverThumbprint", Kind: node.PropString, Label: "Server certificate thumbprint",
				Help: "Optional. The SHA-1 or SHA-256 thumbprint of the server's certificate, in hex. When it is " +
					"set, a server presenting any other certificate is refused. Without it the node accepts " +
					"whatever certificate the server shows, which encrypts the conversation and proves " +
					"nothing about who is on the other end."},
			{Name: "timeout", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 10,
				Help: "How long a request waits for the server before the node gives up on the session."},
		},
		Help: "One OPC UA server. Read, write and subscribe nodes pointing at it share one session.",
	}, newOPCUAServer)
}

func newOPCUAServer(def *node.Definition) (node.Node, error) {
	p := def.Node
	endpoint := strings.TrimSpace(p.PropString("endpoint", ""))
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint is required, for example opc.tcp://10.20.1.20:4840")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "opc.tcp" || u.Hostname() == "" {
		return nil, fmt.Errorf("endpoint %q has to look like opc.tcp://host:4840. The scheme is opc.tcp", endpoint)
	}
	s := &opcuaServer{endpoint: endpoint, svc: def.Services, tlsID: p.PropString("tls", "")}

	switch p.PropString("securityMode", "None") {
	case "None", "":
		s.mode, s.policy = ua.MessageSecurityModeNone, "None"
	case "Sign":
		s.mode = ua.MessageSecurityModeSign
	case "SignAndEncrypt":
		s.mode = ua.MessageSecurityModeSignAndEncrypt
	default:
		return nil, fmt.Errorf("security mode %q is not None, Sign or SignAndEncrypt", p.PropString("securityMode", ""))
	}
	if s.mode != ua.MessageSecurityModeNone {
		s.policy = p.PropString("securityPolicy", "Basic256Sha256")
		if s.policy == "" {
			s.policy = "Basic256Sha256"
		}
		ok := false
		for _, v := range opcuaPolicies {
			ok = ok || v == s.policy
		}
		if !ok {
			return nil, fmt.Errorf("security policy %q is not one of %s", s.policy, strings.Join(opcuaPolicies, ", "))
		}
	}

	switch p.PropString("authType", "anonymous") {
	case "anonymous", "":
	case "username":
		s.useUser = true
		s.user = p.PropString("username", "")
		if s.user == "" {
			return nil, fmt.Errorf("a username is required when logging in as a user")
		}
		s.password, _ = def.Services.Credential("password")
		if s.password == "" {
			return nil, fmt.Errorf("a password is required when logging in as a user")
		}
	default:
		return nil, fmt.Errorf("log in as %q is not anonymous or username", p.PropString("authType", ""))
	}

	if tp := strings.TrimSpace(p.PropString("serverThumbprint", "")); tp != "" {
		tp = strings.NewReplacer(":", "", " ", "").Replace(tp)
		b, err := hex.DecodeString(tp)
		if err != nil || (len(b) != sha1.Size && len(b) != sha256.Size) {
			return nil, fmt.Errorf("server certificate thumbprint has to be 40 hex digits (SHA-1) or 64 (SHA-256), with or without colons")
		}
		s.thumbprint = b
	}
	secs := p.PropFloat("timeout", 10)
	if secs <= 0 {
		return nil, fmt.Errorf("timeout has to be more than zero seconds")
	}
	s.timeout = time.Duration(secs * float64(time.Second))

	if s.mode != ua.MessageSecurityModeNone && node.StandInOf(def.Services) == nil {
		// Nothing is dialled under a flow test, so there's no call for a
		// certificate there either.
		if err := s.loadClientCert(def.Services, u.Hostname()); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// loadClientCert takes the application certificate from the TLS config the
// node points at, or makes one.
func (s *opcuaServer) loadClientCert(svc node.Services, host string) error {
	if s.tlsID != "" {
		cfg, ok := svc.ConfigNode(s.tlsID)
		if !ok {
			return fmt.Errorf("the TLS config %s for the client certificate is not running", s.tlsID)
		}
		tc, ok := cfg.(*tlsConfigNode)
		if !ok || len(tc.certs) == 0 {
			return fmt.Errorf("the TLS config %s has no certificate and key. OPC UA with %s needs the client to have one",
				s.tlsID, s.mode)
		}
		c := tc.certs[0]
		key, ok := c.PrivateKey.(*rsa.PrivateKey)
		if !ok {
			return fmt.Errorf("the client certificate in TLS config %s isn't an RSA key. OPC UA policy %s signs with RSA",
				s.tlsID, s.policy)
		}
		x, err := x509.ParseCertificate(c.Certificate[0])
		if err != nil {
			return fmt.Errorf("the client certificate in TLS config %s doesn't parse: %w", s.tlsID, err)
		}
		if len(x.URIs) == 0 {
			return fmt.Errorf("the client certificate in TLS config %s has no application URI. OPC UA servers match the "+
				"client's application URI against a URI in the certificate's subject alternative name, so the "+
				"certificate needs one, such as urn:hotloop-flow:client", s.tlsID)
		}
		s.clientCertDER, s.clientKey, s.appURI = c.Certificate[0], key, x.URIs[0].String()
		return nil
	}

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "hotloop-flow"
	}
	s.appURI = "urn:" + hostname + ":hotloop-flow"
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("making a client certificate: %w", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	uri, _ := url.Parse(s.appURI)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "HotLoop Flow", Organization: []string{"HotLoop Flow"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment |
			x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{hostname},
		URIs:        []*url.URL{uri},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("making a client certificate: %w", err)
	}
	s.clientCertDER, s.clientKey = der, key
	s.svc.Log(node.LogWarn, "OPC UA %s: no client certificate was chosen, so one was made that lasts a year and "+
		"is new each time the flow starts. Pick a TLS config with a certificate to keep the server's trust", s.endpoint)
	return nil
}

func (s *opcuaServer) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (s *opcuaServer) describe() string { return s.endpoint }

func (s *opcuaServer) requestTimeout() time.Duration { return s.timeout }

func (s *opcuaServer) Close(ctx context.Context, _ bool) error {
	s.mu.Lock()
	cl := s.cl
	s.cl = nil
	s.mu.Unlock()
	if cl != nil {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_ = cl.Close(cctx)
	}
	return nil
}

func (s *opcuaServer) lost(cl *opcua.Client) {
	s.mu.Lock()
	if s.cl == cl {
		s.cl = nil
	}
	s.mu.Unlock()
	if cl != nil {
		// Closing a dead session waits on a server that isn't answering, so it
		// happens off to the side and is given a short leash.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = cl.Close(ctx)
		}()
	}
}

func (s *opcuaServer) client(ctx context.Context) (*opcua.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cl != nil {
		if s.cl.State() == opcua.Connected {
			return s.cl, nil
		}
		old := s.cl
		s.cl = nil
		go func() {
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = old.Close(cctx)
		}()
	}

	cctx, cancel := context.WithTimeout(ctx, s.timeout+5*time.Second)
	defer cancel()

	opts := []opcua.Option{
		opcua.AutoReconnect(false),
		opcua.RequestTimeout(s.timeout),
		opcua.DialTimeout(minDuration(s.timeout, 10*time.Second)),
		opcua.SessionTimeout(time.Minute),
	}
	if s.clientCertDER != nil {
		opts = append(opts, opcua.Certificate(s.clientCertDER), opcua.PrivateKey(s.clientKey))
	}
	if s.appURI != "" {
		opts = append(opts, opcua.ApplicationURI(s.appURI))
	}

	// Discovery goes over an unsecured channel, as the specification has it,
	// and says which endpoints the server offers.
	eps, err := opcua.GetEndpoints(cctx, s.endpoint)
	if err != nil {
		return nil, s.explain("asking the server which endpoints it offers", err)
	}
	ep, err := selectOPCUAEndpoint(eps, s.policy, s.mode)
	if err != nil {
		return nil, err
	}
	ep.EndpointURL = s.endpoint

	if len(s.thumbprint) > 0 {
		if err := checkOPCUAThumbprint(ep.ServerCertificate, s.thumbprint); err != nil {
			return nil, err
		}
	}

	tokenType := ua.UserTokenTypeAnonymous
	if s.useUser {
		tokenType = ua.UserTokenTypeUserName
	}
	if !endpointOffers(ep, tokenType) {
		return nil, fmt.Errorf("%s doesn't take %s on the %s endpoint. It offers %s. Change \"Log in as\", or the "+
			"security settings, to match", s.endpoint, tokenName(tokenType), opcuaEndpointName(ep), offeredTokens(ep))
	}
	if s.useUser {
		opts = append(opts, opcua.AuthUsername(s.user, s.password))
	} else {
		opts = append(opts, opcua.AuthAnonymous())
	}
	opts = append(opts, opcua.SecurityFromEndpoint(ep, tokenType))

	cl, err := opcua.NewClient(s.endpoint, opts...)
	if err != nil {
		return nil, err
	}
	if err := cl.Connect(cctx); err != nil {
		_ = cl.Close(context.Background())
		return nil, s.explain("connecting", err)
	}
	s.cl = cl
	return cl, nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func tokenName(t ua.UserTokenType) string {
	switch t {
	case ua.UserTokenTypeAnonymous:
		return "anonymous logins"
	case ua.UserTokenTypeUserName:
		return "a username and password"
	}
	return t.String()
}

func endpointOffers(ep *ua.EndpointDescription, t ua.UserTokenType) bool {
	for _, tok := range ep.UserIdentityTokens {
		if tok.TokenType == t {
			return true
		}
	}
	return false
}

func offeredTokens(ep *ua.EndpointDescription) string {
	var out []string
	for _, tok := range ep.UserIdentityTokens {
		out = append(out, tokenName(tok.TokenType))
	}
	if len(out) == 0 {
		return "no login methods"
	}
	return strings.Join(out, " and ")
}

func opcuaEndpointName(ep *ua.EndpointDescription) string {
	return strings.TrimPrefix(ep.SecurityPolicyURI, ua.SecurityPolicyURIPrefix) + "/" + ep.SecurityMode.String()
}

// selectOPCUAEndpoint finds the endpoint matching the mode and policy, and when
// there isn't one lists what the server does offer.
func selectOPCUAEndpoint(eps []*ua.EndpointDescription, policy string, mode ua.MessageSecurityMode) (*ua.EndpointDescription, error) {
	want := ua.FormatSecurityPolicyURI(policy)
	var offered []string
	for _, ep := range eps {
		if ep.SecurityPolicyURI == want && ep.SecurityMode == mode {
			return ep, nil
		}
		// Only what this node can use is worth listing. Telling an operator to
		// pick a policy the dropdown doesn't have sends them nowhere.
		short := strings.TrimPrefix(ep.SecurityPolicyURI, ua.SecurityPolicyURIPrefix)
		usable := short == "None"
		for _, p := range opcuaPolicies {
			usable = usable || p == short
		}
		if usable {
			offered = append(offered, opcuaEndpointName(ep))
		}
	}
	if len(offered) == 0 {
		return nil, fmt.Errorf("the server has no endpoint with security policy %s and mode %s, and none that this node "+
			"can use. It supports %s", policy, mode, strings.Join(opcuaPolicies, " and "))
	}
	return nil, fmt.Errorf("the server has no endpoint with security policy %s and mode %s. Of the ones this node can "+
		"use, it offers %s. Change the security mode and policy to one of those", policy, mode, strings.Join(offered, ", "))
}

func checkOPCUAThumbprint(der, want []byte) error {
	var got []byte
	switch len(want) {
	case sha1.Size:
		sum := sha1.Sum(der)
		got = sum[:]
	default:
		sum := sha256.Sum256(der)
		got = sum[:]
	}
	if len(der) == 0 || hex.EncodeToString(got) != hex.EncodeToString(want) {
		return fmt.Errorf("the server's certificate (thumbprint %s) isn't the one this config is pinned to (%s). "+
			"Either the server was given a new certificate, or something else is answering at that address. "+
			"If the new certificate is expected, update the thumbprint",
			strings.ToUpper(hex.EncodeToString(got)), strings.ToUpper(hex.EncodeToString(want)))
	}
	return nil
}

// explain turns the library's errors into what to change.
func (s *opcuaServer) explain(what string, err error) error {
	var sc ua.StatusCode
	if what == "connecting" && errors.As(err, &sc) && sc == ua.StatusBadUserAccessDenied {
		// Activating a session is where a wrong password shows up, and the
		// server says it as access denied.
		return fmt.Errorf("connecting: %s refused the login (BadUserAccessDenied, 0x%X). Check the username and "+
			"password, and that this user is enabled on the server", s.endpoint, uint32(sc))
	}
	return explainOPCUA(s.endpoint, what, err)
}

func explainOPCUA(endpoint, what string, err error) error {
	if err == nil {
		return nil
	}
	var sc ua.StatusCode
	if errors.As(err, &sc) {
		if advice := opcuaStatusAdvice(sc); advice != "" {
			return fmt.Errorf("%s: %s (%s, 0x%X). %s", what, endpoint, opcuaStatusName(sc), uint32(sc), advice)
		}
		return fmt.Errorf("%s: %s answered %s", what, endpoint, err)
	}
	var opErr *net.OpError
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s: no answer from %s in time. The server is silent. Check that it is running and "+
			"reachable and that nothing drops port %s. The session was dropped, and the next request opens a new one",
			what, endpoint, endpointPort(endpoint))
	case errors.As(err, &dnsErr):
		return fmt.Errorf("%s: couldn't find a host called %q. Check the spelling in the endpoint, that this host "+
			"can reach its DNS, or use the server's IP address", what, dnsErr.Name)
	case errors.As(err, &opErr):
		if opErr.Timeout() {
			return fmt.Errorf("%s: couldn't reach %s, the connection attempt timed out. Check the address, the route "+
				"to it and any firewall", what, endpoint)
		}
		if strings.Contains(strings.ToLower(err.Error()), "refused") {
			return fmt.Errorf("%s: %s refused the connection. The host is up and nothing is listening on port %s. "+
				"Check the port, and that the OPC UA server is running", what, endpoint, endpointPort(endpoint))
		}
		return fmt.Errorf("%s: lost the connection to %s (%v). The next request reconnects", what, endpoint, opErr.Err)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("%s: %s closed the connection. Servers do that when they restart, when they reject the "+
			"client's certificate, or when they run out of sessions. The next request reconnects", what, endpoint)
	}
	return fmt.Errorf("%s: %s: %w", what, endpoint, err)
}

func endpointPort(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Port() != "" {
		return u.Port()
	}
	return "4840"
}

func opcuaStatusName(sc ua.StatusCode) string {
	if d, ok := ua.StatusCodes[sc]; ok {
		return strings.TrimPrefix(d.Name, "Status")
	}
	return "unknown status"
}

// opcuaStatusAdvice says what to do about the status codes that come up in
// practice. The ones it doesn't know go through with the server's own text.
func opcuaStatusAdvice(sc ua.StatusCode) string {
	switch sc {
	case ua.StatusBadNodeIDUnknown, ua.StatusBadNodeIDInvalid:
		return "The server has no node with that id. Check the namespace index and the identifier, and that " +
			"the namespace index hasn't changed since the id was copied. Browse the server with a client " +
			"like UaExpert to see the real ids"
	case ua.StatusBadAttributeIDInvalid:
		return "The node doesn't have that attribute. A Value can only be read from a variable node"
	case ua.StatusBadUserAccessDenied:
		return "This login isn't allowed to do that to the node. Check what the user's role may read and write on the server"
	case ua.StatusBadNotReadable:
		return "The node can't be read. It isn't readable by this user or at all"
	case ua.StatusBadNotWritable:
		return "The node can't be written. It is read-only, or not writable by this user. Check its access level on the server"
	case ua.StatusBadTypeMismatch:
		return "The value is the wrong data type for the node. Set the data type on the write node to the node's own, " +
			"or leave it on Same as the node"
	case ua.StatusBadOutOfRange:
		return "The server won't take that value. It is outside the range the node allows"
	case ua.StatusBadIdentityTokenRejected, ua.StatusBadIdentityTokenInvalid, ua.StatusBadUserSignatureInvalid:
		return "The server rejected the login. Check the username and password, and that this user is enabled on the server"
	case ua.StatusBadCertificateUntrusted:
		return "The server doesn't trust this client's certificate. Move it from the server's rejected list to its " +
			"trusted list (the PKI folder, or its admin screen), or pick a TLS config whose certificate it already trusts"
	case ua.StatusBadCertificateURIInvalid:
		return "The application URI in the client certificate doesn't match the one the client presents. Use a " +
			"certificate whose subject alternative name carries the URI"
	case ua.StatusBadCertificateHostNameInvalid:
		return "The client certificate doesn't name this host. Make one that does"
	case ua.StatusBadCertificateTimeInvalid:
		return "A certificate is outside its dates, on the client or on the server. Check both clocks and both certificates"
	case ua.StatusBadSecurityChecksFailed, ua.StatusBadSecurityModeRejected, ua.StatusBadSecurityPolicyRejected:
		return "The server refused the security settings. Check the mode and policy against the endpoints it offers, " +
			"and that it trusts this client's certificate"
	case ua.StatusBadTooManySessions:
		return "The server has no sessions left. Close other clients, or raise the server's session limit"
	case ua.StatusBadSessionIDInvalid, ua.StatusBadSessionClosed, ua.StatusBadSessionNotActivated,
		ua.StatusBadSecureChannelIDInvalid, ua.StatusBadSecureChannelClosed:
		return "The session ended on the server, typically a restart or a timeout. The next request opens a new one"
	case ua.StatusBadTimeout, ua.StatusBadRequestTimeout:
		return "The server didn't answer in time"
	case ua.StatusBadTooManyOperations:
		return "That is more nodes in one request than the server allows. Use fewer at a time"
	case ua.StatusBadWriteNotSupported:
		return "The server won't take a write that carries a timestamp or status. The node writes the value only, so this is the server being strict"
	case ua.StatusBadCommunicationError, ua.StatusBadConnectionClosed, ua.StatusBadServerNotConnected,
		ua.StatusBadServerHalted, ua.StatusBadShutdown:
		return "The connection to the server is gone. The next request reconnects"
	}
	return ""
}

// opcuaDropsSession reports whether an error says the session can't be used
// again. A bad status on one node is an answer, and the session is fine. A
// service fault that says the session or channel is gone, or no answer at all,
// is not.
func opcuaDropsSession(err error) bool {
	var sc ua.StatusCode
	if errors.As(err, &sc) {
		switch sc {
		case ua.StatusBadSessionIDInvalid, ua.StatusBadSessionClosed, ua.StatusBadSessionNotActivated,
			ua.StatusBadSecureChannelIDInvalid, ua.StatusBadSecureChannelClosed, ua.StatusBadTimeout,
			ua.StatusBadRequestTimeout, ua.StatusBadCommunicationError, ua.StatusBadConnectionClosed,
			ua.StatusBadServerNotConnected, ua.StatusBadServerHalted, ua.StatusBadShutdown:
			return true
		}
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Values
// ---------------------------------------------------------------------------

// opcuaValue turns what the library decoded into something a flow can carry.
func opcuaValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case bool, string:
		return t
	case int8:
		return float64(t)
	case uint8:
		return float64(t)
	case int16:
		return float64(t)
	case uint16:
		return float64(t)
	case int32:
		return float64(t)
	case uint32:
		return float64(t)
	case float32:
		return float64(t)
	case float64:
		return t
	case int64:
		return bigInt(strconv.FormatInt(t, 10), float64(t), t >= -(1<<53) && t <= 1<<53)
	case uint64:
		return bigInt(strconv.FormatUint(t, 10), float64(t), t <= 1<<53)
	case time.Time:
		if t.IsZero() {
			return nil
		}
		return t.UTC().Format(time.RFC3339Nano)
	case []byte:
		return append([]byte(nil), t...)
	case *ua.LocalizedText:
		if t == nil {
			return nil
		}
		return t.Text
	case *ua.QualifiedName:
		if t == nil {
			return nil
		}
		return t.Name
	case *ua.NodeID:
		if t == nil {
			return nil
		}
		return t.String()
	case ua.StatusCode:
		return float64(t)
	case *ua.ExtensionObject:
		if t == nil {
			return nil
		}
		return opcuaValue(t.Value)
	}
	// An array of any element type, a structure, anything else: through JSON if
	// it will go, which turns a struct into the object a flow expects.
	if b, err := json.Marshal(v); err == nil {
		var out any
		if json.Unmarshal(b, &out) == nil {
			return out
		}
	}
	return fmt.Sprint(v)
}

func opcuaVariantValue(v *ua.Variant) any {
	if v == nil {
		return nil
	}
	val := v.Value()
	if val == nil {
		return nil
	}
	// Arrays of the common scalars become lists of what the scalars become.
	switch t := val.(type) {
	case []bool:
		return sliceAny(t, func(x bool) any { return x })
	case []int8:
		return sliceAny(t, func(x int8) any { return float64(x) })
	case []int16:
		return sliceAny(t, func(x int16) any { return float64(x) })
	case []uint16:
		return sliceAny(t, func(x uint16) any { return float64(x) })
	case []int32:
		return sliceAny(t, func(x int32) any { return float64(x) })
	case []uint32:
		return sliceAny(t, func(x uint32) any { return float64(x) })
	case []int64:
		return sliceAny(t, func(x int64) any { return opcuaValue(x) })
	case []uint64:
		return sliceAny(t, func(x uint64) any { return opcuaValue(x) })
	case []float32:
		return sliceAny(t, func(x float32) any { return float64(x) })
	case []float64:
		return sliceAny(t, func(x float64) any { return x })
	case []string:
		return sliceAny(t, func(x string) any { return x })
	case []time.Time:
		return sliceAny(t, func(x time.Time) any { return opcuaValue(x) })
	}
	return opcuaValue(val)
}

func sliceAny[T any](in []T, f func(T) any) []any {
	out := make([]any, len(in))
	for i, x := range in {
		out[i] = f(x)
	}
	return out
}

// opcuaTypes are the built-in types a write can name. The numbers are the
// built-in type ids from part 6 of the specification, which are also the
// ns=0 node ids of the types, so reading a node's DataType attribute and
// looking up its integer says which one it wants.
var opcuaTypes = map[string]uint32{
	"Boolean": 1, "SByte": 2, "Byte": 3, "Int16": 4, "UInt16": 5, "Int32": 6, "UInt32": 7,
	"Int64": 8, "UInt64": 9, "Float": 10, "Double": 11, "String": 12, "DateTime": 13, "ByteString": 15,
}

func opcuaTypeName(id uint32) string {
	for k, v := range opcuaTypes {
		if v == id {
			return k
		}
	}
	return ""
}

func opcuaTypeOptions() []node.Option {
	opts := []node.Option{{Value: "auto", Label: "Same as the node"}}
	for _, n := range []string{"Boolean", "SByte", "Byte", "Int16", "UInt16", "Int32", "UInt32", "Int64", "UInt64",
		"Float", "Double", "String", "DateTime", "ByteString"} {
		opts = append(opts, node.Option{Value: n, Label: n})
	}
	return opts
}

// opcuaConvert turns a flow's value into the Go type the library encodes as the
// named OPC UA type.
func opcuaConvert(v any, typ string) (any, error) {
	intIn := func(lo, hi float64) (float64, error) {
		f, ok := asFloat(v)
		if !ok {
			return 0, fmt.Errorf("%v (%s) isn't a number, and %s holds a number", v, describeType(v), typ)
		}
		if f != math.Trunc(f) {
			return 0, fmt.Errorf("%v isn't a whole number, and %s holds only whole numbers", f, typ)
		}
		if f < lo || f > hi {
			return 0, fmt.Errorf("%v is outside what %s holds (%v to %v)", f, typ, lo, hi)
		}
		return f, nil
	}
	switch typ {
	case "Boolean":
		return modbusBool(v)
	case "SByte":
		f, err := intIn(math.MinInt8, math.MaxInt8)
		return int8(f), err
	case "Byte":
		f, err := intIn(0, math.MaxUint8)
		return uint8(f), err
	case "Int16":
		f, err := intIn(math.MinInt16, math.MaxInt16)
		return int16(f), err
	case "UInt16":
		f, err := intIn(0, math.MaxUint16)
		return uint16(f), err
	case "Int32":
		f, err := intIn(math.MinInt32, math.MaxInt32)
		return int32(f), err
	case "UInt32":
		f, err := intIn(0, math.MaxUint32)
		return uint32(f), err
	case "Int64":
		if s, ok := v.(string); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%q doesn't fit Int64", s)
			}
			return n, nil
		}
		f, err := intIn(-(1 << 53), 1<<53)
		if err != nil {
			return nil, fmt.Errorf("%w. Past 2^53 pass the exact number as a string, since a flow's numbers lose digits there", err)
		}
		return int64(f), nil
	case "UInt64":
		if s, ok := v.(string); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%q doesn't fit UInt64", s)
			}
			return n, nil
		}
		f, err := intIn(0, 1<<53)
		if err != nil {
			return nil, fmt.Errorf("%w. Past 2^53 pass the exact number as a string, since a flow's numbers lose digits there", err)
		}
		return uint64(f), nil
	case "Float":
		f, ok := asFloat(v)
		if !ok {
			return nil, fmt.Errorf("%v (%s) isn't a number, and Float holds a number", v, describeType(v))
		}
		g := float32(f)
		if math.IsInf(float64(g), 0) && !math.IsInf(f, 0) {
			return nil, fmt.Errorf("%v is too big for a Float", f)
		}
		return g, nil
	case "Double":
		f, ok := asFloat(v)
		if !ok {
			return nil, fmt.Errorf("%v (%s) isn't a number, and Double holds a number", v, describeType(v))
		}
		return f, nil
	case "String":
		switch t := v.(type) {
		case string:
			return t, nil
		case []byte:
			return string(t), nil
		case engine.ImmutableBytes:
			return string(t), nil
		}
		return stringify(v), nil
	case "DateTime":
		switch t := v.(type) {
		case string:
			tm, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(t))
			if err != nil {
				return nil, fmt.Errorf("%q isn't a date. Use the form 2026-10-18T14:30:00Z, or milliseconds since 1970", t)
			}
			return tm, nil
		default:
			f, ok := asFloat(v)
			if !ok {
				return nil, fmt.Errorf("%v (%s) isn't a date. Use the form 2026-10-18T14:30:00Z, or milliseconds since 1970", v, describeType(v))
			}
			return time.UnixMilli(int64(f)).UTC(), nil
		}
	case "ByteString":
		switch t := v.(type) {
		case []byte:
			return t, nil
		case engine.ImmutableBytes:
			return []byte(t), nil
		case string:
			return []byte(t), nil
		}
		return nil, fmt.Errorf("%v (%s) isn't bytes or a string", v, describeType(v))
	}
	return nil, fmt.Errorf("data type %q isn't one this node can write", typ)
}

// opcuaVariant builds the variant to write: one value, or an array when the
// flow's value is a list.
func opcuaVariant(v any, typ string) (*ua.Variant, error) {
	list, isList := v.([]any)
	if !isList {
		c, err := opcuaConvert(v, typ)
		if err != nil {
			return nil, err
		}
		return ua.NewVariant(c)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("the value is an empty list, so there is nothing to write")
	}
	first, err := opcuaConvert(list[0], typ)
	if err != nil {
		return nil, fmt.Errorf("value 0: %w", err)
	}
	// An array variant is a Go slice of the element type, so build the right one.
	switch first.(type) {
	case bool:
		return arrayVariant[bool](list, typ)
	case int8:
		return arrayVariant[int8](list, typ)
	case uint8:
		return arrayVariant[uint8](list, typ)
	case int16:
		return arrayVariant[int16](list, typ)
	case uint16:
		return arrayVariant[uint16](list, typ)
	case int32:
		return arrayVariant[int32](list, typ)
	case uint32:
		return arrayVariant[uint32](list, typ)
	case int64:
		return arrayVariant[int64](list, typ)
	case uint64:
		return arrayVariant[uint64](list, typ)
	case float32:
		return arrayVariant[float32](list, typ)
	case float64:
		return arrayVariant[float64](list, typ)
	case string:
		return arrayVariant[string](list, typ)
	case time.Time:
		return arrayVariant[time.Time](list, typ)
	}
	return nil, fmt.Errorf("a list of %s can't be written", typ)
}

func arrayVariant[T any](list []any, typ string) (*ua.Variant, error) {
	out := make([]T, len(list))
	for i, item := range list {
		c, err := opcuaConvert(item, typ)
		if err != nil {
			return nil, fmt.Errorf("value %d: %w", i, err)
		}
		out[i] = c.(T)
	}
	return ua.NewVariant(out)
}

// opcuaNodeIDs reads the node id or ids a message asks for.
func opcuaNodeIDs(tv TypedValue, ec EvalContext) ([]*ua.NodeID, []string, bool, error) {
	v, ok, err := tv.Eval(ec)
	if err != nil {
		return nil, nil, false, fmt.Errorf("node id: %w", err)
	}
	if !ok || v == nil {
		return nil, nil, false, fmt.Errorf("the node id resolved to nothing. Set it on the node, or put it on the message")
	}
	var raw []string
	multi := false
	switch t := v.(type) {
	case string:
		raw = []string{t}
	case []any:
		multi = true
		for i, x := range t {
			s, ok := x.(string)
			if !ok {
				return nil, nil, false, fmt.Errorf("node id %d is %v (%s), and has to be a string like ns=2;s=Tag1", i, x, describeType(x))
			}
			raw = append(raw, s)
		}
	default:
		return nil, nil, false, fmt.Errorf("the node id is %v (%s), and has to be a string like ns=2;s=Tag1, or a list of them", v, describeType(v))
	}
	if len(raw) == 0 {
		return nil, nil, false, fmt.Errorf("the list of node ids is empty")
	}
	ids := make([]*ua.NodeID, len(raw))
	for i, s := range raw {
		id, err := parseOPCUANodeID(s)
		if err != nil {
			return nil, nil, false, err
		}
		ids[i] = id
		raw[i] = strings.TrimSpace(s)
	}
	return ids, raw, multi, nil
}

var opcuaNodeIDForm = regexp.MustCompile(`^(ns=[0-9]+;)?[isgb]=.+$`)

func parseOPCUANodeID(s string) (*ua.NodeID, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("the node id is empty. It looks like ns=2;s=Tag1 or ns=3;i=1005")
	}
	// ParseNodeID accepts a bare word as a string identifier, which turns a
	// typo in the namespace prefix into a lookup that fails on the server with
	// a less helpful message.
	if !opcuaNodeIDForm.MatchString(s) {
		return nil, fmt.Errorf("%q isn't a node id. It looks like ns=2;s=Tag1 or ns=3;i=1005, "+
			"with s for a text identifier, i for a number, g for a GUID and b for bytes", s)
	}
	id, err := ua.ParseNodeID(s)
	if err != nil {
		return nil, fmt.Errorf("%q isn't a node id (%v). It looks like ns=2;s=Tag1 or ns=3;i=1005, "+
			"with s for a text identifier, i for a number, g for a GUID and b for bytes", s, err)
	}
	return id, nil
}

// opcuaInfo is what a node leaves on msg.opcua.
func opcuaInfo(endpoint, nodeID string, dv *ua.DataValue) map[string]any {
	m := map[string]any{"server": endpoint, "nodeId": nodeID}
	if dv != nil {
		m["status"] = opcuaStatusName(dv.Status)
		m["statusCode"] = float64(dv.Status)
		if !dv.SourceTimestamp.IsZero() {
			m["sourceTimestamp"] = dv.SourceTimestamp.UTC().Format(time.RFC3339Nano)
		}
		if !dv.ServerTimestamp.IsZero() {
			m["serverTimestamp"] = dv.ServerTimestamp.UTC().Format(time.RFC3339Nano)
		}
	}
	return m
}

func opcuaTarget0(svc node.Services, cfgID string, cached *opcuaTarget) (opcuaTarget, error) {
	if *cached != nil {
		return *cached, nil
	}
	cfg, ok := svc.ConfigNode(cfgID)
	if !ok {
		return nil, fmt.Errorf("the OPC UA server config %s is not running. Open this node and pick a server", cfgID)
	}
	t, ok := cfg.(opcuaTarget)
	if !ok {
		return nil, fmt.Errorf("config node %s is not an OPC UA server", cfgID)
	}
	*cached = t
	return t, nil
}

// ---------------------------------------------------------------------------
// opcua read
// ---------------------------------------------------------------------------

type opcuaReadNode struct {
	cfgID  string
	nodeID TypedValue
	svc    node.Services
	target opcuaTarget
}

func opcuaNodeIDTypes() []string {
	return []string{node.TypeStr, node.TypeMsg, node.TypeFlow, node.TypeGlobal, node.TypeEnv, node.TypeJSON, node.TypeJSONata}
}

func registerOPCUARead() {
	node.MustRegister(node.Descriptor{
		Type:         "opcua read",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "opcua read",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own OPC UA read. Not the same configuration as the community " +
				"node-red-contrib-opcua, which does not run here.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Server",
				ConfigType: "hotloop-flow-opcua", Required: true},
			{Name: "nodeId", Kind: node.PropTypedInput, Label: "Node id", Required: true,
				TypeProp: "nodeIdType", TypedInputTypes: opcuaNodeIDTypes(),
				Placeholder: "ns=2;s=Boiler.Temperature",
				Help:        "A list of ids reads them all in one request."},
		},
		Help: "Reads the value of a node, or of several, when a message arrives. msg.payload is the " +
			"value, a single value for one node id and a list for a list of ids. msg.opcua carries " +
			"the status and the source and server timestamps.\n\n" +
			"A node whose status is bad raises an error that says why. A 64 bit integer past 2^53 " +
			"comes out as a string of its exact digits.",
	}, newOPCUARead)
}

func newOPCUARead(def *node.Definition) (node.Node, error) {
	n := &opcuaReadNode{
		cfgID:  def.Node.PropString("server", ""),
		nodeID: ReadTypedValue(def.Node.Raw, "nodeId", "nodeIdType", node.TypeStr),
		svc:    def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected: pick the OPC UA server to read from")
	}
	if strings.TrimSpace(n.nodeID.Value) == "" {
		return nil, fmt.Errorf("a node id is required, for example ns=2;s=Boiler.Temperature")
	}
	if err := n.nodeID.Check(); err != nil {
		return nil, err
	}
	if n.nodeID.Type == node.TypeStr {
		if _, err := parseOPCUANodeID(n.nodeID.Value); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (n *opcuaReadNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

func (n *opcuaReadNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ids, raw, multi, err := opcuaNodeIDs(n.nodeID, EvalContext{Msg: m, Services: n.svc, Ctx: ctx})
	if err != nil {
		return n.fail(out, err)
	}
	what := "reading " + strings.Join(raw, ", ")
	if len(raw) > 3 {
		what = fmt.Sprintf("reading %d nodes", len(raw))
	}

	var values []any
	var infos []any
	endpoint := "stand-in"

	if si := node.StandInOf(n.svc); si != nil {
		reply, err := si.Call("opcua", map[string]any{"op": "read", "nodeIds": anyStrings(raw)})
		if err != nil {
			return n.fail(out, fmt.Errorf("%s: %w", what, err))
		}
		if l, err := replyList(reply, "values"); err != nil {
			return n.fail(out, err)
		} else if l != nil {
			values = l
		} else if v, ok := reply["value"]; ok {
			values = []any{v}
		} else {
			values = make([]any, len(raw))
		}
		if len(values) != len(raw) {
			return n.fail(out, fmt.Errorf("the scripted reply has %d values, and the read asked for %d nodes", len(values), len(raw)))
		}
		for _, id := range raw {
			infos = append(infos, opcuaInfo(endpoint, id, nil))
		}
	} else {
		tgt, err := opcuaTarget0(n.svc, n.cfgID, &n.target)
		if err != nil {
			return n.fail(out, err)
		}
		endpoint = tgt.describe()
		cl, err := tgt.client(ctx)
		if err != nil {
			return n.fail(out, err)
		}
		rctx, cancel := context.WithTimeout(ctx, tgt.requestTimeout())
		defer cancel()
		req := &ua.ReadRequest{MaxAge: 0, TimestampsToReturn: ua.TimestampsToReturnBoth}
		for _, id := range ids {
			req.NodesToRead = append(req.NodesToRead, &ua.ReadValueID{NodeID: id, AttributeID: ua.AttributeIDValue})
		}
		resp, err := cl.Read(rctx, req)
		if err != nil {
			if opcuaDropsSession(err) || rctx.Err() != nil {
				tgt.lost(cl)
			}
			if rctx.Err() != nil && ctx.Err() == nil {
				err = context.DeadlineExceeded
			}
			return n.fail(out, explainOPCUA(endpoint, what, err))
		}
		if len(resp.Results) != len(ids) {
			return n.fail(out, fmt.Errorf("%s: %s answered %d results to %d nodes", what, endpoint, len(resp.Results), len(ids)))
		}
		for i, dv := range resp.Results {
			if dv.Status != ua.StatusOK && dv.Status != ua.StatusGood {
				bad := explainOPCUA(endpoint, "reading "+raw[i], dv.Status)
				if (dv.Status & 0xC0000000) != 0x80000000 { // uncertain: the value is there, with a caveat
					values = append(values, opcuaVariantValue(dv.Value))
					infos = append(infos, opcuaInfo(endpoint, raw[i], dv))
					continue
				}
				return n.fail(out, bad)
			}
			values = append(values, opcuaVariantValue(dv.Value))
			infos = append(infos, opcuaInfo(endpoint, raw[i], dv))
		}
	}

	if multi {
		m.SetPayload(values)
		m.Data["opcua"] = infos
	} else {
		m.SetPayload(values[0])
		m.Data["opcua"] = infos[0]
	}
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate(statusValue(values), 32)})
	out.Send(0, m)
	return nil
}

func anyStrings(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// ---------------------------------------------------------------------------
// opcua write
// ---------------------------------------------------------------------------

type opcuaWriteNode struct {
	cfgID    string
	nodeID   TypedValue
	value    TypedValue
	dataType string
	svc      node.Services
	target   opcuaTarget
}

func registerOPCUAWrite() {
	node.MustRegister(node.Descriptor{
		Type:         "opcua write",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      1,
		Align:        "right",
		PaletteLabel: "opcua write",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own OPC UA write. Not the same configuration as the community " +
				"node-red-contrib-opcua, which does not run here.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Server",
				ConfigType: "hotloop-flow-opcua", Required: true},
			{Name: "nodeId", Kind: node.PropTypedInput, Label: "Node id", Required: true,
				TypeProp: "nodeIdType", TypedInputTypes: []string{node.TypeStr, node.TypeMsg, node.TypeFlow,
					node.TypeGlobal, node.TypeEnv, node.TypeJSONata},
				Placeholder: "ns=2;s=Boiler.Setpoint"},
			{Name: "value", Kind: node.PropTypedInput, Label: "Value", Default: "payload", Required: true,
				TypeProp: "valueType",
				TypedInputTypes: []string{node.TypeMsg, node.TypeFlow, node.TypeGlobal, node.TypeNum,
					node.TypeStr, node.TypeBool, node.TypeJSON, node.TypeEnv, node.TypeJSONata},
				Help: "A list writes an array node."},
			{Name: "dataType", Kind: node.PropSelect, Label: "Data type", Default: "auto",
				Options: opcuaTypeOptions(),
				Help: "OPC UA servers refuse a write of the wrong type. Same as the node reads the node's own type " +
					"first, which costs one extra request."},
		},
		Help: "Writes a value to a node when a message arrives, then passes the message on. This changes " +
			"the equipment. There is no confirmation step and no read back: the message that arrives at " +
			"the output means the server accepted the write.",
	}, newOPCUAWrite)
}

func newOPCUAWrite(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &opcuaWriteNode{
		cfgID:    p.PropString("server", ""),
		nodeID:   ReadTypedValue(p.Raw, "nodeId", "nodeIdType", node.TypeStr),
		value:    ReadTypedValue(p.Raw, "value", "valueType", node.TypeMsg),
		dataType: p.PropString("dataType", "auto"),
		svc:      def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected: pick the OPC UA server to write to")
	}
	if strings.TrimSpace(n.nodeID.Value) == "" {
		return nil, fmt.Errorf("a node id is required, for example ns=2;s=Boiler.Setpoint")
	}
	if n.value.Value == "" {
		n.value = TypedValue{Type: node.TypeMsg, Value: "payload"}
	}
	if n.dataType != "auto" {
		if _, ok := opcuaTypes[n.dataType]; !ok {
			return nil, fmt.Errorf("data type %q isn't auto or one of Boolean, SByte, Byte, Int16, UInt16, Int32, UInt32, Int64, UInt64, Float, Double, String, DateTime or ByteString", n.dataType)
		}
	}
	for _, tv := range []TypedValue{n.nodeID, n.value} {
		if err := tv.Check(); err != nil {
			return nil, err
		}
	}
	if n.nodeID.Type == node.TypeStr {
		if _, err := parseOPCUANodeID(n.nodeID.Value); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (n *opcuaWriteNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

func (n *opcuaWriteNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc, Ctx: ctx}
	ids, raw, multi, err := opcuaNodeIDs(n.nodeID, ec)
	if err != nil {
		return n.fail(out, err)
	}
	if multi {
		return n.fail(out, fmt.Errorf("a write goes to one node. The node id is a list of %d", len(ids)))
	}
	id, nid := ids[0], raw[0]
	value, ok, err := n.value.Eval(ec)
	if err != nil {
		return n.fail(out, fmt.Errorf("value: %w", err))
	}
	if !ok || value == nil {
		return n.fail(out, fmt.Errorf("value resolved to nothing, so there is nothing to write. "+
			"Check the Value field, which reads %s %q", n.value.Type, n.value.Value))
	}
	what := "writing " + nid
	endpoint := "stand-in"
	typ := n.dataType

	if si := node.StandInOf(n.svc); si != nil {
		if typ == "auto" {
			typ = ""
		} else if _, err := opcuaVariant(value, typ); err != nil {
			return n.fail(out, fmt.Errorf("%s: %w", what, err))
		}
		if _, err := si.Call("opcua", map[string]any{"op": "write", "nodeId": nid, "dataType": typ, "value": value}); err != nil {
			return n.fail(out, fmt.Errorf("%s: %w", what, err))
		}
	} else {
		tgt, err := opcuaTarget0(n.svc, n.cfgID, &n.target)
		if err != nil {
			return n.fail(out, err)
		}
		endpoint = tgt.describe()
		cl, err := tgt.client(ctx)
		if err != nil {
			return n.fail(out, err)
		}
		rctx, cancel := context.WithTimeout(ctx, tgt.requestTimeout())
		defer cancel()
		fail := func(err error) error {
			if opcuaDropsSession(err) || rctx.Err() != nil {
				tgt.lost(cl)
			}
			if rctx.Err() != nil && ctx.Err() == nil {
				err = context.DeadlineExceeded
			}
			return n.fail(out, explainOPCUA(endpoint, what, err))
		}

		if typ == "auto" {
			resp, err := cl.Read(rctx, &ua.ReadRequest{
				NodesToRead:        []*ua.ReadValueID{{NodeID: id, AttributeID: ua.AttributeIDDataType}},
				TimestampsToReturn: ua.TimestampsToReturnNeither,
			})
			if err != nil {
				return fail(err)
			}
			if len(resp.Results) != 1 {
				return n.fail(out, fmt.Errorf("%s: %s answered %d results to 1 node", what, endpoint, len(resp.Results)))
			}
			dv := resp.Results[0]
			if dv.Status != ua.StatusOK && dv.Status != ua.StatusGood {
				return n.fail(out, explainOPCUA(endpoint, what, dv.Status))
			}
			dt, _ := dv.Value.Value().(*ua.NodeID)
			if dt != nil && dt.Namespace() == 0 {
				typ = opcuaTypeName(dt.IntID())
			}
			if typ == "" || typ == "auto" {
				return n.fail(out, fmt.Errorf("%s: the node's data type is %v, which is not one of the plain types this node "+
					"knows. Pick the data type on the node", what, dt))
			}
		}

		variant, err := opcuaVariant(value, typ)
		if err != nil {
			return n.fail(out, fmt.Errorf("%s: %w", what, err))
		}
		resp, err := cl.Write(rctx, &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{{
			NodeID:      id,
			AttributeID: ua.AttributeIDValue,
			Value:       &ua.DataValue{EncodingMask: ua.DataValueValue, Value: variant},
		}}})
		if err != nil {
			return fail(err)
		}
		if len(resp.Results) != 1 {
			return n.fail(out, fmt.Errorf("%s: %s answered %d results to 1 node", what, endpoint, len(resp.Results)))
		}
		if resp.Results[0] != ua.StatusOK && resp.Results[0] != ua.StatusGood {
			return n.fail(out, explainOPCUA(endpoint, what, resp.Results[0]))
		}
	}

	info := opcuaInfo(endpoint, nid, nil)
	if typ != "" {
		info["dataType"] = typ
	}
	m.Data["opcua"] = info
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate("wrote "+fmt.Sprint(value), 32)})
	out.Send(0, m)
	return nil
}

// ---------------------------------------------------------------------------
// opcua subscribe
// ---------------------------------------------------------------------------

type opcuaSubscribeNode struct {
	id       string
	cfgID    string
	nodeIDs  []string
	parsed   []*ua.NodeID
	interval time.Duration
	svc      node.Services
	cancel   context.CancelFunc
	done     chan struct{}
}

func registerOPCUASubscribe() {
	node.MustRegister(node.Descriptor{
		Type:         "opcua subscribe",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "opcua subscribe",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own OPC UA subscription. Not the same configuration as the community " +
				"node-red-contrib-opcua, which does not run here.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Server",
				ConfigType: "hotloop-flow-opcua", Required: true},
			{Name: "nodes", Kind: node.PropList, Label: "Nodes", Fields: []node.Prop{
				{Name: "nodeId", Kind: node.PropString, Label: "Node id", Placeholder: "ns=2;s=Boiler.Temperature"},
			}},
			{Name: "interval", Kind: node.PropNumber, Label: "Sampling and publishing interval (ms)", Default: 1000,
				Help: "How often the server checks each node and sends what changed."},
		},
		Help: "Subscribes to nodes and sends a message each time a value changes. msg.topic is the node id, " +
			"msg.payload the new value, and msg.opcua the status and timestamps.\n\n" +
			"If the server goes away the node says so on its badge and raises an error once, then keeps " +
			"trying, with a longer wait after each failure up to thirty seconds. When the server is back it " +
			"subscribes again and carries on, with no redeploy.",
	}, newOPCUASubscribe)
}

func newOPCUASubscribe(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &opcuaSubscribeNode{
		id:    p.ID,
		cfgID: p.PropString("server", ""),
		svc:   def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected: pick the OPC UA server to subscribe to")
	}
	rows, _ := p.Raw["nodes"].([]any)
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		s := strings.TrimSpace(stringOr(m["nodeId"], ""))
		if s == "" {
			continue
		}
		id, err := parseOPCUANodeID(s)
		if err != nil {
			return nil, err
		}
		n.nodeIDs = append(n.nodeIDs, s)
		n.parsed = append(n.parsed, id)
	}
	if len(n.nodeIDs) == 0 {
		return nil, fmt.Errorf("add at least one node id to subscribe to")
	}
	ms := p.PropFloat("interval", 1000)
	if ms < 10 {
		return nil, fmt.Errorf("the interval is %v ms, and it has to be at least 10", ms)
	}
	n.interval = time.Duration(ms * float64(time.Millisecond))
	return n, nil
}

func (n *opcuaSubscribeNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (n *opcuaSubscribeNode) Start(ctx context.Context, out node.Emitter) error {
	cfg, ok := n.svc.ConfigNode(n.cfgID)
	if !ok {
		return fmt.Errorf("server config node %s is not running", n.cfgID)
	}
	tgt, ok := cfg.(opcuaTarget)
	if !ok {
		return fmt.Errorf("config node %s is not an OPC UA server", n.cfgID)
	}
	if node.StandInOf(n.svc) != nil {
		// A flow test subscribes to nothing. What the server would have sent
		// is whatever the test injects here.
		return nil
	}
	rctx, cancel := context.WithCancel(ctx)
	n.cancel = cancel
	n.done = make(chan struct{})
	out.Status(node.Status{Fill: "yellow", Shape: "ring", Text: "connecting"})
	go func() {
		defer close(n.done)
		n.run(rctx, tgt, out)
	}()
	return nil
}

func (n *opcuaSubscribeNode) Close(ctx context.Context, _ bool) error {
	if n.cancel != nil {
		n.cancel()
		select {
		case <-n.done:
		case <-ctx.Done():
		}
	}
	return nil
}

// run keeps a subscription alive for as long as the node is. Each pass is one
// session: connect, subscribe, then relay changes until something says the
// session is no good, and then wait and begin again.
func (n *opcuaSubscribeNode) run(ctx context.Context, tgt opcuaTarget, out node.Emitter) {
	wait := time.Second
	reported := false
	for ctx.Err() == nil {
		up := time.Now()
		err := n.session(ctx, tgt, out, &reported)
		if ctx.Err() != nil {
			return
		}
		if time.Since(up) > time.Minute {
			wait = time.Second
		}
		text := truncate(err.Error(), 32)
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: text})
		if !reported {
			// Once per outage. The retries that follow would flood a Catch
			// node with the same sentence every few seconds.
			out.Error(err, nil)
			reported = true
		}
		out.Log(node.LogWarn, "OPC UA subscription lost, trying again in %s: %v", wait, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait *= 2; wait > 30*time.Second {
			wait = 30 * time.Second
		}
	}
}

// opcuaStateNode is the server's own state variable (Server_ServerStatus_State),
// which every server has and any session may read.
var opcuaStateNode = ua.NewNumericNodeID(0, 2259)

func (n *opcuaSubscribeNode) session(ctx context.Context, tgt opcuaTarget, out node.Emitter, reported *bool) error {
	endpoint := tgt.describe()
	cl, err := tgt.client(ctx)
	if err != nil {
		return err
	}
	notify := make(chan *opcua.PublishNotificationData, 256)
	// Keepalive and lifetime are counted in intervals. Ten intervals of quiet
	// from a healthy server is a keepalive, not silence, and the lifetime has
	// to be at least three times that or the server cancels the subscription
	// before the client has said anything.
	sub, err := cl.Subscribe(ctx, &opcua.SubscriptionParameters{
		Interval:          n.interval,
		MaxKeepAliveCount: 10,
		LifetimeCount:     30,
	}, notify)
	if err != nil {
		tgt.lost(cl)
		return explainOPCUA(endpoint, "subscribing", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = sub.Cancel(cctx)
	}()

	byHandle := map[uint32]string{}
	var reqs []*ua.MonitoredItemCreateRequest
	for i, id := range n.parsed {
		h := uint32(i + 1)
		byHandle[h] = n.nodeIDs[i]
		r := opcua.NewMonitoredItemCreateRequestWithDefaults(id, ua.AttributeIDValue, h)
		r.RequestedParameters.SamplingInterval = float64(n.interval / time.Millisecond)
		reqs = append(reqs, r)
	}
	rctx, cancel := context.WithTimeout(ctx, tgt.requestTimeout())
	res, err := sub.Monitor(rctx, ua.TimestampsToReturnBoth, reqs...)
	cancel()
	if err != nil {
		tgt.lost(cl)
		return explainOPCUA(endpoint, "subscribing to the nodes", err)
	}
	for i, r := range res.Results {
		if r.StatusCode != ua.StatusOK && r.StatusCode != ua.StatusGood {
			// A node the server refuses is a configuration fault, not a
			// reason to throw the session away. Say which one.
			out.Error(explainOPCUA(endpoint, "subscribing to "+n.nodeIDs[i], r.StatusCode), nil)
		}
	}

	out.Status(node.Status{Fill: "green", Shape: "dot", Text: "subscribed"})
	*reported = false

	// The watchdog. A server that goes silent with the socket open produces no
	// error anywhere, only the absence of messages, and a value that doesn't
	// change looks the same. So when it's been quiet for a while, ask the
	// server something it always knows.
	quiet := 10 * n.interval
	if quiet < 5*time.Second {
		quiet = 5 * time.Second
	}
	timer := time.NewTimer(quiet)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case nd := <-notify:
			if nd == nil {
				continue
			}
			if nd.Error != nil {
				tgt.lost(cl)
				return explainOPCUA(endpoint, "the subscription", nd.Error)
			}
			dc, ok := nd.Value.(*ua.DataChangeNotification)
			if !ok {
				continue
			}
			for _, item := range dc.MonitoredItems {
				if item == nil || item.Value == nil {
					continue
				}
				nid := byHandle[item.ClientHandle]
				m := engine.NewMsg()
				m.SetTopic(nid)
				m.SetPayload(opcuaVariantValue(item.Value.Value))
				m.Data["opcua"] = opcuaInfo(endpoint, nid, item.Value)
				out.Send(0, m)
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(quiet)
		case <-timer.C:
			pctx, cancel := context.WithTimeout(ctx, tgt.requestTimeout())
			_, err := cl.Read(pctx, &ua.ReadRequest{
				NodesToRead:        []*ua.ReadValueID{{NodeID: opcuaStateNode, AttributeID: ua.AttributeIDValue}},
				TimestampsToReturn: ua.TimestampsToReturnNeither,
			})
			timedOut := pctx.Err() != nil
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if timedOut {
					err = context.DeadlineExceeded
				}
				if opcuaDropsSession(err) || timedOut {
					tgt.lost(cl)
				}
				return explainOPCUA(endpoint, "checking the server is still there", err)
			}
			timer.Reset(quiet)
		}
	}
}
