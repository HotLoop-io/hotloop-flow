package nodes

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Sparkplug B, as an edge node and as a host application.
//
// One config node is one MQTT connection with one role. An edge role is one
// Sparkplug edge node: it is born when it connects, dies with its will message
// when it is cut off, answers a rebirth request, and takes the `sparkplug out`
// nodes pointed at it as its devices. A host role is one host application: it
// publishes its STATE, subscribes to the namespace, follows every edge node's
// births, sequence numbers and deaths, and asks for a rebirth when what it sees
// no longer adds up. The two can't share a connection, because each has a will
// message and an MQTT connection has one.
//
// The rules come from the Sparkplug 3.0 specification, and each one that
// matters is named where it is kept. The MQTT client is paho.mqtt.golang at
// 3.1.1, which Sparkplug allows. Auto reconnect is off: a new session needs a
// new bdSeq in its will, and a client that reconnects on its own reuses the old
// one.

func init() {
	registerSparkplugConfig()
	registerSparkplugOut()
	registerSparkplugIn()
	registerSparkplugCommand()
}

const (
	spNamespace    = "spBv1.0"
	spRebirthName  = "Node Control/Rebirth"
	spBdSeqName    = "bdSeq"
	spRoleEdge     = "edge"
	spRoleHost     = "host"
	spMaxBackoff   = 30 * time.Second
	spRebirthRetry = 5 * time.Second
	spSettle       = 250 * time.Millisecond
)

// spEvent is one Sparkplug message as a host or a command listener sees it.
type spEvent struct {
	Topic    string
	Group    string
	Edge     string
	Device   string
	Type     string // NBIRTH, NDEATH, NDATA, DBIRTH, DDEATH, DDATA, NCMD, DCMD, STATE
	Payload  *spPayload
	Metrics  []*spMetric
	Stale    []string
	Seq      int // -1 when the message has none
	Out      bool
	BdSeq    *float64
	State    map[string]any
	Unsolved []string
}

// spListener is a node that wants messages from the server.
type spListener interface {
	deliver(ev *spEvent)
	statusSink(s node.Status)
}

type spServer struct {
	svc  node.Services
	role string

	host      string
	port      int
	tlsCfg    *tls.Config
	user      string
	pass      string
	group     string
	edgeNode  string
	hostID    string
	keepalive time.Duration
	reorder   time.Duration
	aliases   bool

	mu      sync.Mutex
	users   int
	cancel  context.CancelFunc
	done    chan struct{}
	client  mqtt.Client
	ready   bool // edge: born. host: STATE published.
	lastErr string

	// Edge.
	seq       int
	bdNext    int
	bdSeq     int
	outs      map[string]*spOutNode
	listeners map[spListener]struct{}
	rebirthT  *time.Timer

	// Host.
	stateTS uint64
	nodes   map[string]*spHostNode
}

func registerSparkplugConfig() {
	node.MustRegister(node.Descriptor{
		Type:      "hotloop-flow-sparkplug",
		Category:  node.CategoryConfig,
		Color:     colorNetwork,
		Icon:      "mqtt",
		IsConfig:  true,
		LabelProp: "name",
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own Sparkplug B connection, either as an edge node or as a host " +
				"application, on MQTT 3.1.1. It does not wait for a primary host application before it " +
				"is born, it does not move to a second broker, and it does not carry DataSet, Template " +
				"or PropertySet values.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "role", Kind: node.PropSelect, Label: "This is", Default: spRoleEdge,
				Options: []node.Option{
					{Value: spRoleEdge, Label: "An edge node, publishing metrics"},
					{Value: spRoleHost, Label: "A host application, receiving them"},
				}},
			{Name: "broker", Kind: node.PropString, Label: "Broker host", Required: true, Placeholder: "10.20.1.5"},
			{Name: "port", Kind: node.PropNumber, Label: "Port", Default: 1883},
			{Name: "tls", Kind: node.PropConfigRef, Label: "TLS", ConfigType: "tls-config",
				Help: "Optional. A TLS config to connect over TLS, and to present a client certificate."},
			{Name: "username", Kind: node.PropString, Label: "Username"},
			{Name: "password", Kind: node.PropCredential, Label: "Password"},
			{Name: "groupId", Kind: node.PropString, Label: "Group ID",
				Help: "An edge node's group. Required for an edge node. A host application only listens to this " +
					"group when it is set, and to every group when it is empty."},
			{Name: "edgeNodeId", Kind: node.PropString, Label: "Edge node ID",
				Help: "Edge node only. Group ID and edge node ID together have to be unique on the broker, " +
					"or two clients will keep taking each other's connection."},
			{Name: "hostId", Kind: node.PropString, Label: "Host application ID",
				Help: "Host application only. Published as the STATE topic spBv1.0/STATE/<id>."},
			{Name: "reorderTimeout", Kind: node.PropNumber, Label: "Reorder timeout (seconds)", Default: 2,
				Help: "Host application only. How long a gap in an edge node's sequence numbers is waited out before " +
					"asking it to be born again. Zero asks at once."},
			{Name: "useAliases", Kind: node.PropBool, Label: "Use aliases",
				Help: "Edge node only. Births carry each metric's name and a number, and data and commands carry only the " +
					"number, which keeps messages small. Some hosts need it, and some can't read a data message that " +
					"has names but no datatypes."},
			{Name: "keepalive", Kind: node.PropNumber, Label: "Keepalive (seconds)", Default: 30},
		},
		Help: "One Sparkplug B identity on an MQTT broker: either an edge node that Sparkplug Out nodes " +
			"publish through, or a host application that Sparkplug In nodes listen on.",
	}, newSparkplugServer)
}

