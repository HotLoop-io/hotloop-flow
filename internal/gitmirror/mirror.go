// Package gitmirror pushes every deployment to a git repository as a commit
// authored by whoever deployed it.
//
// The deployment log already holds every deploy, but it lives on the box. A
// repository is where the rest of the company already looks: it has a web
// view, a blame, a backup, and people who know how to read it. So with a mirror
// configured, every deployment becomes a commit of the flow file, authored by
// the deployer, with the note as the message, on a branch only Flow writes to.
//
// It is asynchronous on purpose. A deploy never waits for a network round trip
// to a git server, and a git server that is down, slow or refusing never stops
// a deploy: the commits wait in a local clone and go out on the next push that
// works, while a status and a metric say how far behind the mirror is.
//
// Pure Go (go-git), so the binary stays static.
package gitmirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/HotLoop-io/hotloop-flow/internal/history"
	"github.com/HotLoop-io/hotloop-flow/internal/metrics"
)

// Config is where and how to mirror.
type Config struct {
	// URL of the repository, http:// or https://.
	URL string
	// Branch Flow commits to. It should be one nobody else pushes to.
	Branch string
	// Path of the flow file inside the repository.
	Path string
	// EmailDomain makes a deployer's commit email: dana@<domain>.
	EmailDomain string
	// Username and Password for HTTP basic auth. For a hosted repository the
	// password is an access token.
	Username, Password string
}

// trailer marks the deployment a commit came from, so the mirror can work out
// where it got to from the repository alone, after a restart or a wiped cache.
const trailer = "Flow-Deployment: "

var trailerRe = regexp.MustCompile(`(?m)^Flow-Deployment: (\d+)\s*$`)

// Status is how far the mirror has got.
type Status struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
	// Committed is the newest deployment committed to the local clone.
	Committed int64 `json:"committed"`
	// Pushed is the newest deployment the remote has.
	Pushed      int64     `json:"pushed"`
	LastPushAt  time.Time `json:"lastPushAt,omitzero"`
	LastError   string    `json:"lastError,omitempty"`
	LastErrorAt time.Time `json:"lastErrorAt,omitzero"`
}

// Mirror owns the local clone and the push loop.
type Mirror struct {
	cfg     Config
	dir     string
	history *history.Log
	log     *slog.Logger

	wake chan struct{}

	mu       sync.Mutex
	status   Status
	failures int64
	pushes   int64
}

// New prepares a mirror. dir is its local clone, under the data directory.
func New(cfg Config, dir string, h *history.Log, log *slog.Logger) (*Mirror, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	if cfg.Path == "" {
		cfg.Path = "flows.json"
	}
	if cfg.EmailDomain == "" {
		cfg.EmailDomain = "flow.invalid"
	}
	return &Mirror{
		cfg: cfg, dir: dir, history: h, log: log,
		wake:   make(chan struct{}, 1),
		status: Status{URL: Redact(cfg.URL), Branch: cfg.Branch},
	}, nil
}

// Validate refuses a configuration that can't work, at startup rather than at
// the first deploy.
func Validate(cfg Config) error {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("history.git.url %q is not a repository URL", Redact(cfg.URL))
	}
	switch u.Scheme {
	case "https", "http":
	default:
		// file:// and ssh:// both need things the image doesn't have: a git
		// binary for one, keys and known_hosts for the other. Refusing here
		// beats a mirror that fails on every push.
		return fmt.Errorf("history.git.url must be http:// or https://, not %s://: "+
			"the image has no git binary and no SSH keys to speak anything else", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("history.git.url carries a username or password; put them in " +
			"HOTLOOP_FLOW_GIT_USERNAME and HOTLOOP_FLOW_GIT_PASSWORD so the URL can be logged")
	}
	if strings.HasPrefix(cfg.Path, "/") || strings.Contains(cfg.Path, "..") {
		return fmt.Errorf("history.git.path %q must be a relative path inside the repository", cfg.Path)
	}
	return nil
}

// Redact strips credentials from a URL so it can be shown and logged.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// Notify asks for a sync soon. It never blocks, which is what keeps a deploy
// from ever waiting on the mirror.
func (m *Mirror) Notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run syncs whenever notified, and retries a failed push on its own every
// retry interval, until the context ends.
func (m *Mirror) Run(ctx context.Context, retry time.Duration) {
	m.Notify()
	t := time.NewTicker(retry)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-t.C:
			if m.behind() == 0 {
				continue
			}
		}
		if err := m.Sync(ctx); err != nil {
			m.log.Warn("git mirror is behind", "url", Redact(m.cfg.URL), "error", err)
		}
	}
}

