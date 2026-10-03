package mfa

import (
	"encoding/base32"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1 column. The RFC's codes are eight digits; the
// six-digit code is the last six of them, which is what dynamic truncation
// modulo 10^6 gives.
func TestRFC6238Vectors(t *testing.T) {
	key := []byte("12345678901234567890")
	for _, v := range []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	} {
		step := v.unix / 30
		if got := code(key, step, 8); got != v.want {
			t.Errorf("T=%d: %s, want %s", v.unix, got, v.want)
		}
		secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(key)
		if got, _ := Code(secret, step); got != v.want[2:] {
			t.Errorf("T=%d six digits: %s, want %s", v.unix, got, v.want[2:])
		}
	}
}

func TestURIIsWhatAnAuthenticatorScans(t *testing.T) {
	u, err := url.Parse(URI("dana", "JBSWY3DPEHPK3PXP"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" || u.Path != "/HotLoop Flow:dana" {
		t.Fatalf("uri = %s", u)
	}
	q := u.Query()
	if q.Get("secret") != "JBSWY3DPEHPK3PXP" || q.Get("issuer") != "HotLoop Flow" || q.Get("digits") != "6" || q.Get("period") != "30" {
		t.Fatalf("query = %v", q)
	}
	rows, err := QR(u.String())
	if err != nil || len(rows) < 21 || len(rows[0]) != len(rows) {
		t.Fatalf("QR: %v, %d rows", err, len(rows))
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func openStore(t *testing.T, path string, c *clock) *Store {
	t.Helper()
	s, err := Open(path, "a-secret-long-enough-to-count")
	if err != nil {
		t.Fatal(err)
	}
	s.SetClock(c.now)
	return s
}

func codeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	c, err := Code(secret, Step(at))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnrollVerifyAndReplay(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 3, 6, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "mfa.json")
	s := openStore(t, path, c)

	if s.Enabled("dana") {
		t.Fatal("enabled before anything happened")
	}
	secret, err := s.Begin("dana")
	if err != nil {
		t.Fatal(err)
	}
	// Half-finished is not on: nobody gets locked out by an abandoned setup.
	if s.Enabled("dana") {
		t.Fatal("enabled before it was confirmed")
	}
	if err := s.Confirm("dana", "000000"); !errors.Is(err, ErrBadCode) && codeAt(t, secret, c.t) != "000000" {
		t.Fatalf("a wrong confirmation code: %v", err)
	}
	if err := s.Confirm("dana", codeAt(t, secret, c.t)); err != nil {
		t.Fatal(err)
	}
	if !s.Enabled("dana") {
		t.Fatal("not enabled after confirming")
	}
	if _, err := s.Begin("dana"); !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("a second setup over a live one: %v", err)
	}

	// The confirmation code can't be used to sign in, and neither can
	// anything from that step.
	if err := s.Verify("dana", codeAt(t, secret, c.t)); !errors.Is(err, ErrBadCode) {
		t.Fatalf("the confirmation code signed in: %v", err)
	}

	c.t = c.t.Add(Period)
	good := codeAt(t, secret, c.t)
	if err := s.Verify("dana", good); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify("dana", good); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a code was accepted twice: %v", err)
	}

	// A phone thirty seconds slow still works; two minutes slow doesn't.
	c.t = c.t.Add(3 * Period)
	if err := s.Verify("dana", codeAt(t, secret, c.t.Add(-Period))); err != nil {
		t.Fatalf("one step of skew: %v", err)
	}
	c.t = c.t.Add(Period)
	if err := s.Verify("dana", codeAt(t, secret, c.t.Add(-4*Period))); !errors.Is(err, ErrBadCode) {
		t.Fatalf("four steps of skew: %v", err)
	}
	for _, junk := range []string{"", "12345", "1234567", "abcdef"} {
		if err := s.Verify("dana", junk); !errors.Is(err, ErrBadCode) {
			t.Errorf("%q: %v", junk, err)
		}
	}

	// It survives a restart, and the file on disk doesn't give the secret away.
	s2 := openStore(t, path, c)
	if !s2.Enabled("dana") {
		t.Fatal("enrollment lost on restart")
	}
	c.t = c.t.Add(Period)
	if err := s2.Verify("dana", codeAt(t, secret, c.t)); err != nil {
		t.Fatalf("after a restart: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "dana") {
		t.Fatal("the enrollment file holds the secret or the user in plaintext")
	}

	if err := s2.Disable("dana"); err != nil {
		t.Fatal(err)
	}
	if s2.Enabled("dana") {
		t.Fatal("still enabled after disabling")
	}
	if err := s2.Verify("dana", "123456"); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("verify after disabling: %v", err)
	}
}

// A replay can't sneak in through the skew window either: a code from a step
// before the last one used is refused even though it's within the window.
func TestAnOlderCodeAfterANewerOneIsRefused(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 3, 6, 0, 0, 0, time.UTC)}
	s := openStore(t, filepath.Join(t.TempDir(), "mfa.json"), c)
	secret, _ := s.Begin("dana")
	if err := s.Confirm("dana", codeAt(t, secret, c.t)); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(5 * Period)
	if err := s.Verify("dana", codeAt(t, secret, c.t.Add(Period))); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify("dana", codeAt(t, secret, c.t)); !errors.Is(err, ErrBadCode) {
		t.Fatalf("an older code after a newer one: %v", err)
	}
}