func newSparkplugServer(def *node.Definition) (node.Node, error) {
	p := def.Node
	s := &spServer{
		svc:       def.Services,
		role:      p.PropString("role", spRoleEdge),
		host:      strings.TrimSpace(p.PropString("broker", "")),
		port:      p.PropInt("port", 1883),
		user:      p.PropString("username", ""),
		group:     strings.TrimSpace(p.PropString("groupId", "")),
		edgeNode:  strings.TrimSpace(p.PropString("edgeNodeId", "")),
		hostID:    strings.TrimSpace(p.PropString("hostId", "")),
		keepalive: time.Duration(p.PropFloat("keepalive", 30) * float64(time.Second)),
		reorder:   time.Duration(p.PropFloat("reorderTimeout", 2) * float64(time.Second)),
		aliases:   p.PropBool("useAliases", false),
		outs:      map[string]*spOutNode{},
		listeners: map[spListener]struct{}{},
		nodes:     map[string]*spHostNode{},
	}
	s.pass, _ = def.Services.Credential("password")
	if s.role == "" {
		s.role = spRoleEdge
	}
	if s.role != spRoleEdge && s.role != spRoleHost {
		return nil, fmt.Errorf("role %q is not edge or host", s.role)
	}
	if s.host == "" {
		return nil, fmt.Errorf("a broker host is required")
	}
	if s.port < 1 || s.port > 65535 {
		return nil, fmt.Errorf("port %d is out of range, it has to be 1 to 65535", s.port)
	}
	if s.keepalive < time.Second {
		return nil, fmt.Errorf("keepalive has to be at least one second")
	}
	if s.reorder < 0 {
		return nil, fmt.Errorf("reorder timeout can't be negative")
	}
	check := func(label, v string, required bool) error {
		if v == "" && required {
			return fmt.Errorf("%s is required", label)
		}
		if strings.ContainsAny(v, "+/#") {
			return fmt.Errorf("%s %q can't contain +, / or #, which Sparkplug reserves for topics", label, v)
		}
		return nil
	}
	switch s.role {
	case spRoleEdge:
		if err := check("group ID", s.group, true); err != nil {
			return nil, err
		}
		if err := check("edge node ID", s.edgeNode, true); err != nil {
			return nil, err
		}
	case spRoleHost:
		if err := check("host application ID", s.hostID, true); err != nil {
			return nil, err
		}
		if err := check("group ID", s.group, false); err != nil {
			return nil, err
		}
	}
	if ref := tlsConfigRef(p); ref != "" && node.StandInOf(def.Services) == nil {
		t, err := lookupTLSConfig(def.Services, ref)
		if err != nil {
			return nil, err
		}
		s.tlsCfg = t.clientConfig()
	}
	return s, nil
}

func (s *spServer) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (s *spServer) Close(ctx context.Context, _ bool) error {
	s.stop()
	return nil
}

// ---------------------------------------------------------------------------
// Life of the connection
// ---------------------------------------------------------------------------

// acquire starts the connection when the first node that wants it starts, and
// stops it when the last one is gone. A config node that connected on its own
// would publish births before the flow had said what it publishes.
func (s *spServer) acquire(l spListener) {
	s.mu.Lock()
	if l != nil {
		s.listeners[l] = struct{}{}
	}
	s.users++
	start := s.users == 1
	s.mu.Unlock()
	if start {
		ctx, cancel := context.WithCancel(context.Background())
		s.mu.Lock()
		s.cancel = cancel
		s.done = make(chan struct{})
		done := s.done
		s.mu.Unlock()
		go func() {
			defer close(done)
			s.run(ctx)
		}()
	}
}

func (s *spServer) release(l spListener) {
	s.mu.Lock()
	if l != nil {
		delete(s.listeners, l)
	}
	if s.users > 0 {
		s.users--
	}
	stop := s.users == 0
	s.mu.Unlock()
	if stop {
		s.stop()
	}
}

func (s *spServer) stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (s *spServer) broadcastStatus(st node.Status) {
	s.mu.Lock()
	ls := make([]spListener, 0, len(s.listeners))
	for l := range s.listeners {
		ls = append(ls, l)
	}
	s.mu.Unlock()
	for _, l := range ls {
		l.statusSink(st)
	}
}

func (s *spServer) fanOut(ev *spEvent) {
	s.mu.Lock()
	ls := make([]spListener, 0, len(s.listeners))
	for l := range s.listeners {
		ls = append(ls, l)
	}
	s.mu.Unlock()
	for _, l := range ls {
		l.deliver(ev)
	}
}

func (s *spServer) endpoint() string { return fmt.Sprintf("%s:%d", s.host, s.port) }

