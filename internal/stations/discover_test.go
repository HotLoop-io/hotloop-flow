package stations

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The dashboard's list as it answers for a tenant, one row of each kind a real
// answer carries: two Flow installs of the tenant (one on Flux, one with a cluster
// address only), one that is not running, one of another tenant that arrives
// anyway, and an app that is not Flow.
const dashboardRows = `[
 {"name":"flow-a","namespace":"tenant-acme","appName":"hotloop-flow","tenantID":"acme","guiPort":8080,"helmRelease":"flow-a","running":true,
  "access":{"clusterIP":"10.43.0.10","ports":[{"port":8080}],"flux":{"host":"flow-a-acme-cp-01.flux.internal","port":8080,"status":"online"}}},
 {"name":"flow-b","namespace":"tenant-acme","appName":"hotloop-flow","tenantID":"acme","guiPort":8080,"helmRelease":"flow-b","running":true,
  "access":{"clusterIP":"10.43.0.11","ports":[{"port":8080}]}},
 {"name":"flow-c","namespace":"tenant-acme","appName":"hotloop-flow","tenantID":"acme","guiPort":8080,"running":false,"access":{"clusterIP":"10.43.0.12"}},
 {"name":"flow-x","namespace":"tenant-other","appName":"hotloop-flow","tenantID":"other","guiPort":8080,"helmRelease":"flow-x","running":true,"access":{"clusterIP":"10.43.9.9"}},
 {"name":"grafana","namespace":"tenant-acme","appName":"grafana","tenantID":"acme","guiPort":3000,"running":true,"access":{"clusterIP":"10.43.0.13"}}
]`

func dashboard(t *testing.T, status int, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	var seen http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = *r.Clone(r.Context())
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// Stations come from what EmberNet shows for the tenant: Flow only, running
// only, Flux first, and a row of another tenant is dropped and counted.
func TestDiscoverKeepsTheTenantsRunningFlowInstalls(t *testing.T) {
	srv, seen := dashboard(t, 200, dashboardRows)
	d, err := Discover(context.Background(), nil, srv.URL, "tok", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Stations) != 2 || d.OtherTenants != 1 {
		t.Fatalf("%+v", d)
	}
	a, b := d.Stations[0], d.Stations[1]
	if a.Name != "flow-a" || a.URL != "http://flow-a-acme-cp-01.flux.internal:8080" || a.Reach != "flux" {
		t.Errorf("a = %+v", a)
	}
	if b.Name != "flow-b" || b.URL != "http://10.43.0.11:8080" || b.Reach != "cluster" {
		t.Errorf("b = %+v", b)
	}
	if seen.URL.Path != "/api/apps/services" || seen.URL.Query().Get("tenant") != "acme" || seen.Header.Get("Authorization") != "Bearer tok" {
		t.Errorf("asked %s with %q", seen.URL, seen.Header.Get("Authorization"))
	}
}

// A station of another tenant is never used, even when it is the only one the
// dashboard sends, and a tenant is always named.
func TestDiscoverNeverUsesAnotherTenantsStation(t *testing.T) {
	other := `[{"name":"flow-x","namespace":"tenant-other","appName":"hotloop-flow","tenantID":"other","guiPort":8080,"helmRelease":"flow-x","running":true,"access":{"clusterIP":"10.43.9.9"}}]`
	srv, _ := dashboard(t, 200, other)
	d, err := Discover(context.Background(), nil, srv.URL, "tok", "acme")
	if err == nil || len(d.Stations) != 0 || d.OtherTenants != 1 {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := Discover(context.Background(), nil, srv.URL, "tok", "  "); err == nil || !strings.Contains(err.Error(), "no tenant") {
		t.Errorf("no tenant: %v", err)
	}
	if _, err := Discover(context.Background(), nil, srv.URL, "", "acme"); err == nil {
		t.Error("no dashboard token was accepted")
	}
}

func TestDiscoverSaysWhatTheDashboardRefused(t *testing.T) {
	for _, c := range []struct {
		code int
		want string
	}{{401, "did not accept the token"}, {403, "may not see tenant"}, {500, "answered 500"}} {
		srv, _ := dashboard(t, c.code, "no")
		if _, err := Discover(context.Background(), nil, srv.URL, "tok", "acme"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%d: %v", c.code, err)
		}
	}
}

func TestAWindowHoldsTheHoursItNames(t *testing.T) {
	w, err := ParseWindow("02:00-04:00", "")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 11, 19, h, m, 0, 0, time.UTC) }
	if !w.Contains(at(2, 0)) || !w.Contains(at(3, 59)) || w.Contains(at(4, 0)) || w.Contains(at(1, 59)) {
		t.Error("02:00-04:00 holds the wrong hours")
	}
	night, _ := ParseWindow("22:00-02:00", "")
	if !night.Contains(at(23, 0)) || !night.Contains(at(1, 0)) || night.Contains(at(12, 0)) {
		t.Error("a window past midnight holds the wrong hours")
	}
	zoned, err := ParseWindow("02:00-04:00", "America/New_York")
	if err != nil || !zoned.Contains(time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("the zone is not used: %v", err)
	}
	var none *Window
	if none.Contains(at(3, 0)) {
		t.Error("no window held a time")
	}
	for _, bad := range []string{"2-4", "02:00", "25:00-26:00", "03:00-03:00"} {
		if _, err := ParseWindow(bad, ""); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
