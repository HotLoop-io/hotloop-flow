package stations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/diagram"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// Status is how one station ended.
type Status string

const (
	// Planned is a dry run's answer: it would be deployed.
	Planned Status = "planned"
	// Done is deployed and past the golden check.
	Done Status = "done"
	// Unchanged already ran this exact file, and is past the check.
	Unchanged Status = "unchanged"
	// Failed could not be deployed, or was deployed and a check failed and it
	// could not be put back; Detail says which.
	Failed Status = "failed"
	// Reverted failed the golden check and is back on the revision it had.
	Reverted Status = "reverted"
	// RevertFailed failed the check and could not be put back. Look at it.
	RevertFailed Status = "revert-failed"
	// Skipped was not touched because it could not be planned: it did not
	// answer, or it holds no record of its live revision to go back to. The next
	// push picks it up.
	Skipped Status = "skipped"
	// NotStarted was never reached, because the push stopped first.
	NotStarted Status = "not-started"
)

// StationResult is one station's part of a push.
type StationResult struct {
	Station
	Status     Status   `json:"status"`
	Detail     string   `json:"detail,omitempty"`
	PrevRev    string   `json:"prevRev,omitempty"`
	Rev        string   `json:"rev,omitempty"`
	Deployment int64    `json:"deployment,omitempty"`
	Restarts   []string `json:"restarts,omitempty"`
}

