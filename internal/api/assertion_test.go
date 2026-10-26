package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/config"
)

const assertionAudience = "fir-001/flow"

// dashboard is a stand-in for the EmberCORE dashboard's signer, with the same
// token shape: ES256, r then s, kid in the header, issuer, audience, roles.
type dashboard struct {
	key *ecdsa.PrivateKey
}

func newDashboard(t *testing.T) dashboard {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return dashboard{k}
}

func (d dashboard) publicKeyFile(t *testing.T) string {
	t.Helper()
	der, _ := x509.MarshalPKIXPublicKey(&d.key.PublicKey)
	p := filepath.Join(t.TempDir(), "public.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (d dashboard) token(t *testing.T, sub string, roles []string, mutate ...func(map[string]any)) string {
	t.Helper()
	now := time.Now()
	c := map[string]any{
		"iss": "embernet-dashboard", "sub": sub, "aud": assertionAudience, "email": sub, "tenant": "fir-001",
		"role": "", "roles": roles, "nbf": now.Unix(), "exp": now.Add(60 * time.Second).Unix(),
	}
	if len(roles) > 0 {
		c["role"] = roles[0]
	}
	for _, m := range mutate {
		m(c)
	}
	enc := base64.RawURLEncoding
	hb, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": "abc"})
	cb, _ := json.Marshal(c)
	signing := enc.EncodeToString(hb) + "." + enc.EncodeToString(cb)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, d.key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + enc.EncodeToString(sig)
}

func withAssertion(keyFile string, mutate ...func(*config.Assertion)) func(*config.Config) {
	return func(c *config.Config) {
		c.Auth.Assertion.PublicKeyFile = keyFile
		c.Auth.Assertion.Audience = assertionAudience
		for _, m := range mutate {
			m(&c.Auth.Assertion)
		}
	}
}

const header = "X-Embernet-Assertion"

func TestDashboardFullAccessAndReadOnly(t *testing.T) {
	d := newDashboard(t)
	ts := newTestServer(t, map[string][]string{"admin": {"*"}}, withAssertion(d.publicKeyFile(t)))
	full := d.token(t, "eng@example.com", []string{"*"})
	read := d.token(t, "op@example.com", []string{"read"})

	// 503 means the permission check passed and the handler found no runtime.
	cases := []struct {
		name, tok, method, path string
		allowed                 bool
	}{
		{"full reads flows", full, "GET", "/flows", true},
		{"full deploys", full, "POST", "/flows", true},
		{"full injects", full, "POST", "/inject/n1", true},
		{"full reads settings", full, "GET", "/settings", true},
		{"read reads flows", read, "GET", "/flows", true},
		{"read reads nodes", read, "GET", "/nodes", true},
		{"read reads settings", read, "GET", "/settings", true},
		{"read reads status", read, "GET", "/runtime/stats", true},
		{"read cannot deploy", read, "POST", "/flows", false},
		{"read cannot inject", read, "POST", "/inject/n1", false},
	}
	for _, c := range cases {
		var body []byte
		if c.method == "POST" && c.path == "/flows" {
			body = []byte(`[]`)
		}
		res, out := ts.do(t, c.method, c.path, "", body, header, c.tok)
		if c.allowed && (res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusUnauthorized) {
			t.Errorf("%s: %d (%s), want allowed", c.name, res.StatusCode, out)
		}
		if !c.allowed && res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", c.name, res.StatusCode)
		}
	}
}

func TestDashboardSettingsSaysSignedInByTheDashboard(t *testing.T) {
	d := newDashboard(t)
	ts := newTestServer(t, map[string][]string{"admin": {"*"}}, withAssertion(d.publicKeyFile(t)))
	_, out := ts.do(t, "GET", "/settings", "", nil, header, d.token(t, "u@example.com", []string{"*"}))
	var s struct {
		Auth struct{ SSO bool } `json:"auth"`
	}
	if err := json.Unmarshal(out, &s); err != nil || !s.Auth.SSO {
		t.Fatalf("settings.auth.sso = %v (%v) in %s", s.Auth.SSO, err, out)
	}
	_, out = ts.do(t, "GET", "/settings", ts.mustLogin(t, "admin"), nil)
	_ = json.Unmarshal(out, &s)
	if s.Auth.SSO {
		t.Fatalf("a password sign-in says it came from the dashboard: %s", out)
	}
}

