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

// withAdmin gives a test a valid user, so the only refusal left is the one the
// test is about.
func withAdmin(t *testing.T) {
	t.Helper()
	t.Setenv("HOTLOOP_FLOW_ADMIN_USER", "admin")
	t.Setenv("HOTLOOP_FLOW_ADMIN_PASSWORD_HASH", testHash)
}

// The README's table of startup refusals, one row at a time. Each one is a
// promise that a dangerous setup stops at boot with the fix printed, so each
// one gets a test that it actually does.

func TestRefusesAPlaintextPasswordHash(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOTLOOP_FLOW_ADMIN_USER", "admin")
	t.Setenv("HOTLOOP_FLOW_ADMIN_PASSWORD_HASH", "hunter2-in-a-configmap")
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "not a bcrypt hash") ||
		!strings.Contains(err.Error(), "hotloop-flow hash-password") {
		t.Fatalf("err = %v, want the plaintext refused with the command that fixes it", err)
	}
}

func TestRefusesAUserWithNoHash(t *testing.T) {
	clearEnv(t)
	_, err := Load(writeConfig(t, "auth:\n  users:\n    - username: dana\n"))
	if err == nil || !strings.Contains(err.Error(), "no passwordHash") {
		t.Fatalf("err = %v, want a user with no hash refused", err)
	}
}

func TestRefusesNoCredentialSecret(t *testing.T) {
	clearEnv(t)
	withAdmin(t)
	t.Setenv("HOTLOOP_FLOW_CREDENTIAL_SECRET", "")
	_, err := Load("")
	wantInsecure(t, err, "HOTLOOP_FLOW_CREDENTIAL_SECRET")
}

// The explicit way out of the refusal above, for an instance with no secrets.
func TestPlaintextCredentialsNeedTheirOwnOptIn(t *testing.T) {
	clearEnv(t)
	withAdmin(t)
	t.Setenv("HOTLOOP_FLOW_CREDENTIAL_SECRET", "")
	t.Setenv("HOTLOOP_FLOW_ALLOW_PLAINTEXT_CREDENTIALS", "0")
	if _, err := Load(""); err == nil {
		t.Fatal("HOTLOOP_FLOW_ALLOW_PLAINTEXT_CREDENTIALS=0 allowed plaintext credentials")
	}
	t.Setenv("HOTLOOP_FLOW_ALLOW_PLAINTEXT_CREDENTIALS", "true")
	if _, err := Load(""); err != nil {
		t.Fatalf("with the opt-in: %v", err)
	}
}

func TestRefusesDiscoveryWithNoAllowlist(t *testing.T) {
	clearEnv(t)
	withAdmin(t)
	t.Setenv("HOTLOOP_FLOW_DISCOVERY_ENABLED", "true")
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "discovery.allowedCIDRs is empty") {
		t.Fatalf("err = %v, want discovery with an empty allowlist refused", err)
	}
	t.Setenv("HOTLOOP_FLOW_DISCOVERY_CIDRS", "10.20.0.0/24, 10.21.0.0/24")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("with an allowlist: %v", err)
	}
	if len(cfg.Discovery.AllowedCIDRs) != 2 {
		t.Fatalf("AllowedCIDRs = %v, want two", cfg.Discovery.AllowedCIDRs)
	}
}

func TestRefusesExecWithNoAllowlist(t *testing.T) {
	clearEnv(t)
	withAdmin(t)
	_, err := Load(writeConfig(t, "exec:\n  enabled: true\n"))
	if err == nil || !strings.Contains(err.Error(), "exec.allowedCommands is empty") {
		t.Fatalf("err = %v, want exec with an empty allowlist refused", err)
	}
	if _, err := Load(writeConfig(t, "exec:\n  enabled: true\n  allowedCommands: [ping]\n")); err != nil {
		t.Fatalf("with an allowlist: %v", err)
	}
}

// A typo in the file is a startup failure, not a setting that quietly does
// nothing. This is the one that would otherwise leave credentials in
// plaintext for six months.
func TestRefusesAMisspelledField(t *testing.T) {
	clearEnv(t)
	withAdmin(t)
	_, err := Load(writeConfig(t, "data:\n  credentialSecert: oops\n"))
	if err == nil || !strings.Contains(err.Error(), "credentialSecert") {
		t.Fatalf("err = %v, want the misspelled field named", err)
	}
}

// The plain config errors the README says it also refuses on.
func TestRefusesOutOfRangeValues(t *testing.T) {
	cases := map[string]string{
		"server:\n  port: 70000\n":            "not a valid port",
		"server:\n  adminRoot: editor\n":      "must start with /",
		"server:\n  httpRoot: api\n":          "must start with /",
		"runtime:\n  overflow: drop-all\n":    "is not one of block",
		"runtime:\n  inboxCapacity: 0\n":      "at least 1",
		"logging:\n  level: loud\n":           "logging.level",
		"logging:\n  format: xml\n":           "logging.format",
		"data:\n  backupGenerations: -1\n":    "must not be negative",
		"data:\n  dir: \"\"\n":                "data.dir is empty",
		"server:\n  port: [not, a, number]\n": "parsing",
	}
	for body, want := range cases {
		t.Run(strings.TrimSpace(body), func(t *testing.T) {
			clearEnv(t)
			withAdmin(t)
			_, err := Load(writeConfig(t, body))
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want it to mention %q", err, want)
			}
		})
	}
}

// Environment beats the file, which is what lets a chart keep secrets in a
// Secret and everything else in a ConfigMap.
func TestEnvironmentOverridesTheFile(t *testing.T) {
	clearEnv(t)
	withAdmin(t)
	t.Setenv("HOTLOOP_FLOW_PORT", "1990")
	t.Setenv("HOTLOOP_FLOW_OVERFLOW", "drop-oldest")
	cfg, err := Load(writeConfig(t, "server:\n  port: 1881\nruntime:\n  overflow: block\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 1990 || cfg.Runtime.Overflow != "drop-oldest" {
		t.Fatalf("port %d overflow %s, want the environment's 1990 and drop-oldest",
			cfg.Server.Port, cfg.Runtime.Overflow)
	}
}

func TestPrefixGrantsCoverExactlyTheirPrefix(t *testing.T) {
	u := User{Permissions: []string{"flows.*", "status.read"}}
	for perm, want := range map[string]bool{
		"flows.read":    true,
		"flows.write":   true,
		"status.read":   true,
		"status.write":  false,
		"inject.write":  false,
		"flowsx.read":   false,
		"settings.read": false,
	} {
		if got := u.Allows(perm); got != want {
			t.Errorf("Allows(%q) = %v, want %v", perm, got, want)
		}
	}
	if !(User{Permissions: []string{"*"}}).Allows("anything.at.all") {
		t.Error(`"*" does not grant everything`)
	}
}

func TestHashPasswordRefusesShortPasswords(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("hashed a five-character password")
	}
	h, err := HashPassword("long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "long-enough-password") || CheckPassword(h, "long-enough-passwore") {
		t.Fatal("the hash does not check the password it was made from, and only that one")
	}
}
