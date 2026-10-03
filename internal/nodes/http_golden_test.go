package nodes

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowhttp"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Cookies and multipart against what Node-RED 4's own libraries do. The golden
// file comes from testdata/http-golden/golden.mjs, run under the exact versions
// Node-RED pins: Express, cookie, cookie-parser, form-data, multer and
// tough-cookie, with Node-RED's node code between them copied in verbatim.
// Every case here goes through the real node over a real socket.

type httpGolden struct {
	Now             int64 `json:"now"`
	ResponseCookies []struct {
		Name      string         `json:"name"`
		Cookies   map[string]any `json:"cookies"`
		SetCookie []string       `json:"setCookie"`
		Error     string         `json:"error"`
	} `json:"responseCookies"`
	RequestCookies []struct {
		Header        string         `json:"header"`
		Cookies       map[string]any `json:"cookies"`
		SignedCookies map[string]any `json:"signedCookies"`
	} `json:"requestCookies"`
	OutboundCookies []struct {
		Name         string         `json:"name"`
		URL          string         `json:"url"`
		Header       *string        `json:"header"`
		Cookies      map[string]any `json:"cookies"`
		CookieHeader *string        `json:"cookieHeader"`
		Error        string         `json:"error"`
	} `json:"outboundCookies"`
	ResponseSetCookies []struct {
		SetCookie []string       `json:"setCookie"`
		Cookies   map[string]any `json:"cookies"`
	} `json:"responseSetCookies"`
	Forms []struct {
		Name    string  `json:"name"`
		Payload any     `json:"payload"`
		Body    *string `json:"body"`
		Error   string  `json:"error"`
	} `json:"forms"`
	Uploads []struct {
		Name        string         `json:"name"`
		ContentType string         `json:"contentType"`
		Body        string         `json:"body"`
		Status      int            `json:"status"`
		Error       string         `json:"error"`
		Payload     map[string]any `json:"payload"`
		Files       []any          `json:"files"`
	} `json:"uploads"`
	Mime map[string]any `json:"mime"`
}

func loadHTTPGolden(t *testing.T) httpGolden {
	t.Helper()
	raw, err := os.ReadFile("testdata/http-golden/node-red-4-http.json")
	if err != nil {
		t.Fatal(err)
	}
	var g httpGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// revive turns the golden file's {"$date": ms} and {"$buffer": text} back into
// the Date and Buffer the JavaScript side had.
func revive(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 1 {
			if ms, ok := t["$date"].(float64); ok {
				return time.UnixMilli(int64(ms)).UTC()
			}
			if s, ok := t["$buffer"].(string); ok {
				return []byte(s)
			}
		}
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = revive(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = revive(e)
		}
		return out
	}
	return v
}

// liveRoutes serves a route table on a real listener.
func liveRoutes(t *testing.T, r *flowhttp.Router) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h, params, ok := r.Match(req.Method, req.URL.Path)
		if !ok {
			http.NotFound(w, req)
			return
		}
		h.ServeHTTP(w, flowhttp.WithRouteParams(req, params))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// flowTo runs a tiny flow: every message the In node emits is handed to fn.
