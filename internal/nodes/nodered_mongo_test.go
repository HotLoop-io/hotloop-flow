package nodes

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func mongoURI(t *testing.T, cfg string, svc *configServices) string {
	t.Helper()
	return buildConfig(t, "mongodb4-client", "mc", cfg, svc).(*nrMongoClient).uri
}

func TestMongoClientBuildsItsAddressFromTheSimpleTabOrTheURI(t *testing.T) {
	svc := newConfigServices()
	svc.creds["user"], svc.creds["password"] = "app", "p@ss/word"
	got := mongoURI(t, `{"protocol":"mongodb","hostname":"db.example.com","port":"27017","dbName":"plant","authSource":"admin","uriTabActive":"tab-uri-simple"}`, svc)
	if !strings.HasPrefix(got, "mongodb://app:p%40ss%2Fword@db.example.com:27017/plant") || !strings.Contains(got, "authSource=admin") {
		t.Fatalf("uri = %s", strings.ReplaceAll(got, "p%40ss%2Fword", "***"))
	}
	srv := mongoURI(t, `{"protocol":"mongodb+srv","hostname":"cluster.example.net","port":"27017","dbName":"plant"}`, newConfigServices())
	if srv != "mongodb+srv://cluster.example.net/plant" {
		t.Fatalf("srv uri = %s (an srv address takes no port)", srv)
	}
	adv := mongoURI(t, `{"uri":"mongodb://u:p@h1:27017,h2:27017/plant?replicaSet=rs0","uriTabActive":"tab-uri-advanced","dbName":""}`, newConfigServices())
	if !strings.Contains(adv, "replicaSet=rs0") {
		t.Fatalf("advanced uri = %s", adv)
	}
}

// The captured flow keeps the login only in the uri property, on the simple tab.
func TestMongoClientUsesTheStoredURIWhenTheSimpleTabHasNoCredentials(t *testing.T) {
	c := buildConfig(t, "mongodb4-client", "mc", `{"protocol":"mongodb+srv","hostname":"cluster.example.net","port":"","dbName":"sbf",
"appName":"","authSource":"admin","authMechanism":"DEFAULT","tls":false,"connectTimeoutMS":"30000","socketTimeoutMS":"0",
"minPoolSize":"0","maxPoolSize":"100","maxIdleTimeMS":"0","advanced":"{}","uriTabActive":"tab-uri-simple",
"uri":"mongodb+srv://app:test-secret@cluster.example.net/plant?tls=true&authSource=admin&replicaSet=rs0"}`, newConfigServices()).(*nrMongoClient)
	if !strings.Contains(c.uri, "app:test-secret@") || c.db != "sbf" {
		t.Fatalf("uri/db = %s / %s", strings.ReplaceAll(c.uri, "test-secret", "***"), c.db)
	}
	// Only the hosts with neither credentials nor a stored login build from the parts.
	bare := mongoURI(t, `{"protocol":"mongodb+srv","hostname":"cluster.example.net","dbName":"sbf","uri":"mongodb+srv://cluster.example.net/sbf"}`, newConfigServices())
	if strings.Contains(bare, "@") {
		t.Fatalf("a uri with no login was taken for one: %s", bare)
	}
}

