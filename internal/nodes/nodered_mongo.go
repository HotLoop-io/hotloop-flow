package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The MongoDB node types a Node-RED flow uses: mongodb4-client and mongodb4.
// They take the property names node-red-contrib-mongodb4's flow entries carry.
// The driver is go.mongodb.org/mongo-driver/v2 (Apache-2.0). The nodes are
// written here from the package's documented behavior; none of its code is in
// them.

func init() {
	registerNRMongoClient()
	registerNRMongo()
}

// ---------------------------------------------------------------------------
// the client config node
// ---------------------------------------------------------------------------

type nrMongoClient struct {
	uri         string
	db          string
	connectTO   time.Duration
	socketTO    time.Duration
	minPool     uint64
	maxPool     uint64
	maxIdle     time.Duration
	appName     string
	tlsCAFile   string
	tlsKeyFile  string
	tlsInsecure bool
	tls         bool
	advanced    map[string]any

	mu sync.Mutex
	cl *mongo.Client
}

// MongoProvider is how an operation node reaches its config node's client.
type MongoProvider interface {
	Client() (*mongo.Client, error)
	Database() string
}

func registerNRMongoClient() {
	node.MustRegister(node.Descriptor{
		Type:      "mongodb4-client",
		Category:  node.CategoryConfig,
		Color:     colorStorage,
		Icon:      "db",
		IsConfig:  true,
		LabelProp: "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "A MongoDB client on the official Go driver. On the advanced tab the uri is used as written. On " +
				"the simple tab the address is built from protocol, hostname, port and dbName with the node's " +
				"user and password credentials. When the simple tab has no credentials and the flow file's uri " +
				"property holds a full address, the uri is used, because that is the only place such a flow " +
				"file keeps the login. Pool sizes, timeouts, authSource, authMechanism, appName and the TLS " +
				"settings are honored. The advanced JSON may set the driver's own option names in the " +
				"connection string form (for example retryWrites). The connection opens on the first operation.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "protocol", Kind: node.PropSelect, Label: "Protocol", Default: "mongodb", Options: []node.Option{
				{Value: "mongodb", Label: "mongodb"}, {Value: "mongodb+srv", Label: "mongodb+srv"}}},
			{Name: "hostname", Kind: node.PropString, Label: "Host", Default: "127.0.0.1"},
			{Name: "port", Kind: node.PropString, Label: "Port", Default: "27017"},
			{Name: "dbName", Kind: node.PropString, Label: "Database"},
			{Name: "appName", Kind: node.PropString, Label: "Application name"},
			{Name: "authSource", Kind: node.PropString, Label: "Auth source"},
			{Name: "authMechanism", Kind: node.PropString, Label: "Auth mechanism", Default: "DEFAULT"},
			{Name: "tls", Kind: node.PropBool, Label: "TLS"},
			{Name: "tlsCAFile", Kind: node.PropString, Label: "CA file"},
			{Name: "tlsCertificateKeyFile", Kind: node.PropString, Label: "Client certificate and key file"},
			{Name: "tlsInsecure", Kind: node.PropBool, Label: "Allow an invalid certificate"},
			{Name: "connectTimeoutMS", Kind: node.PropString, Label: "Connect timeout (ms)", Default: "30000"},
			{Name: "socketTimeoutMS", Kind: node.PropString, Label: "Socket timeout (ms)", Default: "0"},
			{Name: "minPoolSize", Kind: node.PropString, Label: "Min pool size", Default: "0"},
			{Name: "maxPoolSize", Kind: node.PropString, Label: "Max pool size", Default: "100"},
			{Name: "maxIdleTimeMS", Kind: node.PropString, Label: "Max idle time (ms)", Default: "0"},
			{Name: "uri", Kind: node.PropString, Label: "URI", Help: "The whole connection string, on the advanced tab."},
			{Name: "advanced", Kind: node.PropJSON, Label: "Advanced options", Default: "{}"},
			{Name: "uriTabActive", Kind: node.PropSelect, Label: "Tab", Default: "tab-uri-simple", Options: []node.Option{
				{Value: "tab-uri-simple", Label: "Simple"}, {Value: "tab-uri-advanced", Label: "Advanced"}}},
			{Name: "user", Kind: node.PropCredential, Label: "User"},
			{Name: "password", Kind: node.PropCredential, Label: "Password"},
		},
		Help: "Connection to a MongoDB server or Atlas-style cluster, shared by the mongodb4 nodes that point at it.",
	}, newNRMongoClient)
}

