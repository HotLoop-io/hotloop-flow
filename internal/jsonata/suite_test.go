package jsonata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/recolabs/gnata"
)

// The published JSONata test suite, from jsonata-js, run through this package.
//
// The suite is not committed. CI clones jsonata-js at the pinned tag below,
// checks the commit, and points HOTLOOP_FLOW_JSONATA_SUITE at it, the same way
// it builds the WASM guest rather than committing it: 1,347 files of somebody
// else's test data in this repository would be reviewed by nobody and could
// drift from the release it claims to be. To run it locally:
//
//	git clone --depth 1 --branch v2.2.2 https://github.com/jsonata-js/jsonata.git /tmp/jsonata-js
//	HOTLOOP_FLOW_JSONATA_SUITE=/tmp/jsonata-js/test/test-suite go test ./internal/jsonata/ -run Suite -v
//
// Every case either passes or fails. Nothing is skipped, and the set that
// fails is asserted exactly, so a gnata upgrade that fixes a case shows up here
// as loudly as one that breaks one.
const (
	suiteTag    = "v2.2.2"
	suiteCommit = "6c7e95fdbf4405a1e741852a7cd8cd985b4305bb"

	// suiteCases is how many cases the v2.2.2 suite holds.
	suiteCases = 1686
)

// knownFailures are the published cases this build gets wrong, and why. 1673
// of 1686 pass.
//
// Ten of the thirteen still raise an error where the suite expects one, with a
// different code or token than jsonata-js gives, so a flow sees the failure
// either way. The other three are corners no flow is going to hit: a wildcard
// applied to a function or a regex, and the parent operator with no parent.
var knownFailures = map[string]string{
	"errors/case025.json":                      "T0410 raised without the function name as its token",
	"errors/case026.json":                      "T2001 instead of T2002 for a string returned into +",
	"function-decodeUrl/case002.json":          "D3137 instead of D3140 for a truncated escape",
	"function-decodeUrlComponent/case002.json": "D3137 instead of D3140 for a truncated escape",
	"function-formatNumber/issue786.json[0]":   "D3085 instead of D3086 for a picture with no digits",
	"function-formatNumber/issue786.json[1]":   "D3085 instead of D3086 for a picture with no digits",
	"function-formatNumber/issue786.json[2]":   "D3085 instead of D3086 for a picture with no digits",
	"function-formatNumber/issue786.json[3]":   "D3085 instead of D3086 for a picture with no digits",
	"joins/errors.json[0]":                     "S0214 raised against the next token rather than @",
	"joins/errors.json[1]":                     "S0214 raised against the next token rather than #",
	"parent-operator/errors.json[0]":           "% with no parent evaluates to null instead of raising S0217",
	"wildcards/case010.json[1]":                "* over an array of functions returns the functions",
	"wildcards/case010.json[2]":                "* over a regex returns its flags",
}

type suiteCase struct {
	Expr            string          `json:"expr"`
	ExprFile        string          `json:"expr-file"`
	Data            json.RawMessage `json:"data"`
	Dataset         *string         `json:"dataset"`
	Bindings        json.RawMessage `json:"bindings"`
	Result          json.RawMessage `json:"result"`
	UndefinedResult bool            `json:"undefinedResult"`
	Code            string          `json:"code"`
	Token           *string         `json:"token"`
	Error           json.RawMessage `json:"error"`
	TimeLimit       int             `json:"timelimit"`
	Depth           int             `json:"depth"`

	// Presence of a key matters as much as its value: "data": null is a null
	// input, a missing "data" means look at the dataset.
	keys map[string]bool
}