// run keeps a session alive: connect, work until it is lost, wait, again. The
// wait grows to thirty seconds and starts over after a session that held.
func (s *spServer) run(ctx context.Context) {
	wait := time.Second
	s.broadcastStatus(node.Status{Fill: "yellow", Shape: "ring", Text: "connecting"})
	// A flow starts its nodes one after another, a few milliseconds apart.
	// Connecting before the last of them has said what it publishes would be
	// born with half the metrics and then born again with the rest.
	select {
	case <-ctx.Done():
		return
	case <-time.After(spSettle):
	}
	for ctx.Err() == nil {
		up := time.Now()
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		s.ready, s.client = false, nil
		s.lastErr = err.Error()
		s.mu.Unlock()
		s.broadcastStatus(node.Status{Fill: "red", Shape: "ring", Text: truncate(err.Error(), 32)})
		s.svc.Log(node.LogWarn, "Sparkplug connection to %s lost, trying again in %s: %v", s.endpoint(), wait, err)
		if time.Since(up) > time.Minute {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait *= 2; wait > spMaxBackoff {
			wait = spMaxBackoff
		}
	}
}

func (s *spServer) options(clientID string, lost chan<- error) *mqtt.ClientOptions {
	scheme := "tcp"
	if s.tlsCfg != nil {
		scheme = "ssl"
	}
	o := mqtt.NewClientOptions()
	o.AddBroker(fmt.Sprintf("%s://%s", scheme, s.endpoint()))
	o.SetClientID(clientID)
	// Sparkplug asks for a clean session from every edge node and host.
	o.SetCleanSession(true)
	o.SetKeepAlive(s.keepalive)
	o.SetConnectTimeout(10 * time.Second)
	o.SetAutoReconnect(false)
	// Handlers run in the order messages arrive. A sequence number that is
	// checked out of order is a gap that was never there.
	o.SetOrderMatters(true)
	o.SetProtocolVersion(4)
	if s.user != "" {
		o.SetUsername(s.user)
		if s.pass != "" {
			o.SetPassword(s.pass)
		}
	}
	if s.tlsCfg != nil {
		o.SetTLSConfig(s.tlsCfg)
	}
	o.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		select {
		case lost <- err:
		default:
		}
	})
	return o
}

func spWait(tok mqtt.Token, what string, d time.Duration) error {
	if !tok.WaitTimeout(d) {
		return fmt.Errorf("%s: no answer from the broker in %s", what, d)
	}
	return tok.Error()
}

// explainSparkplugConnect says what a failed connect means.
func (s *spServer) explainConnect(err error) error {
	m := err.Error()
	switch {
	case strings.Contains(m, "refused") && strings.Contains(m, "connection refused"):
		return fmt.Errorf("%s refused the connection. Nothing is listening there: check the host and port, and that the broker is running", s.endpoint())
	case strings.Contains(m, "not Authorized"), strings.Contains(m, "Not Authorized"), strings.Contains(m, "bad user name"):
		return fmt.Errorf("%s rejected the login. Check the username and password, and that this user may publish and subscribe to spBv1.0/#", s.endpoint())
	case strings.Contains(m, "identifier rejected"):
		return fmt.Errorf("%s rejected the client ID. Another client may have it, or the broker restricts them", s.endpoint())
	case strings.Contains(m, "no such host"):
		return fmt.Errorf("couldn't find a host called %q. Check the spelling, that this host can reach its DNS, or use the broker's IP address", s.host)
	case strings.Contains(m, "i/o timeout"), strings.Contains(m, "timed out"), strings.Contains(m, "no answer"):
		return fmt.Errorf("no answer from %s. Check the address, the route to it and any firewall", s.endpoint())
	}
	return fmt.Errorf("connecting to %s: %w", s.endpoint(), err)
}

func (s *spServer) session(ctx context.Context) error {
	if s.role == spRoleEdge {
		return s.edgeSession(ctx)
	}
	return s.hostSession(ctx)
}

// ---------------------------------------------------------------------------
// Edge node
// ---------------------------------------------------------------------------

func (s *spServer) edgeTopic(kind string, device string) string {
	t := fmt.Sprintf("%s/%s/%s/%s", spNamespace, s.group, kind, s.edgeNode)
	if device != "" {
		t += "/" + device
	}
	return t
}

// bdSeqMetric is the birth and death sequence metric, an Int64 in both.
func bdSeqMetric(bd int) *spMetric {
	m, _ := spMetricFor(spBdSeqName, spInt64, float64(bd), true, 0)
	return m
}

func (s *spServer) nextBdSeq() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	bd := s.bdNext
	s.bdNext = (s.bdNext + 1) % 256
	return bd
}

func (s *spServer) edgeSession(ctx context.Context) error {
	bd := s.nextBdSeq()
	death := &spPayload{Metrics: []*spMetric{bdSeqMetric(bd)}}
	dp, err := death.encode()
	if err != nil {
		return err
	}
	lost := make(chan error, 1)
	o := s.options("hotloop-flow-"+s.group+"-"+s.edgeNode, lost)
	// The will is the NDEATH the broker publishes if this connection dies
	// without saying goodbye. It carries this session's bdSeq, which is how a
	// host tells this NDEATH from one for an earlier session.
	o.SetBinaryWill(s.edgeTopic("NDEATH", ""), dp, 1, false)
	c := mqtt.NewClient(o)
	if err := spWait(c.Connect(), "connecting", 15*time.Second); err != nil {
		return s.explainConnect(err)
	}

	sub := func(topic string) error {
		return spWait(c.Subscribe(topic, 1, s.onEdgeCommand), "subscribing to "+topic, 10*time.Second)
	}
	if err := sub(s.edgeTopic("NCMD", "")); err != nil {
		c.Disconnect(100)
		return err
	}
	if err := sub(s.edgeTopic("DCMD", "+")); err != nil {
		c.Disconnect(100)
		return err
	}

	s.mu.Lock()
	s.client, s.bdSeq = c, bd
	s.mu.Unlock()
	if err := s.birth(); err != nil {
		c.Disconnect(100)
		return err
	}

	select {
	case <-ctx.Done():
		// Leaving on purpose: an NDEATH first, because a clean DISCONNECT
		// throws the will away, and the hosts would never hear.
		s.mu.Lock()
		s.ready = false
		s.mu.Unlock()
		spWait(c.Publish(s.edgeTopic("NDEATH", ""), 1, false, dp), "publishing NDEATH", 2*time.Second)
		c.Disconnect(250)
		return ctx.Err()
	case err := <-lost:
		return err
	}
}

