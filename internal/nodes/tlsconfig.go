// newTLSConfig is ported from Node-RED 5.0.7 @node-red/nodes
// core/network/05-tls.js (the TLSConfig constructor and addTLSOptions)
// (Apache-2.0), Copyright JS Foundation and other contributors,
// http://js.foundation. Modified. The rest of this file is HotLoop Flow's own.

package nodes

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/filescope"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/youmark/pkcs8"
	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

func init() {
	registerTLSConfig()
}

// Secrets is the process-wide scope a node may read a secret file from. main
// installs it from the configuration before any flow starts; the zero value
// refuses everything, so a TLS config pointing at a path fails loudly instead
// of reading whatever the process can.
var Secrets = &filescope.Scope{}

// maxSecretFileBytes bounds one certificate, key or PFX file. The biggest real
// chain is a few kilobytes; a path pointed at something enormous should be an
// error, not a heap.
const maxSecretFileBytes = 1 << 20

// tlsConfigNode is Node-RED's tls-config: a certificate and key, the CAs to
// trust, and whether to check the server at all, shared by every node that
// points at it.
type tlsConfigNode struct {
	verify     bool
	serverName string
	alpn       string

	// certs holds the node's own certificate, if it has one. A client sends it
	// when the server asks; a TCP In server presents it.
	certs []tls.Certificate

	// roots replaces the system trust store when set, which is what Node.js
	// does with a ca option. Nil trusts the system roots.
	roots *x509.CertPool
}

func registerTLSConfig() {
	node.MustRegister(node.Descriptor{
		Type:      "tls-config",
		Category:  node.CategoryConfig,
		Color:     colorNetwork,
		Icon:      "lock",
		IsConfig:  true,
		LabelProp: "name",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "Certificate, key and CA from files, from environment variables, " +
				"from uploaded PEM text, or from a PKCS#12 bundle, with a passphrase for " +
				"an encrypted key or bundle, server name, ALPN protocol and the verify " +
				"switch. Two differences. A path is read through the secret scope, the " +
				"data directory plus secrets.allowedPaths, because a TLS config that can " +
				"read any path is a way to probe the filesystem from the editor. And a " +
				"config that is broken, a certificate without its key or a file that is " +
				"not there, fails to deploy. Node-RED logs it and carries on, so the " +
				"connection quietly goes out without the client certificate you " +
				"configured.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "certType", Kind: node.PropSelect, Label: "Certificates from", Default: "files",
				Options: []node.Option{
					{Value: "files", Label: "Files or uploaded PEM"},
					{Value: "env", Label: "Environment variables"},
					{Value: "pfx", Label: "A PKCS#12 bundle"},
				}},
			{Name: "cert", Kind: node.PropString, Label: "Certificate file",
				Help: "Inside the data directory or a path in secrets.allowedPaths."},
			{Name: "key", Kind: node.PropString, Label: "Private key file"},
			{Name: "ca", Kind: node.PropString, Label: "CA certificate file",
				Help: "Replaces the system trust store, as it does in Node-RED."},
			{Name: "p12", Kind: node.PropString, Label: "PKCS#12 file"},
			{Name: "certEnv", Kind: node.PropString, Label: "Certificate variable"},
			{Name: "keyEnv", Kind: node.PropString, Label: "Private key variable"},
			{Name: "caEnv", Kind: node.PropString, Label: "CA certificate variable"},
			{Name: "certdata", Kind: node.PropCredential, Label: "Certificate (PEM)"},
			{Name: "keydata", Kind: node.PropCredential, Label: "Private key (PEM)"},
			{Name: "cadata", Kind: node.PropCredential, Label: "CA certificate (PEM)"},
			{Name: "p12data", Kind: node.PropCredential, Label: "PKCS#12 bundle (base64)"},
			{Name: "passphrase", Kind: node.PropCredential, Label: "Passphrase"},
			{Name: "servername", Kind: node.PropString, Label: "Server name",
				Help: "Sent as SNI and checked against the server's certificate. Leave " +
					"empty to use the host."},
			{Name: "alpnprotocol", Kind: node.PropString, Label: "ALPN protocol"},
			{Name: "verifyservercert", Kind: node.PropBool, Label: "Verify the server certificate",
				Default: true},
		},
		Help: "TLS settings for the TCP, HTTP request, WebSocket and MQTT nodes.",
	}, newTLSConfig)
}

