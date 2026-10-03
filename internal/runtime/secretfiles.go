package runtime

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// PropCredentialFiles is the flow-entry property that points a node's
// credentials at files instead of the credential store:
//
//	"ew_credentialFiles": {"password": "/etc/hotloop-flow/secrets/plc-broker/password"}
//
// It works for every node type, because it sits under Credential, which is how
// every node reads a secret. The flow file carries the path and never the
// password, so the file can go in git, the deployment log and the diff without
// carrying anything worth stealing. The password lives in a Kubernetes Secret
// mounted as a volume, where whoever runs the cluster already manages
// passwords. Node-RED has no equivalent: a credential there is in
// flows_cred.json or nowhere.
const PropCredentialFiles = "ew_credentialFiles"

// ErrNoSecretFiles is returned for a node that references a credential file
// when the runtime was given no way to read one.
var ErrNoSecretFiles = errors.New("this runtime reads no credential files")

// SetSecretFiles installs the reader for ew_credentialFiles. It is the secret
// scope's: the data directory and secrets.allowedPaths, nothing else.
func (rt *Runtime) SetSecretFiles(read func(path string) ([]byte, error)) {
	rt.secretFiles = read
}

// credentialFiles reads every file a node's ew_credentialFiles names. Any one
// that cannot be read fails the node, rather than leaving it to connect with
// no password and fail somewhere less obvious, such as a broker's auth log.
func (rt *Runtime) credentialFiles(n *engine.Node) (map[string]string, error) {
	raw, ok := n.Raw[PropCredentialFiles]
	if !ok || raw == nil {
		return nil, nil
	}
	refs, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must map a credential name to a file path, not %T", PropCredentialFiles, raw)
	}
	if len(refs) == 0 {
		return nil, nil
	}
	if rt.secretFiles == nil {
		return nil, ErrNoSecretFiles
	}
	keys := make([]string, 0, len(refs))
	for k := range refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]string, len(refs))
	for _, key := range keys {
		path, ok := refs[key].(string)
		if !ok || strings.TrimSpace(path) == "" {
			return nil, fmt.Errorf("%s.%s must be a file path", PropCredentialFiles, key)
		}
		b, err := rt.secretFiles(strings.TrimSpace(path))
		if err != nil {
			return nil, fmt.Errorf("credential %q: %w", key, err)
		}
		// A trailing newline is trimmed, and only that. echo writes one,
		// most editors save one, and a password with a newline on the end
		// fails a login with nothing in the log to say why. Every other
		// byte is the password.
		out[key] = strings.TrimRight(string(b), "\r\n")
	}
	return out, nil
}
