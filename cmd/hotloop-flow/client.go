package main

// Flows as code: the commands a CI job runs against a running instance.
//
// The repository is where flows get reviewed, so the repository should be able
// to deploy them, and getting flows back out should give you exactly the file
// that is running. Both commands authenticate with an API token from the
// environment, never a flag (a flag shows up in the process list and in every
// CI log that echoes the command), and never an admin password.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
)

// client talks to a running instance's admin API.
type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(url, tokenFile string) (*client, error) {
	if url == "" {
		url = os.Getenv("HOTLOOP_FLOW_URL")
	}
	if url == "" {
		return nil, errors.New("no instance to talk to: pass -url, or set HOTLOOP_FLOW_URL")
	}
	token := os.Getenv("HOTLOOP_FLOW_TOKEN")
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading the token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		return nil, errors.New("no API token: set HOTLOOP_FLOW_TOKEN, or pass -token-file. " +
			"Make one with: hotloop-flow token")
	}
	return &client{
		base:  strings.TrimSuffix(url, "/"),
		token: token,
		// Generous, because a deploy restarts flows and a big one over an edge
		// link is slow, but not unbounded: a CI job that hangs forever is a
		// pipeline nobody trusts.
		http: &http.Client{Timeout: 2 * time.Minute},
	}, nil
}

// do sends a request and returns the body, turning the API's error answers
// into errors that say what the instance said.
func (c *client) do(method, path string, body []byte, hdr map[string]string) ([]byte, http.Header, error) {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, nil, err
	}
	if res.StatusCode >= 300 {
		var e struct {
			Error string           `json:"error"`
			Tests *flowtest.Report `json:"tests"`
		}
		msg := strings.TrimSpace(string(out))
		if json.Unmarshal(out, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, res.Header, &apiError{status: res.StatusCode, msg: msg, tests: e.Tests}
	}
	return out, res.Header, nil
}

type apiError struct {
	status int
	msg    string
	// tests is the flow test report the deploy gate refused a deploy with.
	tests *flowtest.Report
}

func (e *apiError) Error() string {
	if e.tests != nil {
		// The whole report, the way hotloop-flow test prints it, because
		// the pipeline log is where somebody reads why their merge didn't
		// deploy.
		var b strings.Builder
		_ = flowtest.WriteText(&b, e.tests, false)
		return e.msg + "\n\n" + strings.TrimRight(b.String(), "\n")
	}
	switch e.status {
	case http.StatusUnauthorized:
		return "the instance didn't accept the token: " + e.msg
	case http.StatusForbidden:
		return "the token isn't allowed to do that: " + e.msg
	case http.StatusConflict:
		return "somebody deployed in between, so this deploy was refused rather than overwrite them: " + e.msg
	}
	return fmt.Sprintf("the instance answered %d: %s", e.status, e.msg)
}

// ---------------------------------------------------------------------------
// token
// ---------------------------------------------------------------------------

func cmdToken(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	name := fs.String("name", config.DeployTokenName, "what the deployment log calls this token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	token, hash, err := config.NewToken()
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, `Token, shown once. Store it as a CI secret and hand it to the job as HOTLOOP_FLOW_TOKEN:

  %s

Give the runtime its hash, never the token. For a deploy token, in the environment:

  HOTLOOP_FLOW_DEPLOY_TOKEN_HASH=%s

or in the config file, under auth:

  tokens:
    - name: %s
      hash: "%s"
      permissions: ["flows.read", "flows.write"]
`, token, hash, *name, hash)
	return nil
}

// ---------------------------------------------------------------------------
// deploy
// ---------------------------------------------------------------------------

const deployUsage = `usage: hotloop-flow deploy -file flows.json [-url URL] [-note TEXT] [-type nodes|flows|full] [-expect-rev REV] [-dry-run]

Shows what deploying the file would change, node by node, then deploys it with
the note. A file with nothing to change deploys nothing and adds nothing to the
history. Authenticates with HOTLOOP_FLOW_TOKEN (or -token-file); the URL can
come from HOTLOOP_FLOW_URL.
`

