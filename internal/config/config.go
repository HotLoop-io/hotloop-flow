// Package config loads HotLoop Flow's runtime configuration.
//
// Node-RED's settings.js is executable JavaScript: adminAuth can be a function,
// https can be a function returning cert options, storageModule is a require().
// That makes it impossible to validate, diff, template from a ConfigMap, or
// reason about without running it. HotLoop Flow's configuration is declarative
// YAML with environment overrides, which is what a Helm chart can actually
// produce and what an operator can actually review.
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// Config is the whole configuration surface.
type Config struct {
	Server    Server    `yaml:"server"`
	Data      Data      `yaml:"data"`
	Auth      Auth      `yaml:"auth"`
	Runtime   Runtime   `yaml:"runtime"`
	Discovery Discovery `yaml:"discovery"`
	Exec      Exec      `yaml:"exec"`
	Files     Files     `yaml:"files"`
	Secrets   Secrets   `yaml:"secrets"`
	Logging   Logging   `yaml:"logging"`
	Metrics   Metrics   `yaml:"metrics"`
	History   History   `yaml:"history"`
	Audit     Audit     `yaml:"audit"`
	Tests     Tests     `yaml:"tests"`
}

// Tests is the flow test suite this instance keeps beside its flow file, and
// whether a deploy has to pass it.
type Tests struct {
	// Gate refuses a deploy whose flows fail the suite, with the failing
	// assertions, before anything is written or stopped. Off by default: a
	// gate an operator didn't ask for is a deploy that fails at 3 AM for a
	// reason nobody on shift has heard of.
	Gate bool `yaml:"gate"`

	// Timeout bounds one run of the suite on the wall clock. The tests keep
	// their own clock, so this only bites a flow that's genuinely stuck.
	Timeout time.Duration `yaml:"timeout"`
}

// Audit bounds the audit trail at data.dir/audit.log.
type Audit struct {
	// MaxBytes is the size at which the file rotates.
	MaxBytes int64 `yaml:"maxBytes"`
	// Keep is how many rotated files are kept beside the live one.
	Keep int `yaml:"keep"`
}

// Server is the HTTP listener.
type Server struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`

	// AdminRoot is the path prefix the editor and admin API are served under.
	// The App Store proxies apps by path, so this has to be settable.
	AdminRoot string `yaml:"adminRoot"`

	// HTTPRoot is the prefix a flow's HTTP In nodes are served under, matching
	// Node-RED's httpNodeRoot. It may be the same as AdminRoot — the fixed admin
	// routes are more specific and still win — and a flow that tries to claim a
	// path belonging to the editor or the API is refused at deploy time.
	HTTPRoot string `yaml:"httpRoot"`

	ReadTimeout     time.Duration `yaml:"readTimeout"`
	WriteTimeout    time.Duration `yaml:"writeTimeout"`
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout"`

	// MaxRequestBytes bounds a flow deploy. A flow file is the largest thing
	// the API accepts and an unbounded body is a trivial way to OOM an edge box.
	MaxRequestBytes int64 `yaml:"maxRequestBytes"`
}

// Data is where state lives. All of it under one directory, which is the PVC.
type Data struct {
	Dir             string `yaml:"dir"`
	FlowFile        string `yaml:"flowFile"`
	CredentialsFile string `yaml:"credentialsFile"`

	// CredentialSecret encrypts the credential store. Leaving it empty writes
	// credentials in plaintext, which is refused unless AllowPlaintextCredentials
	// is set — see Validate.
	CredentialSecret          string `yaml:"credentialSecret"`
	AllowPlaintextCredentials bool   `yaml:"allowPlaintextCredentials"`
	BackupGenerations         int    `yaml:"backupGenerations"`
}

