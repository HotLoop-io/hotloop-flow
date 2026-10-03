package node

// StandIn takes the place of everything outside the process while a flow test
// runs: the broker, the database, the web service, the socket, the file and
// the command.
//
// A test that ran an MQTT Out for real would publish to the plant's broker, and
// at that point it isn't a test, it's a deploy. So a node that reaches outside
// asks its services for a stand-in first. When there is one, the node does all
// of its own work right up to the wire (resolves the topic, renders the URL,
// builds the SQL and its parameters, encodes the payload) and hands the result
// to the stand-in instead of the network. The test can then check exactly what
// would have gone out, and say what comes back.
type StandIn interface {
	// Call records what the node would have sent. kind says what sort of
	// thing it is ("mqtt", "http", "postgres", and so on) and sent is the
	// detail, JSON-shaped.
	//
	// reply is what the test says came back. A nil reply means the outside
	// world answered with nothing and success: no rows, an empty 200, a
	// command that exited 0. An error is a failure the test scripted, a
	// database that's down or a host that refused the connection, and the
	// node reports it exactly as it would report the real one.
	Call(kind string, sent map[string]any) (reply map[string]any, err error)
}

// StandInOf returns the stand-in a node's services carry, or nil when the node
// is running for real.
//
// It's an optional method rather than part of Services because nothing but a
// flow test ever provides one, and every other implementation, the runtime's
// and each test harness's, shouldn't have to grow a method that answers nil.
func StandInOf(svc Services) StandIn {
	if p, ok := svc.(interface{ StandIn() StandIn }); ok {
		return p.StandIn()
	}
	return nil
}
