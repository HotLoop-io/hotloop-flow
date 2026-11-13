// Package dashboard serves the dashboard that a Node-RED flow's ui_ nodes
// describe: tabs, groups and widgets, live in a browser.
//
// A flow built for node-red-dashboard has ui_tab and ui_group nodes that lay a
// page out, and ui_text, ui_numeric, ui_button, ui_switch, ui_led, ui_gauge,
// ui_chart, ui_template, ui_text_input, ui_link and ui_spacer nodes that fill
// it. The nodes register here when a flow starts and leave when it stops, so a
// redeploy changes the page and nothing is left behind.
//
// The page is plain HTML, CSS and JavaScript embedded in the binary: no npm
// package, no CDN, nothing fetched from outside the plant. The browser takes
// its state from one Server-Sent Events stream and sends what an operator does
// (a click, a switch, a typed number) by POST, and the node sends it on as a
// message.
//
// Nothing in this package is ported from node-red-dashboard. The behavior is
// from its documentation and from the flows that use it.
package dashboard

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// Kinds of member.
const (
	KindBase   = "base"
	KindTab    = "tab"
	KindGroup  = "group"
	KindWidget = "widget"
	// KindLink is a ui_link: an entry in the menu that opens another address.
	KindLink = "link"
)

// MaxChartPoints caps the points one chart series keeps, whatever removeOlder
// says, so a chart nobody trims cannot grow without end.
const MaxChartPoints = 5000

// Member is one node's place on the dashboard.
type Member struct {
	ID     string
	Kind   string
	Type   string         // the node type, ui_text and so on
	Props  map[string]any // the node's configuration, as the page needs it
	Parent string         // a group's tab id, a widget's group id
	Order  int
	// Flow names the flow tab a widget sits on. When two dashboard tabs share a
	// name, it is what tells them apart.
	Flow string
	// OnEvent receives what an operator did to a widget: {"event": "click"},
	// {"event": "change", "value": 5}, {"event": "send", "msg": {...}}.
	OnEvent func(ev map[string]any)
}

type point struct {
	Series string  `json:"s"`
	T      int64   `json:"t"`
	V      float64 `json:"v"`
}

type state struct {
	Msg    map[string]any `json:"msg,omitempty"`
	Points []point        `json:"points,omitempty"`
	// Switch and numeric widgets keep what they show apart from the last message.
	Value any `json:"value,omitempty"`
}

// Hub holds the dashboard of one process.
type Hub struct {
	mu      sync.Mutex
	members map[string]*Member
	states  map[string]*state
	dirty   map[string]bool
	fresh   map[string][]point // chart points since the last flush
	cleared map[string]bool
	clients map[*client]struct{}

	layoutDirty bool
	flushing    bool
	stop        chan struct{}
}

type client struct {
	ch chan []byte
}

// New returns an empty hub.
func New() *Hub {
	return &Hub{
		members: map[string]*Member{},
		states:  map[string]*state{},
		dirty:   map[string]bool{},
		fresh:   map[string][]point{},
		cleared: map[string]bool{},
		clients: map[*client]struct{}{},
	}
}

// Len is how many members are on the dashboard.
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.members)
}

// Add puts a member on the dashboard, or replaces the one with its id.
func (h *Hub) Add(m *Member) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.members[m.ID] = m
	h.layoutChanged()
}

// Remove takes a member off. Its state goes with it.
func (h *Hub) Remove(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.members[id]; !ok {
		return
	}
	delete(h.members, id)
	delete(h.states, id)
	delete(h.dirty, id)
	delete(h.fresh, id)
	delete(h.cleared, id)
	h.layoutChanged()
}

// layoutChanged tells the browsers to read the layout again, once things have
// settled: a deploy adds hundreds of members in a burst and one reload is plenty.
func (h *Hub) layoutChanged() {
	h.layoutDirty = true
	h.kick()
}

func (h *Hub) kick() {
	if h.flushing {
		return
	}
	h.flushing = true
	go func() {
		time.Sleep(100 * time.Millisecond)
		h.flush()
	}()
}