func TestMongoClientRefusesWhatItCannotBuild(t *testing.T) {
	reg, _ := node.Default.Lookup("mongodb4-client")
	svc := newConfigServices()
	for name, raw := range map[string]map[string]any{
		"no host":     {"hostname": "", "dbName": "d"},
		"no database": {"hostname": "h", "dbName": ""},
		"bad port":    {"hostname": "h", "port": "abc", "dbName": "d"},
		"bad tab":     {"uriTabActive": "tab-uri-advanced", "uri": ""},
		"bad json":    {"hostname": "h", "dbName": "d", "advanced": "{nope"},
	} {
		if _, err := reg.New(&node.Definition{Node: engineConfig("mc", "mongodb4-client", raw), Services: svc}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestMongoNodeRefusesOperationsItDoesNotProvide(t *testing.T) {
	svc := newConfigServices()
	if err := buildErr(t, "mongodb4", `{"clientNode":"mc","mode":"collection","collection":"c","operation":"bulkWrite"}`, svc); err == nil ||
		!strings.Contains(err.Error(), "bulkWrite") {
		t.Fatalf("bulkWrite: %v", err)
	}
	if err := buildErr(t, "mongodb4", `{"clientNode":"mc","mode":"db","operation":"insertOne"}`, svc); err == nil {
		t.Fatal("database mode was accepted")
	}
	// The shape the SBF flows carry.
	build(t, "mongodb4", `{"clientNode":"mc","mode":"collection","collection":"dryer_sensor_readings","operation":"insertOne","output":"toArray","maxTimeMS":"","handleDocId":false}`, svc)
}

func TestMongoValuesGoInAsAJavaScriptDriverStoresThemAndComeOutAsJSON(t *testing.T) {
	d := toBSON(map[string]any{"n": 5.0, "f": 5.5, "big": 5e12, "neg": -3.0, "list": []any{1.0, "x"}, "sub": map[string]any{"k": 2.0}}).(bson.D)
	got := map[string]any{}
	for _, e := range d {
		got[e.Key] = e.Value
	}
	if got["n"] != int32(5) || got["f"] != 5.5 || got["big"] != 5e12 || got["neg"] != int32(-3) {
		t.Fatalf("numbers = %#v", got)
	}
	oid := bson.NewObjectID()
	out := fromBSON(bson.M{"_id": oid, "at": bson.NewDateTimeFromTime(time.Unix(1_700_000_000, 0)), "n": int32(7), "l": int64(9), "a": bson.A{int32(1), "x"}, "d": bson.D{{Key: "k", Value: int32(2)}}}).(map[string]any)
	if out["_id"] != oid.Hex() || out["n"] != 7.0 || out["l"] != 9.0 || !reflect.DeepEqual(out["a"], []any{1.0, "x"}) ||
		!reflect.DeepEqual(out["d"], map[string]any{"k": 2.0}) || !strings.HasPrefix(out["at"].(string), "2023-11-14T22:13:20") {
		t.Fatalf("out = %#v", out)
	}
}

func TestMongoArgumentsPerOperation(t *testing.T) {
	if _, err := nrMongoArgs("insertOne", []any{1.0}, false); err == nil {
		t.Error("insertOne took a list")
	}
	if _, err := nrMongoArgs("insertMany", map[string]any{}, false); err == nil {
		t.Error("insertMany took an object")
	}
	if _, err := nrMongoArgs("updateOne", []any{map[string]any{}}, false); err == nil {
		t.Error("updateOne took one argument")
	}
	a, err := nrMongoArgs("find", []any{map[string]any{"a": 1.0}, map[string]any{"limit": 3.0}}, false)
	if err != nil || a.options["limit"] != 3.0 {
		t.Errorf("find options: %v %#v", err, a.options)
	}
	oid := bson.NewObjectID()
	a, err = nrMongoArgs("deleteOne", map[string]any{"_id": oid.Hex()}, true)
	if err != nil {
		t.Fatal(err)
	}
	if d := a.first.(bson.D); d[0].Value != oid {
		t.Errorf("handleDocId left the _id as %#v", d[0].Value)
	}
}

func TestMongoNodeUnderAFlowTestSendsTheOperationToTheStandIn(t *testing.T) {
	si := &recordingStandIn{}
	svc := newConfigServices()
	svc.standIn = si
	n := build(t, "mongodb4", `{"clientNode":"mc","collection":"readings","operation":"insertOne","output":"toArray"}`, svc)
	e, err := send(t, n, msg(t, `{"payload":{"sensor":"d1","v":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	sent := si.calls[0]["sent"].(map[string]any)
	if sent["collection"] != "readings" || sent["operation"] != "insertOne" {
		t.Fatalf("sent = %#v", sent)
	}
	if pl := e.on(0)[0].Payload().(map[string]any); pl["acknowledged"] != true {
		t.Fatalf("payload = %#v", pl)
	}
}

// ---------------------------------------------------------------------------
// Against a real MongoDB
//
//	HOTLOOP_FLOW_TEST_MONGO=127.0.0.1:17017 go test ./internal/nodes/ -run NodeRedMongo -v
//
// with docker.io/library/mongo:8 and no authentication.
// ---------------------------------------------------------------------------

func TestNodeRedMongoAgainstARealServer(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_MONGO")
	host, port := splitHostPort(t, addr)
	svc := newConfigServices()
	c := buildConfig(t, "mongodb4-client", "mc", `{"protocol":"mongodb","hostname":"`+host+`","port":"`+port+`","dbName":"flowtest","connectTimeoutMS":"5000"}`, svc)
	svc.configs["mc"] = c
	defer c.(node.Closer).Close(context.Background(), true)

	op := func(cfg, payload string) *testEmitter {
		t.Helper()
		n := build(t, "mongodb4", `{"clientNode":"mc","collection":"flx_readings",`+cfg+`}`, svc)
		e, err := send(t, n, msg(t, payload))
		if err != nil {
			t.Fatalf("%s: %v", cfg, err)
		}
		return e
	}
	op(`"operation":"deleteMany","output":"toArray"`, `{"payload":{}}`)

	e := op(`"operation":"insertOne","output":"toArray"`, `{"payload":{"sensor":"dryer-1","value":151.5,"count":3}}`)
	res := e.on(0)[0].Payload().(map[string]any)
	id, _ := res["insertedId"].(string)
	if res["acknowledged"] != true || len(id) != 24 {
		t.Fatalf("insertOne result = %#v", res)
	}
	op(`"operation":"insertMany"`, `{"payload":[{"sensor":"dryer-2","value":149.25,"count":4},{"sensor":"dryer-3","value":150,"count":5}]}`)

	e = op(`"operation":"find","output":"toArray"`, `{"payload":[{},{"sort":{"count":1}}]}`)
	docs := e.on(0)[0].Payload().([]any)
	if len(docs) != 3 || docs[0].(map[string]any)["sensor"] != "dryer-1" || docs[0].(map[string]any)["value"] != 151.5 ||
		docs[0].(map[string]any)["count"] != 3.0 || docs[0].(map[string]any)["_id"] != id {
		t.Fatalf("find = %#v", docs)
	}
	e = op(`"operation":"find","output":"forEach"`, `{"payload":{"value":{"$gt":149.5}}}`)
	if len(e.on(0)) != 2 {
		t.Fatalf("forEach sent %d messages, want the 2 over 149.5", len(e.on(0)))
	}
	e = op(`"operation":"find","output":"count"`, `{"payload":{}}`)
	if e.on(0)[0].Payload() != 3.0 {
		t.Fatalf("count output = %v", e.on(0)[0].Payload())
	}

	e = op(`"operation":"updateOne"`, `{"payload":[{"sensor":"dryer-2"},{"$set":{"value":1.5}}]}`)
	if r := e.on(0)[0].Payload().(map[string]any); r["matchedCount"] != 1.0 || r["modifiedCount"] != 1.0 {
		t.Fatalf("update = %#v", r)
	}
	e = op(`"operation":"updateOne"`, `{"payload":[{"sensor":"dryer-9"},{"$set":{"value":9}},{"upsert":true}]}`)
	if r := e.on(0)[0].Payload().(map[string]any); r["upsertedCount"] != 1.0 {
		t.Fatalf("upsert = %#v", r)
	}
	e = op(`"operation":"countDocuments"`, `{"payload":{}}`)
	if e.on(0)[0].Payload() != 4.0 {
		t.Fatalf("countDocuments = %v", e.on(0)[0].Payload())
	}
	e = op(`"operation":"aggregate","output":"toArray"`, `{"payload":[{"$group":{"_id":null,"total":{"$sum":"$count"}}}]}`)
	if g := e.on(0)[0].Payload().([]any); len(g) != 1 || g[0].(map[string]any)["total"] != 12.0 {
		t.Fatalf("aggregate = %#v", g)
	}
	e = op(`"operation":"findOne","handleDocId":true`, `{"payload":{"_id":"`+id+`"}}`)
	if d := e.on(0)[0].Payload().(map[string]any); d["sensor"] != "dryer-1" {
		t.Fatalf("findOne by id = %#v", d)
	}
	e = op(`"operation":"deleteMany"`, `{"payload":{"sensor":{"$in":["dryer-2","dryer-3"]}}}`)
	if r := e.on(0)[0].Payload().(map[string]any); r["deletedCount"] != 2.0 {
		t.Fatalf("deleteMany = %#v", r)
	}
	// msg.collection overrides the node's.
	e = op(`"operation":"insertOne"`, `{"collection":"flx_other","payload":{"x":1}}`)
	cl, _ := c.(MongoProvider).Client()
	if n, err := cl.Database("flowtest").Collection("flx_other").CountDocuments(context.Background(), bson.D{}); err != nil || n != 1 {
		t.Fatalf("msg.collection: %d, %v", n, err)
	}
	_ = cl.Database("flowtest").Collection("flx_other").Drop(context.Background())
	op(`"operation":"deleteMany"`, `{"payload":{}}`)
}

func engineConfig(id, typ string, raw map[string]any) *engine.Node {
	return &engine.Node{ID: id, Type: typ, IsConfig: true, Raw: raw}
}
