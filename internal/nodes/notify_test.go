package nodes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// The notification nodes against servers that answer the way ntfy, Gotify and
// Discord answer. The same nodes run against the real ntfy and Gotify in the lab
// when HOTLOOP_FLOW_TEST_NTFY and HOTLOOP_FLOW_TEST_GOTIFY name them (below). Discord
// is only ever a fake here: no test channel exists, so what a real Discord does is
// what its documentation says, which is what these fakes are written from.

type seen struct {
	mu   sync.Mutex
	reqs []*http.Request
	body [][]byte
}

func (s *seen) add(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, r)
	s.body = append(s.body, b)
	s.mu.Unlock()
}

func configured(t *testing.T, typ, cfg string, creds map[string]string) (node.Node, *testServices) {
	t.Helper()
	svc := newTestServices()
	for k, v := range creds {
		svc.creds[k] = v
	}
	return build(t, typ, cfg, svc), svc
}

// outNode builds an out node whose config node is cfgNode, under the id "srv".
func outNode(t *testing.T, typ, cfgJSON string, cfgNode node.Node) node.Node {
	t.Helper()
	svc := &servicesWithConfig{testServices: newTestServices(), configs: map[string]node.Node{"srv": cfgNode}}
	return build(t, typ, cfgJSON, svc)
}

func noWait(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	old := notifyWait
	notifyWait = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	t.Cleanup(func() { notifyWait = old })
	return &waits
}

// ---- ntfy

func TestNtfyPublishesThePayloadWithItsHeaders(t *testing.T) {
	var got seen
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got.add(r); w.Write([]byte(`{"id":"x"}`)) }))
	defer ts.Close()
	cfg, _ := configured(t, "hotloop-flow-ntfy", `{"server":"`+ts.URL+`"}`, map[string]string{"token": "tk_secret"})
	n := outNode(t, "ntfy out", `{"server":"srv","topic":"line3","title":"Press 4","priority":"high","tags":"warning,press","click":"https://hl.example/alarms"}`, cfg)
	e, err := send(t, n, msg(t, `{"payload":"oil pressure low"}`))
	if err != nil {
		t.Fatal(err)
	}
	r := got.reqs[0]
	if r.Method != "POST" || r.URL.Path != "/line3" || string(got.body[0]) != "oil pressure low" {
		t.Fatalf("%s %s %q", r.Method, r.URL.Path, got.body[0])
	}
	for k, v := range map[string]string{"Title": "Press 4", "Priority": "4", "Tags": "warning,press", "Click": "https://hl.example/alarms", "Authorization": "Bearer tk_secret"} {
		if r.Header.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, r.Header.Get(k), v)
		}
	}
	if len(e.sent[0]) != 1 {
		t.Fatal("the message was not passed on")
	}
}

func TestNtfyTakesWhatTheNodeLeavesEmptyFromTheMessage(t *testing.T) {
	var got seen
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got.add(r) }))
	defer ts.Close()
	cfg, _ := configured(t, "hotloop-flow-ntfy", `{"server":"`+ts.URL+`","username":"ops"}`, map[string]string{"password": "pw"})
	n := outNode(t, "ntfy out", `{"server":"srv"}`, cfg)
	if _, err := send(t, n, msg(t, `{"payload":{"level":3},"topic":"from_msg","title":"T","priority":"urgent"}`)); err != nil {
		t.Fatal(err)
	}
	r := got.reqs[0]
	if r.URL.Path != "/from_msg" || r.Header.Get("Title") != "T" || r.Header.Get("Priority") != "5" || string(got.body[0]) != `{"level":3}` {
		t.Fatalf("%s %v %q", r.URL.Path, r.Header, got.body[0])
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("ops:pw")); r.Header.Get("Authorization") != want {
		t.Fatalf("auth %q", r.Header.Get("Authorization"))
	}
}