// SetState replaces a widget's state: the last message it was given, and the
// value it shows. The browsers get it on the next flush.
func (h *Hub) SetState(id string, msg map[string]any, value any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.members[id]; !ok {
		return
	}
	st := h.states[id]
	if st == nil {
		st = &state{}
		h.states[id] = st
	}
	if msg != nil {
		st.Msg = msg
	}
	if value != nil {
		st.Value = value
	}
	h.dirty[id] = true
	h.kick()
}

// State returns a widget's last message and value.
func (h *Hub) State(id string) (map[string]any, any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st := h.states[id]; st != nil {
		return st.Msg, st.Value
	}
	return nil, nil
}

// AddPoint adds a point to a chart. keep is how long a point stays, and
// maxPoints how many a series keeps; either may be zero for no limit besides
// MaxChartPoints.
func (h *Hub) AddPoint(id, series string, t time.Time, v float64, keep time.Duration, maxPoints int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.members[id]; !ok {
		return
	}
	st := h.states[id]
	if st == nil {
		st = &state{}
		h.states[id] = st
	}
	p := point{Series: series, T: t.UnixMilli(), V: v}
	st.Points = append(st.Points, p)
	st.Points = trimPoints(st.Points, t, keep, maxPoints)
	h.fresh[id] = append(h.fresh[id], p)
	h.dirty[id] = true
	h.kick()
}

// ClearChart empties a chart.
func (h *Hub) ClearChart(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st := h.states[id]; st != nil {
		st.Points = nil
	}
	delete(h.fresh, id)
	h.cleared[id] = true
	h.dirty[id] = true
	h.kick()
}

func trimPoints(ps []point, now time.Time, keep time.Duration, maxPoints int) []point {
	if keep > 0 {
		cut := now.Add(-keep).UnixMilli()
		i := 0
		for i < len(ps) && ps[i].T < cut {
			i++
		}
		if i > 0 {
			ps = append([]point(nil), ps[i:]...)
		}
	}
	limit := MaxChartPoints
	if maxPoints > 0 && maxPoints < limit {
		limit = maxPoints
	}
	// The limit is per series, so one busy series cannot push another's points out.
	counts := map[string]int{}
	for _, p := range ps {
		counts[p.Series]++
	}
	over := false
	for _, n := range counts {
		if n > limit {
			over = true
			break
		}
	}
	if !over {
		return ps
	}
	skip := map[string]int{}
	for s, n := range counts {
		if n > limit {
			skip[s] = n - limit
		}
	}
	out := ps[:0:0]
	for _, p := range ps {
		if skip[p.Series] > 0 {
			skip[p.Series]--
			continue
		}
		out = append(out, p)
	}
	return out
}

// flush sends what changed since the last one to every browser.
func (h *Hub) flush() {
	h.mu.Lock()
	h.flushing = false
	var events [][]byte
	if h.layoutDirty {
		h.layoutDirty = false
		events = append(events, sse("layout", []byte("{}")))
	}
	if len(h.dirty) > 0 {
		type upd struct {
			ID     string         `json:"id"`
			Msg    map[string]any `json:"msg,omitempty"`
			Value  any            `json:"value,omitempty"`
			Points []point        `json:"points,omitempty"`
			Clear  bool           `json:"clear,omitempty"`
		}
		list := make([]upd, 0, len(h.dirty))
		ids := make([]string, 0, len(h.dirty))
		for id := range h.dirty {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			u := upd{ID: id, Points: h.fresh[id], Clear: h.cleared[id]}
			if st := h.states[id]; st != nil {
				u.Msg, u.Value = st.Msg, st.Value
			}
			list = append(list, u)
		}
		h.dirty = map[string]bool{}
		h.fresh = map[string][]point{}
		h.cleared = map[string]bool{}
		if b, err := json.Marshal(map[string]any{"updates": list}); err == nil {
			events = append(events, sse("update", b))
		}
	}
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	for _, ev := range events {
		for _, c := range clients {
			select {
			case c.ch <- ev:
			default:
				// A browser that cannot keep up reconnects, and gets everything
				// again from the state, rather than the hub waiting on it.
				h.dropClient(c)
			}
		}
	}
}

func (h *Hub) dropClient(c *client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.ch)
	}
	h.mu.Unlock()
}

