package nodes

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// The postgreSQLConfig the SBF flows carry, with the plant's address and
// password swapped for test ones.
const sbfPostgresConfig = `{"name":"","host":"192.0.2.10","hostFieldType":"str","port":5432,"portFieldType":"num",
"database":"industrial_data","databaseFieldType":"str","ssl":"false","sslFieldType":"bool","applicationName":"",
"applicationNameType":"str","max":10,"maxFieldType":"num","idle":1000,"idleFieldType":"num","connectionTimeout":10000,
"connectionTimeoutFieldType":"num","user":"admin","userFieldType":"str","password":"test-password","passwordFieldType":"str"}`

func TestPostgreSQLConfigAcceptsTheShapeTheFlowsCarry(t *testing.T) {
	svc := newConfigServices()
	c := buildConfig(t, "postgreSQLConfig", "pg", sbfPostgresConfig, svc).(*nrPostgresConfig)
	if !strings.Contains(c.dsn, "host=192.0.2.10") || !strings.Contains(c.dsn, "dbname=industrial_data") ||
		!strings.Contains(c.dsn, "sslmode=disable") || !strings.Contains(c.dsn, "password=test-password") {
		t.Fatalf("dsn = %s", strings.ReplaceAll(c.dsn, "test-password", "***"))
	}
	if c.maxConns != 10 {
		t.Fatalf("max = %d", c.maxConns)
	}
}

func TestPostgreSQLConfigReadsEnvironmentVariablesAndCredentials(t *testing.T) {
	svc := newConfigServices()
	svc.env["PGHOST_X"] = "db.internal"
	svc.creds["password"] = "from-credential"
	c := buildConfig(t, "postgreSQLConfig", "pg",
		`{"host":"PGHOST_X","hostFieldType":"env","database":"d","user":"u","password":"in-the-file","ssl":"true","sslFieldType":"bool"}`, svc).(*nrPostgresConfig)
	for _, want := range []string{"host=db.internal", "password=from-credential", "sslmode=verify-full"} {
		if !strings.Contains(c.dsn, want) {
			t.Errorf("dsn lacks %s: %s", want, strings.ReplaceAll(c.dsn, "from-credential", "***"))
		}
	}
	reg, _ := node.Default.Lookup("postgreSQLConfig")
	_, err := reg.New(&node.Definition{Node: &engine.Node{ID: "x", Type: "postgreSQLConfig", IsConfig: true,
		Raw: map[string]any{"host": "msg.host", "hostFieldType": "msg", "database": "d", "user": "u"}}, Services: svc})
	if err == nil || !strings.Contains(err.Error(), "host has type") {
		t.Fatalf("a msg-typed host should be refused clearly, got %v", err)
	}
	_, err = reg.New(&node.Definition{Node: &engine.Node{ID: "x", Type: "postgreSQLConfig", IsConfig: true,
		Raw: map[string]any{"host": "h", "database": "", "user": "u"}}, Services: svc})
	if err == nil {
		t.Fatal("an empty database was accepted")
	}
}

