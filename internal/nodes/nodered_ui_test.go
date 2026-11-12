package nodes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

type uiRig struct {
	t   *testing.T
	svc *configServices
	all []node.Node
}

func newUIRig(t *testing.T) *uiRig {
	t.Helper()
	r := &uiRig{t: t, svc: newConfigServices()}
	t.Cleanup(func() {
		for i := len(r.all) - 1; i >= 0; i-- {
			if c, ok := r.all[i].(node.Closer); ok {
				_ = c.Close(context.Background(), true)
			}
		}
	})
	return r
}

// config builds and starts a config node, such as a tab or a group.
func (r *uiRig) config(typ, id, cfg string) {
	r.t.Helper()
	n := buildConfig(r.t, typ, id, cfg, r.svc)
	r.svc.configs[id] = n
	r.all = append(r.all, n)
	if s, ok := n.(node.Starter); ok {
		if err := s.Start(context.Background(), newTestEmitter()); err != nil {
			r.t.Fatalf("starting %s: %v", typ, err)
		}
	}
}

// widget builds and starts a widget on the flow, with its own emitter.
func (r *uiRig) widget(typ, id, cfg string) (node.Node, *testEmitter) {
	r.t.Helper()
	reg, ok := node.Default.Lookup(typ)
	if !ok {
		r.t.Fatalf("%s is not registered", typ)
	}
	en := parseNodeConfig(r.t, typ, cfg)
	en.ID, en.Raw["id"] = id, id
	n, err := reg.New(&node.Definition{Node: en, Services: r.svc})
	if err != nil {
		r.t.Fatalf("building %s: %v", typ, err)
	}
	r.all = append(r.all, n)
	e := newTestEmitter()
	if err := n.(node.Starter).Start(context.Background(), e); err != nil {
		r.t.Fatalf("starting %s: %v", typ, err)
	}
	return n, e
}