func flowTo(t *testing.T, in node.Node, fn func(*engine.Msg)) {
	t.Helper()
	e := newTestEmitter()
	_, cancel := startNode(t, in, e)
	t.Cleanup(cancel)
	go func() {
		seen := 0
		for {
			msgs := e.on(0)
			for ; seen < len(msgs); seen++ {
				go fn(msgs[seen])
			}
			select {
			case <-t.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
}

func TestGoldenResponseCookies(t *testing.T) {
	g := loadHTTPGolden(t)
	base := liveRoutes(t, withRoutes(t))
	in := build(t, "http in", `{"url":"/c","method":"get"}`, newTestServices())
	resp := build(t, "http response", `{}`, newTestServices())
	resp.(*httpResponseNode).now = func() time.Time { return time.UnixMilli(g.Now) }
	plain := build(t, "http response", `{}`, newTestServices())

	var mu sync.Mutex
	var current map[string]any
	var gotErr error
	flowTo(t, in, func(m *engine.Msg) {
		mu.Lock()
		m.Data["cookies"] = revive(current)
		mu.Unlock()
		if err := resp.Receive(t.Context(), m, newTestEmitter()); err != nil {
			mu.Lock()
			gotErr = err
			mu.Unlock()
			// The node refused, so close the request out without cookies.
			delete(m.Data, "cookies")
			_ = plain.Receive(t.Context(), m, newTestEmitter())
		}
	})

	for _, tc := range g.ResponseCookies {
		t.Run(tc.Name, func(t *testing.T) {
			mu.Lock()
			current, gotErr = tc.Cookies, nil
			mu.Unlock()
			r, err := http.Get(base + "/c")
			if err != nil {
				t.Fatal(err)
			}
			r.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			if tc.Error != "" {
				if gotErr == nil || !strings.Contains(gotErr.Error(), tc.Error) {
					t.Fatalf("want the error %q, got %v", tc.Error, gotErr)
				}
				return
			}
			if gotErr != nil {
				t.Fatalf("refused: %v", gotErr)
			}
			got := r.Header.Values("Set-Cookie")
			if !reflect.DeepEqual(nonNilStrings(got), nonNilStrings(tc.SetCookie)) {
				t.Fatalf("Set-Cookie\n got %q\nwant %q", got, tc.SetCookie)
			}
		})
	}
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func TestGoldenRequestCookies(t *testing.T) {
	g := loadHTTPGolden(t)
	base := liveRoutes(t, withRoutes(t))
	in := build(t, "http in", `{"url":"/c","method":"get"}`, newTestServices())
	resp := build(t, "http response", `{}`, newTestServices())
	seen := make(chan map[string]any, 1)
	flowTo(t, in, func(m *engine.Msg) {
		seen <- m.Data["req"].(map[string]any)
		_ = resp.Receive(t.Context(), m, newTestEmitter())
	})

	for _, tc := range g.RequestCookies {
		t.Run(tc.Header, func(t *testing.T) {
			req, _ := http.NewRequest("GET", base+"/c", nil)
			req.Header.Set("Cookie", tc.Header)
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			r.Body.Close()
			got := <-seen
			if !reflect.DeepEqual(got["cookies"], tc.Cookies) {
				t.Fatalf("msg.req.cookies\n got %#v\nwant %#v", got["cookies"], tc.Cookies)
			}
			if !reflect.DeepEqual(got["signedCookies"], tc.SignedCookies) {
				t.Fatalf("msg.req.signedCookies = %#v", got["signedCookies"])
			}
		})
	}
}

func TestGoldenOutboundCookies(t *testing.T) {
	g := loadHTTPGolden(t)
	sent := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent <- r.Header.Get("Cookie")
	}))
	defer srv.Close()

	for _, tc := range g.OutboundCookies {
		t.Run(tc.Name, func(t *testing.T) {
			u, _ := url.Parse(tc.URL)
			target := srv.URL + u.Path
			n := build(t, "http request", `{"method":"GET","url":"`+target+`","ret":"txt"}`, newTestServices())
			m := engine.NewMsg()
			if tc.Cookies != nil {
				m.Data["cookies"] = tc.Cookies
			}
			if tc.Header != nil {
				m.Data["headers"] = map[string]any{"cookie": *tc.Header}
			}
			_, err := send(t, n, m)
			if tc.Error != "" {
				if err == nil || !strings.Contains(err.Error(), tc.Error) {
					t.Fatalf("want the error %q, got %v", tc.Error, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := <-sent; got != *tc.CookieHeader {
				t.Fatalf("Cookie\n got %q\nwant %q", got, *tc.CookieHeader)
			}
		})
	}
}

// A cookie a redirect sets rides the next hop, and the hop lands in
// msg.redirectList with its cookies, as got and tough-cookie do it for
// Node-RED.
func TestHTTPRequestCarriesRedirectCookies(t *testing.T) {
	var mux http.ServeMux
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "session=s3cr3t; Path=/")
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	})
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("session")
		if err != nil {
			http.Error(w, "no session", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("welcome " + c.Value))
	})
	srv := httptest.NewServer(&mux)
	defer srv.Close()

	n := build(t, "http request", `{"method":"GET","url":"`+srv.URL+`/login","ret":"txt"}`, newTestServices())
	e, err := send(t, n, engine.NewMsg())
	if err != nil {
		t.Fatal(err)
	}
	m := e.on(0)[0]
	if m.Payload() != "welcome s3cr3t" {
		t.Fatalf("the cookie set by the redirect did not come back: %v", m.Payload())
	}
	want := []any{map[string]any{
		"location": "/dashboard",
		"cookies":  map[string]any{"session": map[string]any{"value": "s3cr3t", "Path": "/"}},
	}}
	if !reflect.DeepEqual(m.Data["redirectList"], want) {
		t.Fatalf("msg.redirectList = %#v", m.Data["redirectList"])
	}

	// No redirect is an empty list, not a missing one.
	direct := build(t, "http request", `{"method":"GET","url":"`+srv.URL+`/dashboard","ret":"txt"}`, newTestServices())
	e, err = send(t, direct, engine.NewMsg())
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := e.on(0)[0].Data["redirectList"].([]any); !ok || len(l) != 0 {
		t.Fatalf("msg.redirectList with no redirect = %#v", e.on(0)[0].Data["redirectList"])
	}
}

