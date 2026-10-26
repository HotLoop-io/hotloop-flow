package assertion

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"reflect"
	"testing"
	"time"
)

type signer struct{ key *ecdsa.PrivateKey }

func newSigner(t *testing.T) signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{k}
}

func (s signer) pub() []byte {
	der, _ := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func (s signer) sign(t *testing.T, header map[string]any, claims map[string]any) string {
	t.Helper()
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signing := b64.EncodeToString(hb) + "." + b64.EncodeToString(cb)
	sum := sha256.Sum256([]byte(signing))
	r, sg, err := ecdsa.Sign(rand.Reader, s.key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sg.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig)
}

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func good() map[string]any {
	return map[string]any{
		"iss": "embernet-dashboard", "sub": "u1@example.com", "aud": "fir-001/flow",
		"email": "u1@example.com", "tenant": "fir-001", "role": "*", "roles": []string{"*"},
		"nbf": now.Unix(), "exp": now.Add(60 * time.Second).Unix(),
	}
}

func with(m map[string]any, k string, v any) map[string]any {
	out := map[string]any{}
	for a, b := range m {
		out[a] = b
	}
	if v == nil {
		delete(out, k)
	} else {
		out[k] = v
	}
	return out
}

func verifier(t *testing.T, s signer) *Verifier {
	t.Helper()
	v, err := New(s.pub(), "", "fir-001/flow")
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return now }
	return v
}

var es256 = map[string]any{"alg": "ES256", "typ": "JWT", "kid": "x"}

func TestAValidTokenIsAccepted(t *testing.T) {
	s := newSigner(t)
	c, err := verifier(t, s).Verify(s.sign(t, es256, good()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "u1@example.com" || c.Tenant != "fir-001" || !reflect.DeepEqual(c.Roles, []string{"*"}) {
		t.Fatalf("claims = %+v", c)
	}
}

func TestEveryWayOfBeingRefused(t *testing.T) {
	s := newSigner(t)
	other := newSigner(t)
	v := verifier(t, s)
	cases := map[string]string{
		"signed by another key":              other.sign(t, es256, good()),
		"expired":                            s.sign(t, es256, with(good(), "exp", now.Add(-time.Minute).Unix())),
		"not yet valid":                      s.sign(t, es256, with(good(), "nbf", now.Add(time.Minute).Unix())),
		"no expiry at all":                   s.sign(t, es256, with(good(), "exp", nil)),
		"another audience":                   s.sign(t, es256, with(good(), "aud", "fir-001/other")),
		"another tenant":                     s.sign(t, es256, with(good(), "aud", "zzz/flow")),
		"no audience":                        s.sign(t, es256, with(good(), "aud", nil)),
		"another issuer":                     s.sign(t, es256, with(good(), "iss", "evil")),
		"no subject":                         s.sign(t, es256, with(good(), "sub", nil)),
		"alg none":                           "eyJhbGciOiJub25lIn0." + b64.EncodeToString([]byte(`{"iss":"embernet-dashboard","sub":"x","aud":"fir-001/flow","exp":9999999999}`)) + ".",
		"alg HS256":                          s.sign(t, map[string]any{"alg": "HS256"}, good()),
		"alg ES384":                          s.sign(t, map[string]any{"alg": "ES384"}, good()),
		"not a token":                        "hello",
		"empty":                              "",
		"two parts":                          "a.b",
		"garbage signature":                  s.sign(t, es256, good())[:len(s.sign(t, es256, good()))-8] + "AAAAAAAA",
		"signature taken from another token": "",
	}
	// A token whose signature is another token's is a signature that does not
	// fit it.
	tok := s.sign(t, es256, good())
	forged := s.sign(t, es256, with(good(), "roles", []string{"*", "admin"}))
	cases["signature taken from another token"] = tok[:len(tok)-len(tok[len(tok)-86:])] + forged[len(forged)-86:]
	for name, tok := range cases {
		if c, err := v.Verify(tok); err == nil {
			t.Errorf("%s: accepted: %+v", name, c)
		}
	}
}

func TestClockSkewWithinLeewayIsTolerated(t *testing.T) {
	s := newSigner(t)
	v := verifier(t, s)
	v.now = func() time.Time { return now.Add(62 * time.Second) } // 2s past exp
	if _, err := v.Verify(s.sign(t, es256, good())); err != nil {
		t.Fatalf("2s past exp refused: %v", err)
	}
	v.now = func() time.Time { return now.Add(70 * time.Second) } // 10s past exp
	if _, err := v.Verify(s.sign(t, es256, good())); err == nil {
		t.Fatal("10s past exp accepted")
	}
}

func TestRoleFallsBackToTheSingleRoleClaim(t *testing.T) {
	s := newSigner(t)
	c, err := verifier(t, s).Verify(s.sign(t, es256, with(with(good(), "roles", nil), "role", "read")))
	if err != nil || !reflect.DeepEqual(c.Roles, []string{"read"}) {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestNewRefusesWhatCouldNotWork(t *testing.T) {
	s := newSigner(t)
	if _, err := New(s.pub(), "", ""); err == nil {
		t.Error("no audience accepted")
	}
	if _, err := New([]byte("nope"), "", "a/b"); err == nil {
		t.Error("a non-PEM key accepted")
	}
	k384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&k384.PublicKey)
	if _, err := New(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), "", "a/b"); err == nil {
		t.Error("a P-384 key accepted")
	}
}

func TestRolesBecomePermissions(t *testing.T) {
	cases := []struct {
		roles []string
		want  []string
		ok    bool
	}{
		{[]string{"*"}, []string{"*"}, true},
		{[]string{"read"}, ReadOnlyPermissions, true},
		{[]string{"read", "*"}, []string{"*"}, true},
		{[]string{"admin"}, nil, false},
		{[]string{"Admin", "Viewer"}, nil, false},
		{nil, nil, false},
		{[]string{""}, nil, false},
	}
	for _, c := range cases {
		got, ok := Permissions(c.roles)
		if ok != c.ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Permissions(%v) = %v, %v; want %v, %v", c.roles, got, ok, c.want, c.ok)
		}
	}
	for _, p := range ReadOnlyPermissions {
		for _, w := range []string{"flows.write", "inject.write", "audit.read", "auth.admin"} {
			if p == w {
				t.Errorf("read-only holds %s", w)
			}
		}
	}
}