func msDuration(p *engine.Node, key string) (time.Duration, error) {
	f := p.PropFloat(key, 0)
	if f < 0 || math.IsNaN(f) {
		return 0, fmt.Errorf("%s has to be a number of milliseconds, 0 or more", key)
	}
	return time.Duration(f * float64(time.Millisecond)), nil
}

func newNRMongoClient(def *node.Definition) (node.Node, error) {
	p := def.Node
	c := &nrMongoClient{
		db:          strings.TrimSpace(p.PropString("dbName", "")),
		appName:     p.PropString("appName", ""),
		tlsCAFile:   p.PropString("tlsCAFile", ""),
		tlsKeyFile:  p.PropString("tlsCertificateKeyFile", ""),
		tlsInsecure: p.PropBool("tlsInsecure", false),
		tls:         p.PropBool("tls", false),
	}
	var err error
	if c.connectTO, err = msDuration(p, "connectTimeoutMS"); err != nil {
		return nil, err
	}
	if c.socketTO, err = msDuration(p, "socketTimeoutMS"); err != nil {
		return nil, err
	}
	if c.maxIdle, err = msDuration(p, "maxIdleTimeMS"); err != nil {
		return nil, err
	}
	c.minPool = uint64(max(p.PropInt("minPoolSize", 0), 0))
	c.maxPool = uint64(max(p.PropInt("maxPoolSize", 0), 0))
	if adv := strings.TrimSpace(p.PropString("advanced", "")); adv != "" && adv != "{}" {
		if err := json.Unmarshal([]byte(adv), &c.advanced); err != nil {
			return nil, fmt.Errorf("advanced is not valid JSON: %w", err)
		}
	}

	user, _ := def.Services.Credential("user")
	pass, _ := def.Services.Credential("password")
	stored := strings.TrimSpace(p.PropString("uri", ""))
	advancedTab := p.PropString("uriTabActive", "tab-uri-simple") == "tab-uri-advanced"
	switch {
	case advancedTab:
		if stored == "" {
			return nil, fmt.Errorf("the advanced tab needs a uri")
		}
		c.uri = stored
	case user == "" && pass == "" && strings.Contains(stored, "@"):
		c.uri = stored
	default:
		host := strings.TrimSpace(p.PropString("hostname", ""))
		if host == "" {
			return nil, fmt.Errorf("hostname is required")
		}
		proto := p.PropString("protocol", "mongodb")
		if proto != "mongodb" && proto != "mongodb+srv" {
			return nil, fmt.Errorf("protocol %q is not mongodb or mongodb+srv", proto)
		}
		u := url.URL{Scheme: proto, Host: host, Path: "/" + c.db}
		if port := strings.TrimSpace(p.PropString("port", "")); port != "" && proto == "mongodb" {
			if _, err := strconv.Atoi(port); err != nil {
				return nil, fmt.Errorf("port %q is not a number", port)
			}
			u.Host = host + ":" + port
		}
		if user != "" {
			u.User = url.UserPassword(user, pass)
		}
		q := url.Values{}
		if as := p.PropString("authSource", ""); as != "" {
			q.Set("authSource", as)
		}
		if am := p.PropString("authMechanism", ""); am != "" && am != "DEFAULT" {
			q.Set("authMechanism", am)
		}
		u.RawQuery = q.Encode()
		c.uri = u.String()
	}
	if c.db == "" {
		// A database in the uri's path is the default.
		if u, err := url.Parse(c.uri); err == nil {
			c.db = strings.TrimPrefix(u.Path, "/")
		}
	}
	if c.db == "" {
		return nil, fmt.Errorf("dbName is required: the database the operations run in")
	}
	return c, nil
}

func (c *nrMongoClient) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }
func (c *nrMongoClient) Database() string                                         { return c.db }