func TestDashboardRoleChangeLandsOnTheNextRequest(t *testing.T) {
	d := newDashboard(t)
	ts := newTestServer(t, nil, withAssertion(d.publicKeyFile(t)))
	post := func(tok string) int {
		res, _ := ts.do(t, "POST", "/flows", "", []byte(`[]`), header, tok)
		return res.StatusCode
	}
	if got := post(d.token(t, "same@example.com", []string{"read"})); got != http.StatusForbidden {
		t.Fatalf("read deploying: %d, want 403", got)
	}
	if got := post(d.token(t, "same@example.com", []string{"*"})); got == http.StatusForbidden || got == http.StatusUnauthorized {
		t.Fatalf("after the role became full: %d, want allowed", got)
	}
	if got := post(d.token(t, "same@example.com", []string{"read"})); got != http.StatusForbidden {
		t.Fatalf("after it went back to read: %d, want 403", got)
	}
}

func TestDashboardRefusals(t *testing.T) {
	d := newDashboard(t)
	other := newDashboard(t)
	ts := newTestServer(t, map[string][]string{"admin": {"*"}}, withAssertion(d.publicKeyFile(t)))
	bearer := ts.mustLogin(t, "admin") // a good bearer token rides along on every case

	cases := map[string]struct {
		tok  string
		want int
	}{
		"forged, signed by another key": {other.token(t, "x@example.com", []string{"*"}), http.StatusUnauthorized},
		"expired": {d.token(t, "x@example.com", []string{"*"}, func(c map[string]any) {
			c["exp"] = time.Now().Add(-2 * time.Minute).Unix()
		}), http.StatusUnauthorized},
		"wrong audience, another app":    {d.token(t, "x@example.com", []string{"*"}, func(c map[string]any) { c["aud"] = "fir-001/other" }), http.StatusUnauthorized},
		"wrong audience, another tenant": {d.token(t, "x@example.com", []string{"*"}, func(c map[string]any) { c["aud"] = "zzz/flow" }), http.StatusUnauthorized},
		"wrong issuer":                   {d.token(t, "x@example.com", []string{"*"}, func(c map[string]any) { c["iss"] = "evil" }), http.StatusUnauthorized},
		"alg none":                       {"eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"embernet-dashboard","sub":"x","aud":"fir-001/flow","exp":9999999999,"roles":["*"]}`)) + ".", http.StatusUnauthorized},
		"garbage":                        {"not-a-token", http.StatusUnauthorized},
		"no role":                        {d.token(t, "x@example.com", nil), http.StatusForbidden},
		"unknown role":                   {d.token(t, "x@example.com", []string{"Admin"}), http.StatusForbidden},
	}
	for name, c := range cases {
		// With the valid bearer beside it: a bad assertion is not rescued by it.
		res, _ := ts.do(t, "GET", "/flows", bearer, nil, header, c.tok)
		if res.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", name, res.StatusCode, c.want)
		}
		res, _ = ts.do(t, "GET", "/flows", "", nil, header, c.tok)
		if res.StatusCode != c.want {
			t.Errorf("%s (no bearer): %d, want %d", name, res.StatusCode, c.want)
		}
	}
}

func TestBearerTokensAndPasswordsKeepWorkingBesideIt(t *testing.T) {
	d := newDashboard(t)
	ts := newTestServer(t, map[string][]string{"admin": {"*"}}, withAssertion(d.publicKeyFile(t)))
	tok := ts.mustLogin(t, "admin")
	if res, _ := ts.do(t, "GET", "/flows", tok, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("bearer with no header: %d, want 200", res.StatusCode)
	}
	if res, _ := ts.do(t, "GET", "/flows", "", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("nothing at all: %d, want 401", res.StatusCode)
	}
}