// Auth controls access to the editor and admin API.
type Auth struct {
	// Enabled defaults to true. Starting without it requires an explicit
	// opt-out, because Node-RED ships unauthenticated by default and vendors
	// ship that default: CVE-2025-41656 (CERT@VDE VDE-2025-045) is Pilz's
	// IndustrialPI 4 running Node-RED with no authentication, so anyone who
	// could reach it could run commands on the device. That is Pilz's CVE, not
	// Node-RED's. Node-RED's own team proposed refusing to start without auth
	// in designs#81 and has not shipped it.
	Enabled bool `yaml:"enabled"`

	// Insecure is the explicit opt-out: set only by HOTLOOP_FLOW_INSECURE=true,
	// never by the file, so a ConfigMap that says enabled: false still refuses
	// to start until somebody makes that decision on purpose. Until this field
	// existed Validate could not tell a deliberate opt-out from an accident,
	// refused both, and the variable its own refusal told you to set did
	// nothing.
	Insecure bool `yaml:"-"`

	Users []User `yaml:"users"`

	// Tokens are API tokens for machines: a CI pipeline deploying flows from
	// git, a script exporting them. A pipeline that holds an admin password
	// holds the keys to everything that password opens, and it ends up in a CI
	// secret store, a log line and three people's shell history. A token is
	// scoped to what the job needs and can be thrown away on its own.
	Tokens []Token `yaml:"tokens"`

	// SessionTTL bounds how long an issued token is good for.
	SessionTTL time.Duration `yaml:"sessionTTL"`

	// Lockout slows password guessing down to something useless.
	Lockout Lockout `yaml:"lockout"`
}

// Lockout bounds failed sign-ins.
type Lockout struct {
	// Attempts is how many failures one account may have from one address
	// within Window before it is locked out from that address for Duration.
	Attempts int `yaml:"attempts"`
	// PerAddress is how many failures one address may have within Window,
	// whatever account it tried, before the address is locked out.
	PerAddress int           `yaml:"perAddress"`
	Window     time.Duration `yaml:"window"`
	Duration   time.Duration `yaml:"duration"`
}

// Token is an API token. Only its hash is configured, like a password, so the
// file that grants it never holds the token itself.
type Token struct {
	// Name is what the deployment log and the audit trail call it, as
	// "token:<name>".
	Name string `yaml:"name"`
	// Hash is "sha256:" and the hex SHA-256 of the token. A token is 32 random
	// bytes, so a fast hash is the right one here: there is no weak secret for
	// a slow hash to protect. Generate both with: hotloop-flow token
	Hash string `yaml:"hash"`
	// Permissions, as for a user. A deploy token is ["flows.read", "flows.write"].
	Permissions []string `yaml:"permissions"`
}

// DeployTokenName is the token HOTLOOP_FLOW_DEPLOY_TOKEN_HASH configures.
const DeployTokenName = "deploy"

// DeployTokenPermissions are what a deploy token can do: read the flows to
// diff against, and deploy. Not inject, not settings, not the deployment log's
// rollback either (that needs flows.write, which it has, and is exactly the
// thing a pipeline reverting a merge should be able to do).
var DeployTokenPermissions = []string{"flows.read", "flows.write"}

// TokenPrefix marks a Flow API token, so one pasted into the wrong place is
// recognisable for what it is.
const TokenPrefix = "hlf_"

// NewToken makes an API token and the hash to configure for it.
func NewToken() (token, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("generating a token: %w", err)
	}
	token = TokenPrefix + hex.EncodeToString(b[:])
	return token, HashToken(token), nil
}

// HashToken is the configured form of a token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// FindToken returns the configured token a bearer token belongs to. Every
// configured hash is compared, in constant time, whether or not an earlier one
// matched, so the time it takes says nothing about which token came close.
func (c *Config) FindToken(token string) (Token, bool) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return Token{}, false
	}
	want := []byte(HashToken(token))
	var found Token
	ok := false
	for _, t := range c.Auth.Tokens {
		if subtle.ConstantTimeCompare([]byte(t.Hash), want) == 1 {
			found, ok = t, true
		}
	}
	return found, ok
}

// User is a local account.
type User struct {
	Username string `yaml:"username"`
	// PasswordHash is a bcrypt hash. Plaintext passwords are refused, so a
	// ConfigMap can never contain one by accident.
	PasswordHash string `yaml:"passwordHash"`
	// Permissions is "*" for full access, or a list such as ["flows.read"].
	Permissions []string `yaml:"permissions"`
}

// Runtime tunes the scheduler.
type Runtime struct {
	InboxCapacity int           `yaml:"inboxCapacity"`
	Overflow      string        `yaml:"overflow"`
	BlockTimeout  time.Duration `yaml:"blockTimeout"`
	CloseTimeout  time.Duration `yaml:"closeTimeout"`
}

// Discovery gates the network discovery nodes.
type Discovery struct {
	Enabled bool `yaml:"enabled"`
	// AllowedCIDRs bounds what the scan nodes may probe. This is a plant-floor
	// inventory tool, not a scanner somebody can aim anywhere from an edit
	// dialog, so an empty list with discovery enabled is a configuration error
	// rather than "scan everything".
	AllowedCIDRs []string `yaml:"allowedCIDRs"`
}

