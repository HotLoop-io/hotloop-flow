package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(h *Hub) {
	h.Add(&Member{ID: "base", Kind: KindBase, Props: map[string]any{"theme": map[string]any{"name": "theme-dark"}}})
	h.Add(&Member{ID: "tabB", Kind: KindTab, Order: 2, Props: map[string]any{"name": "Main Screen"}})
	h.Add(&Member{ID: "tabA", Kind: KindTab, Order: 1, Props: map[string]any{"name": "Main Screen"}})
	h.Add(&Member{ID: "tabC", Kind: KindTab, Order: 3, Props: map[string]any{"name": "Alarms"}})
	h.Add(&Member{ID: "g1", Kind: KindGroup, Parent: "tabA", Order: 2, Props: map[string]any{"name": "Second", "width": 6.0}})
	h.Add(&Member{ID: "g0", Kind: KindGroup, Parent: "tabA", Order: 1, Props: map[string]any{"name": "First", "width": 6.0}})
	h.Add(&Member{ID: "gB", Kind: KindGroup, Parent: "tabB", Order: 1, Props: map[string]any{"name": "B", "width": 6.0}})
	h.Add(&Member{ID: "w2", Kind: KindWidget, Type: "ui_text", Parent: "g0", Order: 2, Flow: "1881 Dashboard", Props: map[string]any{"label": "two"}})
	h.Add(&Member{ID: "w1", Kind: KindWidget, Type: "ui_text", Parent: "g0", Order: 1, Flow: "1881 Dashboard", Props: map[string]any{"label": "one"}})
	h.Add(&Member{ID: "wB", Kind: KindWidget, Type: "ui_text", Parent: "gB", Order: 1, Flow: "1882 Dashboard", Props: map[string]any{"label": "b"}})
	h.Add(&Member{ID: "orphan", Kind: KindWidget, Type: "ui_text", Parent: "nogroup", Props: map[string]any{}})
	h.Add(&Member{ID: "lnk", Kind: KindLink, Order: 4, Props: map[string]any{"name": "Dryer 1", "link": "http://10.0.0.1:1880/ui", "target": "thistab"}})
}

func TestLayoutOrdersEverythingByItsOrderAndDropsWhatHasNoGroup(t *testing.T) {
	h := New()
	fixture(h)
	l := h.layout()
	var ids []string
	for _, tb := range l.Tabs {
		ids = append(ids, tb.ID)
	}
	if strings.Join(ids, ",") != "tabA,tabB,tabC" {
		t.Fatalf("tabs = %v", ids)
	}
	if g := l.Tabs[0].Groups; len(g) != 2 || g[0].ID != "g0" || g[1].ID != "g1" {
		t.Fatalf("groups = %+v", g)
	}
	if w := l.Tabs[0].Groups[0].Widgets; len(w) != 2 || w[0].ID != "w1" || w[1].ID != "w2" {
		t.Fatalf("widgets = %+v", w)
	}
	for _, tb := range l.Tabs {
		for _, g := range tb.Groups {
			for _, w := range g.Widgets {
				if w.ID == "orphan" {
					t.Fatal("a widget with no group was laid out")
				}
			}
		}
	}
	if l.Theme["name"] != "theme-dark" || len(l.Links) != 1 || l.Links[0].Name != "Dryer 1" {
		t.Fatalf("theme/links = %v / %v", l.Theme, l.Links)
	}
}

func TestTwoTabsWithOneNameAreToldApartByTheirFlow(t *testing.T) {
	h := New()
	fixture(h)
	l := h.layout()
	if l.Tabs[0].Label != "Main Screen (1881 Dashboard)" || l.Tabs[1].Label != "Main Screen (1882 Dashboard)" {
		t.Fatalf("labels = %q, %q", l.Tabs[0].Label, l.Tabs[1].Label)
	}
	if l.Tabs[2].Label != "Alarms" {
		t.Fatalf("a tab with its own name was renamed: %q", l.Tabs[2].Label)
	}
}

func serve(t *testing.T, h *Hub) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h.Handler("/ui"))
	t.Cleanup(srv.Close)
	return srv
}

func TestTheHandlerServesThePageItsAssetsAndTheLayout(t *testing.T) {
	h := New()
	fixture(h)
	srv := serve(t, h)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := noFollow.Get(srv.URL + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusFound || r.Header.Get("Location") != "/ui/" {
		t.Fatalf("/ui = %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	for path, want := range map[string]string{"/ui/": "<title>Dashboard</title>", "/ui/app.js": "EventSource", "/ui/app.css": "--primary"} {
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 200 || !strings.Contains(string(b), want) {
			t.Fatalf("%s = %d, missing %q", path, r.StatusCode, want)
		}
	}
	r, _ = http.Get(srv.URL + "/ui/api/layout")
	var l layout
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil || len(l.Tabs) != 3 {
		t.Fatalf("layout = %+v, %v", l, err)
	}
	if r, _ := http.Get(srv.URL + "/ui/nothing"); r.StatusCode != 404 {
		t.Fatalf("unknown path = %d", r.StatusCode)
	}
}

type sseReader struct {
	r  *bufio.Reader
	mu sync.Mutex
}

// next reads one event.
func (s *sseReader) next(t *testing.T) (string, string) {
	t.Helper()
	var ev, data string
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			ev = line[7:]
		case strings.HasPrefix(line, "data: "):
			data += line[6:]
		case line == "" && ev != "":
			return ev, data
		}
	}
}