func (r *uiRig) layoutJSON() map[string]any {
	r.t.Helper()
	ui.mu.Lock()
	hub := ui.hub
	ui.mu.Unlock()
	rec := httptest.NewRecorder()
	hub.Handler("/ui").ServeHTTP(rec, httptest.NewRequest("GET", "/ui/api/layout", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// click posts an operator event to a widget the way the page does.
func (r *uiRig) event(id, body string) int {
	r.t.Helper()
	ui.mu.Lock()
	hub := ui.hub
	ui.mu.Unlock()
	rec := httptest.NewRecorder()
	hub.Handler("/ui").ServeHTTP(rec, httptest.NewRequest("POST", "/ui/api/emit", strings.NewReader(`{"id":"`+id+`","event":`+body+`}`)))
	return rec.Code
}

func (r *uiRig) hubState(id string) (map[string]any, any) {
	ui.mu.Lock()
	hub := ui.hub
	ui.mu.Unlock()
	return hub.State(id)
}

func (r *uiRig) page() {
	r.config("ui_base", "base", `{"theme":{"name":"theme-dark"}}`)
	r.config("ui_tab", "tab1", `{"name":"Main Screen","icon":"dashboard","order":1,"disabled":false,"hidden":false}`)
	r.config("ui_group", "grp1", `{"name":"Heater 1","tab":"tab1","order":1,"disp":true,"width":"6","collapse":false}`)
}

func TestTheDashboardIsMountedWhileADashboardNodeRunsAndGoneAfter(t *testing.T) {
	r := newUIRig(t)
	if _, _, ok := Routes.Match("GET", "/ui/api/layout"); ok {
		t.Fatal("the dashboard was mounted with nothing running")
	}
	r.page()
	for _, p := range []string{"/ui", "/ui/", "/ui/api/layout", "/ui/api/events", "/ui/app.js"} {
		if _, _, ok := Routes.Match("GET", p); !ok {
			t.Errorf("%s is not served", p)
		}
	}
	if _, _, ok := Routes.Match("POST", "/ui/api/emit"); !ok {
		t.Error("the operator's input is not served")
	}
	for i := len(r.all) - 1; i >= 0; i-- {
		_ = r.all[i].(node.Closer).Close(context.Background(), true)
	}
	r.all = nil
	if _, _, ok := Routes.Match("GET", "/ui/api/layout"); ok {
		t.Fatal("the dashboard stayed mounted after every node stopped")
	}
}

func TestADashboardNodeFailsClearlyWhenAnotherNodeHoldsThePath(t *testing.T) {
	r := newUIRig(t)
	unbind, err := Routes.Register("http-in-node", flowMethodAny, "/ui", httpNoop{})
	if err != nil {
		t.Fatal(err)
	}
	defer unbind()
	n := buildConfig(t, "ui_tab", "tabX", `{"name":"x"}`, r.svc)
	err = n.(node.Starter).Start(context.Background(), newTestEmitter())
	if err == nil || !strings.Contains(err.Error(), UIPathEnv) {
		t.Fatalf("err = %v, want one that says how to move the dashboard", err)
	}
}

type httpNoop struct{}

func (httpNoop) ServeHTTP(http.ResponseWriter, *http.Request) {}

func TestWidgetsLandInTheirGroupAndTabInOrder(t *testing.T) {
	r := newUIRig(t)
	r.page()
	r.widget("ui_text", "t2", `{"group":"grp1","order":2,"width":4,"height":1,"label":"two","format":"{{msg.payload}} F"}`)
	r.widget("ui_text", "t1", `{"group":"grp1","order":1,"width":"0","height":"0","label":"one"}`)
	r.config("ui_spacer", "sp", `{"name":"spacer","group":"grp1","order":3,"width":1,"height":1}`)
	r.config("ui_link", "lk", `{"name":"Dryer 1","link":"http://172.25.0.1:1880/ui","icon":"open_in_browser","target":"thistab","order":6}`)
	l := r.layoutJSON()
	tabs := l["tabs"].([]any)
	if len(tabs) != 1 {
		t.Fatalf("tabs = %v", tabs)
	}
	ws := tabs[0].(map[string]any)["groups"].([]any)[0].(map[string]any)["widgets"].([]any)
	var order []string
	for _, w := range ws {
		order = append(order, w.(map[string]any)["id"].(string))
	}
	if strings.Join(order, ",") != "t1,t2,sp" {
		t.Fatalf("widgets = %v", order)
	}
	if len(l["links"].([]any)) != 1 {
		t.Fatalf("links = %v", l["links"])
	}
	// The editor's bookkeeping does not go to the browser.
	props := ws[1].(map[string]any)["props"].(map[string]any)
	for _, k := range []string{"x", "y", "z", "wires", "id"} {
		if _, ok := props[k]; ok {
			t.Errorf("props carry %q", k)
		}
	}
	if props["format"] != "{{msg.payload}} F" {
		t.Fatalf("props = %v", props)
	}
}

func TestATextWidgetKeepsTheLastMessageForANewBrowser(t *testing.T) {
	r := newUIRig(t)
	r.page()
	n, _ := r.widget("ui_text", "t1", `{"group":"grp1","label":"Temp","format":"{{msg.payload}} F"}`)
	if _, err := send(t, n, msg(t, `{"payload":151.5,"topic":"temp","big":"`+strings.Repeat("x", 20000)+`"}`)); err != nil {
		t.Fatal(err)
	}
	m, _ := r.hubState("t1")
	if m["payload"] != 151.5 || m["topic"] != "temp" {
		t.Fatalf("state = %v", m)
	}
	if _, kept := m["big"]; kept {
		t.Fatal("a 20 KB property was sent to the browser")
	}
}

func TestAButtonPressSendsItsTypedPayloadAndTopicFromTheLastMessage(t *testing.T) {
	r := newUIRig(t)
	r.page()
	// The shape the SBF flows carry: payload 1 as a number, the topic read from msg.payload.
	n, e := r.widget("ui_button", "b1", `{"group":"grp1","label":"Heater 1","passthru":false,"payload":"1","payloadType":"num","topic":"payload","topicType":"msg","bgcolor":"Gray"}`)
	if code := r.event("b1", `{"event":"click"}`); code != 204 {
		t.Fatalf("press = %d", code)
	}
	got := e.on(0)
	if len(got) != 1 || got[0].Payload() != 1.0 {
		t.Fatalf("sent = %v", got)
	}
	if _, has := got[0].Data["topic"]; has {
		t.Fatalf("a topic read from a message that never came should be absent, got %v", got[0].Data["topic"])
	}
	if _, err := send(t, n, msg(t, `{"payload":"state-a"}`)); err != nil {
		t.Fatal(err)
	}
	r.event("b1", `{"event":"click"}`)
	if got := e.on(0); len(got) != 2 || got[1].Topic() != "state-a" {
		t.Fatalf("second press = %v", got)
	}
	if code := r.event("b1", `{"event":"hover"}`); code != 204 || len(e.on(0)) != 2 {
		t.Fatal("an event that is not a press sent a message")
	}
	if code := r.event("ghost", `{"event":"click"}`); code != 404 {
		t.Fatalf("unknown widget = %d", code)
	}
}

func TestASwitchSendsItsOnAndOffValuesAndFollowsMessages(t *testing.T) {
	r := newUIRig(t)
	r.page()
	// The captured switch is inverted: on sends false, off sends true.
	n, e := r.widget("ui_switch", "s1", `{"group":"grp1","label":"Edit Mode On / Off","passthru":true,"decouple":"false","topic":"topic","topicType":"msg","onvalue":"false","onvalueType":"bool","offvalue":"true","offvalueType":"bool"}`)
	r.event("s1", `{"event":"change","value":true}`)
	r.event("s1", `{"event":"change","value":false}`)
	got := e.on(0)
	if len(got) != 2 || got[0].Payload() != false || got[1].Payload() != true {
		t.Fatalf("sent = %v", got)
	}
	if _, v := r.hubState("s1"); v != false {
		t.Fatalf("not decoupled, so the switch moves at once. value = %v", v)
	}
	// A message with the on value turns it on, and passthru passes it along.
	if _, err := send2(t, n, e, `{"payload":false}`); err != nil {
		t.Fatal(err)
	}
	if _, v := r.hubState("s1"); v != true {
		t.Fatalf("payload equal to the on value should switch it on, value = %v", v)
	}
	if len(e.on(0)) != 3 {
		t.Fatalf("passthru did not pass the message on: %d", len(e.on(0)))
	}
	if _, err := send2(t, n, e, `{"payload":true}`); err != nil {
		t.Fatal(err)
	}
	if _, v := r.hubState("s1"); v != false {
		t.Fatalf("payload equal to the off value should switch it off, value = %v", v)
	}
}

// send2 sends a message into a node that was started with its own emitter.
func send2(t *testing.T, n node.Node, e *testEmitter, body string) (*testEmitter, error) {
	t.Helper()
	return e, n.Receive(context.Background(), msg(t, body), e)
}

func TestADecoupledSwitchOnlyMovesOnAMessage(t *testing.T) {
	r := newUIRig(t)
	r.page()
	n, e := r.widget("ui_switch", "s2", `{"group":"grp1","decouple":"true","onvalue":"true","onvalueType":"bool","offvalue":"false","offvalueType":"bool"}`)
	r.event("s2", `{"event":"change","value":true}`)
	if len(e.on(0)) != 1 || e.on(0)[0].Payload() != true {
		t.Fatalf("sent = %v", e.on(0))
	}
	if _, v := r.hubState("s2"); v == true {
		t.Fatal("a decoupled switch moved on a click")
	}
	_, _ = send2(t, n, e, `{"payload":true}`)
	if _, v := r.hubState("s2"); v != true {
		t.Fatalf("value = %v", v)
	}
}

func TestANumericStopsAtItsLimitsAndTakesOnlyNumbers(t *testing.T) {
	r := newUIRig(t)
	r.page()
	n, e := r.widget("ui_numeric", "n1", `{"group":"grp1","label":"Current<br>Temp","passthru":false,"topic":"topic","topicType":"msg","format":"{{value}}  F","min":0,"max":"3000","step":1}`)
	r.event("n1", `{"event":"change","value":5000}`)
	r.event("n1", `{"event":"change","value":-7}`)
	r.event("n1", `{"event":"change","value":125.5}`)
	var got []any
	for _, m := range e.on(0) {
		got = append(got, m.Payload())
	}
	if len(got) != 3 || got[0] != 3000.0 || got[1] != 0.0 || got[2] != 125.5 {
		t.Fatalf("sent = %v", got)
	}
	_, _ = send2(t, n, e, `{"payload":"42"}`)
	if _, v := r.hubState("n1"); v != 42.0 {
		t.Fatalf("a numeric string should set the value, got %v", v)
	}
	_, _ = send2(t, n, e, `{"payload":"hot"}`)
	if _, v := r.hubState("n1"); v != 42.0 {
		t.Fatalf("text moved the value to %v", v)
	}
	if len(e.statuses) == 0 || e.statuses[len(e.statuses)-1].Text != "not a number" {
		t.Fatalf("statuses = %v", e.statuses)
	}
}

func TestATextInputSendsWhatWasTyped(t *testing.T) {
	r := newUIRig(t)
	r.page()
	_, e := r.widget("ui_text_input", "i1", `{"group":"grp1","mode":"number","delay":"1","topic":"topic","topicType":"msg","sendOnBlur":true,"passthru":true}`)
	r.event("i1", `{"event":"change","value":72.5}`)
	if got := e.on(0); len(got) != 1 || got[0].Payload() != 72.5 {
		t.Fatalf("sent = %v", got)
	}
}

func TestAChartKeepsPointsPerTopicAndClearsOnAnEmptyList(t *testing.T) {
	r := newUIRig(t)
	r.page()
	n, e := r.widget("ui_chart", "c1", `{"group":"grp1","label":"Heater 2","chartType":"line","removeOlder":1,"removeOlderPoints":"","removeOlderUnit":"3600"}`)
	for _, b := range []string{`{"payload":10,"topic":"a"}`, `{"payload":"11.5","topic":"a"}`, `{"payload":3,"topic":"b"}`, `{"payload":"words","topic":"b"}`, `{"payload":true,"topic":"b"}`} {
		if _, err := send2(t, n, e, b); err != nil {
			t.Fatal(err)
		}
	}
	body := r.snapshot()
	if strings.Count(body, `"s":"a"`) != 2 || strings.Count(body, `"s":"b"`) != 1 {
		t.Fatalf("snapshot = %s", body)
	}
	_, _ = send2(t, n, e, `{"payload":[]}`)
	if body := r.snapshot(); strings.Contains(body, `"s":"a"`) {
		t.Fatalf("an empty list did not clear the chart: %s", body)
	}
}

func TestATemplateSendsWhatItsScriptSendsAndCanForward(t *testing.T) {
	r := newUIRig(t)
	r.page()
	n, e := r.widget("ui_template", "tp1", `{"group":"grp1","name":"Custom LED","format":"<div ng-click=\"send({payload: true})\">{{msg.payload}}</div>","templateScope":"local","fwdInMessages":true}`)
	r.event("tp1", `{"event":"send","msg":{"payload":true,"topic":"pressed"}}`)
	if got := e.on(0); len(got) != 1 || got[0].Payload() != true || got[0].Topic() != "pressed" {
		t.Fatalf("sent = %v", got)
	}
	_, _ = send2(t, n, e, `{"payload":"x"}`)
	if len(e.on(0)) != 2 {
		t.Fatalf("fwdInMessages did not forward: %d", len(e.on(0)))
	}
	if code := r.event("tp1", `{"event":"send","msg":"not an object"}`); code != 204 || len(e.on(0)) != 2 {
		t.Fatal("a send with no message object produced a message")
	}
}

func TestEveryCapturedWidgetShapeBuilds(t *testing.T) {
	r := newUIRig(t)
	r.page()
	for typ, cfg := range map[string]string{
		"ui_text":       `{"group":"grp1","order":5,"width":4,"height":1,"name":"","label":"","format":"{{msg.payload}} F","layout":"col-center","className":"","style":false,"font":"","fontSize":16,"color":"#000000"}`,
		"ui_text_input": `{"group":"grp1","order":2,"width":0,"height":0,"passthru":true,"mode":"number","delay":"1","topic":"topic","sendOnBlur":true,"className":"","topicType":"msg"}`,
		"ui_led":        `{"order":5,"group":"grp1","width":0,"height":0,"label":"Heater 1","labelPlacement":"left","labelAlignment":"left","colorForValue":[{"color":"#4f4a4a","value":"false","valueType":"bool"},{"color":"#eb0000","value":"true","valueType":"bool"}],"allowColorForValueInMessage":false,"shape":"circle","showGlow":true,"name":""}`,
		"ui_gauge":      `{"group":"grp1","order":2,"width":"5","height":"3","gtype":"gage","title":"Stage Indicator","label":"{{msg.label}}","format":"{{value}}","min":0,"max":8,"colors":["#aeb2ae","#00b500","#3bd8d6"],"seg1":"0","seg2":"7","diff":false,"className":""}`,
		"ui_chart":      `{"group":"grp1","order":2,"width":"16","height":"12","label":"Heater 2","chartType":"line","legend":"false","xformat":"HH:mm:ss","interpolate":"linear","nodata":"","dot":false,"ymin":"","ymax":"","removeOlder":1,"removeOlderPoints":"","removeOlderUnit":"3600","cutout":0,"useOneColor":false,"useUTC":false,"colors":["#1f77b4"],"outputs":1,"useDifferentColor":false}`,
		"ui_template":   `{"group":"grp1","name":"Custom LED","order":3,"width":"1","height":"1","format":"<div></div>","storeOutMessages":true,"fwdInMessages":true,"resendOnRefresh":true,"templateScope":"local"}`,
	} {
		t.Run(typ, func(t *testing.T) { r.widget(typ, "w-"+typ, cfg) })
	}
	if err := buildErr(t, "ui_text", `{"label":"no group"}`, r.svc); err == nil {
		t.Error("a widget with no group was accepted")
	}
}

// safeWriter is a response writer a test can read while a handler writes to it.
type safeWriter struct {
	mu  sync.Mutex
	buf strings.Builder
	h   http.Header
}

func (w *safeWriter) Header() http.Header { return w.h }
func (w *safeWriter) WriteHeader(int)     {}
func (w *safeWriter) Flush()              {}
func (w *safeWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(b)
}
func (w *safeWriter) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.buf.String() }

// snapshot is what a browser that connects now is told: the state event.
func (r *uiRig) snapshot() string {
	r.t.Helper()
	ui.mu.Lock()
	hub := ui.hub
	ui.mu.Unlock()
	w := &safeWriter{h: http.Header{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hub.Handler("/ui").ServeHTTP(w, httptest.NewRequest("GET", "/ui/api/events", nil).WithContext(ctx))
		close(done)
	}()
	waitFor(r.t, 3*time.Second, "the snapshot", func() bool { return strings.Contains(w.String(), "event: state") })
	cancel()
	<-done
	return w.String()
}