// Exec gates the exec node.
//
// Off by default, and an enabled node with no allowed commands is a
// configuration error rather than "anything goes". In Node-RED nothing sits
// between "can edit a flow" and "can run any command": the exec node runs
// whatever it is given, through a shell. Put that behind a default with no
// authentication and you get CVE-2025-41656, Pilz shipping Node-RED on a
// device where anyone on the network could run commands. This is the thing
// that sits in between.
type Exec struct {
	Enabled bool `yaml:"enabled"`
	// AllowedCommands lists what a flow may run. Entries are matched on the
	// resolved absolute path, so naming "curl" allows the curl on the PATH at
	// the time and not whatever a later mount puts in front of it.
	AllowedCommands []string `yaml:"allowedCommands"`
}

// Files bounds where the file nodes may read and write.
//
// Unlike the exec node this is on by default, scoped to the data directory. A
// flow reading and writing under its own PVC is the ordinary case; reaching
// outside it is not, and Node-RED's file nodes taking any path is what makes
// "can edit a flow" mean "can read any file this process can".
type Files struct {
	// AllowedPaths are extra directory trees on top of the data directory.
	AllowedPaths []string `yaml:"allowedPaths"`
}

// Secrets bounds where a node may read a secret from a file: a TLS
// certificate, key or CA, or a password kept in a file.
//
// Separate from Files because the two are read for different reasons. A file
// node puts what it reads into a message, which a flow can send anywhere. A
// secret goes into a connection and nowhere else. Mount a Kubernetes Secret
// here and the TLS config can use the key without any flow being able to
// read it.
type Secrets struct {
	// AllowedPaths are extra directory trees on top of the data directory.
	AllowedPaths []string `yaml:"allowedPaths"`
}

// History controls the deployment log under data.dir/deployments.
type History struct {
	// Retain is how many deployment records are kept. Zero keeps every one,
	// which is a decision about disk space, not about safety: each record is a
	// copy of the flow file plus the encrypted credentials.
	Retain int `yaml:"retain"`

	// Git mirrors every deployment to a repository. Off unless a URL is set.
	Git GitMirror `yaml:"git"`
}

// GitMirror pushes each deployment to a git repository as a commit authored by
// the deployer.
type GitMirror struct {
	// URL is the repository, http:// or https://. Empty turns the mirror off.
	URL string `yaml:"url"`
	// Branch Flow commits to, and nothing else should. Defaults to main.
	Branch string `yaml:"branch"`
	// Path of the flow file in the repository. Defaults to flows.json.
	Path string `yaml:"path"`
	// EmailDomain makes each deployer's commit email: dana@<domain>.
	// Defaults to flow.invalid, which is reserved and can never reach anybody.
	EmailDomain string `yaml:"emailDomain"`
	// Username for HTTP basic auth. Not a secret, so the file may carry it.
	Username string `yaml:"username"`
	// Password is the access token. Environment only, from a Secret, never
	// the file: HOTLOOP_FLOW_GIT_PASSWORD.
	Password string `yaml:"-"`
}

// Logging controls the runtime log.
type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"` // text or json
}

// Metrics controls the Prometheus endpoint.
type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// Default returns the configuration used when nothing is specified.
func Default() Config {
	return Config{
		Server: Server{
			Host:      "0.0.0.0",
			Port:      1880,
			AdminRoot: "/",
			HTTPRoot:  "/",
			// Generous read timeout: a deploy of a large flow over a slow edge
			// link is legitimate. Write timeout is zero because the comms
			// websocket is long-lived and a deadline would cut it.
			ReadTimeout:     60 * time.Second,
			WriteTimeout:    0,
			ShutdownTimeout: 20 * time.Second,
			MaxRequestBytes: 32 << 20, // 32 MiB
		},
		Data: Data{
			Dir:               "/data",
			FlowFile:          "flows.json",
			CredentialsFile:   "credentials.json",
			BackupGenerations: 3,
		},
		Auth: Auth{
			Enabled:    true,
			SessionTTL: 7 * 24 * time.Hour,
			Lockout: Lockout{
				Attempts: 5, PerAddress: 20,
				Window: 15 * time.Minute, Duration: 15 * time.Minute,
			},
		},
		Runtime: Runtime{
			InboxCapacity: 1024,
			Overflow:      "block",
			BlockTimeout:  30 * time.Second,
			CloseTimeout:  15 * time.Second,
		},
		Logging: Logging{Level: "info", Format: "text"},
		Metrics: Metrics{Enabled: true, Path: "/metrics"},
		History: History{Retain: 100},
		Audit:   Audit{MaxBytes: 16 << 20, Keep: 4},
		Tests:   Tests{Timeout: 2 * time.Minute},
	}
}

