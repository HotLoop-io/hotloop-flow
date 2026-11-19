package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/diagram"
	"github.com/HotLoop-io/hotloop-flow/internal/stations"
)

// A logger that starts by itself and runs until it is stopped: a loop whose body
// waits. ms is how long it waits, so a different ms is a different logger.
func loggerFile(ms int) string {
	return fmt.Sprintf(`[
 {"id":"log","type":"tab","label":"Logger","ew_diagram":{"autostart":true}},
 {"id":"body","type":"tab","label":"Logger body","ew_diagram":{"role":"body"}},
 {"id":"loop","type":"dia-while","z":"log","x":200,"y":100,"body":"Logger body","carried":"","ins":"","outs":"","start":"{}","maxIterations":0,"wires":[]},
 {"id":"iter","type":"dia-body-in","z":"body","x":100,"y":100,"name":"iter","wires":[["pace"]]},
 {"id":"pace","type":"dia-wait","z":"body","x":300,"y":100,"ms":%d,"ew_in":[["value"]],"wires":[]},
 {"id":"no","type":"dia-const","z":"body","x":100,"y":200,"value":"false","wires":[["stop"]],"ew_in":[["value"]]},
 {"id":"stop","type":"dia-body-out","z":"body","x":300,"y":200,"name":"stop","wires":[]}
]`, ms)
}

// station is one real Flow instance, in this process, with diagrams on or off
// (a station that cannot run diagrams is the stand-in for an older install).
type station struct {
	*e2e
	app *application
}

// stationToken is a real deploy token, made the way hotloop-flow token makes
// one, that every station in these tests accepts.
var stationToken, stationHash = func() (string, string) {
	tok, hash, err := config.NewToken()
	if err != nil {
		panic(err)
	}
	return tok, hash
}()

func newStation(t *testing.T, diagrams bool) station {
	t.Helper()
	app, _ := newApp(t)
	if diagrams {
		app.diagrams = diagram.NewManager(nil)
		t.Cleanup(app.diagrams.Close)
	}
	app.cfg.Auth.Tokens = []config.Token{{Name: "push", Hash: stationHash, Permissions: []string{"flows.read", "flows.write", "status.read"}}}
	e := serveAppWith(t, app, map[string][]string{"admin": {"*"}}, func(d *api.Deps) {
		if diagrams {
			d.Diagrams = app.diagrams
		} else {
			d.Diagrams = nil
		}
	})
	return station{e, app}
}

func (s station) deploy(t *testing.T, doc string) {
	t.Helper()
	if code, out := s.call(t, "POST", "/flows", stationToken, []byte(doc)); code != http.StatusOK {
		t.Fatalf("deploy: %d %s", code, out)
	}
}

func (s station) rev() string { return s.app.flowStore.Rev() }

func (s station) st(name string) stations.Station {
	return stations.Station{Name: name, URL: s.URL, Reach: "cluster"}
}

