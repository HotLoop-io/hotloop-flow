package nodes

import (
	"reflect"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/wiretype"
)

// The deploy check reads an Inject node's static values itself. This holds that
// reading to what the node really sends: every value the check believes is
// there is on the message the node emits, and equal.
func TestTheDeployChecksReadingOfInjectLiteralsIsWhatTheNodeSends(t *testing.T) {
	configs := map[string]string{
		"typed props": `{"props":[
			{"p":"payload","v":"42.5","vt":"num"},
			{"p":"topic","v":"line3","vt":"str"},
			{"p":"ok","v":"true","vt":"bool"},
			{"p":"obj","v":"{\"a\":[1,2]}","vt":"json"},
			{"p":"nested.deep","v":"x","vt":"str"},
			{"p":"when","v":"","vt":"date"},
			{"p":"computed","v":"1+1","vt":"jsonata"}]}`,
		"topic row with no value": `{"topic":"from the node","props":[{"p":"payload","v":"hi","vt":"str"},{"p":"topic"}]}`,
		"old style":               `{"payload":"hello","payloadType":"str","topic":"t"}`,
		"old style default":       `{"payload":"","topic":"t"}`,
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			n := build(t, "inject", cfg, newTestServices())
			e, err := send(t, n, msg(t, `{}`))
			if err != nil {
				t.Fatal(err)
			}
			sent := e.on(0)[0]
			believed := wiretype.InjectValues(parseNodeConfig(t, "inject", cfg))
			if len(believed) == 0 {
				t.Fatal("the check found no literals")
			}
			for path, want := range believed {
				got, ok, _ := sent.Get(path)
				if !ok || !reflect.DeepEqual(got, want) {
					t.Errorf("%s: the check believes %#v, the node sent %#v (present %v)", path, want, got, ok)
				}
			}
		})
	}
}