// Load reads a configuration file, applies environment overrides and validates
// the result. An empty path uses the defaults plus the environment.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("reading %s: %w", path, err)
		}
		// KnownFields makes a typo in a ConfigMap a startup failure instead of
		// a setting that silently does nothing. An operator who misspells
		// "credentialSecret" should find out immediately, not when they
		// discover the credential file is plaintext.
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parsing %s: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// applyEnv overlays HOTLOOP_FLOW_* environment variables.
//
// Environment beats file so that a Helm chart can put non-secret settings in a
// ConfigMap and inject the secret ones from a Secret, which is the only sane
// split in Kubernetes.
func applyEnv(cfg *Config) {
	envStr("HOTLOOP_FLOW_HOST", &cfg.Server.Host)
	envInt("HOTLOOP_FLOW_PORT", &cfg.Server.Port)
	envStr("HOTLOOP_FLOW_ADMIN_ROOT", &cfg.Server.AdminRoot)
	envStr("HOTLOOP_FLOW_HTTP_ROOT", &cfg.Server.HTTPRoot)

	envStr("HOTLOOP_FLOW_DATA_DIR", &cfg.Data.Dir)
	envStr("HOTLOOP_FLOW_FLOW_FILE", &cfg.Data.FlowFile)
	envStr("HOTLOOP_FLOW_CREDENTIAL_SECRET", &cfg.Data.CredentialSecret)

	envInt("HOTLOOP_FLOW_INBOX_CAPACITY", &cfg.Runtime.InboxCapacity)
	envStr("HOTLOOP_FLOW_OVERFLOW", &cfg.Runtime.Overflow)

	envStr("HOTLOOP_FLOW_LOG_LEVEL", &cfg.Logging.Level)
	envStr("HOTLOOP_FLOW_LOG_FORMAT", &cfg.Logging.Format)

	// A single admin account can be supplied entirely from the environment,
	// which is what makes a first-run container usable without mounting a file.
	user := os.Getenv("HOTLOOP_FLOW_ADMIN_USER")
	hash := os.Getenv("HOTLOOP_FLOW_ADMIN_PASSWORD_HASH")
	if user != "" && hash != "" {
		cfg.Auth.Users = append(cfg.Auth.Users, User{
			Username:     user,
			PasswordHash: hash,
			Permissions:  []string{"*"},
		})
	}

	envStr("HOTLOOP_FLOW_GIT_URL", &cfg.History.Git.URL)
	envStr("HOTLOOP_FLOW_GIT_USERNAME", &cfg.History.Git.Username)
	envStr("HOTLOOP_FLOW_GIT_PASSWORD", &cfg.History.Git.Password)

	// A deploy token for CI, from a Secret, without a config file.
	if h := os.Getenv("HOTLOOP_FLOW_DEPLOY_TOKEN_HASH"); h != "" {
		cfg.Auth.Tokens = append(cfg.Auth.Tokens, Token{
			Name: DeployTokenName, Hash: h, Permissions: append([]string(nil), DeployTokenPermissions...),
		})
	}

	if envBool("HOTLOOP_FLOW_TESTS_GATE") {
		cfg.Tests.Gate = true
	}
	if envBool("HOTLOOP_FLOW_INSECURE") {
		cfg.Auth.Enabled = false
		cfg.Auth.Insecure = true
	}
	if envBool("HOTLOOP_FLOW_ALLOW_PLAINTEXT_CREDENTIALS") {
		cfg.Data.AllowPlaintextCredentials = true
	}
	if envBool("HOTLOOP_FLOW_DISCOVERY_ENABLED") {
		cfg.Discovery.Enabled = true
	}
	if v := os.Getenv("HOTLOOP_FLOW_DISCOVERY_CIDRS"); v != "" {
		cfg.Discovery.AllowedCIDRs = splitList(v)
	}
	if envBool("HOTLOOP_FLOW_EXEC_ENABLED") {
		cfg.Exec.Enabled = true
	}
	if v := os.Getenv("HOTLOOP_FLOW_EXEC_ALLOWED_COMMANDS"); v != "" {
		cfg.Exec.AllowedCommands = splitList(v)
	}
	if v := os.Getenv("HOTLOOP_FLOW_FILE_ALLOWED_PATHS"); v != "" {
		cfg.Files.AllowedPaths = splitList(v)
	}
	if v := os.Getenv("HOTLOOP_FLOW_SECRET_ALLOWED_PATHS"); v != "" {
		cfg.Secrets.AllowedPaths = splitList(v)
	}
}

