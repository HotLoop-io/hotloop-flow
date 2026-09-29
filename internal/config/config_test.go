package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A well-formed bcrypt hash, so Validate's bcrypt.Cost check passes.
const testHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

// clearEnv blanks every variable Load reads, so a developer's shell can't
// decide a test's outcome.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "HOTLOOP_FLOW_") {
			t.Setenv(k, "")
		}
	}
	// Every test starts from a valid credential setup, so the only thing it
	// can refuse is whatever the test is about.
	t.Setenv("HOTLOOP_FLOW_CREDENTIAL_SECRET", "a-secret-long-enough-to-count")
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func wantInsecure(t *testing.T, err error, mention string) {
	t.Helper()
	var insecure *ErrInsecure
	if !errors.As(err, &insecure) {
		t.Fatalf("err = %v, want an ErrInsecure", err)
	}
	if !strings.Contains(insecure.Error(), mention) {
		t.Fatalf("refusal %q does not mention %q", insecure.Error(), mention)
	}
}

// The bug this file exists for: the refusal told you to set
// HOTLOOP_FLOW_INSECURE=true, and setting it got you the same refusal back,
// from 0.1.0 until this test.
func TestInsecureEnvStartsWithoutAuthOrUsers(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOTLOOP_FLOW_INSECURE", "true")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with HOTLOOP_FLOW_INSECURE=true: %v", err)
	}
	if cfg.Auth.Enabled {
		t.Error("Auth.Enabled = true, want false")
	}
	if !cfg.Auth.Insecure {
		t.Error("Auth.Insecure = false, want true")
	}
}

func TestInsecureOnlyAcceptsExplicitAffirmatives(t *testing.T) {
	for _, v := range []string{"false", "0", "no", "off", "", "maybe"} {
		t.Run(v, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("HOTLOOP_FLOW_INSECURE", v)
			_, err := Load("")
			// Auth stays on, so with no users configured it refuses on users,
			// not on auth being off.
			wantInsecure(t, err, "no users are configured")
		})
	}
}

// A file can turn auth off but can't approve it. The ConfigMap is the wrong
// place for that decision, because it is the place a copy-paste lands.
func TestFileAloneCannotDisableAuth(t *testing.T) {
	clearEnv(t)
	_, err := Load(writeConfig(t, "auth:\n  enabled: false\n"))
	wantInsecure(t, err, "authentication is disabled")
	if !strings.Contains(err.Error(), "HOTLOOP_FLOW_INSECURE=true") {
		t.Errorf("refusal %q does not name the way out", err)
	}
}

func TestFileCannotSetTheOptOut(t *testing.T) {
	clearEnv(t)
	_, err := Load(writeConfig(t, "auth:\n  enabled: false\n  insecure: true\n"))
	if err == nil {
		t.Fatal("a config file set auth.insecure and Load accepted it")
	}
	if !strings.Contains(err.Error(), "insecure") {
		t.Errorf("err = %v, want it to name the unknown field", err)
	}
}

func TestFileDisabledPlusEnvStarts(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOTLOOP_FLOW_INSECURE", "1")
	if _, err := Load(writeConfig(t, "auth:\n  enabled: false\n")); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// Turning auth off is not a blanket waiver. Plaintext credentials still need
// their own opt-out.
func TestInsecureDoesNotWaiveTheCredentialSecret(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOTLOOP_FLOW_CREDENTIAL_SECRET", "")
	t.Setenv("HOTLOOP_FLOW_INSECURE", "true")
	_, err := Load("")
	wantInsecure(t, err, "no credential secret")
}

// A user configured alongside the opt-out is still checked, so a plaintext
// password in the file is still caught the day auth goes back on.
func TestInsecureStillChecksConfiguredUsers(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOTLOOP_FLOW_INSECURE", "true")
	_, err := Load(writeConfig(t, "auth:\n  users:\n    - username: dana\n      passwordHash: hunter2\n"))
	if err == nil || !strings.Contains(err.Error(), "not a bcrypt hash") {
		t.Fatalf("err = %v, want the plaintext password refused", err)
	}
}

func TestAuthOnWithAUserStarts(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOTLOOP_FLOW_ADMIN_USER", "admin")
	t.Setenv("HOTLOOP_FLOW_ADMIN_PASSWORD_HASH", testHash)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Auth.Enabled || cfg.Auth.Insecure {
		t.Errorf("Auth = %+v, want enabled and not insecure", cfg.Auth)
	}
}
