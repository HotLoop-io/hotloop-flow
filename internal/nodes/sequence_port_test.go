package nodes

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// Split, Join and Batch as Node-RED 5 has them. Each test is one behaviour a
// flow written for Node-RED depends on.

func parts(t *testing.T, m *engine.Msg) map[string]any {
	t.Helper()
	p, ok := m.Data[engine.PropParts].(map[string]any)
	if !ok {
		t.Fatalf("no msg.parts on %v", m.Data)
	}
	return p
}

func payloads(ms []*engine.Msg) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m.Payload()
	}
	return out
}

// A serial port hands over whatever bytes it has. A line that straddles two
// reads comes out whole, and the index keeps counting across reads.
func TestSplitStreamsDelimitedText(t *testing.T) {
	sp := build(t, "split", `{"splt":"\\n","spltType":"str","stream":true}`, newTestServices())
	e := newTestEmitter()
	for _, chunk := range []string{"a\nb", "c\nd\n", "e"} {
		if err := pushTo(t, sp, e, engine.NewMsgWithPayload(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	got := e.on(0)
	if !reflect.DeepEqual(payloads(got), []any{"a", "bc", "d"}) {
		t.Fatalf("streamed lines = %v, want a, bc, d with e held back", payloads(got))
	}
	for i, m := range got {
		p := parts(t, m)
		if p["index"] != float64(i) {
			t.Errorf("line %d has index %v: the index has to keep counting across messages", i, p["index"])
		}
		if _, has := p["count"]; has {
			t.Errorf("line %d carries a count, but a stream has no end", i)
		}
		if p["ch"] != "\n" {
			t.Errorf("line %d parts.ch = %q", i, p["ch"])
		}
	}
	// The held back piece is finished by the next read.
	if err := pushTo(t, sp, e, engine.NewMsgWithPayload("f\n")); err != nil {
		t.Fatal(err)
	}
	if last := e.on(0)[3]; last.Payload() != "ef" {
		t.Errorf("the carried piece came out as %v", last.Payload())
	}
}

func TestSplitStreamsFixedLengthRecords(t *testing.T) {
	sp := build(t, "split", `{"splt":"2","spltType":"len","stream":true}`, newTestServices())
	e := newTestEmitter()
	for _, chunk := range []string{"abcde", "fgh"} {
		if err := pushTo(t, sp, e, engine.NewMsgWithPayload(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := payloads(e.on(0)); !reflect.DeepEqual(got, []any{"ab", "cd", "ef", "gh"}) {
		t.Fatalf("records = %v", got)
	}
	for i, m := range e.on(0) {
		if parts(t, m)["index"] != float64(i) {
			t.Errorf("record %d index = %v", i, parts(t, m)["index"])
		}
	}
}

func TestSplitStreamsBuffersOnAByteSequence(t *testing.T) {
	sp := build(t, "split", `{"splt":"[13,10]","spltType":"bin","stream":true}`, newTestServices())
	e := newTestEmitter()
	for _, chunk := range [][]byte{[]byte("one\r"), []byte("\ntwo\r\nthr"), []byte("ee\r\n")} {
		m := engine.NewMsg()
		m.SetPayload(chunk)
		if err := pushTo(t, sp, e, m); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, m := range e.on(0) {
		got = append(got, string(m.Payload().([]byte)))
	}
	if !reflect.DeepEqual(got, []string{"one", "two", "three"}) {
		t.Fatalf("records = %q, want a CRLF split across reads to come out whole", got)
	}
	if ch := parts(t, e.on(0)[0])["ch"]; !reflect.DeepEqual(ch, []any{13.0, 10.0}) {
		t.Errorf("parts.ch = %v, want the byte sequence", ch)
	}
}

// Node-RED counts the empty piece after a trailing delimiter and then never
// sends it, so a Join downstream waits forever. Here the count is what went
// out.
func TestSplitBufferWithTrailingDelimiterCountsWhatItSends(t *testing.T) {
	sp := build(t, "split", `{"splt":",","spltType":"str"}`, newTestServices())
	jn := build(t, "join", `{"mode":"auto"}`, newTestServices())
	m := engine.NewMsg()
	m.SetPayload([]byte("a,b,"))
	split, err := send(t, sp, m)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(split.on(0)); n != 2 {
		t.Fatalf("sent %d pieces, want 2", n)
	}
	e := newTestEmitter()
	for _, p := range split.on(0) {
		if c := parts(t, p)["count"]; c != 2.0 {
			t.Fatalf("parts.count = %v, want 2", c)
		}
		if err := pushTo(t, jn, e, p); err != nil {
			t.Fatal(err)
		}
	}
	if e.total() != 1 {
		t.Fatal("the join never completed")
	}
}

// Split puts the separator in msg.parts.ch so an automatic Join puts it back.
// Joining a comma separated line with newlines would quietly change the data.
func TestJoinRestoresTheSeparatorSplitUsed(t *testing.T) {
	sp := build(t, "split", `{"splt":",","spltType":"str"}`, newTestServices())
	jn := build(t, "join", `{"mode":"auto"}`, newTestServices())
	split, err := send(t, sp, engine.NewMsgWithPayload("21.5,22.0,22.4"))
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEmitter()
	for _, p := range split.on(0) {
		if err := pushTo(t, jn, e, p); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.on(0)[0].Payload(); got != "21.5,22.0,22.4" {
		t.Errorf("rejoined %q", got)
	}
}

// A sequence split again keeps the outer one on a stack, and two automatic
// Joins take it apart again in order.
func TestSplitAndJoinNestedSequences(t *testing.T) {
	outer := build(t, "split", `{}`, newTestServices())
	inner := build(t, "split", `{}`, newTestServices())
	joinInner := build(t, "join", `{"mode":"auto"}`, newTestServices())
	joinOuter := build(t, "join", `{"mode":"auto"}`, newTestServices())

	first, err := send(t, outer, msg(t, `{"payload":[[1,2],[3]],"topic":"line"}`))
	if err != nil {
		t.Fatal(err)
	}
	var innerParts []*engine.Msg
	for _, m := range first.on(0) {
		e, err := send(t, inner, m)
		if err != nil {
			t.Fatal(err)
		}
		innerParts = append(innerParts, e.on(0)...)
	}
	if _, nested := parts(t, innerParts[0])["parts"]; !nested {
		t.Fatal("the inner split did not keep the outer sequence on msg.parts.parts")
	}
	rejoined := newTestEmitter()
	for _, m := range innerParts {
		if err := pushTo(t, joinInner, rejoined, m); err != nil {
			t.Fatal(err)
		}
	}
	final := newTestEmitter()
	for _, m := range rejoined.on(0) {
		if err := pushTo(t, joinOuter, final, m); err != nil {
			t.Fatal(err)
		}
	}
	got := final.on(0)
	if len(got) != 1 || !reflect.DeepEqual(got[0].Payload(), []any{[]any{1.0, 2.0}, []any{3.0}}) {
		t.Fatalf("rebuilt %v", payloads(got))
	}
	if _, left := got[0].Data[engine.PropParts]; left {
		t.Error("the fully rejoined message still has msg.parts")
	}
}

// Splitting a property other than payload records which one, and the Join
// puts the result back there.
func TestSplitAndJoinAnotherProperty(t *testing.T) {
	sp := build(t, "split", `{"property":"readings"}`, newTestServices())
	jn := build(t, "join", `{"mode":"auto"}`, newTestServices())
	split, err := send(t, sp, msg(t, `{"readings":[1,2],"payload":"untouched"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p := parts(t, split.on(0)[0])["property"]; p != "readings" {
		t.Fatalf("parts.property = %v", p)
	}
	e := newTestEmitter()
	for _, m := range split.on(0) {
		if err := pushTo(t, jn, e, m); err != nil {
			t.Fatal(err)
		}
	}
	out := e.on(0)[0]
	if v, _, _ := out.Get("readings"); !reflect.DeepEqual(v, []any{1.0, 2.0}) || out.Payload() != "untouched" {
		t.Errorf("rejoined readings = %v, payload = %v", v, out.Payload())
	}
}

func TestSplitObjectCopiesTheKey(t *testing.T) {
	sp := build(t, "split", `{"addname":"topic"}`, newTestServices())
	e, err := send(t, sp, msg(t, `{"payload":{"press":7,"line":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	var topics []string
	for _, m := range e.on(0) {
		topics = append(topics, m.Topic())
	}
	if !reflect.DeepEqual(topics, []string{"line", "press"}) {
		t.Errorf("topics = %v, want each key copied into msg.topic", topics)
	}
}

// Messages arriving out of order are placed by index, not by arrival.
func TestJoinPlacesByIndex(t *testing.T) {
	jn := build(t, "join", `{"mode":"auto"}`, newTestServices())
	e := newTestEmitter()
	for _, i := range []int{2, 0, 1} {
		m := msg(t, `{"payload":"p`+ftoa(float64(i))+`","parts":{"id":"s","index":`+ftoa(float64(i))+`,"count":3,"type":"array"}}`)
		if err := pushTo(t, jn, e, m); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.on(0)[0].Payload(); !reflect.DeepEqual(got, []any{"p0", "p1", "p2"}) {
		t.Errorf("joined %v", got)
	}
}

// The timeout sends whatever has arrived, counted from the first message.
// msg.restartTimeout starts the wait again; msg.reset throws the group away.
func TestJoinTimeout(t *testing.T) {
	jn := build(t, "join", `{"mode":"custom","build":"array","count":10,"timeout":"0.2"}`, newTestServices())
	e := newTestEmitter()
	start := time.Now()
	for _, m := range []string{`{"payload":1}`, `{"payload":2}`} {
		if err := pushTo(t, jn, e, msg(t, m)); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 2*time.Second, "the timeout to send", func() bool { return e.total() == 1 })
	if took := time.Since(start); took < 150*time.Millisecond {
		t.Errorf("sent after %s, before the timeout", took)
	}
	if got := e.on(0)[0].Payload(); !reflect.DeepEqual(got, []any{1.0, 2.0}) {
		t.Errorf("timed out group = %v", got)
	}

	// restartTimeout pushes the deadline back.
	e2 := newTestEmitter()
	if err := pushTo(t, jn, e2, msg(t, `{"payload":3}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if err := pushTo(t, jn, e2, msg(t, `{"payload":4,"restartTimeout":true}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if e2.total() != 0 {
		t.Fatal("sent at the original deadline despite msg.restartTimeout")
	}
	waitFor(t, 2*time.Second, "the restarted timeout", func() bool { return e2.total() == 1 })

	// reset drops the group, and nothing is sent for it.
	e3 := newTestEmitter()
	if err := pushTo(t, jn, e3, msg(t, `{"payload":5}`)); err != nil {
		t.Fatal(err)
	}
	if err := pushTo(t, jn, e3, msg(t, `{"reset":true}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(350 * time.Millisecond)
	if e3.total() != 0 {
		t.Error("a reset group was still sent")
	}
}

func TestJoinObjectByKeyAndMerged(t *testing.T) {
	byKey := build(t, "join", `{"mode":"custom","build":"object","key":"topic","count":2}`, newTestServices())
	e := newTestEmitter()
	for _, m := range []string{`{"topic":"temp","payload":21.5}`, `{"topic":"rpm","payload":1450}`} {
		if err := pushTo(t, byKey, e, msg(t, m)); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.on(0)[0].Payload(); !reflect.DeepEqual(got, map[string]any{"temp": 21.5, "rpm": 1450.0}) {
		t.Errorf("keyed object = %v", got)
	}

	merged := build(t, "join", `{"mode":"custom","build":"merged","count":3}`, newTestServices())
	e = newTestEmitter()
	for _, m := range []string{`{"payload":{"a":1}}`, `{"payload":{"b":2,"a":9}}`, `{"payload":{"c":3}}`} {
		if err := pushTo(t, merged, e, msg(t, m)); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.on(0)[0].Payload(); !reflect.DeepEqual(got, map[string]any{"a": 9.0, "b": 2.0, "c": 3.0}) {
		t.Errorf("merged = %v", got)
	}
}

// With accumulate on, the result keeps growing and goes out every time the
// count is reached again.
func TestJoinAccumulates(t *testing.T) {
	jn := build(t, "join", `{"mode":"custom","build":"object","key":"topic","count":2,"accumulate":true}`, newTestServices())
	e := newTestEmitter()
	for _, m := range []string{`{"topic":"a","payload":1}`, `{"topic":"b","payload":2}`, `{"topic":"a","payload":3}`} {
		if err := pushTo(t, jn, e, msg(t, m)); err != nil {
			t.Fatal(err)
		}
	}
	got := payloads(e.on(0))
	want := []any{map[string]any{"a": 1.0, "b": 2.0}, map[string]any{"a": 3.0, "b": 2.0}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("accumulated = %v, want %v", got, want)
	}
}

// Array.join renders each element the way JavaScript's String() does.
func TestJoinStringRendersValuesLikeJavaScript(t *testing.T) {
	jn := build(t, "join", `{"mode":"custom","build":"string","joiner":"|","count":6}`, newTestServices())
	e := newTestEmitter()
	for _, m := range []string{`{"payload":1}`, `{"payload":2.5}`, `{"payload":null}`, `{"payload":true}`,
		`{"payload":[1,2]}`, `{"payload":{"x":1}}`} {
		if err := pushTo(t, jn, e, msg(t, m)); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.on(0)[0].Payload(); got != "1|2.5||true|1,2|[object Object]" {
		t.Errorf("joined %q", got)
	}
	for f, want := range map[float64]string{1e21: "1e+21", 1e-7: "1e-7", 0.000001: "0.000001", 123456789: "123456789", -0.5: "-0.5"} {
		if got := jsNumberString(f); got != want {
			t.Errorf("jsNumberString(%v) = %q, want %q", f, got, want)
		}
	}
}

func TestJoinBufferWithAJoiner(t *testing.T) {
	jn := build(t, "join", `{"mode":"custom","build":"buffer","joiner":"[44]","joinerType":"bin","count":3}`, newTestServices())
	e := newTestEmitter()
	for _, v := range []any{[]byte("ab"), "cd", []any{101.0}} {
		m := engine.NewMsg()
		m.SetPayload(v)
		if err := pushTo(t, jn, e, m); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.on(0)[0].Payload(); !reflect.DeepEqual(got, []byte("ab,cd,e")) {
		t.Errorf("joined %q", got)
	}
}

// Reduce folds a sequence with a JSONata expression: an average of a split
// array here, with $A the running total, a fixup dividing by $N, and the
// result on the last message.
func TestJoinReduce(t *testing.T) {
	sp := build(t, "split", `{}`, newTestServices())
	jn := build(t, "join", `{"mode":"reduce","reduceExp":"$A + payload","reduceInit":"0","reduceInitType":"num",
        "reduceFixup":"$A / $N","reduceRight":false}`, newTestServices())
	split, err := send(t, sp, msg(t, `{"payload":[20,22,27]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEmitter()
	for _, m := range split.on(0) {
		if err := pushTo(t, jn, e, m); err != nil {
			t.Fatal(err)
		}
	}
	if got := payloads(e.on(0)); !reflect.DeepEqual(got, []any{23.0}) {
		t.Fatalf("reduced to %v, want the average 23", got)
	}

	// reduceRight folds from the last index down, and $I is the index.
	right := build(t, "join", `{"mode":"reduce","reduceExp":"$A & $string($I)","reduceInit":"","reduceInitType":"str","reduceRight":true}`,
		newTestServices())
	split, _ = send(t, sp, msg(t, `{"payload":["a","b","c"]}`))
	e = newTestEmitter()
	for _, m := range split.on(0) {
		if err := pushTo(t, right, e, m); err != nil {
			t.Fatal(err)
		}
	}
	if got := payloads(e.on(0)); !reflect.DeepEqual(got, []any{"210"}) {
		t.Errorf("right fold = %v, want 210", got)
	}

	// A message that is not part of a sequence passes through untouched.
	e = newTestEmitter()
	if err := pushTo(t, jn, e, msg(t, `{"payload":"loose"}`)); err != nil {
		t.Fatal(err)
	}
	if got := payloads(e.on(0)); !reflect.DeepEqual(got, []any{"loose"}) {
		t.Errorf("a loose message came out as %v", got)
	}
}

func TestJoinReduceRefusesABrokenExpression(t *testing.T) {
	err := buildErr(t, "join", `{"mode":"reduce","reduceExp":"$A +"}`, newTestServices())
	if err == nil || !strings.Contains(err.Error(), "JSONata") {
		t.Fatalf("err = %v", err)
	}
}

// Interval mode groups whatever arrived in each period, and can mark an empty
// period with an empty sequence.
func TestBatchByInterval(t *testing.T) {
	b := build(t, "batch", `{"mode":"interval","interval":"0.2","allowEmptySequence":true}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, b, e)
	defer cancel()
	for i := range 3 {
		if err := pushTo(t, b, e, engine.NewMsgWithPayload(float64(i))); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 2*time.Second, "the first period's group", func() bool { return e.total() == 3 })
	got := e.on(0)
	if !reflect.DeepEqual(payloads(got), []any{0.0, 1.0, 2.0}) {
		t.Fatalf("group = %v", payloads(got))
	}
	first := got[0].ID()
	for i, m := range got {
		p := parts(t, m)
		if p["id"] != first || p["index"] != float64(i) || p["count"] != 3.0 {
			t.Errorf("message %d parts = %v; the id is the first message's id", i, p)
		}
	}
	// The next period has nothing in it.
	waitFor(t, 2*time.Second, "the empty sequence", func() bool { return e.total() == 4 })
	empty := e.on(0)[3]
	if empty.Payload() != nil || parts(t, empty)["count"] != 1.0 {
		t.Errorf("empty sequence = %v", empty.Data)
	}
	cancel()
}

// honourParts ends a group at the end of an incoming sequence instead of
// letting it run into the next one.
func TestBatchHonoursIncomingSequences(t *testing.T) {
	b := build(t, "batch", `{"mode":"count","count":10,"honourParts":true}`, newTestServices())
	sp := build(t, "split", `{}`, newTestServices())
	split, _ := send(t, sp, msg(t, `{"payload":["a","b","c"]}`))
	e := newTestEmitter()
	for _, m := range split.on(0) {
		if err := pushTo(t, b, e, m); err != nil {
			t.Fatal(err)
		}
	}
	got := e.on(0)
	if len(got) != 3 {
		t.Fatalf("sent %d, want the group of 3 to end with its sequence", len(got))
	}
	// The split's parts survive under the batch's id, index and count.
	if parts(t, got[0])["type"] != "array" {
		t.Error("the incoming msg.parts fields were dropped")
	}
}

// Concatenate glues complete sequences together in the order their topics are
// listed, whatever order they arrived in.
func TestBatchConcatenates(t *testing.T) {
	b := build(t, "batch", `{"mode":"concat","topics":[{"topic":"first"},{"topic":"second"}]}`, newTestServices())
	e := newTestEmitter()
	in := []string{
		`{"topic":"second","payload":"s0","parts":{"id":"B","index":0,"count":1}}`,
		`{"topic":"first","payload":"f0","parts":{"id":"A","index":0,"count":2}}`,
		`{"topic":"ignored","payload":"x"}`,
		`{"topic":"first","payload":"f1","parts":{"id":"A","index":1,"count":2}}`,
	}
	for _, m := range in {
		if err := pushTo(t, b, e, msg(t, m)); err != nil {
			t.Fatal(err)
		}
	}
	if got := payloads(e.on(0)); !reflect.DeepEqual(got, []any{"f0", "f1", "s0"}) {
		t.Fatalf("concatenated %v", got)
	}
	if err := pushTo(t, b, e, msg(t, `{"topic":"first","payload":"no parts"}`)); err == nil {
		t.Error("a message with no msg.parts was accepted into a concatenation")
	}
}

func TestBatchReset(t *testing.T) {
	b := build(t, "batch", `{"mode":"count","count":2}`, newTestServices())
	e := newTestEmitter()
	for _, m := range []string{`{"payload":1}`, `{"reset":true}`, `{"payload":2}`} {
		if err := pushTo(t, b, e, msg(t, m)); err != nil {
			t.Fatal(err)
		}
	}
	if e.total() != 0 {
		t.Errorf("a reset batch still sent %v", payloads(e.on(0)))
	}
}
