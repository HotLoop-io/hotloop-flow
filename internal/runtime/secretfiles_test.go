package runtime

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/filescope"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Credentials from files, read through the real secret scope over real files.

// probe is a node that records what its credentials were when it was built,
// the moment every real node reads them.
type probe struct {
	mu   sync.Mutex
	seen map[string]map[string]string
}

func (p *probe) register(t *testing.T, tr *testRegistry) {
	t.Helper()
	p.seen = map[string]map[string]string{}
	if err := tr.Register(node.Descriptor{
		Type: "cred-probe", Category: node.CategoryCommon, Color: "#E31837", Icon: "cog",
		Inputs: 1, Outputs: 0, Compatibility: node.Compatibility{Level: node.CompatOnly},
	}, func(def *node.Definition) (node.Node, error) {
		got := map[string]string{}
		for _, k := range []string{"password", "token", "user"} {
			if v, ok := def.Services.Credential(k); ok {
				got[k] = v
			}
		}
		p.mu.Lock()
		p.seen[def.Node.ID] = got
		p.mu.Unlock()
		return passNode{}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (p *probe) of(id string) map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen[id]
}

// scopeReader is what main installs: a read through the secret scope.
func scopeReader(t *testing.T, roots ...string) func(string) ([]byte, error) {
	t.Helper()
	s, err := filescope.NewSecretScope(t.TempDir(), roots)
	if err != nil {
		t.Fatal(err)
	}
	return func(p string) ([]byte, error) {
		checked, err := s.Check(p)
		if err != nil {
			return nil, err
		}
		f, err := os.Open(checked)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(f)
	}
}

func probeFlow(t *testing.T, files map[string]any) *engine.Flows {
	t.Helper()
	refs, _ := json.Marshal(files)
	return mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"p1","type":"cred-probe","z":"t1","x":1,"y":1,"wires":[],"ew_credentialFiles":`+string(refs)+`}
    ]`)
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCredentialFilesFeedTheNode(t *testing.T) {
	tr := newTestRegistry()
	var p probe
	p.register(t, tr)
	mount := t.TempDir()
	pass := writeFile(t, mount, "password", "hunter2-but-longer\n")
	token := writeFile(t, mount, "token", "  spaces stay  \r\n\r\n")

	rt := New(tr.Registry, probeFlow(t, map[string]any{"password": pass, "token": token}), Options{})
	drain(rt)
	// The store has its own password and a user. The file wins for the key
	// it names; the store still answers for the rest.
	rt.SetCredentials(func(string) map[string]string {
		return map[string]string{"password": "from-the-store", "user": "line3"}
	})
	rt.SetSecretFiles(scopeReader(t, mount))
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatalf("Start: %v", fails)
	}
	defer rt.Stop(context.Background())

	got := p.of("p1")
	if got["password"] != "hunter2-but-longer" {
		t.Errorf("password = %q, want the file's, without its newline", got["password"])
	}
	if got["token"] != "  spaces stay  " {
		t.Errorf("token = %q: only trailing line breaks come off", got["token"])
	}
	if got["user"] != "line3" {
		t.Errorf("user = %q, want the store's", got["user"])
	}
}

// Every way a reference can be wrong fails the node, by name, instead of
// starting it with no password.
func TestCredentialFilesThatCannotBeRead(t *testing.T) {
	mount := t.TempDir()
	outside := writeFile(t, t.TempDir(), "password", "x")

	cases := []struct {
		name   string
		refs   any
		reader bool
		want   string
	}{
		{"a file that is not there", map[string]any{"password": filepath.Join(mount, "nope")}, true, "nope"},
		{"a file outside the secret scope", map[string]any{"password": outside}, true, "secrets.allowedPaths"},
		{"an empty path", map[string]any{"password": "  "}, true, "must be a file path"},
		{"a number for a path", map[string]any{"password": 7}, true, "must be a file path"},
		{"not a map at all", "/run/secrets/password", true, "must map a credential name"},
		{"no reader installed", map[string]any{"password": outside}, false, ErrNoSecretFiles.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestRegistry()
			var p probe
			p.register(t, tr)
			refs, _ := json.Marshal(tc.refs)
			flows := mustFlows(t, `[
                {"id":"t1","type":"tab","label":"T"},
                {"id":"p1","type":"cred-probe","z":"t1","x":1,"y":1,"wires":[],"ew_credentialFiles":`+string(refs)+`}
            ]`)
			rt := New(tr.Registry, flows, Options{})
			drain(rt)
			if tc.reader {
				rt.SetSecretFiles(scopeReader(t, mount))
			}
			fails := rt.Start(context.Background())
			defer rt.Stop(context.Background())
			if len(fails) != 1 || fails[0].NodeID != "p1" || !strings.Contains(fails[0].Err.Error(), tc.want) {
				t.Fatalf("failures %v, want p1 failing with %q", fails, tc.want)
			}
			if p.of("p1") != nil {
				t.Fatal("the node was built anyway")
			}
		})
	}
}

// A partial deploy that repoints a reference rebuilds the node with the new
// file's contents; one that leaves it alone leaves the node alone.
func TestCredentialFilesOnAPartialDeploy(t *testing.T) {
	tr := newTestRegistry()
	var p probe
	p.register(t, tr)
	mount := t.TempDir()
	a := writeFile(t, mount, "a", "first")
	b := writeFile(t, mount, "b", "second")

	rt := New(tr.Registry, probeFlow(t, map[string]any{"password": a}), Options{})
	drain(rt)
	rt.SetSecretFiles(scopeReader(t, mount))
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatal(fails)
	}
	defer rt.Stop(context.Background())

	res, err := rt.Update(context.Background(), probeFlow(t, map[string]any{"password": b}), UpdateOptions{Mode: DeployNodes})
	if err != nil || len(res.Failures) > 0 {
		t.Fatal(err, res.Failures)
	}
	if got := p.of("p1")["password"]; got != "second" {
		t.Fatalf("after repointing, password = %q", got)
	}
	if len(res.Restarted) != 1 || res.Restarted[0] != "p1" {
		t.Fatalf("restarted %v", res.Restarted)
	}
}