func sse(event string, data []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "event: %s\ndata: ", event)
	b.Write(bytes.ReplaceAll(data, []byte("\n"), []byte("\ndata: ")))
	b.WriteString("\n\n")
	return b.Bytes()
}

// ---------------------------------------------------------------------------
// layout
// ---------------------------------------------------------------------------

type layoutWidget struct {
	ID    string         `json:"id"`
	Type  string         `json:"type"`
	Order int            `json:"order"`
	Props map[string]any `json:"props"`
}

type layoutGroup struct {
	ID      string         `json:"id"`
	Order   int            `json:"order"`
	Props   map[string]any `json:"props"`
	Widgets []layoutWidget `json:"widgets"`
}

type layoutTab struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Label  string         `json:"label"`
	Order  int            `json:"order"`
	Props  map[string]any `json:"props"`
	Groups []layoutGroup  `json:"groups"`
}

type layoutLink struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Link   string `json:"link"`
	Target string `json:"target"`
	Order  int    `json:"order"`
}

type layout struct {
	Theme map[string]any `json:"theme"`
	Tabs  []layoutTab    `json:"tabs"`
	Links []layoutLink   `json:"links"`
}

// Layout is the dashboard as the page draws it.
func (h *Hub) Layout() any { return h.layout() }

func (h *Hub) layout() layout {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out layout
	out.Theme = map[string]any{}
	tabs := map[string]*layoutTab{}
	var tabIDs []string
	for id, m := range h.members {
		switch m.Kind {
		case KindBase:
			if t, ok := m.Props["theme"].(map[string]any); ok && len(out.Theme) == 0 {
				out.Theme = t
			}
		case KindLink:
			name, _ := m.Props["name"].(string)
			link, _ := m.Props["link"].(string)
			target, _ := m.Props["target"].(string)
			out.Links = append(out.Links, layoutLink{ID: id, Name: name, Link: link, Target: target, Order: m.Order})
		case KindTab:
			name, _ := m.Props["name"].(string)
			tabs[id] = &layoutTab{ID: id, Name: name, Label: name, Order: m.Order, Props: m.Props}
			tabIDs = append(tabIDs, id)
		}
	}
	groups := map[string]*layoutGroup{}
	groupTab := map[string]string{}
	for id, m := range h.members {
		if m.Kind != KindGroup {
			continue
		}
		groups[id] = &layoutGroup{ID: id, Order: m.Order, Props: m.Props}
		groupTab[id] = m.Parent
	}
	flowOf := map[string]map[string]int{} // tab id -> flow label -> widget count
	for _, m := range h.members {
		if m.Kind != KindWidget {
			continue
		}
		g := groups[m.Parent]
		if g == nil {
			continue // a widget whose group is not in the flow has nowhere to go
		}
		g.Widgets = append(g.Widgets, layoutWidget{ID: m.ID, Type: m.Type, Order: m.Order, Props: m.Props})
		if m.Flow != "" {
			if flowOf[groupTab[g.ID]] == nil {
				flowOf[groupTab[g.ID]] = map[string]int{}
			}
			flowOf[groupTab[g.ID]][m.Flow]++
		}
	}
	for id, g := range groups {
		sort.SliceStable(g.Widgets, func(i, j int) bool {
			if g.Widgets[i].Order != g.Widgets[j].Order {
				return g.Widgets[i].Order < g.Widgets[j].Order
			}
			return g.Widgets[i].ID < g.Widgets[j].ID
		})
		if t := tabs[groupTab[id]]; t != nil {
			t.Groups = append(t.Groups, *g)
		}
	}
	// Two tabs with one name are told apart by the flow their widgets sit on.
	byName := map[string][]string{}
	for _, id := range tabIDs {
		byName[tabs[id].Name] = append(byName[tabs[id].Name], id)
	}
	for _, ids := range byName {
		if len(ids) < 2 {
			continue
		}
		for _, id := range ids {
			best, n := "", 0
			for label, c := range flowOf[id] {
				if c > n || (c == n && label < best) {
					best, n = label, c
				}
			}
			if best != "" {
				tabs[id].Label = tabs[id].Name + " (" + best + ")"
			}
		}
	}
	for _, id := range tabIDs {
		t := tabs[id]
		sort.SliceStable(t.Groups, func(i, j int) bool {
			if t.Groups[i].Order != t.Groups[j].Order {
				return t.Groups[i].Order < t.Groups[j].Order
			}
			return t.Groups[i].ID < t.Groups[j].ID
		})
		out.Tabs = append(out.Tabs, *t)
	}
	sort.SliceStable(out.Links, func(i, j int) bool {
		if out.Links[i].Order != out.Links[j].Order {
			return out.Links[i].Order < out.Links[j].Order
		}
		return out.Links[i].ID < out.Links[j].ID
	})
	sort.SliceStable(out.Tabs, func(i, j int) bool {
		if out.Tabs[i].Order != out.Tabs[j].Order {
			return out.Tabs[i].Order < out.Tabs[j].Order
		}
		return out.Tabs[i].ID < out.Tabs[j].ID
	})
	return out
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// Handler serves the page, its assets, the layout, the event stream and the
// operator's input. base is the path it is mounted at, root included.
func (h *Hub) Handler(base string) http.Handler {
	assets, _ := fs.Sub(webFS, "web")
	files := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := strings.TrimRight(base, "/")
		p := r.URL.Path
		rel := ""
		if strings.HasPrefix(p, base) {
			rel = strings.Trim(strings.TrimPrefix(p, base), "/")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		switch {
		case p == base:
			// The page's own files are named relative to its address, so it has to end in a slash.
			http.Redirect(w, r, base+"/", http.StatusFound)
		case rel == "" || rel == "index.html":
			w.Header().Set("Cache-Control", "no-store")
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/index.html"
			serveFile(w, r2, assets)
		case rel == "api/layout":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(h.layout())
		case rel == "api/events":
			h.serveEvents(w, r)
		case rel == "api/emit":
			h.serveEmit(w, r)
		case rel == "app.js" || rel == "app.css":
			w.Header().Set("Cache-Control", "no-cache")
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/" + rel
			files.ServeHTTP(w, r2)
		default:
			http.NotFound(w, r)
		}
	})
}

