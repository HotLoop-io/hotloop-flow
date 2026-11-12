package nodes

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The PostgreSQL node types a Node-RED flow uses: postgreSQLConfig and
// postgresql. They take the property names node-red-contrib-postgresql's flow
// entries carry, including the "...FieldType" companions that say whether a
// config value is a string, a number, a boolean or an environment variable.
// The connection and the query run on Flow's own pgx pool, the same one the
// "postgres" node uses. Nothing is ported from the community package.

func init() {
	registerNRPostgresConfig()
	registerNRPostgres()
}

func registerNRPostgresConfig() {
	typed := func(name, label string, def any, help string) node.Prop {
		return node.Prop{Name: name, Kind: node.PropString, Label: label, Default: def, Help: help}
	}
	node.MustRegister(node.Descriptor{
		Type:      "postgreSQLConfig",
		Category:  node.CategoryConfig,
		Color:     colorStorage,
		Icon:      "db",
		IsConfig:  true,
		LabelProp: "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "A PostgreSQL or TimescaleDB connection pool on Flow's own driver. Each setting may be a " +
				"string, a number, a boolean or an environment variable, by its ...FieldType companion; other " +
				"types (msg, flow, global) have no message to read from in a config node and stop the node. " +
				"ssl true verifies the server certificate, as node-postgres does, and ssl may also be one of " +
				"disable, prefer, require, verify-full or no-verify. The password is read from the node's " +
				"credential when there is one, else from the flow file's password property, as the community " +
				"node keeps it. Connections open on the first query, not at deploy, and an idle connection is " +
				"kept for at least a minute whatever idle says, so a plant reading once a second does not " +
				"reconnect for every reading.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			typed("host", "Host", "localhost", ""),
			typed("port", "Port", 5432, ""),
			typed("database", "Database", "", ""),
			typed("ssl", "SSL", "false", "false, true, disable, prefer, require, verify-full or no-verify."),
			typed("applicationName", "Application name", "", ""),
			typed("max", "Max connections", 10, ""),
			typed("idle", "Idle timeout (ms)", 1000, ""),
			typed("connectionTimeout", "Connection timeout (ms)", 10000, ""),
			typed("user", "User", "", ""),
			{Name: "password", Kind: node.PropCredential, Label: "Password"},
		},
		Help: "Connection to a PostgreSQL or TimescaleDB database, shared by every postgresql node that points at it.",
	}, newNRPostgresConfig)
}

// nrTypedConfig resolves one config value by its FieldType companion.
func nrTypedConfig(def *node.Definition, name, dflt string) (string, error) {
	raw, has := def.Node.Raw[name]
	val := dflt
	if has && raw != nil {
		switch t := raw.(type) {
		case string:
			val = t
		case float64:
			val = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			val = strconv.FormatBool(t)
		default:
			val = fmt.Sprint(t)
		}
	}
	switch typ := def.Node.PropString(name+"FieldType", "str"); typ {
	case "str", "num", "bool", "":
		return val, nil
	case "env":
		v, ok := def.Services.Env(val)
		if !ok {
			return "", fmt.Errorf("%s is the environment variable %q and it is not set", name, val)
		}
		return v, nil
	default:
		return "", fmt.Errorf("%s has type %q. A config node has no message, so use str, num, bool or env", name, typ)
	}
}

