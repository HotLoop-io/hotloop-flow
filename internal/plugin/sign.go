package plugin

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// A signature is ed25519 over this message, not over either file alone:
//
//	hotloop-flow-plugin-v1
//	module sha256 <hex of plugin.wasm>
//	descriptor sha256 <hex of plugin.json>
//
// Both files are covered because the descriptor is where a plugin asks for
// host access. A signature over the module alone would let anyone with write
// access to the directory add a capability to a signed plugin.
const signedPrefix = "hotloop-flow-plugin-v1\n"

// SignedMessage is what gets signed for these two files.
func SignedMessage(module, descriptor []byte) []byte {
	m := sha256.Sum256(module)
	d := sha256.Sum256(descriptor)
	return []byte(signedPrefix +
		"module sha256 " + hex.EncodeToString(m[:]) + "\n" +
		"descriptor sha256 " + hex.EncodeToString(d[:]) + "\n")
}

// Signature is plugin.sig.
type Signature struct {
	Format    int    `json:"format"`
	Alg       string `json:"alg"`
	KeyID     string `json:"keyId"`
	Signature string `json:"signature"`
}

// KeyID names a public key: the first eight bytes of its SHA-256, in hex. It
// is how a refusal says which key signed a plugin without printing the key.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// Sign signs a plugin's module and descriptor.
func Sign(priv ed25519.PrivateKey, module, descriptor []byte) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("not an ed25519 private key")
	}
	pub := priv.Public().(ed25519.PublicKey)
	sig := Signature{
		Format:    FormatVersion,
		Alg:       "ed25519",
		KeyID:     KeyID(pub),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, SignedMessage(module, descriptor))),
	}
	return json.MarshalIndent(sig, "", "  ")
}

// Refusal reasons a verification can end in, so a caller can tell an unsigned
// plugin from a tampered one without matching on text.
var (
	ErrUnsigned     = errors.New("unsigned")
	ErrBadSig       = errors.New("signature does not match")
	ErrUntrusted    = errors.New("signing key is not trusted")
	ErrMalformedSig = errors.New("signature file is malformed")
)

// Trust is the set of publisher keys an operator has decided to believe.
type Trust struct {
	keys map[string]ed25519.PublicKey // by key id
}

// NewTrust builds a Trust from public keys.
func NewTrust(keys ...ed25519.PublicKey) *Trust {
	t := &Trust{keys: map[string]ed25519.PublicKey{}}
	for _, k := range keys {
		t.keys[KeyID(k)] = k
	}
	return t
}

// Len is how many keys are trusted.
func (t *Trust) Len() int {
	if t == nil {
		return 0
	}
	return len(t.keys)
}

// IDs lists the trusted key ids, for the plugin list.
func (t *Trust) IDs() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.keys))
	for id := range t.keys {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Verify checks plugin.sig against the module and descriptor and returns the id
// of the key that signed them. sig is nil for a plugin with no signature file.
//
// The key is found by the id in the signature file and the signature is
// checked against that key and no other. A signature from a key the operator
// hasn't trusted is a different refusal from a signature that doesn't match,
// because the fix is different: one needs the key added, the other means the
// files are not what was signed.
func (t *Trust) Verify(sig, module, descriptor []byte) (string, error) {
	if sig == nil {
		return "", fmt.Errorf("%w: %s is missing", ErrUnsigned, SignatureFile)
	}
	var s Signature
	if err := json.Unmarshal(sig, &s); err != nil {
		return "", fmt.Errorf("%w: %v", ErrMalformedSig, err)
	}
	if s.Format != FormatVersion || s.Alg != "ed25519" {
		return "", fmt.Errorf("%w: format %d, algorithm %q; this build reads format %d, ed25519",
			ErrMalformedSig, s.Format, s.Alg, FormatVersion)
	}
	raw, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return "", fmt.Errorf("%w: the signature is not %d base64 bytes", ErrMalformedSig, ed25519.SignatureSize)
	}
	if t.Len() == 0 {
		return s.KeyID, fmt.Errorf("%w: no trusted keys are configured, so no plugin can load (plugins.trustedKeys)", ErrUntrusted)
	}
	pub, ok := t.keys[s.KeyID]
	if !ok {
		return s.KeyID, fmt.Errorf("%w: it was signed with key %s, which is not in plugins.trustedKeys", ErrUntrusted, s.KeyID)
	}
	if !ed25519.Verify(pub, SignedMessage(module, descriptor), raw) {
		return s.KeyID, fmt.Errorf("%w: %s or %s was changed after it was signed with key %s, or the signature is damaged",
			ErrBadSig, ModuleFile, DescriptorFile, s.KeyID)
	}
	return s.KeyID, nil
}

// GenerateKey makes a publisher key pair as PEM: a PKCS #8 private key and a
// PKIX public key, which is what `openssl genpkey -algorithm ed25519` and
// `openssl pkey -pubout` produce, so either side can be made with OpenSSL.
func GenerateKey() (privPEM, pubPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	pb, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pb}), nil
}

// ParsePrivateKey reads a PKCS #8 PEM ed25519 private key.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("not a PEM PRIVATE KEY (PKCS #8)")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("the key is not an ed25519 key")
	}
	return priv, nil
}

// PublicKeyText is the one-line form of a public key an operator can paste into
// a config file or an environment variable.
func PublicKeyText(pub ed25519.PublicKey) string {
	return "ed25519:" + base64.StdEncoding.EncodeToString(pub)
}

// ParsePublicKey reads a trusted key in either form: "ed25519:<base64>", or a
// PEM PUBLIC KEY.
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	text = strings.TrimSpace(text)
	if rest, ok := strings.CutPrefix(text, "ed25519:"); ok {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("an ed25519 key is %d bytes of base64", ed25519.PublicKeySize)
		}
		return ed25519.PublicKey(raw), nil
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("not an ed25519:<base64> key or a PEM PUBLIC KEY")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("the key is not an ed25519 key")
	}
	return pub, nil
}

// LoadTrust reads the configured trusted keys. Each entry is a key written out
// ("ed25519:<base64>" or PEM) or the path of a file holding one, which is how a
// Kubernetes Secret or ConfigMap mounted as a file gets in.
func LoadTrust(entries []string) (*Trust, error) {
	var keys []ed25519.PublicKey
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		text := e
		if !strings.HasPrefix(e, "ed25519:") && !strings.Contains(e, "-----BEGIN") {
			data, err := os.ReadFile(e)
			if err != nil {
				return nil, fmt.Errorf("trusted key %q: %w", e, err)
			}
			text = string(data)
		}
		k, err := ParsePublicKey(text)
		if err != nil {
			return nil, fmt.Errorf("trusted key %q: %w", shorten(e), err)
		}
		keys = append(keys, k)
	}
	return NewTrust(keys...), nil
}

func shorten(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