func cmdDeploy(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	url := fs.String("url", "", "the instance, e.g. http://line3-flows:8080 (or HOTLOOP_FLOW_URL)")
	file := fs.String("file", "", "the flow file to deploy")
	note := fs.String("note", "", "the note for the deployment log")
	typ := fs.String("type", "", "how much to restart: nodes (the default), flows or full")
	expect := fs.String("expect-rev", "", "refuse unless this revision is live, e.g. the one a reviewed plan was made against")
	dryRun := fs.Bool("dry-run", false, "show what would change and deploy nothing")
	tokenFile := fs.String("token-file", "", "read the API token from this file instead of HOTLOOP_FLOW_TOKEN")
	fs.Usage = func() { fmt.Fprint(fs.Output(), deployUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" || fs.NArg() != 0 {
		fs.Usage()
		return errors.New("deploy needs -file and nothing else on the line")
	}

	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	// Parsed here first, so a broken file is a failed pipeline with the line
	// number, not a 400 from a box three hops away.
	if _, err := engine.ParseFlows(data); err != nil {
		return fmt.Errorf("%s: %w", *file, err)
	}

	c, err := newClient(*url, *tokenFile)
	if err != nil {
		return err
	}

	out, _, err := c.do("POST", "/flows/diff", data, nil)
	if err != nil {
		return fmt.Errorf("comparing with what is live: %w", err)
	}
	var diff struct {
		From    string `json:"from"`
		Entries []any  `json:"entries"`
		Text    string `json:"text"`
	}
	if err := json.Unmarshal(out, &diff); err != nil {
		return fmt.Errorf("reading the diff: %w", err)
	}
	fmt.Fprint(stdout, diff.Text)

	if len(diff.Entries) == 0 {
		fmt.Fprintln(stdout, "Nothing to deploy.")
		return nil
	}
	if *dryRun {
		fmt.Fprintln(stdout, "Dry run: nothing deployed.")
		return nil
	}

	// Deployed against the revision the diff was made from, so what was just
	// printed is exactly what gets deployed. If somebody deploys in between,
	// that's a 409, not a surprise.
	rev := diff.From
	if *expect != "" {
		rev = *expect
	}
	body, err := json.Marshal(struct {
		Flows json.RawMessage `json:"flows"`
		Note  string          `json:"note,omitempty"`
	}{Flows: data, Note: *note})
	if err != nil {
		return err
	}
	out, _, err = c.do("POST", "/flows", body, map[string]string{
		"HotLoop-Flow-Deployment-Rev":  rev,
		"HotLoop-Flow-Deployment-Type": *typ,
	})
	if err != nil {
		return err
	}
	var res struct {
		Rev        string   `json:"rev"`
		Deployment int64    `json:"deployment"`
		Type       string   `json:"type"`
		Restarted  []string `json:"restarted"`
		Started    []string `json:"started"`
		Stopped    []string `json:"stopped"`
		Warnings   []string `json:"warnings"`
		Failures   []struct {
			ID, Type, Error string
		} `json:"failures"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return fmt.Errorf("reading the deploy answer: %w", err)
	}
	fmt.Fprintf(stdout, "Deployed: deployment %d, rev %s, %s deploy, %d started, %d restarted, %d stopped.\n",
		res.Deployment, res.Rev, res.Type, len(res.Started), len(res.Restarted), len(res.Stopped))
	for _, w := range res.Warnings {
		fmt.Fprintf(stdout, "warning: %s\n", w)
	}
	if len(res.Failures) > 0 {
		for _, f := range res.Failures {
			fmt.Fprintf(stdout, "node %s (%s) failed to start: %s\n", f.ID, f.Type, f.Error)
		}
		// Deployed, but not everything runs. A pipeline should go red for
		// that: the rest of the flow is running, and somebody has to look.
		return fmt.Errorf("%d node(s) failed to start", len(res.Failures))
	}
	return nil
}

// ---------------------------------------------------------------------------
// export
// ---------------------------------------------------------------------------

func cmdExport(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	url := fs.String("url", "", "the instance (or HOTLOOP_FLOW_URL)")
	outPath := fs.String("o", "", "write here instead of standard output")
	seq := fs.Int64("deployment", 0, "export this deployment from the history instead of what is live")
	tokenFile := fs.String("token-file", "", "read the API token from this file instead of HOTLOOP_FLOW_TOKEN")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: hotloop-flow export [-url URL] [-o flows.json] [-deployment N]")
	}
	c, err := newClient(*url, *tokenFile)
	if err != nil {
		return err
	}
	path := "/flows/export"
	if *seq > 0 {
		path = fmt.Sprintf("/deployments/%d/flows", *seq)
	}
	data, _, err := c.do("GET", path, nil, nil)
	if err != nil {
		return err
	}
	if *outPath == "" {
		_, err := stdout.Write(data)
		return err
	}
	// 0644: a flow file is meant to be read, diffed and committed. The
	// credentials were never in it.
	return os.WriteFile(*outPath, data, 0o644)
}