func TestPostgreSQLNodeRendersTheQueryAndPassesParametersSeparately(t *testing.T) {
	si := &recordingStandIn{}
	svc := newConfigServices()
	svc.standIn = si
	n := build(t, "postgresql", `{"postgreSQLConfig":"pg","query":"SELECT * FROM {{{msg.table}}} WHERE id = $1 AND name = $2"}`, svc)
	e, err := send(t, n, msg(t, `{"table":"dryer","params":[7,"o'brien"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(si.calls) != 1 {
		t.Fatalf("stand-in calls = %d", len(si.calls))
	}
	sent := si.calls[0]["sent"].(map[string]any)
	if sent["query"] != "SELECT * FROM dryer WHERE id = $1 AND name = $2" {
		t.Fatalf("query = %v", sent["query"])
	}
	if got := sent["params"].([]any); !reflect.DeepEqual(got, []any{7.0, "o'brien"}) {
		t.Fatalf("params = %#v (values go as parameters, never into the text)", got)
	}
	m := e.on(0)[0]
	if _, ok := m.Payload().([]any); !ok {
		t.Fatalf("payload = %T, want the list of rows", m.Payload())
	}
	if pg := m.Data["pgsql"].(map[string]any); pg["command"] != "SELECT" {
		t.Fatalf("pgsql = %#v", pg)
	}
}

func TestPostgreSQLNodeRefusesBadParamsAndEmptyQueries(t *testing.T) {
	svc := newConfigServices()
	svc.standIn = &recordingStandIn{}
	n := build(t, "postgresql", `{"postgreSQLConfig":"pg","query":"SELECT $1"}`, svc)
	if _, err := send(t, n, msg(t, `{"params":"nope"}`)); err == nil || !strings.Contains(err.Error(), "msg.params") {
		t.Fatalf("string params: %v", err)
	}
	empty := build(t, "postgresql", `{"postgreSQLConfig":"pg","query":""}`, svc)
	if _, err := send(t, empty, msg(t, `{}`)); err == nil {
		t.Fatal("an empty query ran")
	}
	// The placeholder the SBF flows carry parses and fails at the database, not at deploy.
	build(t, "postgresql", `{"postgreSQLConfig":"pg","query":"SELECT * FROM ;","split":false,"rowsPerMsg":1,"outputs":1}`, svc)
}

// ---------------------------------------------------------------------------
// Against a real PostgreSQL
//
//	HOTLOOP_FLOW_TEST_PG=127.0.0.1:15432 go test ./internal/nodes/ -run NodeRedPostgres -v
//
// with user flow, password flowtest, database flowtest (CI and the Beast run
// docker.io/library/postgres:17 with POSTGRES_USER=flow POSTGRES_PASSWORD=flowtest POSTGRES_DB=flowtest).
// ---------------------------------------------------------------------------

func TestNodeRedPostgresAgainstARealDatabase(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_PG")
	host, port := splitHostPort(t, addr)
	svc := newConfigServices()
	cfg := `{"host":"` + host + `","port":` + port + `,"portFieldType":"num","database":"flowtest","user":"flow","password":"flowtest","ssl":"false","max":4}`
	c := buildConfig(t, "postgreSQLConfig", "pg", cfg, svc)
	svc.configs["pg"] = c
	defer c.(node.Closer).Close(context.Background(), true)

	run := func(query, payload string) *testEmitter {
		t.Helper()
		n := build(t, "postgresql", `{"postgreSQLConfig":"pg","query":`+jsonString(query)+`}`, svc)
		e, err := send(t, n, msg(t, payload))
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return e
	}
	run(`DROP TABLE IF EXISTS flx_readings`, `{}`)
	run(`CREATE TABLE flx_readings (id serial PRIMARY KEY, sensor text, value double precision, at timestamptz DEFAULT now())`, `{}`)

	e := run(`INSERT INTO flx_readings (sensor, value) VALUES ($1, $2), ($3, $4)`,
		`{"params":["dryer-1",151.5,"dryer-2",149.25]}`)
	if pg := e.on(0)[0].Data["pgsql"].(map[string]any); pg["command"] != "INSERT" || pg["rowCount"] != 2.0 {
		t.Fatalf("insert pgsql = %#v", pg)
	}

	e = run(`SELECT sensor, value FROM flx_readings WHERE sensor = '{{{msg.payload}}}' ORDER BY id`, `{"payload":"dryer-1"}`)
	rows := e.on(0)[0].Payload().([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["value"] != 151.5 || rows[0].(map[string]any)["sensor"] != "dryer-1" {
		t.Fatalf("select rows = %#v", rows)
	}

	// split, two outputs
	n := build(t, "postgresql", `{"postgreSQLConfig":"pg","query":"SELECT sensor FROM flx_readings ORDER BY id","split":true,"rowsPerMsg":1,"outputs":2}`, svc)
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	parts := e.on(0)
	if len(parts) != 2 || parts[1].Payload().(map[string]any)["sensor"] != "dryer-2" {
		t.Fatalf("split gave %d messages", len(parts))
	}
	if p := parts[1].Data["parts"].(map[string]any); p["index"] != 1.0 || p["count"] != 2.0 {
		t.Fatalf("parts = %#v", p)
	}
	if len(e.on(1)) != 1 {
		t.Fatal("no completion message on the second output")
	}

	// A timestamp comes out as text a Switch node can read.
	e = run(`SELECT at FROM flx_readings LIMIT 1`, `{}`)
	if at, ok := e.on(0)[0].Payload().([]any)[0].(map[string]any)["at"].(string); !ok || !strings.Contains(at, "T") {
		t.Fatalf("timestamp = %#v", e.on(0)[0].Payload())
	}

	// A bad query is an error for a Catch node.
	bad := build(t, "postgresql", `{"postgreSQLConfig":"pg","query":"SELECT * FROM ;"}`, svc)
	if _, err := send(t, bad, msg(t, `{}`)); err == nil {
		t.Fatal("the SBF placeholder query should fail at the database")
	}
	run(`DROP TABLE flx_readings`, `{}`)
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }
