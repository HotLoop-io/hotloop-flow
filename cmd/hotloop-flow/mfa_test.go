package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/audit"
	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/mfa"
)

// signIn posts a login with an optional code and returns the status, the
// token if there was one, and the body.
func (e *e2e) signIn(t *testing.T, user, pass, code string) (int, string, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass, "code": code})
	res, err := http.Post(e.URL+"/auth/token", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	tok, _ := out["access_token"].(string)
	return res.StatusCode, tok, out
}

// The whole life of a second factor, through the API, with the clock walked
// forward a step at a time so every code is a fresh one.
func TestTwoFactorSignIn(t *testing.T) {
	app, _ := newApp(t)
	e := serveApp(t, app, map[string][]string{"dana": {"*"}, "sam": {"flows.*"}})
	now := time.Date(2026, 10, 3, 6, 0, 0, 0, time.UTC)
	e.mfa.SetClock(func() time.Time { return now })
	tick := func() { now = now.Add(mfa.Period) }
	codeNow := func(secret string) string {
		c, err := mfa.Code(secret, mfa.Step(now))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	dana := e.login(t, "dana")
	code, out := e.call(t, "POST", "/auth/mfa/setup", dana, nil)
	if code != http.StatusOK {
		t.Fatalf("setup: %d %s", code, out)
	}
	var setup struct {
		Secret string   `json:"secret"`
		URI    string   `json:"uri"`
		QR     []string `json:"qr"`
	}
	if err := json.Unmarshal(out, &setup); err != nil || setup.Secret == "" || len(setup.QR) < 21 {
		t.Fatalf("setup answer: %v %s", err, out)
	}

	// Until it's confirmed, the password alone still signs in.
	if status, tok, _ := e.signIn(t, "dana", e2ePassword, ""); status != http.StatusOK || tok == "" {
		t.Fatalf("sign-in during an unconfirmed setup: %d", status)
	}
	if code, _ := e.call(t, "POST", "/auth/mfa/confirm", dana, []byte(`{"code":"000000"}`)); code != http.StatusBadRequest && codeNow(setup.Secret) != "000000" {
		t.Fatalf("a wrong confirmation code: %d", code)
	}
	if code, out := e.call(t, "POST", "/auth/mfa/confirm", dana, []byte(`{"code":"`+codeNow(setup.Secret)+`"}`)); code != http.StatusOK {
		t.Fatalf("confirm: %d %s", code, out)
	}

	// Now the password alone is not enough, and says what's missing.
	status, tok, body := e.signIn(t, "dana", e2ePassword, "")
	if status != http.StatusUnauthorized || tok != "" || body["mfa"] != "required" {
		t.Fatalf("password only: %d %v", status, body)
	}
	// A wrong password doesn't get as far as being asked for a code.
	if status, _, body := e.signIn(t, "dana", "wrong-password", ""); status != http.StatusUnauthorized || body["mfa"] != nil {
		t.Fatalf("a wrong password: %d %v", status, body)
	}
	if status, tok, _ := e.signIn(t, "dana", e2ePassword, "123456"); status != http.StatusUnauthorized || tok != "" {
		if codeNow(setup.Secret) != "123456" {
			t.Fatalf("a wrong code signed in: %d", status)
		}
	}

	tick()
	good := codeNow(setup.Secret)
	status, tok, _ = e.signIn(t, "dana", e2ePassword, good)
	if status != http.StatusOK || tok == "" {
		t.Fatalf("password and code: %d", status)
	}
	if status, tok, _ := e.signIn(t, "dana", e2ePassword, good); status != http.StatusUnauthorized || tok != "" {
		t.Fatalf("the same code twice: %d", status)
	}

	// Turning it off takes a code too.
	if code, _ := e.call(t, "POST", "/auth/mfa/disable", tok, []byte(`{"code":"`+good+`"}`)); code != http.StatusBadRequest {
		t.Fatalf("disable with a used code: %d", code)
	}
	tick()
	if code, out := e.call(t, "POST", "/auth/mfa/disable", tok, []byte(`{"code":"`+codeNow(setup.Secret)+`"}`)); code != http.StatusOK {
		t.Fatalf("disable: %d %s", code, out)
	}
	if status, tok, _ := e.signIn(t, "dana", e2ePassword, ""); status != http.StatusOK || tok == "" {
		t.Fatalf("after turning it off: %d", status)
	}

	var events []string
	for _, en := range e.auditTrail(t, tok, "?user=dana") {
		events = append(events, en.Event)
	}
	for _, want := range []string{audit.MFAEnabled, audit.MFAFailed, audit.MFADisabled} {
		found := false
		for _, ev := range events {
			if ev == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no %s in the audit trail: %v", want, events)
		}
	}
}

// A lost phone. An account with auth.admin turns somebody's second factor off;
// one without can't, and an API token can't touch anybody's.
func TestResettingSomebodyElsesSecondFactor(t *testing.T) {
	app, _ := newApp(t)
	tok, hash, _ := config.NewToken()
	app.cfg.Auth.Tokens = []config.Token{{Name: "ci", Hash: hash, Permissions: config.DeployTokenPermissions}}
	e := serveApp(t, app, map[string][]string{"dana": {"*"}, "sam": {"flows.*"}})
	now := time.Date(2026, 10, 3, 6, 0, 0, 0, time.UTC)
	e.mfa.SetClock(func() time.Time { return now })

	sam := e.login(t, "sam")
	_, out := e.call(t, "POST", "/auth/mfa/setup", sam, nil)
	var setup struct{ Secret string }
	_ = json.Unmarshal(out, &setup)
	c, _ := mfa.Code(setup.Secret, mfa.Step(now))
	if code, _ := e.call(t, "POST", "/auth/mfa/confirm", sam, []byte(`{"code":"`+c+`"}`)); code != http.StatusOK {
		t.Fatal("confirm failed")
	}

	if code, _ := e.call(t, "POST", "/auth/mfa/reset", sam, []byte(`{"username":"sam"}`)); code != http.StatusForbidden {
		t.Fatalf("reset without auth.admin: %d", code)
	}
	if code, _ := e.call(t, "POST", "/auth/mfa/setup", tok, nil); code != http.StatusUnauthorized {
		t.Fatalf("an API token set up two-factor: %d", code)
	}
	dana := e.login(t, "dana")
	if code, out := e.call(t, "POST", "/auth/mfa/reset", dana, []byte(`{"username":"sam"}`)); code != http.StatusOK {
		t.Fatalf("reset: %d %s", code, out)
	}
	if status, tok, _ := e.signIn(t, "sam", e2ePassword, ""); status != http.StatusOK || tok == "" {
		t.Fatalf("sam after the reset: %d", status)
	}
	resets := e.auditTrail(t, dana, "?event=mfa.reset")
	if len(resets) != 1 || resets[0].User != "dana" || resets[0].Detail["for"] != "sam" {
		t.Fatalf("the reset in the trail: %+v", resets)
	}
}