func (c *nrMongoClient) Client() (*mongo.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cl != nil {
		return c.cl, nil
	}
	o := options.Client().ApplyURI(c.uri)
	if c.connectTO > 0 {
		o.SetConnectTimeout(c.connectTO)
		o.SetServerSelectionTimeout(c.connectTO)
	}
	if c.socketTO > 0 {
		o.SetTimeout(c.socketTO)
	}
	if c.maxPool > 0 {
		o.SetMaxPoolSize(c.maxPool)
	}
	if c.minPool > 0 {
		o.SetMinPoolSize(c.minPool)
	}
	if c.maxIdle > 0 {
		o.SetMaxConnIdleTime(c.maxIdle)
	}
	if c.appName != "" {
		o.SetAppName(c.appName)
	}
	if v, ok := c.advanced["retryWrites"].(bool); ok {
		o.SetRetryWrites(v)
	}
	if c.tls || c.tlsCAFile != "" || c.tlsKeyFile != "" || c.tlsInsecure {
		cfg, err := nrMongoTLS(c)
		if err != nil {
			return nil, err
		}
		o.SetTLSConfig(cfg)
	}
	cl, err := mongo.Connect(o)
	if err != nil {
		return nil, fmt.Errorf("connecting to MongoDB: %w", err)
	}
	c.cl = cl
	return cl, nil
}

func (c *nrMongoClient) Close(ctx context.Context, _ bool) error {
	c.mu.Lock()
	cl := c.cl
	c.cl = nil
	c.mu.Unlock()
	if cl != nil {
		return cl.Disconnect(ctx)
	}
	return nil
}

// ---------------------------------------------------------------------------
// values in and out
// ---------------------------------------------------------------------------

// toBSON turns a message value into what the driver stores. A number with no
// fraction that fits in 32 bits goes in as an int32, as a JavaScript driver
// stores it, so a counter written from a flow reads back as an integer.
func toBSON(v any) any {
	switch t := v.(type) {
	case float64:
		if t == math.Trunc(t) && t >= math.MinInt32 && t <= math.MaxInt32 {
			return int32(t)
		}
		return t
	case engine.ImmutableBytes:
		return []byte(t)
	case map[string]any:
		out := make(bson.D, 0, len(t))
		for k, val := range t {
			out = append(out, bson.E{Key: k, Value: toBSON(val)})
		}
		return out
	case []any:
		out := make(bson.A, len(t))
		for i, val := range t {
			out[i] = toBSON(val)
		}
		return out
	}
	return v
}

// fromBSON turns what the driver returns into JSON-shaped values a flow can
// work with: ids and dates as text, numbers as float64, documents as maps.
func fromBSON(v any) any {
	switch t := v.(type) {
	case bson.ObjectID:
		return t.Hex()
	case bson.DateTime:
		return t.Time().UTC().Format(time.RFC3339Nano)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case bson.D:
		out := make(map[string]any, len(t))
		for _, e := range t {
			out[e.Key] = fromBSON(e.Value)
		}
		return out
	case bson.M:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = fromBSON(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = fromBSON(val)
		}
		return out
	case bson.A:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = fromBSON(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = fromBSON(val)
		}
		return out
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case int:
		return float64(t)
	case bson.Decimal128:
		return t.String()
	case bson.Binary:
		return []byte(t.Data)
	case bson.Null:
		return nil
	}
	return v
}

// ---------------------------------------------------------------------------
// the operation node
// ---------------------------------------------------------------------------

var nrMongoOps = []string{"insertOne", "insertMany", "find", "findOne", "updateOne", "updateMany",
	"replaceOne", "deleteOne", "deleteMany", "countDocuments", "aggregate"}

type nrMongoNode struct {
	cfgID      string
	collection string
	operation  string
	output     string
	maxTime    time.Duration
	handleID   bool
	svc        node.Services
	provider   MongoProvider
}