func TestGoldenResponseCookiesFromSetCookie(t *testing.T) {
	g := loadHTTPGolden(t)
	var mu sync.Mutex
	var current []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range current {
			w.Header().Add("Set-Cookie", c)
		}
	}))
	defer srv.Close()
	n := build(t, "http request", `{"method":"GET","url":"`+srv.URL+`","ret":"txt"}`, newTestServices())

	for _, tc := range g.ResponseSetCookies {
		t.Run(strings.Join(tc.SetCookie, " | "), func(t *testing.T) {
			mu.Lock()
			current = tc.SetCookie
			mu.Unlock()
			e, err := send(t, n, engine.NewMsg())
			if err != nil {
				t.Fatal(err)
			}
			m := e.on(0)[0]
			if !reflect.DeepEqual(m.Data["responseCookies"], tc.Cookies) {
				t.Fatalf("msg.responseCookies\n got %#v\nwant %#v", m.Data["responseCookies"], tc.Cookies)
			}
			list, _ := m.Data["headers"].(map[string]any)["set-cookie"].([]any)
			if len(list) != len(tc.SetCookie) {
				t.Fatalf("msg.headers['set-cookie'] = %#v, want one entry per header", list)
			}
		})
	}
}

func TestGoldenMultipartForms(t *testing.T) {
	g := loadHTTPGolden(t)
	type capture struct {
		contentType string
		body        []byte
	}
	got := make(chan capture, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- capture{r.Header.Get("Content-Type"), b}
	}))
	defer srv.Close()
	n := build(t, "http request", `{"method":"POST","url":"`+srv.URL+`","ret":"txt"}`, newTestServices())

	for _, tc := range g.Forms {
		t.Run(tc.Name, func(t *testing.T) {
			m := engine.NewMsg()
			m.SetPayload(revive(tc.Payload))
			m.Data["headers"] = map[string]any{"Content-Type": "multipart/form-data"}
			_, err := send(t, n, m)
			if tc.Error != "" {
				if err == nil {
					t.Fatalf("want an error like %q, the form was sent", tc.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c := <-got
			boundary, ok := strings.CutPrefix(c.contentType, "multipart/form-data; boundary=")
			if !ok || len(boundary) != 50 || !strings.HasPrefix(boundary, strings.Repeat("-", 26)) {
				t.Fatalf("Content-Type %q is not form-data's", c.contentType)
			}
			body := strings.ReplaceAll(string(c.body), boundary, "BOUNDARY")
			if want := latin1Bytes(*tc.Body); body != string(want) {
				t.Fatalf("body\n got %q\nwant %q", body, want)
			}
		})
	}
}

// latin1Bytes is Buffer.from(s, 'latin1'): the golden file stores the body
// one byte per character.
func latin1Bytes(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		out = append(out, byte(r))
	}
	return out
}

func TestGoldenUploads(t *testing.T) {
	g := loadHTTPGolden(t)
	base := liveRoutes(t, withRoutes(t))
	in := build(t, "http in", `{"url":"/up","method":"post","upload":true}`, newTestServices())
	resp := build(t, "http response", `{}`, newTestServices())
	seen := make(chan *engine.Msg, 1)
	flowTo(t, in, func(m *engine.Msg) {
		seen <- m
		_ = resp.Receive(t.Context(), m, newTestEmitter())
	})

	for _, tc := range g.Uploads {
		t.Run(tc.Name, func(t *testing.T) {
			body, _ := base64.StdEncoding.DecodeString(tc.Body)
			r, err := http.Post(base+"/up", tc.ContentType, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			answer, _ := io.ReadAll(r.Body)
			r.Body.Close()

			if tc.Status != 0 {
				if r.StatusCode != tc.Status || string(answer) != "Internal Server Error" {
					t.Fatalf("status %d %q, want %d Internal Server Error", r.StatusCode, answer, tc.Status)
				}
				_, _, perr := parseUpload(tc.ContentType, body)
				if perr == nil || perr.Error() != tc.Error {
					t.Fatalf("the refusal is %v, multer's is %q", perr, tc.Error)
				}
				return
			}
			if r.StatusCode != 200 {
				t.Fatalf("status %d: %s", r.StatusCode, answer)
			}
			m := <-seen
			if !reflect.DeepEqual(m.Payload(), tc.Payload) {
				t.Fatalf("msg.payload\n got %#v\nwant %#v", m.Payload(), tc.Payload)
			}
			req := m.Data["req"].(map[string]any)
			files, _ := req["files"].([]any)
			var plain []any
			for _, f := range files {
				fm := f.(map[string]any)
				c := make(map[string]any, len(fm))
				for k, v := range fm {
					c[k] = v
				}
				c["buffer"] = base64.StdEncoding.EncodeToString(fm["buffer"].(engine.ImmutableBytes))
				plain = append(plain, c)
			}
			if plain == nil {
				plain = []any{}
			}
			if !reflect.DeepEqual(plain, tc.Files) {
				t.Fatalf("msg.req.files\n got %#v\nwant %#v", plain, tc.Files)
			}
		})
	}
}

// Without upload ticked, a multipart body stays bytes, as it does in
// Node-RED, and so does a PUT with it ticked: multer only runs for POST.
func TestUploadsOnlyWhenAskedAndOnlyOnPost(t *testing.T) {
	base := liveRoutes(t, withRoutes(t))
	body := "--XB\r\nContent-Disposition: form-data; name=\"k\"\r\n\r\nv\r\n--XB--\r\n"
	for _, tc := range []struct {
		cfg, path, method string
	}{
		{`{"url":"/off","method":"post"}`, "/off", "POST"},
		{`{"url":"/put","method":"put","upload":true}`, "/put", "PUT"},
	} {
		in := build(t, "http in", tc.cfg, newTestServices())
		resp := build(t, "http response", `{}`, newTestServices())
		seen := make(chan *engine.Msg, 1)
		flowTo(t, in, func(m *engine.Msg) {
			seen <- m
			_ = resp.Receive(t.Context(), m, newTestEmitter())
		})
		req, _ := http.NewRequest(tc.method, base+tc.path, strings.NewReader(body))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=XB")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		m := <-seen
		if _, isBytes := m.Payload().(engine.ImmutableBytes); !isBytes {
			t.Fatalf("%s: payload is %T, want the raw bytes", tc.cfg, m.Payload())
		}
		if _, has := m.Data["req"].(map[string]any)["files"]; has {
			t.Fatalf("%s: msg.req.files appeared without an upload", tc.cfg)
		}
	}
}

func TestGoldenMimeTable(t *testing.T) {
	g := loadHTTPGolden(t)
	for name, want := range g.Mime {
		got, ok := mimeLookup(name)
		switch w := want.(type) {
		case bool:
			if ok {
				t.Errorf("%s: got %q, mime-types knows nothing", name, got)
			}
		case string:
			if got != w {
				t.Errorf("%s: got %q, mime-types says %q", name, got, w)
			}
		}
	}
	// And nothing in the table that the golden file did not check.
	for ext := range mimeTypes {
		if _, ok := g.Mime[ext]; !ok {
			t.Errorf("%s is in the table but not in the golden file", ext)
		}
	}
}
