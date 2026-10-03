// Package mfa is two-factor sign-in with time-based one-time codes (TOTP,
// RFC 6238): the six digits from an authenticator app on somebody's phone.
//
// Free, built in, and on for anybody who wants it. Nothing about signing in
// safely gets gated behind a tier, ever. An engine that can move equipment is
// the last place a stolen password should be enough.
//
// Enrollments live in their own file under the data directory, encrypted with
// the credential secret exactly the way node credentials are, so the file on a
// shared volume is useless to whoever copies it.
package mfa

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // RFC 6238's default, and the only one every authenticator app speaks.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/store"
	"rsc.io/qr"
)

// The parameters every authenticator app assumes when the URI doesn't say
// otherwise. Changing any of them breaks somebody's phone.
const (
	Period = 30 * time.Second
	Digits = 6
	// Skew is how many periods either side of now a code is accepted for, so
	// a phone thirty seconds off still signs in.
	Skew = 1
	// Issuer is the name an authenticator app shows above the code.
	Issuer = "HotLoop Flow"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret makes a 160-bit secret, base32 the way authenticator apps take it.
func NewSecret() (string, error) {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a TOTP secret: %w", err)
	}
	return b32.EncodeToString(b[:]), nil
}

// Code is the code for a secret at a time step.
func Code(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", fmt.Errorf("the TOTP secret is not base32: %w", err)
	}
	return code(key, step, Digits), nil
}

func code(key []byte, step int64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	// Dynamic truncation, RFC 4226 section 5.3.
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod)
}

// Step is the time step a moment falls in.
func Step(t time.Time) int64 { return t.Unix() / int64(Period/time.Second) }

// URI is the otpauth:// link an authenticator app scans.
func URI(user, secret string) string {
	label := url.PathEscape(Issuer + ":" + user)
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", Issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", strconv.Itoa(Digits))
	v.Set("period", strconv.Itoa(int(Period/time.Second)))
	return "otpauth://totp/" + label + "?" + v.Encode()
}

// QR renders a URI as a QR code, one string per row, '1' for a dark module.
// The editor draws it as SVG rectangles: no image to fetch and no data: URL for
// a strict proxy's content policy to refuse.
func QR(uri string) ([]string, error) {
	c, err := qr.Encode(uri, qr.M)
	if err != nil {
		return nil, err
	}
	rows := make([]string, c.Size)
	for y := 0; y < c.Size; y++ {
		var b strings.Builder
		for x := 0; x < c.Size; x++ {
			if c.Black(x, y) {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
		rows[y] = b.String()
	}
	return rows, nil
}

// Errors.
var (
	ErrNotEnrolled     = errors.New("two-factor sign-in is not set up for this user")
	ErrAlreadyEnrolled = errors.New("two-factor sign-in is already on for this user; turn it off first to set it up again")
	ErrNoPending       = errors.New("there is no two-factor setup waiting to be confirmed; start it again")
	ErrBadCode         = errors.New("that code is wrong, expired or already used")
)

// Store holds every user's enrollment, encrypted at rest.
type Store struct {
	// The credential store already does exactly the encryption this needs,
	// so it does it here too: one construction, reviewed once.
	file *store.CredentialStore
	now  func() time.Time

	mu sync.Mutex
}

// Open reads the enrollments at path, encrypted with secret.
func Open(path, secret string) (*Store, error) {
	f := store.NewCredentialStore(path, secret)
	if err := f.Load(); err != nil {
		return nil, fmt.Errorf("reading two-factor enrollments: %w", err)
	}
	return &Store{file: f, now: time.Now}, nil
}

// SetClock replaces the clock, for tests that need to walk through time steps.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

const (
	keySecret  = "secret"
	keyPending = "pending"
	keyLast    = "lastStep"
	keySince   = "since"
)

// Enabled reports whether a user has two-factor sign-in on.
func (s *Store) Enabled(user string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Get(user)[keySecret] != ""
}

// Begin starts enrolling a user and returns the new secret. Nothing is required
// at sign-in until Confirm proves the phone has it, so a setup abandoned half
// way can't lock anybody out.
func (s *Store) Begin(user string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.file.Get(user)
	if cur[keySecret] != "" {
		return "", ErrAlreadyEnrolled
	}
	secret, err := NewSecret()
	if err != nil {
		return "", err
	}
	s.file.Set(user, map[string]string{keyPending: secret})
	return secret, s.file.Save()
}

// Confirm finishes enrolling with a code from the phone.
func (s *Store) Confirm(user, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.file.Get(user)
	pending := cur[keyPending]
	if pending == "" {
		return ErrNoPending
	}
	step, ok := match(pending, code, s.now(), 0)
	if !ok {
		return ErrBadCode
	}
	s.file.Set(user, map[string]string{
		keySecret: pending,
		keyLast:   strconv.FormatInt(step, 10),
		keySince:  s.now().UTC().Format(time.RFC3339),
	})
	return s.file.Save()
}

// Verify checks a sign-in code. A code is good once: the step it matched is
// remembered and no code from that step or earlier works again, so a code read
// over somebody's shoulder is worthless the moment they've used it.
func (s *Store) Verify(user, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.file.Get(user)
	secret := cur[keySecret]
	if secret == "" {
		return ErrNotEnrolled
	}
	last, _ := strconv.ParseInt(cur[keyLast], 10, 64)
	step, ok := match(secret, code, s.now(), last)
	if !ok {
		return ErrBadCode
	}
	cur[keyLast] = strconv.FormatInt(step, 10)
	s.file.Set(user, cur)
	return s.file.Save()
}

// Disable turns two-factor sign-in off for a user.
func (s *Store) Disable(user string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file.Get(user) == nil {
		return ErrNotEnrolled
	}
	s.file.Set(user, nil)
	return s.file.Save()
}

// match finds the step within the skew that a code belongs to and that is
// newer than last. Every candidate is compared, in constant time, whether or
// not an earlier one matched.
func match(secret, given string, now time.Time, last int64) (int64, bool) {
	given = strings.ReplaceAll(strings.TrimSpace(given), " ", "")
	if len(given) != Digits {
		return 0, false
	}
	center := Step(now)
	var found int64
	ok := false
	for step := center - Skew; step <= center+Skew; step++ {
		want, err := Code(secret, step)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(given)) == 1 && step > last && !ok {
			found, ok = step, true
		}
	}
	return found, ok
}