// Status reports how far the mirror has got.
func (m *Mirror) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Mirror) behind() int64 {
	latest, ok := m.history.Latest()
	if !ok {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if latest.Seq > m.status.Pushed {
		return latest.Seq - m.status.Pushed
	}
	return 0
}

// Families are the mirror's metrics.
func (m *Mirror) Families() []metrics.Family {
	return []metrics.Family{
		{Name: "hotloop_flow_git_mirror_push_failures_total", Type: "counter",
			Help: "Pushes to the git mirror that failed. The commits wait in the local clone and go out on the next push that works.",
			Collect: func() []metrics.Sample {
				m.mu.Lock()
				defer m.mu.Unlock()
				return []metrics.Sample{{Value: float64(m.failures)}}
			}},
		{Name: "hotloop_flow_git_mirror_pushes_total", Type: "counter",
			Help: "Pushes to the git mirror that succeeded.",
			Collect: func() []metrics.Sample {
				m.mu.Lock()
				defer m.mu.Unlock()
				return []metrics.Sample{{Value: float64(m.pushes)}}
			}},
		{Name: "hotloop_flow_git_mirror_behind_deployments", Type: "gauge",
			Help: "Deployments the git mirror's remote doesn't have yet. Alert when this stays above zero.",
			Collect: func() []metrics.Sample {
				return []metrics.Sample{{Value: float64(m.behind())}}
			}},
	}
}

// Sync commits every deployment the local clone doesn't have yet, then pushes.
// A failure is recorded in the status and the metrics and returned, and
// changes nothing that already happened: what was committed stays committed
// and goes out next time.
func (m *Mirror) Sync(ctx context.Context) error {
	err := m.sync(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.failures++
		m.status.LastError = err.Error()
		m.status.LastErrorAt = time.Now().UTC()
		return err
	}
	m.status.LastError = ""
	return nil
}

func (m *Mirror) sync(ctx context.Context) error {
	repo, err := m.open(ctx)
	if err != nil {
		return err
	}
	last, err := lastMirrored(repo)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.status.Committed = last
	m.mu.Unlock()

	if err := m.commitPending(repo, last); err != nil {
		return err
	}

	err = repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gitconfig.RefSpec{gitconfig.RefSpec("refs/heads/" + m.cfg.Branch + ":refs/heads/" + m.cfg.Branch)},
		Auth:       m.auth(),
	})
	upToDate := errors.Is(err, git.NoErrAlreadyUpToDate)
	if err != nil && !upToDate {
		return fmt.Errorf("pushing to %s: %w", Redact(m.cfg.URL), err)
	}
	committed, err := lastMirrored(repo)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.status.Committed = committed
	m.status.Pushed = committed
	if !upToDate {
		m.status.LastPushAt = time.Now().UTC()
		m.pushes++
	}
	m.mu.Unlock()
	return nil
}

func (m *Mirror) auth() transport.AuthMethod {
	if m.cfg.Username == "" && m.cfg.Password == "" {
		return nil
	}
	user := m.cfg.Username
	if user == "" {
		// Hosted git servers take a token as the password and ignore the
		// username, but basic auth needs one.
		user = "hotloop-flow"
	}
	return &githttp.BasicAuth{Username: user, Password: m.cfg.Password}
}

// open returns the local clone, making it on first use: a clone of the branch
// when the remote has it, a fresh repository pointed at the remote when it
// doesn't (an empty repository, or a new branch).
func (m *Mirror) open(ctx context.Context) (*git.Repository, error) {
	repo, err := git.PlainOpen(m.dir)
	if err == nil {
		return repo, nil
	}
	if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("opening the mirror's clone at %s: %w", m.dir, err)
	}

	repo, err = git.PlainCloneContext(ctx, m.dir, false, &git.CloneOptions{
		URL:           m.cfg.URL,
		Auth:          m.auth(),
		ReferenceName: plumbing.NewBranchReferenceName(m.cfg.Branch),
		SingleBranch:  true,
	})
	if err == nil {
		return repo, nil
	}
	_ = os.RemoveAll(m.dir)
	if !errors.Is(err, transport.ErrEmptyRemoteRepository) && !isMissingBranch(err) {
		return nil, fmt.Errorf("cloning %s: %w", Redact(m.cfg.URL), err)
	}

	// Nothing there yet. Start the branch here and push it.
	repo, err = git.PlainInitWithOptions(m.dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(m.cfg.Branch)},
	})
	if err != nil {
		return nil, fmt.Errorf("starting the mirror's clone at %s: %w", m.dir, err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{m.cfg.URL}}); err != nil {
		return nil, err
	}
	return repo, nil
}