func wantRev(t *testing.T, doc string) string {
	t.Helper()
	rep, err := stations.Push(context.Background(), stations.Options{
		Stations: []stations.Station{{Name: "none", URL: "http://127.0.0.1:1"}}, Flows: []byte(doc), Token: "x", DryRun: true, Settle: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rep.Rev
}

func push(t *testing.T, doc string, stns []stations.Station, adjust func(*stations.Options)) stations.Report {
	t.Helper()
	o := stations.Options{Tenant: "acme", Stations: stns, Flows: []byte(doc), Token: stationToken, Settle: 3 * time.Second, Note: "test push"}
	if adjust != nil {
		adjust(&o)
	}
	rep, err := stations.Push(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func statusOf(rep stations.Report) string {
	var parts []string
	for _, r := range rep.Results {
		parts = append(parts, r.Name+"="+string(r.Status))
	}
	return strings.Join(parts, " ")
}

// Three real stations on one logger, and a push of a changed file that does not
// restart it: every station ends on the same revision, in the order the list
// gave, and the report says so.
func TestAPushPutsOneFileOnEveryStation(t *testing.T) {
	a, b, c := newStation(t, true), newStation(t, true), newStation(t, true)
	v1, v2 := loggerFile(40), strings.Replace(loggerFile(40), `"x":300`, `"x":340`, 1)
	for _, s := range []station{a, b, c} {
		s.deploy(t, v1)
	}
	rep := push(t, v2, []stations.Station{a.st("a"), b.st("b"), c.st("c")}, nil)
	if !rep.OK() || statusOf(rep) != "a=done b=done c=done" {
		t.Fatalf("%s\n%+v", statusOf(rep), rep)
	}
	want := wantRev(t, v2)
	for i, s := range []station{a, b, c} {
		if s.rev() != want || rep.Results[i].Rev != want || rep.Results[i].Deployment == 0 {
			t.Errorf("station %d: rev %s, report %+v, want %s", i, s.rev(), rep.Results[i], want)
		}
	}
	// A second push of the same file has nothing to do.
	again := push(t, v2, []stations.Station{a.st("a"), b.st("b"), c.st("c")}, nil)
	if !again.OK() || statusOf(again) != "a=unchanged b=unchanged c=unchanged" {
		t.Fatalf("a second push: %s", statusOf(again))
	}
}

// The middle station cannot run the diagram: it fails the golden check, goes back
// to the deployment it was on, and the push stops. The first is done, the third
// was never started and is as it was.
func TestAStationThatFailsTheCheckIsPutBackAndThePushStops(t *testing.T) {
	a, b, c := newStation(t, true), newStation(t, false), newStation(t, true)
	v1, v2 := loggerFile(40), loggerFile(50)
	for _, s := range []station{a, b, c} {
		s.deploy(t, v1)
	}
	oldB, oldC := b.rev(), c.rev()
	rep := push(t, v2, []stations.Station{a.st("a"), b.st("b"), c.st("c")}, func(o *stations.Options) {
		o.Confirm = true // the logger restarts; that is agreed here
		o.Settle = 500 * time.Millisecond
	})
	if statusOf(rep) != "a=done b=reverted c=not-started" || !rep.Stopped || rep.OK() {
		t.Fatalf("%s\n%+v", statusOf(rep), rep)
	}
	want := wantRev(t, v2)
	if a.rev() != want {
		t.Errorf("the first station is on %s, want %s", a.rev(), want)
	}
	if b.rev() != oldB {
		t.Errorf("the failed station is on %s, want its old %s", b.rev(), oldB)
	}
	if c.rev() != oldC {
		t.Errorf("the station never started is on %s, want its old %s", c.rev(), oldC)
	}
	if !strings.Contains(rep.Results[1].Detail, "not on the station") {
		t.Errorf("the report does not say why: %q", rep.Results[1].Detail)
	}
	// Going back is a deployment of its own and the log keeps both.
	if got := b.list(t, stationToken).Deployments; len(got) < 3 || got[0].Kind != "rollback" {
		t.Errorf("the failed station's log: %+v", got)
	}
}

// A push that would restart a running logger changes nothing until somebody says
// it may: not with no word, not with a window that is not now, and then yes with
// the word or with a window that is.
func TestAPushThatWouldRestartALoggerStopsAndAsksFirst(t *testing.T) {
	a, b := newStation(t, true), newStation(t, true)
	v1, v2 := loggerFile(40), loggerFile(60)
	a.deploy(t, v1)
	b.deploy(t, v1)
	waitRunning(t, a)
	waitRunning(t, b)
	oldA, oldB := a.rev(), b.rev()
	stns := []stations.Station{a.st("a"), b.st("b")}

	rep := push(t, v2, stns, nil)
	if !rep.NeedsConfirm || !rep.Stopped || statusOf(rep) != "a=not-started b=not-started" {
		t.Fatalf("%s %+v", statusOf(rep), rep)
	}
	if len(rep.Results[0].Restarts) != 1 || rep.Results[0].Restarts[0] != "Logger" {
		t.Errorf("the report does not name the logger: %+v", rep.Results[0])
	}
	if a.rev() != oldA || b.rev() != oldB {
		t.Fatal("a push that was waiting for a yes changed a station")
	}

	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	out, _ := stations.ParseWindow("02:00-04:00", "")
	rep = push(t, v2, stns, func(o *stations.Options) { o.Window, o.Now = out, func() time.Time { return now } })
	if !rep.NeedsConfirm || a.rev() != oldA {
		t.Fatalf("a window that is not now let it through: %s", statusOf(rep))
	}

	rep = push(t, v2, stns, func(o *stations.Options) { o.DryRun = true })
	if rep.Stopped || statusOf(rep) != "a=planned b=planned" || a.rev() != oldA {
		t.Fatalf("a dry run: %s", statusOf(rep))
	}

	asked := 0
	rep = push(t, v2, stns, func(o *stations.Options) {
		o.Ask = func(r []stations.StationResult) bool { asked = len(r); return false }
	})
	if asked != 2 || !rep.NeedsConfirm || a.rev() != oldA {
		t.Fatalf("asked about %d stations, said no, and %s", asked, statusOf(rep))
	}

	in, _ := stations.ParseWindow("11:00-13:00", "")
	rep = push(t, v2, stns, func(o *stations.Options) { o.Window, o.Now = in, func() time.Time { return now } })
	if !rep.OK() || statusOf(rep) != "a=done b=done" {
		t.Fatalf("a window that holds now: %s", statusOf(rep))
	}
	waitRunning(t, a)
	if a.rev() == oldA || a.rev() != wantRev(t, v2) {
		t.Errorf("the station did not take the file: %s", a.rev())
	}
}

// A station that does not answer is skipped and the rest are done, and the report
// says which. One with no record of its live revision is skipped too, because a
// failed push could not be undone there.
func TestAStationThatDoesNotAnswerIsSkippedAndTheRestAreDone(t *testing.T) {
	a, c := newStation(t, true), newStation(t, true)
	v1, v2 := loggerFile(40), strings.Replace(loggerFile(40), `"x":300`, `"x":340`, 1)
	a.deploy(t, v1)
	c.deploy(t, v1)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	rep := push(t, v2, []stations.Station{a.st("a"), {Name: "dead", URL: deadURL}, c.st("c")}, nil)
	if statusOf(rep) != "a=done dead=skipped c=done" || rep.Stopped {
		t.Fatalf("%s", statusOf(rep))
	}
	if !strings.Contains(rep.Results[1].Detail, "did not answer") {
		t.Errorf("detail: %q", rep.Results[1].Detail)
	}
	if rep.OK() == false {
		t.Error("skipped stations are not a failed push")
	}

	fresh := newStation(t, true) // never deployed to by the API: no record of what it runs
	rep = push(t, v2, []stations.Station{fresh.st("fresh"), a.st("a")}, nil)
	if rep.Results[0].Status != stations.Skipped || !strings.Contains(rep.Results[0].Detail, "no deployment record") {
		t.Fatalf("%s %q", statusOf(rep), rep.Results[0].Detail)
	}
}

// The station routes and tools: the list comes from the dashboard for the
// configured tenant, a request cannot name another, a push reports every station,
// and the routes are not there at all without the settings.
func TestTheStationRoutesPushForTheConfiguredTenant(t *testing.T) {
	a, b := newStation(t, true), newStation(t, true)
	v1, v2 := loggerFile(40), strings.Replace(loggerFile(40), `"x":300`, `"x":340`, 1)
	a.deploy(t, v1)
	b.deploy(t, v1)
	rows := fmt.Sprintf(`[
 {"name":"flow-a","appName":"hotloop-flow","tenantID":"acme","guiPort":8080,"helmRelease":"flow-a","running":true,"access":{"flux":{"host":%q,"port":%d}}},
 {"name":"flow-b","appName":"hotloop-flow","tenantID":"acme","guiPort":8080,"helmRelease":"flow-b","running":true,"access":{"flux":{"host":%q,"port":%d}}},
 {"name":"flow-z","appName":"hotloop-flow","tenantID":"other","guiPort":8080,"helmRelease":"flow-z","running":true,"access":{"clusterIP":"10.9.9.9"}}]`,
		hostOf(a.URL), portOf(a.URL), hostOf(b.URL), portOf(b.URL))
	var asked string
	dash := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RawQuery
		if r.Header.Get("Authorization") != "Bearer dash-token" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(rows))
	}))
	t.Cleanup(dash.Close)

	hub := newStation(t, true)
	hub.app.cfg.Stations = config.Stations{
		DashboardURL: dash.URL, Tenant: "acme",
		DashboardTokenFile: writeFile(t, t.TempDir(), "dash.token", "dash-token\n"),
		DeployTokenFile:    writeFile(t, t.TempDir(), "deploy.token", stationToken+"\n"),
	}
	srv := serveAppWith(t, hub.app, map[string][]string{"admin": {"*"}, "reader": {"flows.read"}}, func(d *api.Deps) {
		d.Stations = hub.app.stationsService()
	})
	admin, reader := srv.login(t, "admin"), srv.login(t, "reader")

	code, out := srv.call(t, "GET", "/stations", reader, nil)
	var d stations.Discovery
	if code != 200 || json.Unmarshal(out, &d) != nil || len(d.Stations) != 2 || d.OtherTenants != 1 || asked != "tenant=acme" {
		t.Fatalf("%d %s (asked %q)", code, out, asked)
	}
	if code, _ := srv.call(t, "POST", "/stations/push", reader, []byte(`{}`)); code != http.StatusForbidden {
		t.Errorf("a reader pushed: %d", code)
	}
	body, _ := json.Marshal(map[string]any{"flows": json.RawMessage(v2), "note": "from a route"})
	code, out = srv.call(t, "POST", "/stations/push", admin, body)
	var rep struct {
		stations.Report
		OK bool `json:"ok"`
	}
	if code != 200 || json.Unmarshal(out, &rep) != nil || !rep.OK || len(rep.Results) != 2 || rep.Tenant != "acme" {
		t.Fatalf("%d %s", code, out)
	}
	if a.rev() != wantRev(t, v2) || b.rev() != wantRev(t, v2) {
		t.Error("the stations did not take the file")
	}

	bare := newStation(t, true)
	if code, _ := bare.call(t, "GET", "/stations", stationToken, nil); code != http.StatusNotFound {
		t.Errorf("without the settings the route answered %d", code)
	}
}

