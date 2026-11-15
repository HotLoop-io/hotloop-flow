package nodes

import (
	"sync/atomic"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// replayGate is paho's message store with one addition: it knows when paho owes a
// replay of the store and has not yet taken it.
//
// After a reconnect paho marks the connection up and only then replays every
// publish still in its store (paho.mqtt.golang v1.5.1, client.go:345-346: the
// connection is started, which sets the status to connected, and resume runs
// after it). Resume takes its snapshot of the store with All
// (client.go:1058) and sends each stored publish again with the DUP flag
// (client.go:1115-1128), with no check against a publish already on its way. A
// publish handed to paho in between is put in the store first (client.go:809)
// and sent directly (client.go:828), so the snapshot holds it and it is sent a
// second time. That duplicate has no unconfirmed send behind it, which is what a
// durable output promises about every duplicate it causes.
//
// So the link is not reported up until the snapshot has been taken: a publish
// made after it is not in it. MQTT 3.1.1 section 4.4 has a client resend
// unacknowledged publishes on reconnect, and section 4.3.2 allows the duplicate
// that causes; what is not allowed here is sending a message twice by our own
// doing.
type replayGate struct {
	mqtt.Store
	due atomic.Bool
}

func newReplayGate(inner mqtt.Store) *replayGate {
	if inner == nil {
		inner = mqtt.NewMemoryStore()
	}
	g := &replayGate{Store: inner}
	g.due.Store(true) // a first connection replays or resets the store too
	return g
}

// arm says a connection is about to be made and the store will be replayed or
// reset. Paho calls the reconnecting handler before every attempt
// (client.go:321).
func (g *replayGate) arm() { g.due.Store(true) }

// All is the replay's snapshot (client.go:1058). Once it has returned, a publish
// is not in it.
func (g *replayGate) All() []string {
	keys := g.Store.All()
	g.due.Store(false)
	return keys
}

// Reset is what a first connection with a clean session does instead of a replay
// (client.go:293).
func (g *replayGate) Reset() {
	g.Store.Reset()
	g.due.Store(false)
}

// Ready is whether paho has finished with the store for this connection.
func (g *replayGate) Ready() bool { return !g.due.Load() }