func serveFile(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	f, err := assets.Open("index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.Copy(w, f)
}

func (h *Hub) serveEvents(w http.ResponseWriter, r *http.Request) {
	// The response may be wrapped by the server's request log, so ask through
	// the controller, which reaches the writer underneath.
	rc := http.NewResponseController(w)
	flush := func() { _ = rc.Flush() }
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	c := &client{ch: make(chan []byte, 512)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	// Everything the page needs to show now, in one event, taken under the same
	// lock the client was added under so no update falls between.
	type snap struct {
		ID     string         `json:"id"`
		Msg    map[string]any `json:"msg,omitempty"`
		Value  any            `json:"value,omitempty"`
		Points []point        `json:"points,omitempty"`
	}
	snaps := make([]snap, 0, len(h.states))
	for id, st := range h.states {
		snaps = append(snaps, snap{ID: id, Msg: st.Msg, Value: st.Value, Points: st.Points})
	}
	h.mu.Unlock()
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].ID < snaps[j].ID })
	body, _ := json.Marshal(map[string]any{"states": snaps})
	_, _ = w.Write(sse("state", body))
	flush()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	defer h.dropClient(c)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-c.ch:
			if !ok {
				return
			}
			if _, err := w.Write(ev); err != nil {
				return
			}
			flush()
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flush()
		}
	}
}

func (h *Hub) serveEmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// A page on another site must not be able to press a button here: when the
	// browser says where the request came from, it has to be this host.
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || u.Host != r.Host {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var ev struct {
		ID    string         `json:"id"`
		Event map[string]any `json:"event"`
	}
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "that is not an event: "+err.Error(), http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	m := h.members[ev.ID]
	h.mu.Unlock()
	if m == nil || m.OnEvent == nil {
		http.Error(w, "no widget "+ev.ID+" is listening", http.StatusNotFound)
		return
	}
	m.OnEvent(ev.Event)
	w.WriteHeader(http.StatusNoContent)
}
