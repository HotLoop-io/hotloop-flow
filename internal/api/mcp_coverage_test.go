package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every route the admin API serves is a tool, or is on the list below with the
// reason it is not. The list is short and every entry is a person's own sign-in,
// a probe, a stream or a redirect. Flow's MCP reads and writes like every MCP in
// the HotLoop family, and a route added without a tool, or a tool taken away,
// fails here until the page that says what an agent can do says it too.

var flowsRoutes = map[string][]string{
	"GET /flows":                       {"list_flows", "get_flows", "get_flow"},
	"POST /flows":                      {"deploy_flows"},
	"GET /deployments":                 {"list_deployments"},
	"POST /deployments/{seq}/rollback": {"rollback_deployment"},
	"POST /inject/{id}":                {"inject_node"},
}

var routesNotTools = map[string]string{
	"GET /health":            "a probe, no token",
	"GET /ready":             "a probe, no token",
	"GET /auth/mode":         "a probe, no token",
	"POST /auth/token":       "the sign-in itself",
	"POST /auth/revoke":      "ends the session that sends it",
	"GET /auth/mfa":          "a person's own second factor",
	"POST /auth/mfa/setup":   "a person's own second factor",
	"POST /auth/mfa/confirm": "a person's own second factor",
	"POST /auth/mfa/disable": "a person's own second factor",
	"GET /comms":             "the editor's live socket, a stream; its Debug messages and node status are in the traces",
	"GET /panel/{id}":        "a redirect to the editor's panel page",
}

var routeLiteral = regexp.MustCompile(`"(GET|POST|PUT|DELETE|PATCH) "\+s\.path\("([^"]+)"\)`)

func TestEveryAdminRouteIsAToolOrIsLeftOutWithAReason(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	served := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range routeLiteral.FindAllStringSubmatch(string(raw), -1) {
			served[m[1]+" "+m[2]] = true
		}
	}
	if len(served) < 40 {
		t.Fatalf("found only %d routes; the scan is wrong", len(served))
	}

	tools := map[string]string{}
	for _, tl := range allRouteTools() {
		key := tl.method + " " + tl.path
		if prev, dup := tools[key]; dup && tl.name != prev {
			// Two tools may share a route (get_flows and get_flow), never in this table.
			t.Errorf("%s and %s are both the tool for %s", prev, tl.name, key)
		}
		tools[key] = tl.name
	}
	for route, names := range flowsRoutes {
		for _, n := range names {
			found := false
			for _, b := range BuiltinToolNames() {
				found = found || b == n
			}
			if !found {
				t.Errorf("%s is the tool for %s and is not a built-in tool", n, route)
			}
		}
		tools[route] = strings.Join(names, ",")
	}

	for route := range served {
		_, isTool := tools[route]
		reason := routesNotTools[route]
		switch {
		case !isTool && reason == "":
			t.Errorf("the admin route %s is no tool and is not on the list of routes left out, with a reason", route)
		case isTool && reason != "":
			t.Errorf("%s is both a tool and on the list of routes left out", route)
		}
	}
	for route := range tools {
		if !served[route] {
			t.Errorf("a tool names the route %s, which the admin API does not serve", route)
		}
	}
	for route := range routesNotTools {
		if !served[route] {
			t.Errorf("%s is on the list of routes left out and is not a route", route)
		}
	}
}

// docs/MCP.md is the page an agent's owner reads: it names every tool with its
// route and permission, and says how many there are.
func TestEveryToolIsInTheDocsWithItsRouteAndPermission(t *testing.T) {
	raw, err := os.ReadFile("../../docs/MCP.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, tl := range allRouteTools() {
		row := fmt.Sprintf("| `%s` | `%s %s` | `%s` |", tl.name, tl.method, tl.path, tl.perm)
		if !strings.Contains(doc, row) {
			t.Errorf("docs/MCP.md has no row beginning %q", row)
		}
	}
	for _, name := range BuiltinToolNames() {
		if !strings.Contains(doc, "| `"+name+"` |") {
			t.Errorf("docs/MCP.md has no row for %s", name)
		}
	}
	want := fmt.Sprintf("There are %d tools of Flow's own", len(BuiltinToolNames()))
	if !strings.Contains(doc, want) {
		t.Errorf("docs/MCP.md does not say %q", want)
	}
}