func hostOf(u string) string { return strings.Split(strings.TrimPrefix(u, "http://"), ":")[0] }
func portOf(u string) int {
	var p int
	fmt.Sscanf(strings.Split(strings.TrimPrefix(u, "http://"), ":")[1], "%d", &p)
	return p
}

func waitRunning(t *testing.T, s station) {
	t.Helper()
	eventually(t, "the logger to run", 10*time.Second, func() bool {
		r := s.app.diagrams.Current("log")
		if r == nil {
			return false
		}
		st, _ := r.State()
		return string(st) == "running"
	})
}

// The command asks the dashboard, prints the list, and its exit status says how a
// push ended: 4 when it is waiting for a yes, 0 when it is done.
func TestTheStationsCommand(t *testing.T) {
	a := newStation(t, true)
	v1, v2 := loggerFile(40), loggerFile(70)
	a.deploy(t, v1)
	rows := fmt.Sprintf(`[{"name":"flow-a","appName":"hotloop-flow","tenantID":"acme","guiPort":8080,"helmRelease":"flow-a","running":true,"access":{"flux":{"host":%q,"port":%d}}}]`, hostOf(a.URL), portOf(a.URL))
	dash := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(rows)) }))
	t.Cleanup(dash.Close)
	dir := t.TempDir()
	dtok := writeFile(t, dir, "dash.token", "dash\n")
	stok := writeFile(t, dir, "station.token", stationToken+"\n")
	file := writeFile(t, dir, "flows.json", v2)
	common := []string{"-dashboard", dash.URL, "-dashboard-token-file", dtok, "-tenant", "acme"}

	var out strings.Builder
	if err := cmdStations(append([]string{"list"}, common...), &out); err != nil || !strings.Contains(out.String(), "flow-a\t"+a.URL) {
		t.Fatalf("list: %v %q", err, out.String())
	}
	if err := cmdStations([]string{"list", "-dashboard", dash.URL, "-dashboard-token-file", dtok}, &out); err == nil || !strings.Contains(err.Error(), "no tenant") {
		t.Errorf("without a tenant: %v", err)
	}
	push := append([]string{"push", "-file", file, "-token-file", stok, "-settle", "3s"}, common...)
	out.Reset()
	var code exitCode
	oldRev := a.rev()
	if err := cmdStations(push, &out); !errors.As(err, &code) || code != 4 || a.rev() != oldRev {
		t.Fatalf("waiting for a yes: %v %q", err, out.String())
	}
	out.Reset()
	if err := cmdStations(append(push, "-confirm-restart"), &out); err != nil || a.rev() != wantRev(t, v2) || !strings.Contains(out.String(), "done") {
		t.Fatalf("confirmed: %v %q", err, out.String())
	}
}
