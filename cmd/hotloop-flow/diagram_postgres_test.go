package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HotLoop-io/hotloop-flow/internal/diagram"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// B7 and F5: a database that stops accepting loses nothing. A diagram writes a
// row for every pass of a loop into a real PostgreSQL through a connection that
// is cut for a while in the middle of the run. Every row arrives, in order, and a
// row that waited on disk keeps the time it was made.
func TestADiagramLosesNoRowsAcrossADatabaseOutage(t *testing.T) {
	pgOutageRun(t, pgOutage{total: 300, paceMs: 20, outage: 4 * time.Second, cutAfter: 40})
}

// The same, and one row in the backlog is one the database refuses for a key that
// is already there. It is let go with an audit event that carries it, and the rows
// behind it are written: one bad row never stops the log.
func TestADiagramLetsGoOfARowTheDatabaseRefuses(t *testing.T) {
	pgOutageRun(t, pgOutage{total: 300, paceMs: 20, outage: 4 * time.Second, cutAfter: 40, poison: 150})
}

// The size the spec names: a database down for five minutes at 100 samples a
// second leaves 30,000 rows waiting on disk, and they all arrive, in order, in the
// time a plant would call catching up, not the time one round trip per row would
// take. The queue is filled the way a process that died leaves it, rows on disk
// with the times they were made and nothing running, and the pump of the diagram
// that owns it does the rest.
func TestADiagramDrainsAThirtyThousandRowBacklog(t *testing.T) {
	pgOutageRun(t, pgOutage{total: 30000, outage: 2 * time.Second, preload: true, drainWithin: 60 * time.Second})
}

type pgOutage struct {
	total       int
	paceMs      int           // wait between passes, 0 for none
	outage      time.Duration // how long the link stays cut
	cutAfter    int           // rows to see arrive before the cut; 0 cuts before the run starts
	preload     bool          // fill the queue on disk with every row instead of running the diagram
	poison      int           // a pass whose row the table already holds, or 0 for none
	drainWithin time.Duration // when set, the backlog must be written this soon after the link is back
}