func newTLSConfig(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &tlsConfigNode{
		verify:     p.PropBool("verifyservercert", true),
		serverName: strings.TrimSpace(p.PropString("servername", "")),
		alpn:       strings.TrimSpace(p.PropString("alpnprotocol", "")),
	}
	if node.StandInOf(def.Services) != nil {
		// Under a flow test nothing is dialled or served, so nothing is
		// encrypted and there's no certificate to present or check. Loading
		// one would read secret files off the disk the test runs on, which a
		// test doesn't touch, and would refuse to start wherever they aren't.
		return n, nil
	}

	certType := orDefault(p.PropString("certType", ""), "files")
	passphrase, _ := def.Services.Credential("passphrase")

	certPath := strings.TrimSpace(p.PropString("cert", ""))
	keyPath := strings.TrimSpace(p.PropString("key", ""))
	caPath := strings.TrimSpace(p.PropString("ca", ""))
	p12Path := strings.TrimSpace(p.PropString("p12", ""))

	// Where the material comes from follows Node-RED's constructor exactly,
	// including its order of precedence: a PFX file, then environment
	// variables, then paths, then whatever was uploaded into the credentials.
	var certPEM, keyPEM, caPEM, pfx []byte
	var err error
	switch {
	case certType == "pfx" && p12Path != "":
		if pfx, err = readSecretFile(p12Path); err != nil {
			return nil, err
		}

	case certType == "env":
		if certPEM, err = envSecret(def.Services, p.PropString("certEnv", "")); err != nil {
			return nil, fmt.Errorf("the certificate: %w", err)
		}
		if keyPEM, err = envSecret(def.Services, p.PropString("keyEnv", "")); err != nil {
			return nil, fmt.Errorf("the private key: %w", err)
		}
		if caPEM, err = envSecret(def.Services, p.PropString("caEnv", "")); err != nil {
			return nil, fmt.Errorf("the CA certificate: %w", err)
		}

	case certPath != "" || keyPath != "" || caPath != "":
		if (certPath != "") != (keyPath != "") {
			return nil, errCertWithoutKey
		}
		for _, f := range []struct {
			path string
			dst  *[]byte
		}{{certPath, &certPEM}, {keyPath, &keyPEM}, {caPath, &caPEM}} {
			if f.path == "" {
				continue
			}
			if *f.dst, err = readSecretFile(f.path); err != nil {
				return nil, err
			}
		}

	default:
		certData, _ := def.Services.Credential("certdata")
		keyData, _ := def.Services.Credential("keydata")
		caData, _ := def.Services.Credential("cadata")
		p12Data, _ := def.Services.Credential("p12data")
		if (certData != "") != (keyData != "") && p12Data == "" {
			return nil, errCertWithoutKey
		}
		certPEM, keyPEM, caPEM = []byte(certData), []byte(keyData), []byte(caData)
		if p12Data != "" {
			if pfx, err = base64.StdEncoding.DecodeString(strings.TrimSpace(p12Data)); err != nil {
				return nil, fmt.Errorf("the uploaded PKCS#12 bundle is not valid base64: %w", err)
			}
		}
	}

	// Which of it is used depends on the type, again as Node-RED's
	// addTLSOptions has it: a PFX config uses only the bundle, the others only
	// the PEM.
	if certType == "pfx" {
		if len(pfx) > 0 {
			if err := n.loadPFX(pfx, passphrase); err != nil {
				return nil, err
			}
		}
		return n, nil
	}

	if (len(certPEM) > 0) != (len(keyPEM) > 0) {
		return nil, errCertWithoutKey
	}
	if len(certPEM) > 0 {
		keyPEM, err = decryptKeyPEM(keyPEM, passphrase)
		if err != nil {
			return nil, err
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("the certificate and key do not load: %w", err)
		}
		n.certs = []tls.Certificate{pair}
	}
	if len(caPEM) > 0 {
		n.roots = x509.NewCertPool()
		if !n.roots.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("the CA certificate holds no PEM certificate")
		}
	}
	return n, nil
}

var errCertWithoutKey = errors.New("a certificate needs its private key and a key needs its " +
	"certificate; set both or neither")

// loadPFX unpacks a PKCS#12 bundle. Its CA certificates go out with the leaf
// as the chain and are added to the trust store on top of the system roots,
// which is what Node.js does with the extra certificates in a pfx.
func (n *tlsConfigNode) loadPFX(pfx []byte, passphrase string) error {
	key, leaf, cas, err := pkcs12.DecodeChain(pfx, passphrase)
	if err != nil {
		if errors.Is(err, pkcs12.ErrIncorrectPassword) {
			return errors.New("the PKCS#12 bundle did not open with the passphrase given")
		}
		return fmt.Errorf("the PKCS#12 bundle does not load: %w", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
	if len(cas) > 0 {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		for _, ca := range cas {
			cert.Certificate = append(cert.Certificate, ca.Raw)
			roots.AddCert(ca)
		}
		n.roots = roots
	}
	n.certs = []tls.Certificate{cert}
	return nil
}

// decryptKeyPEM returns the key unencrypted. Both kinds of encrypted PEM key
// Node.js takes a passphrase for are handled: PKCS#8 (BEGIN ENCRYPTED PRIVATE
// KEY, what openssl writes today) and the old OpenSSL form with a Proc-Type
// header.
func decryptKeyPEM(keyPEM []byte, passphrase string) ([]byte, error) {
	rest := keyPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			// No encrypted block. X509KeyPair reports anything else wrong.
			return keyPEM, nil
		}
		switch {
		case block.Type == "ENCRYPTED PRIVATE KEY":
			if passphrase == "" {
				return nil, errors.New("the private key is encrypted and no passphrase is set")
			}
			key, err := pkcs8.ParsePKCS8PrivateKey(block.Bytes, []byte(passphrase))
			if err != nil {
				return nil, fmt.Errorf("the private key did not decrypt with the passphrase given: %w", err)
			}
			der, err := x509.MarshalPKCS8PrivateKey(key)
			if err != nil {
				return nil, fmt.Errorf("re-encoding the decrypted key: %w", err)
			}
			return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil

		case x509.IsEncryptedPEMBlock(block):
			// Deprecated in Go because the format is weak, which is true and
			// beside the point: it is what an old key on a plant floor is in.
			if passphrase == "" {
				return nil, errors.New("the private key is encrypted and no passphrase is set")
			}
			der, err := x509.DecryptPEMBlock(block, []byte(passphrase))
			if err != nil {
				return nil, fmt.Errorf("the private key did not decrypt with the passphrase given: %w", err)
			}
			return pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: der}), nil
		}
	}
}

// ReadSecretFile reads a file through the secret scope. It is how a tls-config
// reads its certificate, key and CA, and how the runtime reads a credential a
// node names in ew_credentialFiles.
func ReadSecretFile(path string) ([]byte, error) { return readSecretFile(path) }

// readSecretFile reads one certificate, key or bundle through the secret scope.
func readSecretFile(path string) ([]byte, error) {
	checked, err := Secrets.Check(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(checked)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", checked, err)
	}
	if len(b) > maxSecretFileBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes, which no certificate or password is", checked, maxSecretFileBytes)
	}
	return b, nil
}

var envRef = regexp.MustCompile(`\$\{([^}]+)\}`)

// envSecret reads PEM text from an environment variable, named the way
// Node-RED's env property type names it: a bare name, ${NAME}, or text with
// ${NAME} references in it. Empty means the field is not used.
func envSecret(svc node.Services, ref string) ([]byte, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil
	}
	lookup := func(name string) string {
		v, _ := svc.Env(name)
		return v
	}
	var v string
	if !envRef.MatchString(ref) {
		v = lookup(ref)
	} else {
		v = envRef.ReplaceAllStringFunc(ref, func(m string) string {
			return lookup(m[2 : len(m)-1])
		})
	}
	if strings.TrimSpace(v) == "" {
		// Node-RED hands an empty buffer to OpenSSL here, and the failure turns
		// up at connect time as a TLS error that never mentions the variable.
		return nil, fmt.Errorf("the environment variable %s is empty or not set", ref)
	}
	return []byte(v), nil
}

func (n *tlsConfigNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

// clientConfig is the TLS configuration for dialling out.
//
// ServerName is left empty unless configured, so Go fills it from the host
// being dialled, which is the host Node.js checks the certificate against too.
func (n *tlsConfigNode) clientConfig() *tls.Config {
	cfg := &tls.Config{
		Certificates: n.certs,
		RootCAs:      n.roots,
		ServerName:   n.serverName,
		// Off only when the user unticked it, as in Node-RED.
		InsecureSkipVerify: !n.verify,
	}
	if n.alpn != "" {
		cfg.NextProtos = []string{n.alpn}
	}
	return cfg
}

// serverConfig is the TLS configuration for a listener. Node-RED's TCP server
// does not ask the client for a certificate, so neither does this.
func (n *tlsConfigNode) serverConfig() (*tls.Config, error) {
	if len(n.certs) == 0 {
		return nil, errors.New("a TLS server needs a certificate and key, and this TLS config has none")
	}
	cfg := &tls.Config{Certificates: n.certs}
	if n.alpn != "" {
		cfg.NextProtos = []string{n.alpn}
	}
	return cfg, nil
}

// tlsConfigRef reads the id of the tls-config a node points at. The editor
// writes "_ADD_" for an unticked "use TLS" box on some versions; that means
// none, the same as empty.
func tlsConfigRef(p *engine.Node) string {
	id := strings.TrimSpace(p.PropString("tls", ""))
	if id == "_ADD_" {
		return ""
	}
	return id
}

// lookupTLSConfig resolves a tls-config by id.
func lookupTLSConfig(svc node.Services, id string) (*tlsConfigNode, error) {
	if svc == nil {
		return nil, errors.New("no runtime services available")
	}
	cfg, ok := svc.ConfigNode(id)
	if !ok {
		// Node-RED dereferences the missing node and the flow dies with a
		// TypeError. Plaintext instead of TLS would be worse than either.
		return nil, fmt.Errorf("the TLS config %s is not deployed or failed to start", id)
	}
	t, ok := cfg.(*tlsConfigNode)
	if !ok {
		return nil, fmt.Errorf("node %s is not a TLS config", id)
	}
	return t, nil
}