func isMissingBranch(err error) bool {
	var noMatch git.NoMatchingRefSpecError
	return errors.As(err, &noMatch) || errors.Is(err, plumbing.ErrReferenceNotFound) ||
		strings.Contains(err.Error(), "couldn't find remote ref")
}

// lastMirrored reads the newest deployment in the clone off its commit
// trailer. Zero for a clone with no commits from Flow yet.
func lastMirrored(repo *git.Repository) (int64, error) {
	head, err := repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	iter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	var seq int64
	stop := errors.New("found")
	err = iter.ForEach(func(c *object.Commit) error {
		if mm := trailerRe.FindStringSubmatch(c.Message); mm != nil {
			seq, _ = strconv.ParseInt(mm[1], 10, 64)
			return stop
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return 0, err
	}
	return seq, nil
}

// commitPending commits every deployment after last, oldest first, one commit
// each, authored by whoever deployed it at the time they deployed it.
func (m *Mirror) commitPending(repo *git.Repository, last int64) error {
	var pending []history.Record
	for _, r := range m.history.List(0) {
		if r.Seq > last {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Seq < pending[j].Seq })

	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	target := filepath.Join(m.dir, filepath.FromSlash(m.cfg.Path))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}

	for _, meta := range pending {
		rec, err := m.history.Get(meta.Seq)
		if errors.Is(err, history.ErrNotFound) {
			// Retention removed it before the mirror caught up. Nothing to
			// commit, and the next one says where it picks up.
			continue
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, rec.Flows, 0o600); err != nil {
			return err
		}
		if _, err := wt.Add(filepath.ToSlash(m.cfg.Path)); err != nil {
			return err
		}
		_, err = wt.Commit(message(rec), &git.CommitOptions{
			Author:            &object.Signature{Name: authorName(rec.User), Email: m.email(rec.User), When: rec.Time},
			Committer:         &object.Signature{Name: "HotLoop Flow", Email: "flow@" + m.cfg.EmailDomain, When: time.Now().UTC()},
			AllowEmptyCommits: true,
		})
		if err != nil {
			return fmt.Errorf("committing deployment %d: %w", rec.Seq, err)
		}
		m.mu.Lock()
		m.status.Committed = rec.Seq
		m.mu.Unlock()
	}
	return nil
}

func message(r history.Record) string {
	subject := r.Note
	if subject == "" {
		subject = fmt.Sprintf("%s %d", r.Kind, r.Seq)
	}
	if i := strings.IndexByte(subject, '\n'); i >= 0 {
		subject = subject[:i]
	}
	var b strings.Builder
	b.WriteString(subject)
	b.WriteString("\n\n")
	switch r.Kind {
	case history.KindRollback:
		fmt.Fprintf(&b, "Rollback to deployment %d.\n", r.RollbackOf)
	case history.KindBaseline:
		b.WriteString("The flow file as it was on disk, recorded at startup.\n")
	}
	if r.Remote != "" {
		fmt.Fprintf(&b, "Deployed from %s.\n", r.Remote)
	}
	fmt.Fprintf(&b, "\nFlow-Rev: %s\n", r.Rev)
	if r.ParentRev != "" {
		fmt.Fprintf(&b, "Flow-Parent-Rev: %s\n", r.ParentRev)
	}
	b.WriteString(trailer + strconv.FormatInt(r.Seq, 10) + "\n")
	return b.String()
}

func authorName(user string) string {
	if user == "" {
		return "nobody signed in"
	}
	return user
}

var emailUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (m *Mirror) email(user string) string {
	local := emailUnsafe.ReplaceAllString(user, "-")
	if local == "" {
		local = "anonymous"
	}
	return local + "@" + m.cfg.EmailDomain
}