func openStream(t *testing.T, srv *httptest.Server) *sseReader {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/ui/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
	return &sseReader{r: bufio.NewReader(resp.Body)}
}

func TestAnUpdateReachesABrowserAndANewBrowserStartsFromTheState(t *testing.T) {
	h := New()
	fixture(h)
	srv := serve(t, h)
	h.SetState("w1", map[string]any{"payload": 21.5}, nil)

	s := openStream(t, srv)
	ev, data := s.next(t)
	if ev != "state" || !strings.Contains(data, `"id":"w1"`) || !strings.Contains(data, "21.5") {
		t.Fatalf("first event = %s %s", ev, data)
	}
	h.SetState("w2", map[string]any{"payload": "hi"}, nil)
	h.AddPoint("w1", "temp", time.Now(), 3, time.Hour, 0)
	got := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for !(got["w2"] && got["w1pt"]) {
		done := make(chan [2]string, 1)
		go func() { e, d := s.next(t); done <- [2]string{e, d} }()
		select {
		case r := <-done:
			if r[0] == "update" {
				got["w2"] = got["w2"] || strings.Contains(r[1], `"id":"w2"`)
				got["w1pt"] = got["w1pt"] || strings.Contains(r[1], `"points"`)
			}
		case <-deadline:
			t.Fatalf("updates seen: %v", got)
		}
	}
}

func TestALayoutChangeIsAnnounced(t *testing.T) {
	h := New()
	srv := serve(t, h)
	s := openStream(t, srv)
	s.next(t) // state
	h.Add(&Member{ID: "t", Kind: KindTab, Props: map[string]any{"name": "New"}})
	ev, _ := s.next(t)
	if ev != "layout" {
		t.Fatalf("event = %s", ev)
	}
}

func TestEmitReachesTheWidgetAndRefusesAForeignOrigin(t *testing.T) {
	h := New()
	var mu sync.Mutex
	var got []map[string]any
	h.Add(&Member{ID: "b", Kind: KindWidget, Type: "ui_button", OnEvent: func(ev map[string]any) { mu.Lock(); got = append(got, ev); mu.Unlock() }})
	srv := serve(t, h)
	post := func(origin, body string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/ui/api/emit", strings.NewReader(body))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if c := post("", `{"id":"b","event":{"event":"click"}}`); c != 204 {
		t.Fatalf("emit = %d", c)
	}
	if c := post(srv.URL, `{"id":"b","event":{"event":"click"}}`); c != 204 {
		t.Fatalf("same-origin emit = %d", c)
	}
	if c := post("https://evil.example", `{"id":"b","event":{"event":"click"}}`); c != 403 {
		t.Fatalf("foreign origin = %d, want 403", c)
	}
	if c := post("", `{"id":"nope","event":{}}`); c != 404 {
		t.Fatalf("unknown widget = %d", c)
	}
	if c := post("", `not json`); c != 400 {
		t.Fatalf("bad body = %d", c)
	}
	r, _ := http.Get(srv.URL + "/ui/api/emit")
	if r.StatusCode != 405 {
		t.Fatalf("GET emit = %d", r.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0]["event"] != "click" {
		t.Fatalf("events delivered = %v", got)
	}
}

func TestChartPointsAreTrimmedByAgeAndByCountPerSeries(t *testing.T) {
	h := New()
	h.Add(&Member{ID: "c", Kind: KindWidget, Type: "ui_chart"})
	now := time.Now()
	h.AddPoint("c", "a", now.Add(-2*time.Hour), 1, time.Hour, 0)
	h.AddPoint("c", "a", now, 2, time.Hour, 0)
	for i := 0; i < 10; i++ {
		h.AddPoint("c", "b", now.Add(time.Duration(i)*time.Second), float64(i), time.Hour, 4)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	counts := map[string]int{}
	for _, p := range h.states["c"].Points {
		counts[p.Series]++
	}
	if counts["a"] != 1 || counts["b"] != 4 {
		t.Fatalf("points kept = %v, want a:1 (the old one aged out) b:4", counts)
	}
}

func TestRemovingAMemberDropsItsState(t *testing.T) {
	h := New()
	fixture(h)
	h.SetState("w1", map[string]any{"payload": 1}, nil)
	h.Remove("w1")
	if m, _ := h.State("w1"); m != nil || h.Len() != 11 {
		t.Fatalf("state %v, len %d", m, h.Len())
	}
}