// nextSeq is the sequence number for the next message the edge node sends,
// zero to 255 and round again. The caller holds the lock.
func (s *spServer) nextSeq() uint64 {
	n := s.seq
	s.seq = (s.seq + 1) % 256
	return uint64(n)
}

// birth publishes the NBIRTH and then a DBIRTH for each device, which is the
// whole of what a host needs to know before data makes sense. It runs at
// connect and again on every rebirth request, with the same bdSeq. Nothing else
// is published while it runs.
func (s *spServer) birth() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil || !s.client.IsConnected() {
		return fmt.Errorf("not connected")
	}
	s.seq = 0
	now := spNowMillis()

	nb := &spPayload{Timestamp: now, HasTimestamp: true}
	nb.Metrics = append(nb.Metrics, bdSeqMetric(s.bdSeq))
	rb, _ := spMetricFor(spRebirthName, spBoolean, false, true, now)
	nb.Metrics = append(nb.Metrics, rb)
	// Aliases are numbers for the edge node's whole set of metrics, so they are
	// dealt out here, node first and then the devices in order.
	next := uint64(1)
	if s.aliases {
		for _, d := range s.deviceOrder() {
			for _, dc := range s.outs[d].decls {
				dc.alias, next = next, next+1
			}
		}
	}
	if o, ok := s.outs[""]; ok {
		ms, err := o.birthMetrics(now, s.aliases)
		if err != nil {
			return err
		}
		nb.Metrics = append(nb.Metrics, ms...)
	}
	nb.Seq, nb.HasSeq = s.nextSeq(), true
	if err := s.publishLocked(s.edgeTopic("NBIRTH", ""), nb); err != nil {
		return err
	}

	for _, d := range s.deviceOrder() {
		if d == "" {
			continue
		}
		ms, err := s.outs[d].birthMetrics(now, s.aliases)
		if err != nil {
			return err
		}
		db := &spPayload{Timestamp: now, HasTimestamp: true, Metrics: ms}
		db.Seq, db.HasSeq = s.nextSeq(), true
		if err := s.publishLocked(s.edgeTopic("DBIRTH", d), db); err != nil {
			return err
		}
		s.outs[d].alive = true
	}
	s.ready = true
	s.lastErr = ""
	count := len(nb.Metrics)
	go s.broadcastStatus(node.Status{Fill: "green", Shape: "dot", Text: fmt.Sprintf("born, %d metrics", count)})
	return nil
}

// deviceOrder is the node's own metrics first, then the devices by name. The
// caller holds the lock.
func (s *spServer) deviceOrder() []string {
	devs := make([]string, 0, len(s.outs))
	for d := range s.outs {
		if d != "" {
			devs = append(devs, d)
		}
	}
	sort.Strings(devs)
	if _, ok := s.outs[""]; ok {
		devs = append([]string{""}, devs...)
	}
	return devs
}

// publishLocked publishes at QoS 0, which is what every edge message but the
// will asks for. The caller holds the lock, which keeps sequence numbers and
// the order of the messages on the wire the same thing.
func (s *spServer) publishLocked(topic string, p *spPayload) error {
	b, err := p.encode()
	if err != nil {
		return err
	}
	tok := s.client.Publish(topic, 0, false, b)
	tok.WaitTimeout(2 * time.Second)
	return tok.Error()
}

// scheduleRebirth coalesces the nodes that register while a flow is starting
// into one rebirth.
func (s *spServer) scheduleRebirth() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return
	}
	if s.rebirthT != nil {
		s.rebirthT.Stop()
	}
	s.rebirthT = time.AfterFunc(300*time.Millisecond, func() {
		if err := s.birth(); err != nil {
			s.svc.Log(node.LogWarn, "Sparkplug rebirth failed: %v", err)
		}
	})
}

// registerOut adds a device, or the node's own metrics when the device is
// empty. One node per device: two writers of one device's metrics would be
// publishing against each other's sequence of births.
func (s *spServer) registerOut(o *spOutNode) error {
	s.mu.Lock()
	if s.role != spRoleEdge {
		s.mu.Unlock()
		return fmt.Errorf("this Sparkplug connection is a host application. Sparkplug Out publishes as an edge node, so pick or make an edge node connection")
	}
	if prev, ok := s.outs[o.device]; ok && prev != o {
		s.mu.Unlock()
		what := "the edge node's own metrics"
		if o.device != "" {
			what = "device " + o.device
		}
		return fmt.Errorf("another Sparkplug Out node already publishes %s on this connection. A device has one", what)
	}
	s.outs[o.device] = o
	s.mu.Unlock()
	s.scheduleRebirth()
	return nil
}