func newNRPostgresConfig(def *node.Definition) (node.Node, error) {
	get := func(name, dflt string) (string, error) { return nrTypedConfig(def, name, dflt) }
	host, err := get("host", "localhost")
	if err != nil {
		return nil, err
	}
	if host == "" {
		return nil, fmt.Errorf("host is required")
	}
	portS, err := get("port", "5432")
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(strings.TrimSpace(portS))
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %q is not a number from 1 to 65535", portS)
	}
	database, err := get("database", "")
	if err != nil {
		return nil, err
	}
	if database == "" {
		return nil, fmt.Errorf("database is required")
	}
	user, err := get("user", "")
	if err != nil {
		return nil, err
	}
	if user == "" {
		return nil, fmt.Errorf("user is required")
	}
	sslS, err := get("ssl", "false")
	if err != nil {
		return nil, err
	}
	sslmode := ""
	switch strings.ToLower(strings.TrimSpace(sslS)) {
	case "false", "", "disable":
		sslmode = "disable"
	case "true", "verify-full":
		sslmode = "verify-full"
	case "prefer":
		sslmode = "prefer"
	case "require", "no-verify":
		sslmode = "require"
	default:
		return nil, fmt.Errorf("ssl %q is not false, true, disable, prefer, require, verify-full or no-verify", sslS)
	}
	app, err := get("applicationName", "")
	if err != nil {
		return nil, err
	}
	if app == "" {
		app = "hotloop-flow"
	}
	maxS, err := get("max", "10")
	if err != nil {
		return nil, err
	}
	maxConns, err := strconv.Atoi(strings.TrimSpace(maxS))
	if err != nil || maxConns < 1 {
		return nil, fmt.Errorf("max %q is not a number of connections, 1 or more", maxS)
	}
	idleS, err := get("idle", "1000")
	if err != nil {
		return nil, err
	}
	idle, err := strconv.ParseFloat(strings.TrimSpace(idleS), 64)
	if err != nil || idle < 0 {
		return nil, fmt.Errorf("idle %q is not a number of milliseconds", idleS)
	}
	ctoS, err := get("connectionTimeout", "10000")
	if err != nil {
		return nil, err
	}
	cto, err := strconv.ParseFloat(strings.TrimSpace(ctoS), 64)
	if err != nil || cto < 0 {
		return nil, fmt.Errorf("connectionTimeout %q is not a number of milliseconds", ctoS)
	}

	password, ok := def.Services.Credential("password")
	if !ok {
		password, err = get("password", "")
		if err != nil {
			return nil, err
		}
	}
	dsn := fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s application_name=%s",
		quotePGKeyword(host), port, quotePGKeyword(database), quotePGKeyword(user),
		quotePGKeyword(password), sslmode, quotePGKeyword(app))
	c := &nrPostgresConfig{postgresConfig: postgresConfig{dsn: dsn, maxConns: int32(maxConns)}}
	c.idle = time.Duration(idle * float64(time.Millisecond))
	c.connectTimeout = time.Duration(cto * float64(time.Millisecond))
	return c, nil
}

// nrPostgresConfig is Flow's pool with the settings this node type adds.
type nrPostgresConfig struct {
	postgresConfig
	idle           time.Duration
	connectTimeout time.Duration
}

// Pool opens the pool on first use, honoring the idle and connection timeouts.
func (c *nrPostgresConfig) Pool(ctx context.Context) (*pgxpool.Pool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pool != nil {
		return c.pool, nil
	}
	cfg, err := pgxpool.ParseConfig(c.dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid connection settings: %w", err)
	}
	cfg.MaxConns = c.maxConns
	cfg.MaxConnLifetime = time.Hour
	if c.idle > 0 {
		// node-postgres closes an idle client after this long. A pool that did
		// the same would reconnect for every sensor reading, so it is a floor of
		// a minute here, which is what keeps a one second setting from churning.
		cfg.MaxConnIdleTime = max(c.idle, time.Minute)
	} else {
		cfg.MaxConnIdleTime = 15 * time.Minute
	}
	if c.connectTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = c.connectTimeout
	} else {
		cfg.ConnConfig.ConnectTimeout = 10 * time.Second
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	c.pool = pool
	return pool, nil
}

// ---------------------------------------------------------------------------
// postgresql
// ---------------------------------------------------------------------------

type nrPostgresNode struct {
	cfgID    string
	query    string
	tmpl     []mustacheNode
	split    bool
	perMsg   int
	outputs  int
	svc      node.Services
	provider PoolProvider
}

func registerNRPostgres() {
	node.MustRegister(node.Descriptor{
		Type:         "postgresql",
		Category:     node.CategoryStorage,
		Color:        colorStorage,
		Icon:         "db",
		Inputs:       1,
		Outputs:      1,
		OutputsProp:  "outputs",
		PaletteLabel: "postgresql",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Runs the query as a Mustache template against the message ({{{msg.topic}}} and {{{topic}}} both work, and put a value " +
				"in as text, so prefer $1, $2 with msg.params, which go to the database as parameters). " +
				"msg.payload is the list of rows and msg.pgsql is {command, rowCount}. With split on, the rows " +
				"go out rowsPerMsg at a time (a row object when it is 1, else a list) with msg.parts, and a " +
				"second output, when there are two, sends one message when the last group is out. msg.queryParameters " +
				"(named parameters) is not supported. A failed query raises an error for a Catch node.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "query", Kind: node.PropText, Label: "Query", Language: "sql", Default: "SELECT 1;"},
			{Name: "postgreSQLConfig", Kind: node.PropConfigRef, Label: "Server", ConfigType: "postgreSQLConfig", Required: true},
			{Name: "split", Kind: node.PropBool, Label: "Split results"},
			{Name: "rowsPerMsg", Kind: node.PropNumber, Label: "Rows per message", Default: 1},
			{Name: "outputs", Kind: node.PropNumber, Label: "Outputs", Default: 1},
		},
		Help: "Runs a SQL query against PostgreSQL or TimescaleDB. Put values in with $1, $2 and msg.params, " +
			"so the database does the quoting. msg.payload is the rows.",
	}, newNRPostgres)
}