func registerNRMongo() {
	opts := make([]node.Option, len(nrMongoOps))
	for i, o := range nrMongoOps {
		opts[i] = node.Option{Value: o, Label: o}
	}
	node.MustRegister(node.Descriptor{
		Type:         "mongodb4",
		Category:     node.CategoryStorage,
		Color:        colorStorage,
		Icon:         "db",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "mongodb4",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Collection operations: insertOne, insertMany, find, findOne, updateOne, updateMany, " +
				"replaceOne, deleteOne, deleteMany, countDocuments and aggregate. msg.payload is the document, " +
				"documents, filter or pipeline, or a list of the arguments ([filter, options] for find, [filter, update, " +
				"options] for updates), and msg.collection and msg.operation override the node's. A find sends one " +
				"message with the list (toArray), one per document (forEach), or the count (count). The other " +
				"operations send the driver's result as msg.payload. A document comes out as plain JSON: ids as " +
				"text, dates as ISO text, numbers as numbers. Database mode (runCommand, listCollections and the " +
				"other database-level operations) and the other operations the package offers are not provided, and a " +
				"node set to one does not start.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "clientNode", Kind: node.PropConfigRef, Label: "Server", ConfigType: "mongodb4-client", Required: true},
			{Name: "mode", Kind: node.PropSelect, Label: "Mode", Default: "collection", Options: []node.Option{
				{Value: "collection", Label: "Collection"}, {Value: "db", Label: "Database"}}},
			{Name: "collection", Kind: node.PropString, Label: "Collection"},
			{Name: "operation", Kind: node.PropSelect, Label: "Operation", Default: "find", Options: opts},
			{Name: "output", Kind: node.PropSelect, Label: "Output", Default: "toArray", Options: []node.Option{
				{Value: "toArray", Label: "One message with the list"}, {Value: "forEach", Label: "One message per document"},
				{Value: "count", Label: "The count"}}},
			{Name: "maxTimeMS", Kind: node.PropString, Label: "Max time (ms)"},
			{Name: "handleDocId", Kind: node.PropBool, Label: "Turn an _id string in a filter into an ObjectId"},
		},
		Help: "Runs an operation on a MongoDB collection. msg.payload is what to insert or the filter to find by.",
	}, newNRMongo)
}

func newNRMongo(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &nrMongoNode{
		cfgID:      p.PropString("clientNode", ""),
		collection: strings.TrimSpace(p.PropString("collection", "")),
		operation:  p.PropString("operation", "find"),
		output:     p.PropString("output", "toArray"),
		handleID:   p.PropBool("handleDocId", false),
		svc:        def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected: pick the MongoDB connection")
	}
	if mode := p.PropString("mode", "collection"); mode != "collection" {
		return nil, fmt.Errorf("mode %q is not supported: only collection operations are provided", mode)
	}
	if !nrMongoKnown(n.operation) {
		return nil, fmt.Errorf("operation %q is not supported. The collection operations provided are %s",
			n.operation, strings.Join(nrMongoOps, ", "))
	}
	switch n.output {
	case "toArray", "forEach", "count", "":
	default:
		return nil, fmt.Errorf("output %q is not toArray, forEach or count", n.output)
	}
	if s := strings.TrimSpace(p.PropString("maxTimeMS", "")); s != "" {
		ms, err := strconv.ParseFloat(s, 64)
		if err != nil || ms < 0 {
			return nil, fmt.Errorf("maxTimeMS %q is not a number of milliseconds", s)
		}
		n.maxTime = time.Duration(ms * float64(time.Millisecond))
	}
	return n, nil
}

func nrMongoKnown(op string) bool {
	for _, o := range nrMongoOps {
		if o == op {
			return true
		}
	}
	return false
}