func envStr(key string, dst *string) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		*dst = v
	}
}

func envInt(key string, dst *int) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

// envBool treats only the explicit affirmatives as true, so HOTLOOP_FLOW_INSECURE=0
// or =false does not disable authentication by accident.
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ErrInsecure is returned when authentication is disabled without the explicit
// opt-out. It is a distinct type so main can print the remedy rather than a
// bare error.
type ErrInsecure struct{ Reason string }

func (e *ErrInsecure) Error() string {
	return "refusing to start: " + e.Reason
}

// Validate checks the configuration and refuses the dangerous combinations.
func (c *Config) Validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d is not a valid port", c.Server.Port)
	}
	if !strings.HasPrefix(c.Server.AdminRoot, "/") {
		return fmt.Errorf("server.adminRoot %q must start with /", c.Server.AdminRoot)
	}
	if !strings.HasPrefix(c.Server.HTTPRoot, "/") {
		return fmt.Errorf("server.httpRoot %q must start with /", c.Server.HTTPRoot)
	}
	if c.Data.Dir == "" {
		return fmt.Errorf("data.dir is empty")
	}
	if c.Data.BackupGenerations < 0 {
		return fmt.Errorf("data.backupGenerations must not be negative")
	}
	if lo := c.Auth.Lockout; lo.Attempts < 1 || lo.PerAddress < lo.Attempts || lo.Window <= 0 || lo.Duration <= 0 {
		return fmt.Errorf("auth.lockout needs attempts of at least 1, perAddress of at least attempts, " +
			"and a window and duration above zero; there is no setting that turns it off")
	}
	if c.Audit.MaxBytes < 4096 {
		return fmt.Errorf("audit.maxBytes must be at least 4096; a trail that rotates on every line is no trail")
	}
	if c.Audit.Keep < 1 {
		return fmt.Errorf("audit.keep must be at least 1")
	}
	if c.History.Retain < 0 {
		return fmt.Errorf("history.retain must not be negative; 0 keeps every deployment")
	}
	if c.Tests.Timeout <= 0 {
		return fmt.Errorf("tests.timeout must be above zero; it's how long a run of the flow tests may take")
	}

	switch c.Runtime.Overflow {
	case "block", "drop-newest", "drop-oldest", "error":
	default:
		return fmt.Errorf("runtime.overflow %q is not one of block, drop-newest, drop-oldest, error",
			c.Runtime.Overflow)
	}
	if c.Runtime.InboxCapacity < 1 {
		return fmt.Errorf("runtime.inboxCapacity must be at least 1")
	}

	switch c.Logging.Level {
	case "error", "warn", "info", "debug", "trace":
	default:
		return fmt.Errorf("logging.level %q is not one of error, warn, info, debug, trace", c.Logging.Level)
	}
	switch c.Logging.Format {
	case "text", "json":
	default:
		return fmt.Errorf("logging.format %q is not text or json", c.Logging.Format)
	}

	// Authentication. This is the check that exists because Node-RED does not
	// have it: CVE-2025-41656 is a vendor shipping Node-RED at that default,
	// and anyone who could reach the device could run commands on it.
	if !c.Auth.Enabled && !c.Auth.Insecure {
		return &ErrInsecure{Reason: "authentication is disabled. " +
			"Anyone who can reach this port can deploy a flow, and a flow can run commands. " +
			"Set HOTLOOP_FLOW_INSECURE=true to override this on a trusted, isolated network."}
	}
	if c.Auth.Enabled && len(c.Auth.Users) == 0 {
		return &ErrInsecure{Reason: "authentication is enabled but no users are configured. " +
			"Set auth.users in the config file, or HOTLOOP_FLOW_ADMIN_USER and " +
			"HOTLOOP_FLOW_ADMIN_PASSWORD_HASH in the environment. " +
			"Generate a hash with: hotloop-flow hash-password"}
	}
	for i, u := range c.Auth.Users {
		if u.Username == "" {
			return fmt.Errorf("auth.users[%d] has no username", i)
		}
		if u.PasswordHash == "" {
			return fmt.Errorf("auth.users[%d] (%s) has no passwordHash", i, u.Username)
		}
		// Refuse anything that is not a bcrypt hash, so a plaintext password
		// cannot end up in a ConfigMap by accident. bcrypt.Cost parses the
		// prefix and fails on anything else.
		if _, err := bcrypt.Cost([]byte(u.PasswordHash)); err != nil {
			return fmt.Errorf("auth.users[%d] (%s): passwordHash is not a bcrypt hash. "+
				"Generate one with: hotloop-flow hash-password", i, u.Username)
		}
	}

	// API tokens. Hashes only, same reasoning as passwords: the file that
	// grants a token must never be enough to use it.
	seen := map[string]bool{}
	for i, t := range c.Auth.Tokens {
		if !tokenName.MatchString(t.Name) {
			return fmt.Errorf("auth.tokens[%d]: name %q must be letters, digits, dots, dashes or underscores", i, t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("auth.tokens[%d]: there are two tokens called %q, and the deployment log couldn't tell them apart", i, t.Name)
		}
		seen[t.Name] = true
		if !tokenHash.MatchString(t.Hash) {
			return fmt.Errorf("auth.tokens[%d] (%s): hash is not a token hash. "+
				"Generate a token and its hash with: hotloop-flow token", i, t.Name)
		}
		if len(t.Permissions) == 0 {
			return fmt.Errorf("auth.tokens[%d] (%s) has no permissions; a deploy token is [flows.read, flows.write]", i, t.Name)
		}
	}

	// Credentials at rest.
	if c.Data.CredentialSecret == "" && !c.Data.AllowPlaintextCredentials {
		return &ErrInsecure{Reason: "no credential secret is set, so node credentials " +
			"would be written to disk in plaintext. " +
			"Set data.credentialSecret or HOTLOOP_FLOW_CREDENTIAL_SECRET, " +
			"or set HOTLOOP_FLOW_ALLOW_PLAINTEXT_CREDENTIALS=true if this instance holds no secrets."}
	}

	// Discovery.
	if c.Discovery.Enabled && len(c.Discovery.AllowedCIDRs) == 0 {
		return fmt.Errorf("discovery.enabled is true but discovery.allowedCIDRs is empty; " +
			"list the networks the scan nodes may probe, or disable discovery")
	}

	// The exec node. Same shape as discovery and for the same reason: an empty
	// allowlist read permissively is how a narrow capability becomes a shell.
	if c.Exec.Enabled && len(c.Exec.AllowedCommands) == 0 {
		return fmt.Errorf("exec.enabled is true but exec.allowedCommands is empty; " +
			"list the commands a flow may run, or disable the exec node")
	}

	return nil
}