func TestPublishedJSONataSuite(t *testing.T) {
	dir := os.Getenv("HOTLOOP_FLOW_JSONATA_SUITE")
	if dir == "" {
		t.Skip("set HOTLOOP_FLOW_JSONATA_SUITE to the test/test-suite directory of jsonata-js " + suiteTag)
	}

	datasets := map[string]any{}
	files, err := filepath.Glob(filepath.Join(dir, "datasets", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no datasets under %s: %v", dir, err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		v, err := decode(raw)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		datasets[strings.TrimSuffix(filepath.Base(f), ".json")] = v
	}

	groups, err := os.ReadDir(filepath.Join(dir, "groups"))
	if err != nil {
		t.Fatal(err)
	}
	var total, passed int
	failed := map[string]string{}
	for _, g := range groups {
		if !g.IsDir() {
			continue
		}
		gdir := filepath.Join(dir, "groups", g.Name())
		names, err := filepath.Glob(filepath.Join(gdir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(names)
		for _, name := range names {
			cases, err := readCases(name)
			if err != nil {
				t.Fatal(err)
			}
			for i, c := range cases {
				total++
				id := fmt.Sprintf("%s/%s", g.Name(), filepath.Base(name))
				if len(cases) > 1 {
					id = fmt.Sprintf("%s[%d]", id, i)
				}
				if err := runCase(gdir, c, datasets); err != nil {
					failed[id] = err.Error()
					continue
				}
				passed++
			}
		}
	}

	t.Logf("JSONata %s published suite: %d of %d cases pass", suiteTag, passed, total)
	if total != suiteCases {
		t.Errorf("the suite holds %d cases, want %d: is this jsonata-js %s (%s)?", total, suiteCases, suiteTag, suiteCommit)
	}
	for _, id := range sortedIDs(failed) {
		if _, known := knownFailures[id]; !known {
			t.Errorf("FAIL %s: %s", id, failed[id])
		}
	}
	for _, id := range sortedIDs(knownFailures) {
		if _, still := failed[id]; !still {
			t.Errorf("%s passes now; take it off knownFailures", id)
		}
	}
	if want := suiteCases - len(knownFailures); passed != want {
		t.Errorf("%d cases pass, want exactly %d", passed, want)
	}
}

func sortedIDs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func readCases(path string) ([]*suiteCase, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var objs []map[string]json.RawMessage
	if t := strings.TrimSpace(string(raw)); strings.HasPrefix(t, "[") {
		if err := json.Unmarshal(raw, &objs); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else {
		var one map[string]json.RawMessage
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		objs = []map[string]json.RawMessage{one}
	}
	out := make([]*suiteCase, 0, len(objs))
	for _, o := range objs {
		b, _ := json.Marshal(o)
		c := &suiteCase{keys: map[string]bool{}}
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for k := range o {
			c.keys[k] = true
		}
		out = append(out, c)
	}
	return out, nil
}

// runCase follows jsonata-js's own runner, test/run-test-suite.js, branch for
// branch.
func runCase(dir string, c *suiteCase, datasets map[string]any) error {
	expr := c.Expr
	if c.ExprFile != "" {
		b, err := os.ReadFile(filepath.Join(dir, c.ExprFile))
		if err != nil {
			return err
		}
		expr = string(b)
	}

	// The runner timeboxes a case that carries both limits. Its timebox raises
	// U1001 for either, which is the code gnata's built-in stack limit raises
	// too, so the time limit is the one that needs passing on.
	var opts []gnata.Option
	if c.keys["timelimit"] && c.keys["depth"] {
		opts = append(opts, gnata.WithTimeout(time.Duration(c.TimeLimit)*time.Millisecond))
	}
	x, err := Compile(expr, nil, opts...)
	if err != nil {
		if c.Code == "" {
			return fmt.Errorf("unexpected compile error: %v", err)
		}
		return matchError(err, c.Code, c.Token)
	}

	var input any
	switch {
	case c.keys["data"]:
		input, err = decode(c.Data)
		if err != nil {
			return err
		}
		if input == nil {
			input = gnata.Null
		}
	case c.Dataset == nil:
		input = nil
	default:
		ds, ok := datasets[*c.Dataset]
		if !ok {
			return fmt.Errorf("unknown dataset %q", *c.Dataset)
		}
		input = ds
	}

	var vars map[string]any
	if len(c.Bindings) > 0 && string(c.Bindings) != "null" {
		if err := json.Unmarshal(c.Bindings, &vars); err != nil {
			return err
		}
	}

	got, defined, err := x.Eval(context.Background(), input, vars)
	switch {
	case c.keys["undefinedResult"]:
		if err != nil {
			return fmt.Errorf("want undefined, got error %v", err)
		}
		if defined {
			return fmt.Errorf("want undefined, got %s", show(got))
		}
	case c.keys["result"]:
		if err != nil {
			return fmt.Errorf("want %s, got error %v", c.Result, err)
		}
		var want any
		if err := json.Unmarshal(c.Result, &want); err != nil {
			return err
		}
		if !defined || !reflect.DeepEqual(got, want) {
			return fmt.Errorf("want %s, got %s (defined=%v)", c.Result, show(got), defined)
		}
	case c.keys["error"]:
		// The runner checks that the error contains every field given here,
		// which in this suite is a code and sometimes a token.
		var want struct {
			Code  string  `json:"code"`
			Token *string `json:"token"`
		}
		if jerr := json.Unmarshal(c.Error, &want); jerr != nil {
			return jerr
		}
		if err == nil {
			return fmt.Errorf("want error %s, got %s", c.Error, show(got))
		}
		return matchError(err, want.Code, want.Token)
	case c.Code != "":
		if err == nil {
			return fmt.Errorf("want error %s, got %s", c.Code, show(got))
		}
		return matchError(err, c.Code, nil)
	default:
		return errors.New("the case says nothing to check")
	}
	return nil
}

func matchError(err error, code string, token *string) error {
	if got := ErrorCode(err); got != code {
		return fmt.Errorf("want error code %s, got %q (%v)", code, got, err)
	}
	if token != nil {
		if got := errorToken(err); got != *token {
			return fmt.Errorf("want error token %q, got %q", *token, got)
		}
	}
	return nil
}

var tokenText = regexp.MustCompile(`at token ("(?:[^"\\]|\\.)*")`)

// errorToken reads the token a compile error was raised against.
func errorToken(err error) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		v := reflect.ValueOf(e)
		if v.Kind() == reflect.Pointer && !v.IsNil() && v.Elem().Kind() == reflect.Struct {
			if f := v.Elem().FieldByName("Token"); f.IsValid() && f.Kind() == reflect.String {
				return f.String()
			}
		}
		if m := tokenText.FindStringSubmatch(e.Error()); m != nil {
			if tok, err := strconv.Unquote(m[1]); err == nil {
				return tok
			}
		}
	}
	return ""
}

// decode reads suite JSON the way the production path delivers values: numbers
// as float64. Objects stay ordered, because the suite is testing the language
// against ordered input and several cases depend on key order.
func decode(raw []byte) (any, error) {
	v, err := gnata.DecodeJSON(raw)
	if err != nil {
		return nil, err
	}
	return floats(v), nil
}

func floats(v any) any {
	switch t := v.(type) {
	case json.Number:
		f, _ := t.Float64()
		return f
	case []any:
		for i := range t {
			t[i] = floats(t[i])
		}
	case *gnata.OrderedMap:
		for _, k := range t.Keys() {
			e, _ := t.Get(k)
			t.Set(k, floats(e))
		}
	}
	return v
}

func show(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(b)
}