func TestNtfyKeepsTheReasonAndNoSecret(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":40301,"http":403,"error":"forbidden: topic mysecrettopic needs tk_secret"}`, 403)
	}))
	defer ts.Close()
	cfg, _ := configured(t, "hotloop-flow-ntfy", `{"server":"`+ts.URL+`"}`, map[string]string{"token": "tk_secret"})
	n := outNode(t, "ntfy out", `{"server":"srv","topic":"mysecrettopic"}`, cfg)
	e, err := send(t, n, msg(t, `{"payload":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("the reason was lost: %v", err)
	}
	if strings.Contains(err.Error(), "mysecrettopic") || strings.Contains(err.Error(), "tk_secret") {
		t.Fatalf("a secret is in the error: %v", err)
	}
	for _, s := range e.statuses {
		if strings.Contains(s.Text, "mysecrettopic") {
			t.Fatal("the topic is in the status")
		}
	}
	if len(e.sent[0]) != 0 {
		t.Fatal("a failed message was passed on")
	}
}

func TestNtfyRefusesATopicThatIsNotOne(t *testing.T) {
	cfg, _ := configured(t, "hotloop-flow-ntfy", `{"server":"http://127.0.0.1:1"}`, nil)
	n := outNode(t, "ntfy out", `{"server":"srv","topic":"a/b"}`, cfg)
	if _, err := send(t, n, msg(t, `{"payload":"x"}`)); err == nil || !strings.Contains(err.Error(), "topic") {
		t.Fatalf("a topic with a slash was sent: %v", err)
	}
}

func TestNtfyWaitsOutOneRateLimitAndNoMore(t *testing.T) {
	waits := noWait(t)
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(429)
	}))
	defer ts.Close()
	cfg, _ := configured(t, "hotloop-flow-ntfy", `{"server":"`+ts.URL+`"}`, nil)
	n := outNode(t, "ntfy out", `{"server":"srv","topic":"t"}`, cfg)
	if _, err := send(t, n, msg(t, `{"payload":"x"}`)); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("%v", err)
	}
	if calls != 2 || len(*waits) != 1 || (*waits)[0] != 3*time.Second {
		t.Fatalf("calls %d waits %v", calls, *waits)
	}
	// A wait longer than a minute is not waited on.
	*waits = nil
	calls = 0
	ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(429)
	})
	if _, err := send(t, n, msg(t, `{"payload":"x"}`)); err == nil || calls != 1 || len(*waits) != 0 {
		t.Fatalf("calls %d waits %v err %v", calls, *waits, err)
	}
}

func TestNtfyConfigRefusesWhatIsNotAnAddress(t *testing.T) {
	if err := buildErr(t, "hotloop-flow-ntfy", `{"server":"ntfy.sh"}`, newTestServices()); err == nil {
		t.Fatal("a server with no scheme was accepted")
	}
}

// ---- Gotify

func TestGotifyPostsAMessageAsTheApplication(t *testing.T) {
	var got seen
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got.add(r); w.Write([]byte(`{"id":1}`)) }))
	defer ts.Close()
	cfg, _ := configured(t, "hotloop-flow-gotify", `{"server":"`+ts.URL+`/"}`, map[string]string{"token": "AppTok"})
	n := outNode(t, "gotify out", `{"server":"srv","title":"Line 3","priority":8}`, cfg)
	if _, err := send(t, n, msg(t, `{"payload":"door open"}`)); err != nil {
		t.Fatal(err)
	}
	r := got.reqs[0]
	var j map[string]any
	_ = json.Unmarshal(got.body[0], &j)
	if r.URL.Path != "/message" || r.Header.Get("X-Gotify-Key") != "AppTok" || j["title"] != "Line 3" || j["message"] != "door open" || j["priority"] != float64(8) {
		t.Fatalf("%s %v %v", r.URL.Path, r.Header, j)
	}
}

func TestGotifyRefusesWithoutAToken(t *testing.T) {
	if err := buildErr(t, "hotloop-flow-gotify", `{"server":"http://x"}`, newTestServices()); err == nil {
		t.Fatal("a Gotify server with no token was accepted")
	}
}

func TestGotifyKeepsTheReasonAndNoToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"Unauthorized","errorCode":401,"errorDescription":"you need to provide a valid access token AppTok"}`))
	}))
	defer ts.Close()
	cfg, _ := configured(t, "hotloop-flow-gotify", `{"server":"`+ts.URL+`"}`, map[string]string{"token": "AppTok"})
	n := outNode(t, "gotify out", `{"server":"srv"}`, cfg)
	_, err := send(t, n, msg(t, `{"payload":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "valid access token") || strings.Contains(err.Error(), "AppTok") {
		t.Fatalf("%v", err)
	}
}

// ---- Discord

func TestDiscordWebhookPostsContentAndAsksForAnAnswer(t *testing.T) {
	var got seen
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got.add(r); w.Write([]byte(`{"id":"1"}`)) }))
	defer ts.Close()
	n, _ := configured(t, "discord out", `{}`, map[string]string{"url": ts.URL + "/api/webhooks/123/tok"})
	if _, err := send(t, n, msg(t, `{"payload":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	r := got.reqs[0]
	var j map[string]any
	_ = json.Unmarshal(got.body[0], &j)
	if r.URL.Path != "/api/webhooks/123/tok" || r.URL.Query().Get("wait") != "true" || j["content"] != "hello" {
		t.Fatalf("%s %s %v", r.URL.Path, r.URL.RawQuery, j)
	}
	if am, _ := j["allowed_mentions"].(map[string]any); am == nil || len(am["parse"].([]any)) != 0 {
		t.Fatalf("a message could ping: %v", j["allowed_mentions"])
	}
}

func TestDiscordCutsWhatDiscordWouldRefuseAndSendsAnEmbedForATitle(t *testing.T) {
	var got seen
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got.add(r) }))
	defer ts.Close()
	n, _ := configured(t, "discord out", `{"title":"Alarm","color":"red"}`, map[string]string{"url": ts.URL + "/api/webhooks/1/t"})
	long := strings.Repeat("😀", 3000)
	if _, err := send(t, n, msg(t, `{"payload":"`+long+`"}`)); err != nil {
		t.Fatal(err)
	}
	var j struct {
		Embeds []struct {
			Title string
			Desc  string `json:"description"`
			Color int
		}
		Content string
	}
	_ = json.Unmarshal(got.body[0], &j)
	if len(j.Embeds) != 1 || j.Embeds[0].Title != "Alarm" || j.Embeds[0].Color != discordColors["red"] || j.Content != "" {
		t.Fatalf("%+v", j)
	}
	if utf16Len(j.Embeds[0].Desc) > discordDescMax {
		t.Fatalf("description is %d units", utf16Len(j.Embeds[0].Desc))
	}
	// Plain content is cut at 2,000 units, counted the way Discord counts.
	n2, _ := configured(t, "discord out", `{}`, map[string]string{"url": ts.URL + "/api/webhooks/1/t"})
	if _, err := send(t, n2, msg(t, `{"payload":"`+long+`"}`)); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(got.body[1], &j)
	if utf16Len(j.Content) > discordContentMax || !strings.HasSuffix(j.Content, "…") {
		t.Fatalf("content %d units", utf16Len(j.Content))
	}
}