// unregisterOut takes a device away. Gone for good, it dies: a DDEATH, so a
// host stops showing its tags as live. A restart in place doesn't say that,
// since the node is back a moment later and reborn.
func (s *spServer) unregisterOut(o *spOutNode, removed bool) {
	s.mu.Lock()
	if s.outs[o.device] != o {
		s.mu.Unlock()
		return
	}
	delete(s.outs, o.device)
	if removed && o.device != "" && o.alive && s.ready && s.client != nil && s.client.IsConnected() {
		p := &spPayload{Timestamp: spNowMillis(), HasTimestamp: true}
		p.Seq, p.HasSeq = s.nextSeq(), true
		_ = s.publishLocked(s.edgeTopic("DDEATH", o.device), p)
		o.alive = false
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if removed {
		s.scheduleRebirth()
	}
}

// publishData sends the metrics that changed, as NDATA or DDATA, after making
// sure the device has been born.
func (s *spServer) publishData(o *spOutNode, updates []spUpdate, device bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.client == nil || !s.client.IsConnected() {
		reason := "the edge node isn't connected yet"
		if s.lastErr != "" {
			reason = "the edge node isn't connected: " + s.lastErr
		}
		return fmt.Errorf("%s, so nothing was published. Sparkplug sends no data before a birth, and the message isn't held for later", reason)
	}
	now := spNowMillis()
	// What the metrics hold now, for the next birth, which has to say so.
	for _, u := range updates {
		o.values[u.decl.name] = u.value
	}
	if !o.alive && device {
		// A device that died and came back gets its birth first, with what it
		// last held, and the new values go out as data right behind it.
		ms, err := o.birthMetrics(now, s.aliases)
		if err != nil {
			return err
		}
		db := &spPayload{Timestamp: now, HasTimestamp: true, Metrics: ms}
		db.Seq, db.HasSeq = s.nextSeq(), true
		if err := s.publishLocked(s.edgeTopic("DBIRTH", o.device), db); err != nil {
			return err
		}
		o.alive = true
	}
	p := &spPayload{Timestamp: now, HasTimestamp: true}
	for _, u := range updates {
		ts := u.timestamp
		if ts == 0 {
			ts = now
		}
		m, err := spMetricFor(u.decl.name, u.decl.dt, u.value, false, ts)
		if err != nil {
			return fmt.Errorf("metric %q: %w", u.decl.name, err)
		}
		if s.aliases && u.decl.alias != 0 {
			// With aliases the name is for births. Data carries the number.
			m.Name, m.Alias, m.HasAlias = "", u.decl.alias, true
		}
		p.Metrics = append(p.Metrics, m)
	}
	p.Seq, p.HasSeq = s.nextSeq(), true
	kind := "NDATA"
	if device {
		kind = "DDATA"
	}
	return s.publishLocked(s.edgeTopic(kind, o.device), p)
}

// publishDeviceDeath is a device saying it has gone, on purpose.
func (s *spServer) publishDeviceDeath(o *spOutNode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.client == nil || !s.client.IsConnected() {
		return fmt.Errorf("the edge node isn't connected, so there is no one to tell")
	}
	if !o.alive {
		return nil
	}
	p := &spPayload{Timestamp: spNowMillis(), HasTimestamp: true}
	p.Seq, p.HasSeq = s.nextSeq(), true
	if err := s.publishLocked(s.edgeTopic("DDEATH", o.device), p); err != nil {
		return err
	}
	o.alive = false
	return nil
}

// onEdgeCommand handles what a host sends this edge node: a rebirth request
// the node answers itself, and anything else it hands to the Sparkplug In
// nodes, which are the flow's way of acting on a command.
func (s *spServer) onEdgeCommand(_ mqtt.Client, m mqtt.Message) {
	parts := strings.Split(m.Topic(), "/")
	if len(parts) < 4 || parts[0] != spNamespace {
		return
	}
	kind := parts[2]
	device := ""
	if len(parts) == 5 {
		device = parts[4]
	}
	p, err := decodeSPPayload(m.Payload())
	if err != nil {
		s.svc.Log(node.LogWarn, "Sparkplug %s on %s: %v", kind, m.Topic(), err)
		return
	}
	ev := &spEvent{Topic: m.Topic(), Group: parts[1], Edge: parts[3], Device: device, Type: kind, Payload: p, Seq: -1}

	s.mu.Lock()
	decl := map[string]uint32{}
	byAlias := map[uint64]string{}
	if o, ok := s.outs[device]; ok {
		for _, d := range o.decls {
			decl[d.name] = d.dt
			if d.alias != 0 {
				byAlias[d.alias] = d.name
			}
		}
	}
	s.mu.Unlock()

	rebirth := false
	for _, mt := range p.Metrics {
		if mt.Name == "" && mt.HasAlias {
			if name, ok := byAlias[mt.Alias]; ok {
				mt.Name = name
			}
		}
		dt, known := decl[mt.Name]
		if !known && mt.HasDataType {
			dt = mt.DataType
		}
		if mt.Name == spRebirthName && kind == "NCMD" {
			if mt.raw.kind == 'b' && mt.raw.u != 0 {
				rebirth = true
			}
			continue
		}
		if known || mt.HasDataType {
			_ = mt.resolve(dt)
		} else {
			mt.Value = spInfer(mt.raw)
		}
		ev.Metrics = append(ev.Metrics, mt)
	}
	if rebirth {
		// Stop sending data, be born again with the same bdSeq. The lock
		// that birth takes is the pause in the data.
		go func() {
			if err := s.birth(); err != nil {
				s.svc.Log(node.LogWarn, "Sparkplug rebirth failed: %v", err)
			}
		}()
	}
	if len(ev.Metrics) > 0 {
		s.fanOut(ev)
	}
}

// spInfer is the value of a metric whose datatype nobody said, by how it was
// carried: a flow gets a number, a boolean, a string or bytes, and a metric
// that was carried as a 64 bit integer is read as signed.
func spInfer(r spRaw) any {
	switch r.kind {
	case 'u':
		return float64(int64(r.u))
	case 'f', 'd':
		return r.f
	case 'b':
		return r.u != 0
	case 's':
		return r.s
	case 'x':
		return r.x
	}
	return nil
}

// ---------------------------------------------------------------------------
// Host application
// ---------------------------------------------------------------------------

type spHostDevice struct {
	online  bool
	types   map[string]uint32
	aliases map[uint64]string
}

type spHostNode struct {
	group, edge string
	online      bool
	born        bool
	lastSeq     int
	want        int
	seen        map[int]bool
	gap         bool
	timer       *time.Timer
	types       map[string]uint32
	aliases     map[uint64]string
	devices     map[string]*spHostDevice
	askedAt     time.Time
}

func (s *spServer) stateTopic() string { return spNamespace + "/STATE/" + s.hostID }

func (s *spServer) statePayload(online bool) []byte {
	b, _ := json.Marshal(map[string]any{"online": online, "timestamp": s.stateTS})
	return b
}

func (s *spServer) hostSession(ctx context.Context) error {
	s.mu.Lock()
	s.stateTS = spNowMillis()
	s.nodes = map[string]*spHostNode{}
	s.mu.Unlock()

	lost := make(chan error, 1)
	o := s.options("hotloop-flow-host-"+s.hostID, lost)
	// The will is the STATE death certificate, retained, with the same
	// timestamp the birth will use. A producer that sees an offline STATE with
	// an older timestamp than the last online one knows to ignore it.
	o.SetBinaryWill(s.stateTopic(), s.statePayload(false), 1, true)
	c := mqtt.NewClient(o)
	if err := spWait(c.Connect(), "connecting", 15*time.Second); err != nil {
		return s.explainConnect(err)
	}

	// Subscribe to the namespace and to the STATE topics, and only then say
	// online. Publishing STATE first would invite edge nodes to be born while
	// nobody is listening.
	filter := spNamespace + "/#"
	if s.group != "" {
		filter = spNamespace + "/" + s.group + "/#"
	}
	if err := spWait(c.Subscribe(filter, 1, s.onHostMessage), "subscribing to "+filter, 10*time.Second); err != nil {
		c.Disconnect(100)
		return err
	}
	if s.group != "" {
		if err := spWait(c.Subscribe(spNamespace+"/STATE/+", 1, s.onHostMessage), "subscribing to the STATE topics", 10*time.Second); err != nil {
			c.Disconnect(100)
			return err
		}
	}
	s.mu.Lock()
	s.client = c
	s.mu.Unlock()
	if err := spWait(c.Publish(s.stateTopic(), 1, true, s.statePayload(true)), "publishing STATE", 10*time.Second); err != nil {
		c.Disconnect(100)
		return err
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	s.broadcastStatus(node.Status{Fill: "green", Shape: "dot", Text: "online"})

	select {
	case <-ctx.Done():
		s.mu.Lock()
		s.ready = false
		for _, n := range s.nodes {
			if n.timer != nil {
				n.timer.Stop()
			}
		}
		s.mu.Unlock()
		spWait(c.Publish(s.stateTopic(), 1, true, s.statePayload(false)), "publishing STATE offline", 2*time.Second)
		c.Disconnect(250)
		return ctx.Err()
	case err := <-lost:
		return err
	}
}

func (n *spHostNode) key() string { return n.group + "/" + n.edge }

func (s *spServer) hostNode(group, edge string) *spHostNode {
	k := group + "/" + edge
	n, ok := s.nodes[k]
	if !ok {
		n = &spHostNode{group: group, edge: edge, lastSeq: -1, seen: map[int]bool{},
			types: map[string]uint32{}, aliases: map[uint64]string{}, devices: map[string]*spHostDevice{}}
		s.nodes[k] = n
	}
	return n
}

// onHostMessage is every message on the namespace.
func (s *spServer) onHostMessage(c mqtt.Client, m mqtt.Message) {
	parts := strings.Split(m.Topic(), "/")
	if len(parts) < 3 || parts[0] != spNamespace {
		return
	}
	if parts[1] == "STATE" && len(parts) == 3 {
		s.onState(c, parts[2], m)
		return
	}
	if len(parts) < 4 {
		return
	}
	ev := &spEvent{Topic: m.Topic(), Group: parts[1], Type: parts[2], Edge: parts[3], Seq: -1}
	if len(parts) == 5 {
		ev.Device = parts[4]
	}
	switch ev.Type {
	case "NBIRTH", "NDEATH", "NDATA", "DBIRTH", "DDEATH", "DDATA", "NCMD", "DCMD":
	default:
		return
	}
	if (ev.Device != "") != strings.HasPrefix(ev.Type, "D") {
		s.svc.Log(node.LogWarn, "Sparkplug %s on %s has the wrong number of topic levels for its type", ev.Type, m.Topic())
		return
	}
	p, err := decodeSPPayload(m.Payload())
	if err != nil {
		s.svc.Log(node.LogWarn, "Sparkplug %s on %s: %v", ev.Type, m.Topic(), err)
		return
	}
	ev.Payload = p
	if p.HasSeq {
		ev.Seq = int(p.Seq)
	}
	s.hostApply(c, ev)
	s.fanOut(ev)
}

// hostApply is the host application's picture of the edge node. It checks the
// sequence, learns names, aliases and datatypes from the births, resolves the
// data that follows them, and marks what a death takes down.
func (s *spServer) hostApply(c mqtt.Client, ev *spEvent) {
	p := ev.Payload
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.hostNode(ev.Group, ev.Edge)

	askRebirth := func(why string) {
		if time.Since(n.askedAt) < spRebirthRetry {
			return
		}
		n.askedAt = time.Now()
		s.svc.Log(node.LogInfo, "Sparkplug host asking %s/%s to be born again: %s", ev.Group, ev.Edge, why)
		go s.requestRebirth(c, ev.Group, ev.Edge)
	}

	// Sequence. A birth starts it over and a death or command has none.
	switch ev.Type {
	case "NBIRTH":
		if n.timer != nil {
			n.timer.Stop()
		}
		n.seen, n.gap, n.timer = map[int]bool{}, false, nil
		n.lastSeq, n.want = ev.Seq, (ev.Seq+1)%256
		n.askedAt = time.Time{}
	case "DBIRTH", "NDATA", "DDATA", "DDEATH":
		if ev.Seq >= 0 && n.born {
			s.checkSeq(c, n, ev, askRebirth)
		}
	}

	resolve := func(ms []*spMetric, types map[string]uint32, aliases map[uint64]string, birth bool) {
		for _, mt := range ms {
			if mt.Name == "" && mt.HasAlias {
				if name, ok := aliases[mt.Alias]; ok {
					mt.Name = name
				} else {
					ev.Unsolved = append(ev.Unsolved, fmt.Sprintf("alias %d", mt.Alias))
				}
			}
			dt, ok := types[mt.Name]
			switch {
			case mt.HasDataType:
				dt = mt.DataType
			case !ok:
				mt.Value = spInfer(mt.raw)
				if mt.Name != "" {
					ev.Unsolved = append(ev.Unsolved, mt.Name)
				}
				continue
			}
			if err := mt.resolve(dt); err != nil {
				s.svc.Log(node.LogWarn, "Sparkplug %s/%s: %v", ev.Group, ev.Edge, err)
			}
			if birth && mt.Name != "" {
				types[mt.Name] = dt
				if mt.HasAlias {
					aliases[mt.Alias] = mt.Name
				}
			}
		}
		ev.Metrics = ms
	}

	switch ev.Type {
	case "NBIRTH":
		n.types, n.aliases = map[string]uint32{}, map[uint64]string{}
		n.devices = map[string]*spHostDevice{}
		resolve(p.Metrics, n.types, n.aliases, true)
		n.online, n.born = true, true
		for _, mt := range p.Metrics {
			if mt.Name == spBdSeqName && mt.Value != nil {
				if f, ok := mt.Value.(float64); ok {
					ev.BdSeq = &f
				}
			}
		}
	case "DBIRTH":
		d := &spHostDevice{online: true, types: map[string]uint32{}, aliases: map[uint64]string{}}
		n.devices[ev.Device] = d
		resolve(p.Metrics, d.types, d.aliases, true)
	case "NDATA":
		if !n.born {
			askRebirth("data before any birth")
		}
		resolve(p.Metrics, n.types, n.aliases, false)
	case "DDATA":
		d, ok := n.devices[ev.Device]
		if !ok {
			askRebirth("data from device " + ev.Device + " before its birth")
			d = &spHostDevice{types: map[string]uint32{}, aliases: map[uint64]string{}}
		}
		resolve(p.Metrics, d.types, d.aliases, false)
	case "NDEATH":
		for name := range n.types {
			ev.Stale = append(ev.Stale, name)
		}
		for dn, d := range n.devices {
			d.online = false
			for name := range d.types {
				ev.Stale = append(ev.Stale, dn+"/"+name)
			}
		}
		sort.Strings(ev.Stale)
		n.online = false
		resolve(p.Metrics, map[string]uint32{spBdSeqName: spInt64}, nil, false)
		for _, mt := range p.Metrics {
			if mt.Name == spBdSeqName && mt.Value != nil {
				if f, ok := mt.Value.(float64); ok {
					ev.BdSeq = &f
				}
			}
		}
		if n.timer != nil {
			n.timer.Stop()
			n.timer = nil
		}
	case "DDEATH":
		if d, ok := n.devices[ev.Device]; ok {
			d.online = false
			for name := range d.types {
				ev.Stale = append(ev.Stale, name)
			}
			sort.Strings(ev.Stale)
		}
	case "NCMD", "DCMD":
		for _, mt := range p.Metrics {
			if mt.HasDataType {
				_ = mt.resolve(mt.DataType)
			} else {
				mt.Value = spInfer(mt.raw)
			}
		}
		ev.Metrics = p.Metrics
	}
	if len(ev.Unsolved) > 0 && (ev.Type == "NDATA" || ev.Type == "DDATA") {
		askRebirth("data it can't name or type: " + strings.Join(ev.Unsolved, ", "))
	}
}

// checkSeq is the reorder logic. The sequence has to run on by one. When it
// doesn't, the host waits out the reorder timeout for the missing messages and
// asks for a rebirth if they don't turn up. The messages that did arrive are
// still delivered, marked as out of order, since the flow can use them and the
// rebirth will put things right. The caller holds the lock.
func (s *spServer) checkSeq(c mqtt.Client, n *spHostNode, ev *spEvent, ask func(string)) {
	if ev.Seq == n.want && !n.gap {
		n.lastSeq, n.want = ev.Seq, (ev.Seq+1)%256
		return
	}
	if !n.gap {
		n.gap = true
		n.seen = map[int]bool{}
		missing := fmt.Sprintf("expected sequence number %d and got %d", n.want, ev.Seq)
		if s.reorder == 0 {
			ask(missing)
		} else {
			n.timer = time.AfterFunc(s.reorder, func() {
				s.mu.Lock()
				still := n.gap
				s.mu.Unlock()
				if still {
					s.mu.Lock()
					n.askedAt = time.Time{}
					s.mu.Unlock()
					go s.requestRebirth(c, n.group, n.edge)
				}
			})
		}
	}
	ev.Out = true
	n.seen[ev.Seq] = true
	// The missing ones arriving fills the gap, and a full gap is a closed one.
	for n.seen[n.want] {
		delete(n.seen, n.want)
		n.lastSeq, n.want = n.want, (n.want+1)%256
	}
	if len(n.seen) == 0 {
		n.gap = false
		if n.timer != nil {
			n.timer.Stop()
			n.timer = nil
		}
	}
}

func (s *spServer) requestRebirth(c mqtt.Client, group, edge string) {
	mt, _ := spMetricFor(spRebirthName, spBoolean, true, true, 0)
	p := &spPayload{Timestamp: spNowMillis(), HasTimestamp: true, Metrics: []*spMetric{mt}}
	b, err := p.encode()
	if err != nil {
		return
	}
	c.Publish(fmt.Sprintf("%s/%s/NCMD/%s", spNamespace, group, edge), 0, false, b)
}

// onState is a STATE message from any host application, ours included.
func (s *spServer) onState(c mqtt.Client, host string, m mqtt.Message) {
	var st map[string]any
	if err := json.Unmarshal(m.Payload(), &st); err != nil {
		s.svc.Log(node.LogWarn, "Sparkplug STATE on %s isn't JSON: %v", m.Topic(), err)
		return
	}
	// If anyone delivers our own offline certificate, say online again at once
	// and with the timestamp we connected with. That is the case of a broker
	// that kept a will it should have dropped.
	if host == s.hostID {
		if on, _ := st["online"].(bool); !on {
			go c.Publish(s.stateTopic(), 1, true, s.statePayload(true))
		}
	}
	ev := &spEvent{Topic: m.Topic(), Type: "STATE", Edge: host, Seq: -1, State: st}
	s.fanOut(ev)
}

// publishCommand sends an NCMD or a DCMD, which are the one thing a host
// application publishes besides STATE.
func (s *spServer) publishCommand(group, edge, device string, metrics []*spMetric) error {
	s.mu.Lock()
	c := s.client
	ready := s.ready
	s.mu.Unlock()
	if s.role != spRoleHost {
		return fmt.Errorf("this Sparkplug connection is an edge node. Commands go from a host application, so pick or make a host connection")
	}
	if !ready || c == nil || !c.IsConnected() {
		return fmt.Errorf("the host application isn't connected to the broker, so the command wasn't sent")
	}
	topic := fmt.Sprintf("%s/%s/NCMD/%s", spNamespace, group, edge)
	if device != "" {
		topic = fmt.Sprintf("%s/%s/DCMD/%s/%s", spNamespace, group, edge, device)
	}
	p := &spPayload{Timestamp: spNowMillis(), HasTimestamp: true, Metrics: metrics}
	b, err := p.encode()
	if err != nil {
		return err
	}
	return spWait(c.Publish(topic, 0, false, b), "publishing the command", 5*time.Second)
}

// knownAlias is the alias the host learned for a metric from the birth, if the
// edge node used one. A host sends commands for such a metric by number alone.
func (s *spServer) knownAlias(group, edge, device, name string) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[group+"/"+edge]
	if !ok {
		return 0, false
	}
	aliases := n.aliases
	if device != "" {
		d, ok := n.devices[device]
		if !ok {
			return 0, false
		}
		aliases = d.aliases
	}
	for a, nm := range aliases {
		if nm == name {
			return a, true
		}
	}
	return 0, false
}

// knownType is the datatype the host learned for a metric from the birth.
func (s *spServer) knownType(group, edge, device, name string) (uint32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[group+"/"+edge]
	if !ok {
		return 0, false
	}
	if device == "" {
		dt, ok := n.types[name]
		return dt, ok
	}
	d, ok := n.devices[device]
	if !ok {
		return 0, false
	}
	dt, ok := d.types[name]
	return dt, ok
}