func TestDashboardSignInIsOffUnlessAKeyIsConfigured(t *testing.T) {
	d := newDashboard(t)
	ts := newTestServer(t, map[string][]string{"admin": {"*"}})
	// The header means nothing: it is not read, so it signs nobody in.
	if res, _ := ts.do(t, "GET", "/flows", "", nil, header, d.token(t, "x@example.com", []string{"*"})); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a signed token with no key configured: %d, want 401", res.StatusCode)
	}
	// And a bearer token beside the header behaves exactly as without it.
	tok := ts.mustLogin(t, "admin")
	if res, _ := ts.do(t, "GET", "/flows", tok, nil, header, "junk"); res.StatusCode != http.StatusOK {
		t.Fatalf("bearer with a stray header: %d, want 200", res.StatusCode)
	}
	_, out := ts.do(t, "GET", "/auth/mode", "", nil)
	if strings.Contains(string(out), `true`) {
		t.Fatalf("/auth/mode with no key: %s", out)
	}
	var m struct{ Dashboard, HideLogin bool }
	_ = json.Unmarshal(out, &m)
	if m.Dashboard || m.HideLogin {
		t.Fatalf("/auth/mode with no key: %s", out)
	}
}

func TestAuthModeNeedsNoSignInAndLeaksNothing(t *testing.T) {
	d := newDashboard(t)
	ts := newTestServer(t, map[string][]string{"admin": {"*"}}, withAssertion(d.publicKeyFile(t)))
	res, out := ts.do(t, "GET", "/auth/mode", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d", res.StatusCode)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if len(m) != 2 || m["dashboard"] != true || m["hideLogin"] != true {
		t.Fatalf("/auth/mode = %s", out)
	}
	ts2 := newTestServer(t, map[string][]string{"admin": {"*"}}, withAssertion(d.publicKeyFile(t), func(a *config.Assertion) { a.HideLogin = false }))
	_, out = ts2.do(t, "GET", "/auth/mode", "", nil)
	_ = json.Unmarshal(out, &m)
	if m["hideLogin"] != false {
		t.Fatalf("hideLogin false is not honored: %s", out)
	}
}

func TestConfigRefusesABrokenDashboardSignIn(t *testing.T) {
	d := newDashboard(t)
	good := d.publicKeyFile(t)
	bad := filepath.Join(t.TempDir(), "bad.pem")
	_ = os.WriteFile(bad, []byte("nope"), 0o600)
	for name, mut := range map[string]func(*config.Assertion){
		"no audience":    func(a *config.Assertion) { a.PublicKeyFile, a.Audience = good, "" },
		"unreadable key": func(a *config.Assertion) { a.PublicKeyFile = filepath.Join(t.TempDir(), "missing.pem") },
		"key not a key":  func(a *config.Assertion) { a.PublicKeyFile = bad },
		"no header":      func(a *config.Assertion) { a.Header = "" },
	} {
		cfg := config.Default()
		hash, _ := config.HashPassword(testPassword)
		cfg.Data.CredentialSecret = "a-credential-secret-for-the-test"
		cfg.Auth.Users = []config.User{{Username: "a", PasswordHash: hash, Permissions: []string{"*"}}}
		cfg.Auth.Assertion.PublicKeyFile, cfg.Auth.Assertion.Audience = good, assertionAudience
		mut(&cfg.Auth.Assertion)
		// The refusal has to be about this and not about some other setting the
		// test forgot, or every case would pass for the wrong reason.
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "assertion") {
			t.Errorf("%s: Validate said %v", name, err)
		}
	}
	cfg := config.Default()
	hash, _ := config.HashPassword(testPassword)
	cfg.Data.CredentialSecret = "a-credential-secret-for-the-test"
	cfg.Auth.Users = []config.User{{Username: "a", PasswordHash: hash, Permissions: []string{"*"}}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("no key configured: %v", err)
	}
	cfg.Auth.Assertion.PublicKeyFile, cfg.Auth.Assertion.Audience = good, assertionAudience
	if err := cfg.Validate(); err != nil {
		t.Errorf("a good key: %v", err)
	}
}