func TestDiscordNeverShowsTheWebhookURL(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Unknown Webhook","code":10015}`))
	}))
	url := ts.URL + "/api/webhooks/123/supersecret"
	n, _ := configured(t, "discord out", `{}`, map[string]string{"url": url})
	e, err := send(t, n, msg(t, `{"payload":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "Unknown Webhook") || strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("%v", err)
	}
	for _, s := range e.statuses {
		if strings.Contains(s.Text, "supersecret") {
			t.Fatal("the URL is in the status")
		}
	}
	// A transport failure names the address it could not reach, which is the credential.
	ts.Close()
	_, err = send(t, n, msg(t, `{"payload":"x"}`))
	if err == nil || strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("the URL is in a transport error: %v", err)
	}
}

func TestDiscordWaitsForTheTimeItIsTold(t *testing.T) {
	waits := noWait(t)
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(429)
			w.Write([]byte(`{"message":"You are being rate limited.","retry_after":1.5,"global":false}`))
		}
	}))
	defer ts.Close()
	n, _ := configured(t, "discord out", `{}`, map[string]string{"url": ts.URL + "/api/webhooks/1/t"})
	if _, err := send(t, n, msg(t, `{"payload":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(*waits) != 1 || (*waits)[0] != 1500*time.Millisecond {
		t.Fatalf("calls %d waits %v", calls, *waits)
	}
}

func TestDiscordWebhookConfigRefusesWhatIsNotAnAddress(t *testing.T) {
	if err := buildErr(t, "discord out", `{}`, newTestServices()); err == nil {
		t.Fatal("no webhook URL was accepted")
	}
}

func TestDiscordBotPostsToAChannelAsTheBot(t *testing.T) {
	var got seen
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got.add(r); w.Write([]byte(`{"id":"9"}`)) }))
	defer ts.Close()
	cfg, _ := configured(t, "hotloop-flow-discord-bot", `{"api":"`+ts.URL+`"}`, map[string]string{"token": "bottok"})
	n := outNode(t, "discord bot out", `{"bot":"srv","channel":"123456789012345678"}`, cfg)
	if _, err := send(t, n, msg(t, `{"payload":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	r := got.reqs[0]
	if r.URL.Path != "/channels/123456789012345678/messages" || r.Header.Get("Authorization") != "Bot bottok" {
		t.Fatalf("%s %v", r.URL.Path, r.Header)
	}
	// A channel that is not an ID is refused before anything is sent.
	bad := outNode(t, "discord bot out", `{"bot":"srv","channel":"general"}`, cfg)
	if _, err := send(t, bad, msg(t, `{"payload":"hi"}`)); err == nil || len(got.reqs) != 1 {
		t.Fatalf("a channel name was sent: %v", err)
	}
	// msg.channel is used when the node leaves it empty.
	open := outNode(t, "discord bot out", `{"bot":"srv"}`, cfg)
	if _, err := send(t, open, msg(t, `{"payload":"hi","channel":"223456789012345678"}`)); err != nil || got.reqs[1].URL.Path != "/channels/223456789012345678/messages" {
		t.Fatalf("%v", err)
	}
}

func TestEveryNotificationNodeHasADocumentedPlaceInTheMatrix(t *testing.T) {
	for _, typ := range []string{"ntfy out", "gotify out", "discord out", "discord bot out", "hotloop-flow-ntfy", "hotloop-flow-gotify", "hotloop-flow-discord-bot"} {
		if _, ok := node.Default.Lookup(typ); !ok {
			t.Errorf("%s is not registered", typ)
		}
	}
}

// ---- the real servers in the lab

// HOTLOOP_FLOW_TEST_NTFY=http://host:port names a real ntfy. The node publishes and
// ntfy is asked for what it holds.
func TestNtfyAgainstARealServer(t *testing.T) {
	base := os.Getenv("HOTLOOP_FLOW_TEST_NTFY")
	if base == "" {
		t.Skip("HOTLOOP_FLOW_TEST_NTFY is not set")
	}
	cfg, _ := configured(t, "hotloop-flow-ntfy", `{"server":"`+base+`"}`, nil)
	topic := "flowtest" + time.Now().Format("150405")
	n := outNode(t, "ntfy out", `{"server":"srv","topic":"`+topic+`","title":"Press 4","priority":"high","tags":"warning"}`, cfg)
	if _, err := send(t, n, msg(t, `{"payload":"oil pressure low"}`)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(base + "/" + topic + "/json?poll=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var j struct {
		Message  string
		Title    string
		Priority int
		Tags     []string
	}
	if err := json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)[0]), &j); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if j.Message != "oil pressure low" || j.Title != "Press 4" || j.Priority != 4 || len(j.Tags) != 1 || j.Tags[0] != "warning" {
		t.Fatalf("ntfy holds %+v", j)
	}
}

// HOTLOOP_FLOW_TEST_GOTIFY=http://host:port names a real Gotify with its first-run
// login admin:admin. An application is made through its API, the node posts as it,
// and the message is read back.
func TestGotifyAgainstARealServer(t *testing.T) {
	base := os.Getenv("HOTLOOP_FLOW_TEST_GOTIFY")
	if base == "" {
		t.Skip("HOTLOOP_FLOW_TEST_GOTIFY is not set")
	}
	req, _ := http.NewRequest("POST", base+"/application", strings.NewReader(`{"name":"flowtest"}`))
	req.SetBasicAuth("admin", "admin")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var app struct {
		Token string `json:"token"`
		ID    int    `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	if app.Token == "" {
		t.Fatalf("no application was made: %s", resp.Status)
	}
	cfg, _ := configured(t, "hotloop-flow-gotify", `{"server":"`+base+`"}`, map[string]string{"token": app.Token})
	n := outNode(t, "gotify out", `{"server":"srv","title":"Line 3","priority":8}`, cfg)
	if _, err := send(t, n, msg(t, `{"payload":"door open"}`)); err != nil {
		t.Fatal(err)
	}
	get, _ := http.NewRequest("GET", base+"/application/"+strconv.Itoa(app.ID)+"/message", nil)
	get.SetBasicAuth("admin", "admin")
	r2, err := http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var list struct {
		Messages []struct {
			Message  string
			Title    string
			Priority int
		}
	}
	_ = json.NewDecoder(r2.Body).Decode(&list)
	if len(list.Messages) != 1 || list.Messages[0].Message != "door open" || list.Messages[0].Title != "Line 3" || list.Messages[0].Priority != 8 {
		t.Fatalf("Gotify holds %+v", list)
	}
	// A wrong token is refused with Gotify's own reason and the token is not in it.
	bad, _ := configured(t, "hotloop-flow-gotify", `{"server":"`+base+`"}`, map[string]string{"token": "NotAToken123"})
	_, err = send(t, outNode(t, "gotify out", `{"server":"srv"}`, bad), msg(t, `{"payload":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "NotAToken123") {
		t.Fatalf("%v", err)
	}
}

// Under a flow test nothing leaves the process: the node hands what it would have
// sent to the stand-in.
func TestTheNotificationNodesHandWhatTheyWouldSendToAStandIn(t *testing.T) {
	cases := []struct {
		typ, cfgType, cfg, nodeCfg string
		creds                      map[string]string
	}{
		{"ntfy out", "hotloop-flow-ntfy", `{"server":"http://127.0.0.1:1"}`, `{"server":"srv","topic":"t"}`, nil},
		{"gotify out", "hotloop-flow-gotify", `{"server":"http://127.0.0.1:1"}`, `{"server":"srv"}`, map[string]string{"token": "x"}},
		{"discord bot out", "hotloop-flow-discord-bot", `{"api":"http://127.0.0.1:1"}`, `{"bot":"srv","channel":"123456789012345678"}`, map[string]string{"token": "x"}},
		{"kafka out", "hotloop-flow-kafka", `{"brokers":"127.0.0.1:1"}`, `{"broker":"srv","topic":"t"}`, nil},
	}
	for _, c := range cases {
		cfg, _ := configured(t, c.cfgType, c.cfg, c.creds)
		si := &recordingStandIn{}
		base := newTestServices()
		base.standIn = si
		svc := &servicesWithConfig{testServices: base, configs: map[string]node.Node{"srv": cfg}}
		n := build(t, c.typ, c.nodeCfg, svc)
		e, err := send(t, n, msg(t, `{"payload":"hi"}`))
		if err != nil || len(si.calls) != 1 || len(e.sent[0]) != 1 {
			t.Errorf("%s: err %v calls %d sent %d", c.typ, err, len(si.calls), len(e.sent[0]))
		}
	}
}