func (n *nrMongoNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	op := n.operation
	if s := msgString(m, "operation"); s != "" {
		op = s
		if !nrMongoKnown(op) {
			return fmt.Errorf("msg.operation %q is not supported. The collection operations provided are %s", op, strings.Join(nrMongoOps, ", "))
		}
	}
	coll := n.collection
	if s := msgString(m, "collection"); s != "" {
		coll = s
	}
	if coll == "" {
		return fmt.Errorf("there is no collection: set it in the node or in msg.collection")
	}
	args, err := nrMongoArgs(op, m.Payload(), n.handleID)
	if err != nil {
		return err
	}
	if n.maxTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, n.maxTime)
		defer cancel()
	}

	var results []any // documents, for find and aggregate
	var single any    // the result of any other operation
	isList := op == "find" || op == "aggregate"

	if si := node.StandInOf(n.svc); si != nil {
		reply, err := si.Call("mongodb", map[string]any{"collection": coll, "operation": op, "args": args.sent()})
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		if isList {
			if results, err = replyList(reply, "documents"); err != nil {
				return err
			}
		} else if v, ok := reply["result"]; ok {
			single = v
		} else {
			single = map[string]any{"acknowledged": true}
		}
	} else {
		if n.provider == nil {
			cfg, ok := n.svc.ConfigNode(n.cfgID)
			if !ok {
				return fmt.Errorf("the MongoDB connection %s is not running. Open this node and pick a server", n.cfgID)
			}
			pr, ok := cfg.(MongoProvider)
			if !ok {
				return fmt.Errorf("config node %s is not a MongoDB connection", n.cfgID)
			}
			n.provider = pr
		}
		cl, err := n.provider.Client()
		if err != nil {
			out.Status(node.Status{Fill: "red", Shape: "ring", Text: "not connected"})
			return err
		}
		c := cl.Database(n.provider.Database()).Collection(coll)
		results, single, err = nrMongoRun(ctx, c, op, args)
		if err != nil {
			out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
			return fmt.Errorf("%s on %s: %w", op, coll, err)
		}
	}

	out.Status(node.Status{Fill: "green", Shape: "dot", Text: op})
	if !isList {
		m.SetPayload(single)
		out.Send(0, m)
		return nil
	}
	switch n.output {
	case "count":
		m.SetPayload(float64(len(results)))
		out.Send(0, m)
	case "forEach":
		for _, d := range results {
			c := m.Clone()
			c.SetPayload(d)
			out.Send(0, c)
		}
	default:
		if results == nil {
			results = []any{}
		}
		m.SetPayload(results)
		out.Send(0, m)
	}
	return nil
}

// nrArgs are the arguments of one operation, converted for the driver.
type nrArgs struct {
	first   any // document, documents, filter or pipeline
	second  any // update, or options for a find
	options map[string]any
	raw     []any
}

func (a nrArgs) sent() []any { return a.raw }

// nrMongoArgs reads msg.payload into arguments the way the operation needs them.
func nrMongoArgs(op string, payload any, handleID bool) (nrArgs, error) {
	var a nrArgs
	list, isList := payload.([]any)
	switch op {
	case "insertOne":
		doc, ok := payload.(map[string]any)
		if !ok {
			return a, fmt.Errorf("insertOne needs msg.payload to be one document (an object), got %T", payload)
		}
		a.first, a.raw = toBSON(doc), []any{payload}
	case "insertMany":
		if !isList || len(list) == 0 {
			return a, fmt.Errorf("insertMany needs msg.payload to be a list of documents, one or more")
		}
		for i, d := range list {
			if _, ok := d.(map[string]any); !ok {
				return a, fmt.Errorf("insertMany: item %d is not a document", i)
			}
		}
		a.first, a.raw = toBSON(list), list
	case "aggregate":
		if !isList {
			return a, fmt.Errorf("aggregate needs msg.payload to be the pipeline, a list of stages")
		}
		a.first, a.raw = toBSON(list), list
	case "updateOne", "updateMany", "replaceOne":
		if !isList || len(list) < 2 {
			return a, fmt.Errorf("%s needs msg.payload to be [filter, update] or [filter, update, options]", op)
		}
		a.first, a.second, a.raw = nrFilter(list[0], handleID), toBSON(list[1]), list
		if len(list) > 2 {
			a.options, _ = list[2].(map[string]any)
		}
	default: // find, findOne, deleteOne, deleteMany, countDocuments
		var filter any = payload
		if isList {
			if len(list) == 0 {
				filter = map[string]any{}
			} else {
				filter = list[0]
				if len(list) > 1 {
					a.options, _ = list[1].(map[string]any)
				}
			}
			a.raw = list
		} else {
			a.raw = []any{payload}
		}
		if filter == nil {
			filter = map[string]any{}
		}
		if _, ok := filter.(map[string]any); !ok {
			return a, fmt.Errorf("%s needs a filter that is an object, got %T", op, filter)
		}
		a.first = nrFilter(filter, handleID)
	}
	return a, nil
}

