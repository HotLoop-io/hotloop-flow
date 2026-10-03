package flowtest

import (
	"fmt"
	"sort"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// contextEntry is one scope's worth of context in a test: what it starts with,
// or what it has to hold at the end.
type contextEntry struct {
	scope  node.ContextScope
	id     string // the tab or node id; empty for global
	label  string // how a failure names it
	values map[string]any
	// expect is which expectation this is, for a check at the end.
	expect int
}

func (e contextEntry) store(ctxs *store.ScopedContexts) node.Context {
	switch e.scope {
	case node.ScopeGlobal:
		return ctxs.Global()
	case node.ScopeFlow:
		return ctxs.Flow(e.id)
	default:
		return ctxs.Node(e.id)
	}
}

// resolveContext turns context as a test writes it into the scopes the
// runtime keys it by.
func (g *graphView) resolveContext(cv *ContextValues, expect int) ([]contextEntry, error) {
	if cv == nil {
		return nil, nil
	}
	var out []contextEntry
	if len(cv.Global) > 0 {
		out = append(out, contextEntry{scope: node.ScopeGlobal, label: "global context", values: cv.Global, expect: expect})
	}
	for _, ref := range sortedFlowKeys(cv.Flow) {
		id, label, err := g.findFlow(ref)
		if err != nil {
			return nil, err
		}
		out = append(out, contextEntry{scope: node.ScopeFlow, id: id,
			label: "flow context of " + label, values: cv.Flow[ref], expect: expect})
	}
	for _, ref := range sortedFlowKeys(cv.Node) {
		n, err := g.findAny(ref)
		if err != nil {
			return nil, err
		}
		out = append(out, contextEntry{scope: node.ScopeNode, id: n.ID,
			label: "node context of " + g.label(n.ID), values: cv.Node[ref], expect: expect})
	}
	return out, nil
}

func sortedFlowKeys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// findFlow resolves a flow context scope: a tab by id or label, or a subflow
// instance by its node's id, since each instance keeps flow context of its own.
func (g *graphView) findFlow(ref string) (id, label string, err error) {
	if t, ok := g.expanded.Tabs[ref]; ok {
		return t.ID, fmt.Sprintf("%q (%s)", t.Label, t.ID), nil
	}
	var found []*engine.Tab
	for _, t := range g.flows.Tabs {
		if t.Label == ref {
			found = append(found, t)
		}
	}
	switch len(found) {
	case 1:
		return found[0].ID, fmt.Sprintf("%q (%s)", found[0].Label, found[0].ID), nil
	case 0:
		return "", "", fmt.Errorf("there's no flow with the id or label %q", ref)
	}
	ids := make([]string, len(found))
	for i, t := range found {
		ids[i] = t.ID
	}
	sort.Strings(ids)
	return "", "", fmt.Errorf("%d flows are labelled %q (%s); use an id", len(found), ref, strings.Join(ids, ", "))
}

// findAny resolves a node the way find does, but takes configuration nodes
// too, by id or by name: a broker's credentials belong to the broker.
func (g *graphView) findAny(ref string) (*engine.Node, error) {
	if n, ok := g.expanded.Nodes[ref]; ok {
		return n, nil
	}
	if n, err := g.find(ref); err == nil {
		return n, nil
	}
	var named []*engine.Node
	for _, id := range g.flows.Order {
		if n, ok := g.flows.Nodes[id]; ok && n.IsConfig && n.Name == ref {
			named = append(named, n)
		}
	}
	if len(named) == 1 {
		return named[0], nil
	}
	return g.find(ref)
}

// startContext puts a test's starting context in place.
func startContext(ctxs *store.ScopedContexts, entries []contextEntry) error {
	for _, e := range entries {
		st := e.store(ctxs)
		for _, k := range sortedKeys(e.values) {
			if err := st.Set(k, e.values[k]); err != nil {
				return fmt.Errorf("setting %s %s: %w", e.label, k, err)
			}
		}
	}
	return nil
}

// checkContext compares context at the end of a test with what the test
// expected there.
func checkContext(ctxs *store.ScopedContexts, entries []contextEntry) []Problem {
	var problems []Problem
	for _, e := range entries {
		st := e.store(ctxs)
		for _, k := range sortedKeys(e.values) {
			want := e.values[k]
			got, ok, err := st.Get(k)
			var why string
			switch {
			case err != nil:
				why = fmt.Sprintf("%s: reading it: %v", k, err)
			case want == nil:
				if ok && got != nil {
					why = fmt.Sprintf("%s: wanted it unset, and it's %s", k, show(got))
				}
			case !ok:
				why = fmt.Sprintf("%s: not set%s", k, holds(st))
			default:
				if match, diff := contains(want, got, k); !match {
					why = strings.Replace(diff, "msg.", "", 1)
				}
			}
			if why == "" {
				continue
			}
			p := Problem{Expect: e.expect, Message: e.label + ": " + why}
			if e.scope == node.ScopeNode {
				p.Node = e.id
			}
			problems = append(problems, p)
		}
	}
	return problems
}

// holds lists what a context scope does hold, for a failure that says a key
// isn't there: usually the answer is a typo.
func holds(st node.Context) string {
	keys, err := st.Keys()
	if err != nil || len(keys) == 0 {
		return "; nothing is"
	}
	sort.Strings(keys)
	return "; it holds " + strings.Join(keys, ", ")
}

// resolveCredentials turns credentials as a test writes them into what the
// runtime hands each node, by id.
func (g *graphView) resolveCredentials(in map[string]map[string]any) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, ref := range sortedFlowKeys(in) {
		n, err := g.findAny(ref)
		if err != nil {
			return nil, err
		}
		creds := make(map[string]string, len(in[ref]))
		for k, v := range in[ref] {
			// A YAML number or boolean is still the text a person typed into
			// a password box.
			creds[k] = fmt.Sprint(v)
		}
		out[n.ID] = creds
	}
	return out, nil
}

// secretFiles stands in for the credential files a node can point at with
// ew_credentialFiles. A test reads nothing off the disk, so a file a node
// names answers with the credential the test gives that node for that field,
// and with nothing when the test gives none: nothing logs in to anything
// under a test, so an empty password is only ever a problem for a node that
// refuses to start without one, and that says so.
func secretFiles(flows *engine.Flows, creds map[string]map[string]string) func(path string) ([]byte, error) {
	byPath := map[string]string{}
	for id, n := range flows.Nodes {
		refs, ok := n.Raw[runtime.PropCredentialFiles].(map[string]any)
		if !ok {
			continue
		}
		for key, p := range refs {
			path, ok := p.(string)
			if !ok {
				continue
			}
			if v, ok := creds[id][key]; ok {
				byPath[strings.TrimSpace(path)] = v
			}
		}
	}
	return func(path string) ([]byte, error) {
		return []byte(byPath[path]), nil
	}
}