// Report is the whole push, in the order the stations were taken.
type Report struct {
	Tenant string `json:"tenant"`
	// Rev is the revision every station is to end on.
	Rev     string          `json:"rev"`
	Results []StationResult `json:"results"`
	// Stopped is true when the push did not run to the end of the list.
	Stopped bool `json:"stopped"`
	// NeedsConfirm is true when it stopped before touching anything because a
	// logger would restart and nobody had said it may.
	NeedsConfirm bool   `json:"needsConfirm,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// OK says the push ran to the end with no station failed or reverted.
func (r Report) OK() bool {
	if r.Stopped {
		return false
	}
	for _, s := range r.Results {
		switch s.Status {
		case Failed, Reverted, RevertFailed:
			return false
		}
	}
	return true
}

// Options is one push.
type Options struct {
	Tenant   string
	Stations []Station
	// Flows is the flow file to put on every station.
	Flows []byte
	Note  string
	// Token is the deploy token the stations accept.
	Token string
	// Confirm says the operator has agreed to restart loggers. Window says when
	// that is agreed in advance. Ask is asked, when set, if neither holds.
	Confirm bool
	Window  *Window
	Ask     func(restarts []StationResult) bool
	// Settle is how long a station's diagrams get to start before the golden
	// check calls them not running. Zero is five seconds.
	Settle time.Duration
	DryRun bool
	HTTP   *http.Client
	Now    func() time.Time

	// expect is the diagrams of the pushed file that start by themselves: id to
	// name. A station that does not list one has failed the check.
	expect map[string]string
}

// autostartOf is the diagrams of a set that start by themselves and are not the
// body of a loop in another.
func autostartOf(set *diagram.Set) map[string]string {
	out := map[string]string{}
	for _, id := range set.Order {
		d := set.Diagrams[id]
		if auto, _ := d.Settings["autostart"].(bool); !auto {
			continue
		}
		if role, _ := d.Settings["role"].(string); role == "body" {
			continue
		}
		name := d.Label
		if name == "" {
			name = id
		}
		out[id] = name
	}
	return out
}

// Push puts the file on each station in turn.
//
// First it plans every station without changing one: what is live, which
// deployment it can go back to, and which running loggers the file would restart
// or stop. If any would, and the operator has not said that may happen (Confirm,
// a Window that holds now, or Ask answering yes), nothing is touched and the
// report says what would have restarted. Then it goes station by station: deploy,
// golden check, and on a failed check put that one station back and stop. Stations
// after it are not started.
func Push(ctx context.Context, o Options) (Report, error) {
	flows, err := engine.ParseFlows(o.Flows)
	if err != nil {
		return Report{}, fmt.Errorf("the flow file does not parse: %w", err)
	}
	want, err := store.RevisionOf(flows)
	if err != nil {
		return Report{}, err
	}
	if len(o.Stations) == 0 {
		return Report{}, errors.New("no stations")
	}
	if o.Token == "" {
		return Report{}, errors.New("no deploy token for the stations")
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 2 * time.Minute}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Settle <= 0 {
		o.Settle = 5 * time.Second
	}
	rep := Report{Tenant: o.Tenant, Rev: want}
	newSet := diagram.CompileAll(flows)
	o.expect = autostartOf(newSet)

	plans := make([]*plan, len(o.Stations))
	for i, st := range o.Stations {
		plans[i] = o.plan(ctx, st, want, newSet)
		rep.Results = append(rep.Results, plans[i].result)
	}

	var restarts []StationResult
	for _, p := range plans {
		if p.ok && len(p.result.Restarts) > 0 {
			restarts = append(restarts, p.result)
		}
	}
	if len(restarts) > 0 && !o.DryRun && !o.Confirm && !o.Window.Contains(o.Now()) {
		if o.Ask == nil || !o.Ask(restarts) {
			for i, p := range plans {
				if p.ok {
					rep.Results[i].Status = NotStarted
					rep.Results[i].Detail = "waiting for the operator to agree to the restarts"
				}
			}
			rep.Stopped, rep.NeedsConfirm = true, true
			rep.Reason = "this push would restart running loggers and nobody has said that may happen: confirm it, or give a window for it"
			return rep, nil
		}
	}
	if o.DryRun {
		for i, p := range plans {
			if p.ok {
				rep.Results[i].Status = Planned
			}
		}
		return rep, nil
	}

	for i, p := range plans {
		if !p.ok {
			continue
		}
		if rep.Stopped {
			rep.Results[i].Status = NotStarted
			rep.Results[i].Detail = "the push stopped before this station"
			continue
		}
		res := o.run(ctx, p, want)
		rep.Results[i] = res
		switch res.Status {
		case Failed, Reverted, RevertFailed:
			rep.Stopped = true
			rep.Reason = fmt.Sprintf("station %s: %s", res.Name, res.Detail)
		}
	}
	return rep, nil
}

type plan struct {
	st     Station
	c      *stationClient
	live   string
	prev   int64
	ok     bool
	result StationResult
}

func (o Options) plan(ctx context.Context, st Station, want string, newSet *diagram.Set) *plan {
	p := &plan{st: st, c: &stationClient{base: strings.TrimSuffix(st.URL, "/"), token: o.Token, hc: o.HTTP}}
	p.result = StationResult{Station: st}
	skip := func(format string, a ...any) *plan {
		p.result.Status, p.result.Detail = Skipped, fmt.Sprintf(format, a...)
		return p
	}
	liveBytes, rev, err := p.c.export(ctx)
	if err != nil {
		return skip("it did not answer: %v", err)
	}
	p.live, p.result.PrevRev = rev, rev
	if rev != want {
		seq, err := p.c.recordOf(ctx, rev)
		if err != nil {
			return skip("%v", err)
		}
		p.prev = seq
	}
	sums, err := p.c.diagrams(ctx)
	if err != nil {
		return skip("it did not list its diagrams: %v", err)
	}
	if rev != want {
		liveFlows, err := engine.ParseFlows(liveBytes)
		if err != nil {
			return skip("its live flow file does not parse here: %v", err)
		}
		liveSet := diagram.CompileAll(liveFlows)
		for _, d := range sums {
			if !d.Autostart || d.Role == "body" || (d.State != "running" && d.State != "paused") {
				continue
			}
			if newSet.DefinitionSig(d.ID) != liveSet.DefinitionSig(d.ID) {
				name := d.Label
				if name == "" {
					name = d.ID
				}
				p.result.Restarts = append(p.result.Restarts, name)
			}
		}
		sort.Strings(p.result.Restarts)
	}
	p.ok = true
	p.result.Status = Planned
	return p
}

func (o Options) run(ctx context.Context, p *plan, want string) StationResult {
	res := p.result
	res.Restarts = nil
	if p.live == want {
		if problems := o.golden(ctx, p.c, want, ""); len(problems) > 0 {
			res.Status, res.Detail = Failed, "already on this revision, and not running as it should: "+strings.Join(problems, "; ")
			return res
		}
		res.Status, res.Rev = Unchanged, want
		return res
	}
	body, _ := json.Marshal(struct {
		Flows json.RawMessage `json:"flows"`
		Note  string          `json:"note,omitempty"`
	}{Flows: o.Flows, Note: o.note()})
	out, err := p.c.send(ctx, http.MethodPost, "/flows", body, map[string]string{
		"HotLoop-Flow-Deployment-Rev":  p.live,
		"HotLoop-Flow-Deployment-Type": "nodes",
	})
	if err != nil {
		// A broken connection may have come after the save. Ask.
		if _, rev, xerr := p.c.export(ctx); xerr != nil || rev != want {
			res.Status, res.Detail = Failed, "not deployed: "+err.Error()
			return res
		}
		out = nil
	}
	var dep struct {
		Rev        string `json:"rev"`
		Deployment int64  `json:"deployment"`
		Failures   []struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Error string `json:"error"`
		} `json:"failures"`
	}
	_ = json.Unmarshal(out, &dep)
	res.Rev, res.Deployment = dep.Rev, dep.Deployment
	var failed []string
	for _, f := range dep.Failures {
		failed = append(failed, fmt.Sprintf("node %s (%s) did not start: %s", f.ID, f.Type, f.Error))
	}
	if problems := append(failed, o.golden(ctx, p.c, want, dep.Rev)...); len(problems) > 0 {
		return o.revert(ctx, p, res, want, strings.Join(problems, "; "))
	}
	res.Status = Done
	return res
}

func (o Options) note() string {
	n := strings.TrimSpace(o.Note)
	if n == "" {
		n = "pushed to the tenant's stations"
	}
	return n
}

// golden is the check after a deploy: the station answers the revision every
// station is to have (and said so when it deployed), and every diagram that
// starts by itself and has no problem is running or has run to its end. A
// diagram given time to start and still not running, or one that failed, fails
// the check. It returns what is wrong, or nothing.
func (o Options) golden(ctx context.Context, c *stationClient, want, deployed string) []string {
	var problems []string
	if deployed != "" && deployed != want {
		problems = append(problems, fmt.Sprintf("it answered revision %s when it deployed, not %s", deployed, want))
	}
	if _, rev, err := c.export(ctx); err != nil {
		problems = append(problems, "it did not answer afterwards: "+err.Error())
		return problems
	} else if rev != want {
		problems = append(problems, fmt.Sprintf("it is on revision %s, not %s", rev, want))
	}
	deadline := time.Now().Add(o.Settle)
	for {
		sums, err := c.diagrams(ctx)
		if err != nil {
			return append(problems, "it did not list its diagrams: "+err.Error())
		}
		var bad []string
		waiting := false
		listed := map[string]bool{}
		for _, d := range sums {
			listed[d.ID] = true
		}
		for id, name := range o.expect {
			if !listed[id] {
				bad = append(bad, fmt.Sprintf("diagram %s is not on the station: it cannot run diagrams, or did not take the file", name))
			}
		}
		for _, d := range sums {
			if !d.Autostart || d.Role == "body" {
				continue
			}
			name := d.Label
			if name == "" {
				name = d.ID
			}
			switch {
			case len(d.Problems) > 0:
				bad = append(bad, fmt.Sprintf("diagram %s cannot run: %s", name, d.Problems[0].Text))
			case d.State == "failed" || d.State == "stopped":
				bad = append(bad, fmt.Sprintf("diagram %s is %s", name, d.State))
			case d.State != "running" && d.State != "done":
				waiting = true
				bad = append(bad, fmt.Sprintf("diagram %s did not start (it is %s)", name, d.State))
			}
		}
		if len(bad) == 0 {
			return problems
		}
		if !waiting || time.Now().After(deadline) {
			return append(problems, bad...)
		}
		select {
		case <-ctx.Done():
			return append(problems, "cancelled while waiting for its diagrams")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// revert puts one station back on the deployment it was on, as a new deployment
// of its own, and checks it is back.
func (o Options) revert(ctx context.Context, p *plan, res StationResult, want, why string) StationResult {
	if p.prev == 0 {
		res.Status, res.Detail = RevertFailed, why+"; it has no record to go back to"
		return res
	}
	body, _ := json.Marshal(map[string]string{"note": "reverted: the golden check of a push failed"})
	_, err := p.c.send(ctx, http.MethodPost, fmt.Sprintf("/deployments/%d/rollback", p.prev), body, map[string]string{
		"HotLoop-Flow-Deployment-Rev":  want,
		"HotLoop-Flow-Deployment-Type": "nodes",
	})
	if err != nil {
		res.Status, res.Detail = RevertFailed, why+"; going back failed: "+err.Error()
		return res
	}
	if _, rev, xerr := p.c.export(ctx); xerr != nil || rev != p.live {
		res.Status, res.Detail = RevertFailed, why+"; after going back it is not on its old revision "+p.live
		return res
	}
	res.Status, res.Detail, res.Rev = Reverted, why+"; back on its revision "+p.live, p.live
	return res
}

// stationClient is one station's admin API.
type stationClient struct {
	base, token string
	hc          *http.Client
}

func (c *stationClient) send(ctx context.Context, method, path string, body []byte, hdr map[string]string) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	out, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(out))
		if json.Unmarshal(out, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		switch res.StatusCode {
		case http.StatusUnauthorized:
			msg = "the station did not accept the deploy token: " + msg
		case http.StatusForbidden:
			msg = "the deploy token is not allowed to do that (a push needs flows.read, flows.write and status.read): " + msg
		case http.StatusConflict:
			msg = "somebody deployed there in between, so this was refused rather than overwrite them: " + msg
		}
		return nil, fmt.Errorf("%d: %s", res.StatusCode, msg)
	}
	return out, nil
}

// export is the live flow file and its revision.
func (c *stationClient) export(ctx context.Context) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/flows/export", nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, "", err
	}
	if res.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	rev := res.Header.Get("HotLoop-Flow-Deployment-Rev")
	if rev == "" {
		return nil, "", errors.New("it did not say its revision")
	}
	return b, rev, nil
}

// recordOf is the newest deployment record that holds revision rev.
func (c *stationClient) recordOf(ctx context.Context, rev string) (int64, error) {
	out, err := c.send(ctx, http.MethodGet, "/deployments", nil, nil)
	if err != nil {
		return 0, fmt.Errorf("it did not list its deployments: %w", err)
	}
	var l struct {
		Deployments []struct {
			Seq int64  `json:"seq"`
			Rev string `json:"rev"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(out, &l); err != nil {
		return 0, err
	}
	var best int64
	for _, d := range l.Deployments {
		if d.Rev == rev && d.Seq > best {
			best = d.Seq
		}
	}
	if best == 0 {
		return 0, fmt.Errorf("it has no deployment record of its live revision %s to go back to, so a failed push could not be undone there; deploy to it once by hand first", rev)
	}
	return best, nil
}

type diagramSummary struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Role      string `json:"role"`
	Autostart bool   `json:"autostart"`
	State     string `json:"state"`
	Problems  []struct {
		Text string `json:"text"`
	} `json:"problems"`
}

func (c *stationClient) diagrams(ctx context.Context) ([]diagramSummary, error) {
	out, err := c.send(ctx, http.MethodGet, "/diagrams", nil, nil)
	if err != nil {
		// A station built without diagrams answers 404: it has none to check.
		if strings.HasPrefix(err.Error(), "404") {
			return nil, nil
		}
		return nil, err
	}
	var l struct {
		Diagrams []diagramSummary `json:"diagrams"`
	}
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, err
	}
	return l.Diagrams, nil
}