// nrFilter converts a filter, and an _id string that looks like an ObjectId
// into one when handleDocId is on.
func nrFilter(f any, handleID bool) any {
	m, ok := f.(map[string]any)
	if !ok {
		return toBSON(f)
	}
	if handleID {
		if s, ok := m["_id"].(string); ok {
			if oid, err := bson.ObjectIDFromHex(s); err == nil {
				cp := make(map[string]any, len(m))
				for k, v := range m {
					cp[k] = v
				}
				d := toBSON(cp).(bson.D)
				for i := range d {
					if d[i].Key == "_id" {
						d[i].Value = oid
					}
				}
				return d
			}
		}
	}
	return toBSON(m)
}

func nrMongoRun(ctx context.Context, c *mongo.Collection, op string, a nrArgs) (docs []any, single any, err error) {
	switch op {
	case "insertOne":
		r, err := c.InsertOne(ctx, a.first)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"acknowledged": r.Acknowledged, "insertedId": fromBSON(r.InsertedID)}, nil
	case "insertMany":
		r, err := c.InsertMany(ctx, a.first)
		if err != nil {
			return nil, nil, err
		}
		ids := make([]any, len(r.InsertedIDs))
		for i, id := range r.InsertedIDs {
			ids[i] = fromBSON(id)
		}
		return nil, map[string]any{"acknowledged": r.Acknowledged, "insertedCount": float64(len(ids)), "insertedIds": ids}, nil
	case "find":
		fo := options.Find()
		if v, ok := a.options["limit"].(float64); ok {
			fo.SetLimit(int64(v))
		}
		if v, ok := a.options["skip"].(float64); ok {
			fo.SetSkip(int64(v))
		}
		if v, ok := a.options["sort"]; ok {
			fo.SetSort(toBSON(v))
		}
		if v, ok := a.options["projection"]; ok {
			fo.SetProjection(toBSON(v))
		}
		cur, err := c.Find(ctx, a.first, fo)
		if err != nil {
			return nil, nil, err
		}
		defer cur.Close(ctx)
		var raw []bson.M
		if err := cur.All(ctx, &raw); err != nil {
			return nil, nil, err
		}
		for _, d := range raw {
			docs = append(docs, fromBSON(d))
		}
		return docs, nil, nil
	case "findOne":
		var d bson.M
		err := c.FindOne(ctx, a.first).Decode(&d)
		if err == mongo.ErrNoDocuments {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		return nil, fromBSON(d), nil
	case "updateOne", "updateMany", "replaceOne":
		upsert := false
		if v, ok := a.options["upsert"].(bool); ok {
			upsert = v
		}
		var r *mongo.UpdateResult
		switch op {
		case "updateOne":
			r, err = c.UpdateOne(ctx, a.first, a.second, options.UpdateOne().SetUpsert(upsert))
		case "updateMany":
			r, err = c.UpdateMany(ctx, a.first, a.second, options.UpdateMany().SetUpsert(upsert))
		default:
			r, err = c.ReplaceOne(ctx, a.first, a.second, options.Replace().SetUpsert(upsert))
		}
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"acknowledged": r.Acknowledged, "matchedCount": float64(r.MatchedCount),
			"modifiedCount": float64(r.ModifiedCount), "upsertedCount": float64(r.UpsertedCount),
			"upsertedId": fromBSON(r.UpsertedID)}, nil
	case "deleteOne", "deleteMany":
		var r *mongo.DeleteResult
		if op == "deleteOne" {
			r, err = c.DeleteOne(ctx, a.first)
		} else {
			r, err = c.DeleteMany(ctx, a.first)
		}
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"acknowledged": r.Acknowledged, "deletedCount": float64(r.DeletedCount)}, nil
	case "countDocuments":
		n, err := c.CountDocuments(ctx, a.first)
		if err != nil {
			return nil, nil, err
		}
		return nil, float64(n), nil
	case "aggregate":
		cur, err := c.Aggregate(ctx, a.first)
		if err != nil {
			return nil, nil, err
		}
		defer cur.Close(ctx)
		var raw []bson.M
		if err := cur.All(ctx, &raw); err != nil {
			return nil, nil, err
		}
		for _, d := range raw {
			docs = append(docs, fromBSON(d))
		}
		return docs, nil, nil
	}
	return nil, nil, fmt.Errorf("operation %q is not supported", op)
}