func pgOutageRun(t *testing.T, o pgOutage) {
	addr := os.Getenv("HOTLOOP_FLOW_TEST_PG")
	if addr == "" {
		t.Skip("set HOTLOOP_FLOW_TEST_PG=host:port to run this against a real PostgreSQL (user flow, password flowtest, database flowtest)")
	}
	total := o.total
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	table := "flowlab_rows_" + run

	ctx := context.Background()
	hostPort := strings.Split(addr, ":")
	conn, err := pgx.Connect(ctx, "host="+hostPort[0]+" port="+hostPort[1]+" dbname=flowtest user=flow password=flowtest sslmode=disable")
	if err != nil {
		t.Fatalf("connecting the test client to %s: %v", addr, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE TABLE `+table+` (id bigserial PRIMARY KEY, n int UNIQUE, sensor text, made_at timestamptz, inserted_at timestamptz DEFAULT clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `DROP TABLE `+table)
	if o.poison > 0 {
		if _, err := conn.Exec(ctx, `INSERT INTO `+table+` (n, sensor) VALUES ($1, 'already here')`, o.poison); err != nil {
			t.Fatal(err)
		}
	}

	g := newGate(t, addr)
	host, port, _ := net.SplitHostPort(g.addr)

	app := newTestApp(t)
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "state.db"), store.SQLiteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	spool, err := store.OpenSpool(db, store.SpoolOptions{AckFlush: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	var auditMu sync.Mutex
	var audited []map[string]any
	app.diagrams = diagram.NewManager(nil)
	defer app.diagrams.Close()
	app.diagrams.SetServices(diagram.Services{
		ConfigNode: func(id string) (node.Node, bool) { return app.currentRuntime().ConfigNode(id) },
		Spool:      spool,
		Audit: func(event, who string, detail map[string]any) {
			if event == diagram.AuditRowRejected {
				auditMu.Lock()
				audited = append(audited, detail)
				auditMu.Unlock()
			}
		},
	})

	pace := ""
	if o.paceMs > 0 {
		pace = fmt.Sprintf(`,
  {"id":"pace","type":"dia-wait","z":"body","x":300,"y":300,"ms":%d,"wires":[]}`, o.paceMs)
	}
	flow := fmt.Sprintf(`[
  {"id":"pg","type":"hotloop-flow-postgres","host":%q,"port":%s,"database":"flowtest","user":"flow","sslmode":"disable","maxConns":2,"credentials":{"password":"flowtest"}},
  {"id":"main","type":"tab","label":"Rows","ew_diagram":{}},
  {"id":"body","type":"tab","label":"Rows body","ew_diagram":{"role":"body"}},
  {"id":"loop","type":"dia-while","z":"main","x":200,"y":100,"body":"Rows body","carried":"","ins":"","outs":"","start":"{}","maxIterations":0,"wires":[]},
  {"id":"iter","type":"dia-body-in","z":"body","x":100,"y":100,"name":"iter","ew_in":[["n","a"],[]],"wires":[["bun","last"],[]]},
  {"id":"sens","type":"dia-const","z":"body","x":100,"y":150,"value":"\"dryer-1\"","ew_in":[["sensor"]],"wires":[["bun"]]},
  {"id":"bun","type":"dia-bundle","z":"body","x":300,"y":100,"names":"n,sensor","ew_in":[["row"],[]],"wires":[["out"],[]]},
  {"id":"out","type":"dia-postgres-out","z":"body","x":500,"y":100,"server":"pg","table":%q,"columns":"n,sensor","timeColumn":"made_at","durable":true,"wires":[]},
  {"id":"last","type":"dia-compare","z":"body","x":300,"y":200,"op":"ge","ew_in":[["value"],[]],"wires":[["stop"],[]]},
  {"id":"top","type":"dia-const","z":"body","x":100,"y":200,"value":"%%d","ew_in":[["b"],[]],"wires":[["last"],[]]},
  {"id":"stop","type":"dia-body-out","z":"body","x":500,"y":200,"name":"stop","wires":[]}%s
]`, host, port, table, pace)
	flow = fmt.Sprintf(flow, total-1)
	deployJSON(t, app, flow, runtime.DeployFull)

	d, _ := app.diagrams.Diagram("main")
	if len(d.Problems) > 0 {
		t.Fatalf("the diagram has problems: %v", d.Problems)
	}
	count := func() int {
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE sensor = 'dryer-1'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if o.cutAfter == 0 {
		g.cut()
	}
	if o.preload {
		fillPgBacklog(t, spool.Wire("dia:body/out"), table, total)
	} else if _, err := app.diagrams.Start("main", diagram.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	if o.cutAfter > 0 {
		eventually(t, "rows to arrive before the cut", 20*time.Second, func() bool { return count() >= o.cutAfter })
		g.cut() // the database goes away
	}
	cutAt := time.Now()
	time.Sleep(o.outage)
	if o.cutAfter == 0 && !o.preload {
		// The whole run is written to disk while the database is away.
		eventually(t, "the diagram to finish while the database is away", 120*time.Second, func() bool {
			r := app.diagrams.Current("main")
			if r == nil {
				return false
			}
			st, _ := r.State()
			return string(st) == "done"
		})
	}
	g.restore(t)
	restoredAt := time.Now()
	if !o.preload {
		eventually(t, "the diagram to finish", 120*time.Second, func() bool {
			r := app.diagrams.Current("main")
			if r == nil {
				return false
			}
			st, _ := r.State()
			return string(st) == "done"
		})
	}
	want := total
	if o.poison > 0 {
		want = total - 1 // the refused row is not in the table, the one that was already there is
	}
	eventually(t, "every row to arrive", 120*time.Second, func() bool { return count() >= want })
	arrived := time.Since(restoredAt)
	if o.drainWithin > 0 && arrived > o.drainWithin {
		t.Fatalf("the backlog took %s to write after the database was back, want under %s", arrived, o.drainWithin)
	}
	time.Sleep(500 * time.Millisecond)

	rows, err := conn.Query(ctx, `SELECT n, sensor, made_at, inserted_at FROM `+table+` WHERE sensor = 'dryer-1' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	next, held := 0, 0
	var prevMade time.Time
	for rows.Next() {
		var n int
		var sensor string
		var made, ins time.Time
		if err := rows.Scan(&n, &sensor, &made, &ins); err != nil {
			t.Fatal(err)
		}
		if next == o.poison && o.poison > 0 {
			next++ // that pass's row is the one the table already held
		}
		if n != next {
			t.Fatalf("out of order or missing: got %d, want %d", n, next)
		}
		next++
		if made.Before(prevMade) {
			t.Fatalf("row %d was made at %s, before the row ahead of it at %s", n, made, prevMade)
		}
		prevMade = made
		// A row that went to disk was written after the database came back and was
		// still stamped with the time it was made, while it was away.
		if ins.After(restoredAt) && made.Before(restoredAt) {
			held++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if next != total {
		t.Fatalf("got rows up to %d, want %d", next, total)
	}
	if held == 0 {
		t.Fatalf("no row kept the time it was made across the outage")
	}
	direct, spilled, drained, depth, ok := app.diagrams.OutboxStats("body", "out")
	if o.preload {
		spilled = int64(total) // they were put on disk by the fill, not by a run
	}
	if !ok || spilled == 0 || drained == 0 || depth != 0 {
		t.Fatalf("outbox: ok=%v direct=%d spilled=%d drained=%d depth=%d; the outage should have spilled to disk and drained", ok, direct, spilled, drained, depth)
	}
	rejected := app.diagrams.OutboxRejected("body", "out")
	auditMu.Lock()
	events := append([]map[string]any(nil), audited...)
	auditMu.Unlock()
	if o.poison > 0 {
		if rejected != 1 || len(events) != 1 {
			t.Fatalf("rejected %d, audited %d; want the one row the table already held", rejected, len(events))
		}
		if row, _ := events[0]["row"].(string); !strings.Contains(row, "150") {
			t.Fatalf("the audit event does not carry the row: %v", events[0])
		}
	} else if rejected != 0 || len(events) != 0 {
		t.Fatalf("rejected %d, audited %d; nothing should have been refused", rejected, len(events))
	}
	t.Logf("%d straight in, %d through disk, %d kept their own time, %d refused, all written %s after the link was back (cut at %s)", direct, spilled, held, rejected, arrived.Round(time.Millisecond), cutAt.Format("15:04:05"))
}

// fillPgBacklog puts total rows on a durable output's queue, the way a run that
// was cut off from its database leaves them: oldest first, each stamped with the
// time it was made, the first five minutes back and one every ten milliseconds
// after. The sequence numbers are taken in order and the rows are written in
// parallel, so the order of the queue is the order of the passes.
func fillPgBacklog(t *testing.T, q *store.WireQueue, table string, total int) {
	t.Helper()
	start := time.Now().UTC().Add(-5 * time.Minute)
	seqs := make([]uint64, total)
	for i := range seqs {
		seqs[i] = q.AllocSeq()
	}
	errs := make(chan error, 32)
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < total; i += 32 {
				env := map[string]any{
					"table": table, "columns": []string{"n", "sensor"}, "values": []any{i, "dryer-1"},
					"timeColumn": "made_at", "time": start.Add(time.Duration(i) * 10 * time.Millisecond), "onConflict": "error",
				}
				body, _ := json.Marshal(env)
				if _, err := q.AppendSeq(seqs[i], body); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("filling the queue: %v", err)
	default:
	}
}
