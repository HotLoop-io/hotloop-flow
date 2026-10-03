package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/audit"
)

type auditEntries struct {
	Entries []audit.Entry `json:"entries"`
}

func (e *e2e) auditTrail(t *testing.T, token, query string) []audit.Entry {
	t.Helper()
	code, out := e.call(t, "GET", "/audit"+query, token, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /audit%s: %d %s", query, code, out)
	}
	var a auditEntries
	if err := json.Unmarshal(out, &a); err != nil {
		t.Fatal(err)
	}
	return a.Entries
}

// The roadmap's list, one at a time, through the real API: logins, failed
// logins, deploys, rollbacks and injects, each with who and from where.
func TestEverythingThatMattersLeavesATrail(t *testing.T) {
	app, _ := newApp(t)
	e := serveApp(t, app, map[string][]string{"dana": {"*"}, "sam": {"flows.*"}})

	// Two failed logins: one for a user that exists, one for one that doesn't.
	for _, u := range []string{"dana", "mallory"} {
		body, _ := json.Marshal(map[string]string{"username": u, "password": "guess"})
		req, _ := http.NewRequest("POST", e.URL+"/auth/token", bytes.NewReader(body))
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	dana := e.login(t, "dana")
	sam := e.login(t, "sam")

	first := e.deployDoc(t, dana, setFlow("one"))
	e.deployDoc(t, sam, setFlow("two"))
	// A stale deploy is refused, and that's worth a line too.
	e.call(t, "POST", "/flows", sam, []byte(setFlow("three")), "HotLoop-Flow-Deployment-Rev", "stale")
	if code, out := e.rollbackTo(t, dana, first, `{"note":"two broke it"}`); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, out)
	}
	if code, out := e.call(t, "POST", "/inject/c1", dana, nil); code != http.StatusOK {
		t.Fatalf("inject: %d %s", code, out)
	}
	e.call(t, "POST", "/auth/revoke", sam, nil)

	trail := e.auditTrail(t, dana, "")
	var got []string
	for i := len(trail) - 1; i >= 0; i-- {
		got = append(got, trail[i].Event+":"+trail[i].User)
	}
	want := []string{
		"login.failed:dana", "login.failed:mallory",
		"login:dana", "login:sam",
		"deploy:dana", "deploy:sam", "deploy.refused:sam",
		"rollback:dana", "inject:dana", "logout:sam",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("trail:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	byEvent := map[string]audit.Entry{}
	for _, en := range trail {
		if en.Remote == "" {
			t.Errorf("%s has no remote address", en.Event)
		}
		if _, seen := byEvent[en.Event+en.User]; !seen {
			byEvent[en.Event+en.User] = en
		}
	}
	if f := byEvent["login.faileddana"]; f.Detail["knownUser"] != true || f.ForwardedFor != "203.0.113.9" || strings.Contains(f.Remote, "203.0.113.9") {
		t.Errorf("failed login for a real user: %+v", f)
	}
	if f := byEvent["login.failedmallory"]; f.Detail["knownUser"] != false {
		t.Errorf("failed login for an unknown user: %+v", f)
	}
	if d := byEvent["deploydana"]; d.Detail["deployment"] != float64(first) || d.Detail["rev"] == "" {
		t.Errorf("deploy entry: %+v", d)
	}
	if r := byEvent["rollbackdana"]; r.Detail["to"] != float64(first) || !strings.Contains(r.Detail["note"].(string), "two broke it") {
		t.Errorf("rollback entry: %+v", r)
	}
	if i := byEvent["injectdana"]; i.Detail["node"] != "c1" {
		t.Errorf("inject entry: %+v", i)
	}
	if r := byEvent["deploy.refusedsam"]; r.Detail["reason"] != "stale revision" {
		t.Errorf("refused deploy entry: %+v", r)
	}

	// Filters.
	if logins := e.auditTrail(t, dana, "?event=login."); len(logins) != 2 {
		t.Errorf("?event=login. gave %d entries", len(logins))
	}
	if mine := e.auditTrail(t, dana, "?user=sam&limit=2"); len(mine) != 2 || mine[0].Event != audit.Logout {
		t.Errorf("?user=sam&limit=2 gave %+v", mine)
	}
	if code, _ := e.call(t, "GET", "/audit?since=yesterday", dana, nil); code != http.StatusBadRequest {
		t.Errorf("a bad since: %d", code)
	}
}

// Reading the trail is its own permission. Being able to deploy doesn't mean
// being able to read who else logged in.
func TestReadingTheTrailNeedsAuditRead(t *testing.T) {
	app, _ := newApp(t)
	e := serveApp(t, app, map[string][]string{"dana": {"*"}, "sam": {"flows.*", "status.read"}, "auditor": {"audit.read"}})
	if code, _ := e.call(t, "GET", "/audit", e.login(t, "sam"), nil); code != http.StatusForbidden {
		t.Errorf("sam read the audit trail: %d", code)
	}
	if code, _ := e.call(t, "GET", "/audit", e.login(t, "auditor"), nil); code != http.StatusOK {
		t.Errorf("the auditor couldn't: %d", code)
	}
}
