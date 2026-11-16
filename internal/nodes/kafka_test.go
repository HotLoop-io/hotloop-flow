package nodes

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

func TestKafkaConfigIsHeldToWhatItNeeds(t *testing.T) {
	for _, c := range []struct{ cfg, want string }{
		{`{"brokers":""}`, "at least one broker"},
		{`{"brokers":"kafka"}`, "needs a port"},
		{`{"brokers":"k:9092","sasl":"plain"}`, "username and a password"},
		{`{"brokers":"k:9092","sasl":"kerberos"}`, "not none, plain"},
	} {
		svc := newTestServices()
		if err := buildErr(t, "hotloop-flow-kafka", c.cfg, svc); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.cfg, err, c.want)
		}
	}
	svc := newTestServices()
	svc.creds["password"] = "pw"
	for _, m := range []string{"plain", "scram-sha-256", "scram-sha-512"} {
		if _, err := build(t, "hotloop-flow-kafka", `{"brokers":"a:9092, b:9092","sasl":"`+m+`","username":"u"}`, svc).(*kafkaConn), error(nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKafkaRefusesATopicThatIsNotOneBeforeSending(t *testing.T) {
	cfg := build(t, "hotloop-flow-kafka", `{"brokers":"127.0.0.1:1"}`, newTestServices())
	n := outNode(t, "kafka out", `{"broker":"srv","topic":"a b"}`, cfg)
	if _, err := send(t, n, msg(t, `{"payload":"x"}`)); err == nil || !strings.Contains(err.Error(), "topic") {
		t.Fatalf("%v", err)
	}
}

func TestKafkaErrorsWhenNoBrokerAnswersAndKeepsThePassword(t *testing.T) {
	svc := newTestServices()
	svc.creds["password"] = "hunter2pw"
	cfg := build(t, "hotloop-flow-kafka", `{"brokers":"127.0.0.1:1","sasl":"plain","username":"u"}`, svc)
	n := outNode(t, "kafka out", `{"broker":"srv","topic":"t"}`, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	e := newTestEmitter()
	err := n.Receive(ctx, msg(t, `{"payload":"x"}`), e)
	if err == nil || strings.Contains(err.Error(), "hunter2pw") || len(e.sent[0]) != 0 {
		t.Fatalf("%v", err)
	}
	_ = cfg.(*kafkaConn).Close(context.Background(), false)
}

// HOTLOOP_FLOW_TEST_KAFKA=host:port names a real broker (Redpanda in the lab). The
// node produces and a consumer reads the record back with its key.
func TestKafkaAgainstARealBroker(t *testing.T) {
	addr := os.Getenv("HOTLOOP_FLOW_TEST_KAFKA")
	if addr == "" {
		t.Skip("HOTLOOP_FLOW_TEST_KAFKA is not set")
	}
	topic := "flowtest-" + time.Now().Format("150405")
	svc := newTestServices()
	cfg := build(t, "hotloop-flow-kafka", `{"brokers":"`+addr+`"}`, svc)
	defer cfg.(*kafkaConn).Close(context.Background(), false)
	n := outNode(t, "kafka out", `{"broker":"srv","topic":"`+topic+`","key":"press4"}`, cfg)
	if _, err := send(t, n, msg(t, `{"payload":{"psi":41.5}}`)); err != nil {
		t.Fatal(err)
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(addr), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fetches := cl.PollFetches(ctx)
	if err := fetches.Err(); err != nil {
		t.Fatal(err)
	}
	var got []*kgo.Record
	fetches.EachRecord(func(r *kgo.Record) { got = append(got, r) })
	if len(got) != 1 || string(got[0].Key) != "press4" || string(got[0].Value) != `{"psi":41.5}` {
		t.Fatalf("broker holds %d records", len(got))
	}
}
