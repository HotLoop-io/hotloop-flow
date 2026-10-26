// Package assertion verifies the signed assertion the EmberCORE dashboard puts
// in X-Embernet-Assertion on every request it proxies to an app, and turns the
// roles in it into Flow permissions.
//
// The dashboard signs with a private key it never shares. Flow holds only the
// public half, from a mounted file, so a pod that reads it can check a token
// and cannot make one. A forged header from any other pod in the cluster fails
// the signature, which is why this is a signed token and not a plain header.
//
// The token is ES256 (ECDSA on P-256 with SHA-256, the signature as r then s,
// 64 bytes) and carries an issuer, an audience of "<tenant>/<release>", a
// subject (the person's email), roles, and a lifetime of about a minute. The
// dashboard mints a fresh one per request, so nothing here keeps state: a role
// changed in the dashboard lands on the next request, and there is nothing to
// revoke.
package assertion

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"
)

// DefaultHeader is the request header the dashboard sets.
const DefaultHeader = "X-Embernet-Assertion"

// DefaultIssuer is the iss claim the dashboard signs.
const DefaultIssuer = "embernet-dashboard"

// leeway is the clock skew tolerated either side of nbf and exp.
const leeway = 5 * time.Second

// ErrRefused is every way a token is refused. Callers learn nothing more, and
// neither does whoever sent it.
var ErrRefused = errors.New("assertion refused")

// Claims is what a verified assertion says.
type Claims struct {
	Subject string
	Email   string
	Tenant  string
	Roles   []string
}

type wireClaims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  string   `json:"aud"`
	Email     string   `json:"email"`
	Tenant    string   `json:"tenant"`
	Role      string   `json:"role"`
	Roles     []string `json:"roles"`
	NotBefore *int64   `json:"nbf"`
	Expires   *int64   `json:"exp"`
}

// Verifier checks assertions against one public key, one issuer and one
// audience.
type Verifier struct {
	key      *ecdsa.PublicKey
	issuer   string
	audience string
	now      func() time.Time
}

// New is a Verifier for the PKIX PEM public key in pemBytes. The audience is
// required: a token made for one app must open no other, even though every app
// holds the same public key.
func New(pemBytes []byte, issuer, audience string) (*Verifier, error) {
	if strings.TrimSpace(audience) == "" {
		return nil, errors.New("assertion: an audience is required, <tenant>/<release>")
	}
	if issuer == "" {
		issuer = DefaultIssuer
	}
	pub, err := ParsePublicKeyPEM(pemBytes)
	if err != nil {
		return nil, err
	}
	return &Verifier{key: pub, issuer: issuer, audience: audience, now: time.Now}, nil
}

// Load is New with the key read from a file.
func Load(path, issuer, audience string) (*Verifier, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("assertion: reading the public key: %w", err)
	}
	return New(b, issuer, audience)
}

// ParsePublicKeyPEM reads a PKIX public key and insists it is ECDSA on P-256.
func ParsePublicKeyPEM(b []byte) (*ecdsa.PublicKey, error) {
	blk, _ := pem.Decode([]byte(strings.TrimSpace(string(b))))
	if blk == nil {
		return nil, errors.New("assertion: the public key is not PEM")
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("assertion: the public key is not PKIX: %w", err)
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("assertion: the public key is not ECDSA P-256")
	}
	return pub, nil
}

var b64 = base64.RawURLEncoding

// Verify checks token and returns its claims. It refuses anything but ES256,
// a signature that does not verify, a token with no expiry, an expired or not
// yet valid one, another issuer, another audience, and one with no subject.
func (v *Verifier) Verify(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrRefused
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, ErrRefused
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(hb, &h) != nil || h.Alg != "ES256" {
		return nil, ErrRefused
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, ErrRefused
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(v.key, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, ErrRefused
	}
	bb, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, ErrRefused
	}
	var c wireClaims
	if json.Unmarshal(bb, &c) != nil {
		return nil, ErrRefused
	}
	if c.Issuer != v.issuer || c.Audience != v.audience || c.Subject == "" {
		return nil, ErrRefused
	}
	now := v.now()
	if c.Expires == nil || now.Add(-leeway).Unix() >= *c.Expires {
		return nil, ErrRefused
	}
	if c.NotBefore != nil && now.Add(leeway).Unix() < *c.NotBefore {
		return nil, ErrRefused
	}
	roles := c.Roles
	if len(roles) == 0 && c.Role != "" {
		roles = []string{c.Role}
	}
	return &Claims{Subject: c.Subject, Email: c.Email, Tenant: c.Tenant, Roles: roles}, nil
}

// The two roles the dashboard's role map gives a Flow instance: "*" is full
// access, "read" is read-only. They are the words the Node-RED kind uses, so
// one tenant map reads the same for both.
const (
	RoleFull = "*"
	RoleRead = "read"
)

// ReadOnlyPermissions are what "read" holds: look at the flows, the node
// palette, the settings and the status, and change nothing. Not the deployment
// log, which says who deployed what, and not injecting a message.
var ReadOnlyPermissions = []string{"flows.read", "nodes.read", "settings.read", "status.read"}

// Permissions turns roles into Flow permissions. "*" wins over "read". A role
// that is neither is ignored, and none at all is ok == false: the person gets
// no access rather than a guess.
func Permissions(roles []string) (perms []string, ok bool) {
	read := false
	for _, r := range roles {
		switch r {
		case RoleFull:
			return []string{"*"}, true
		case RoleRead:
			read = true
		}
	}
	if read {
		return append([]string(nil), ReadOnlyPermissions...), true
	}
	return nil, false
}