func newNRPostgres(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &nrPostgresNode{
		cfgID:   p.PropString("postgreSQLConfig", ""),
		query:   p.PropString("query", ""),
		split:   p.PropBool("split", false),
		perMsg:  p.PropInt("rowsPerMsg", 1),
		outputs: p.PropInt("outputs", 1),
		svc:     def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected: pick the PostgreSQL connection")
	}
	if n.perMsg < 1 {
		n.perMsg = 1
	}
	if strings.TrimSpace(n.query) != "" {
		t, err := parseMustache(n.query)
		if err != nil {
			return nil, fmt.Errorf("query: %w", err)
		}
		n.tmpl = t
	}
	return n, nil
}

func (n *nrPostgresNode) pool(ctx context.Context) (*pgxpool.Pool, error) {
	if n.provider == nil {
		cfg, ok := n.svc.ConfigNode(n.cfgID)
		if !ok {
			return nil, fmt.Errorf("the PostgreSQL connection %s is not running. Open this node and pick a server", n.cfgID)
		}
		pp, ok := cfg.(PoolProvider)
		if !ok {
			return nil, fmt.Errorf("config node %s is not a PostgreSQL connection", n.cfgID)
		}
		n.provider = pp
	}
	return n.provider.Pool(ctx)
}

func (n *nrPostgresNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	if n.tmpl == nil {
		return fmt.Errorf("the query is empty")
	}
	sql, err := renderMustache(n.tmpl, &mustacheScope{stack: []any{m.Data, map[string]any{"msg": m.Data}}})
	if err != nil {
		return fmt.Errorf("query template: %w", err)
	}
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return fmt.Errorf("the query is empty once the message is put in")
	}
	var params []any
	if raw, ok := m.Data["params"]; ok && raw != nil {
		l, isList := raw.([]any)
		if !isList {
			return fmt.Errorf("msg.params has to be a list, one entry for each $1, $2, got %T", raw)
		}
		for _, v := range l {
			params = append(params, normaliseForPG(v))
		}
	}

	var rows []any
	var command string
	var affected int64

	if si := node.StandInOf(n.svc); si != nil {
		reply, err := si.Call("postgres", map[string]any{"query": sql, "params": params})
		if err != nil {
			out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
			return fmt.Errorf("query: %w", err)
		}
		if rows, err = replyList(reply, "rows"); err != nil {
			return err
		}
		rows = append([]any{}, rows...)
		command, affected = firstWord(sql), int64(len(rows))
	} else {
		pool, err := n.pool(ctx)
		if err != nil {
			out.Status(node.Status{Fill: "red", Shape: "ring", Text: "not connected"})
			return err
		}
		rs, err := pool.Query(ctx, sql, params...)
		if err != nil {
			out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
			return fmt.Errorf("query: %w", err)
		}
		collected, err := pgx.CollectRows(rs, pgx.RowToMap)
		if err != nil {
			out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
			return fmt.Errorf("query: %w", err)
		}
		rows = make([]any, len(collected))
		for i, r := range collected {
			rows[i] = normalisePGRow(r)
		}
		tag := rs.CommandTag()
		command = firstWord(tag.String())
		affected = tag.RowsAffected()
	}
	pgsql := func() map[string]any {
		return map[string]any{"command": command, "rowCount": float64(affected)}
	}
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: fmt.Sprintf("%d rows", affected)})

	if !n.split {
		m.SetPayload(rows)
		m.Data["pgsql"] = pgsql()
		out.Send(0, m)
		return nil
	}
	id := m.ID()
	count := (len(rows) + n.perMsg - 1) / n.perMsg
	for i := 0; i < count; i++ {
		end := min((i+1)*n.perMsg, len(rows))
		part := m.Clone()
		if n.perMsg == 1 {
			part.SetPayload(rows[i])
		} else {
			part.SetPayload(append([]any{}, rows[i*n.perMsg:end]...))
		}
		part.Data["pgsql"] = pgsql()
		part.Data["parts"] = map[string]any{"id": id, "type": "array", "index": float64(i), "count": float64(count)}
		out.Send(0, part)
	}
	if n.outputs > 1 {
		done := m.Clone()
		delete(done.Data, "payload")
		done.Data["pgsql"] = pgsql()
		out.Send(1, done)
	}
	return nil
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.ToUpper(strings.TrimRight(s, ";"))
}