var (
	tokenName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	tokenHash = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// FlowPath is the absolute path to the flow file.
func (c *Config) FlowPath() string { return filepath.Join(c.Data.Dir, c.Data.FlowFile) }

// CredentialsPath is the absolute path to the credential store.
func (c *Config) CredentialsPath() string {
	return filepath.Join(c.Data.Dir, c.Data.CredentialsFile)
}

// AuditPath is where the audit trail lives.
func (c *Config) AuditPath() string { return filepath.Join(c.Data.Dir, "audit.log") }

// SessionsPath is where sign-in sessions are kept across restarts.
func (c *Config) SessionsPath() string { return filepath.Join(c.Data.Dir, "sessions.json") }

// HistoryDir is where the deployment log lives.
func (c *Config) HistoryDir() string { return filepath.Join(c.Data.Dir, "deployments") }

// TestsPath is the flow test suite, next to the flow file it tests and named
// after it: flows.json's is flows.test.yaml.
func (c *Config) TestsPath() string {
	return strings.TrimSuffix(c.FlowPath(), filepath.Ext(c.FlowPath())) + ".test.yaml"
}

// Addr is the listen address.
func (c *Config) Addr() string { return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port) }

// FindUser returns a configured user by name.
func (c *Config) FindUser(username string) (User, bool) {
	for _, u := range c.Auth.Users {
		if u.Username == username {
			return u, true
		}
	}
	return User{}, false
}

// Allows reports whether a user holds a permission. "*" grants everything.
func (u User) Allows(perm string) bool {
	for _, p := range u.Permissions {
		if p == "*" || p == perm {
			return true
		}
		// A prefix grant such as "flows.*" covers "flows.write".
		if strings.HasSuffix(p, ".*") && strings.HasPrefix(perm, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// HashPassword produces a bcrypt hash for the hash-password command.
func HashPassword(plain string) (string, error) {
	if len(plain) < 8 {
		return "", fmt.Errorf("password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CheckPassword verifies a plaintext password against a stored hash in constant
// time.
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
