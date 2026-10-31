package nodes

import (
	"fmt"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// The MCP client node against a real HotLoop's own MCP server, or it skips.
//
//	HOTLOOP_FLOW_TEST_HOTLOOP=http://localhost:18480 \
//	HOTLOOP_FLOW_TEST_HOTLOOP_ADMIN=admin \
//	HOTLOOP_FLOW_TEST_HOTLOOP_ADMIN_PASSWORD=... \
//	HOTLOOP_FLOW_TEST_HOTLOOP_MCP_TOKEN=... \
//	HOTLOOP_FLOW_TEST_MODBUS=127.0.0.1:5020 \
//	go test ./internal/nodes/ -run HotloopMCP -v
//
// HotLoop has to be started with HOTLOOP_MCP_TOKEN set to the same token, and
// the provisioning is the one the other HotLoop tests do.

// mcpClientNode builds an MCP client node pointed at HotLoop's /mcp, with the token.
func (l *hotloopLive) mcpClientNode(t *testing.T, token, cfg string) node.Node {
	t.Helper()
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	svc.creds["token"] = token
	srv := build(t, "hotloop-flow-mcp-server", fmt.Sprintf(`{"url":%q,"timeout":20}`, l.base+"/mcp"), svc)
	svc.configs["mcp"] = srv
	t.Cleanup(func() { _ = srv.(node.Closer).Close(t.Context(), false) })
	return build(t, "mcp client", cfg, svc)
}

func TestHotloopMCPClientNodeAgainstTheRealHotloop(t *testing.T) {
	token := requireEnv(t, "HOTLOOP_FLOW_TEST_HOTLOOP_MCP_TOKEN")
	l := newHotloopLive(t)

	// The tools HotLoop lists are the ones its documentation names.
	list := l.mcpClientNode(t, token, `{"server":"mcp","operation":"list"}`)
	e, err := send(t, list, msg(t, `{}`))
	if err != nil {
		t.Fatalf("listing HotLoop's tools: %v", err)
	}
	tools, _ := e.on(0)[0].Payload().([]any)
	have := map[string]bool{}
	for _, x := range tools {
		have[x.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"get_plant_status", "list_devices", "get_states", "read_tag", "write_tag"} {
		if !have[want] {
			t.Errorf("HotLoop does not list %s: %v", want, have)
		}
	}
	t.Logf("HotLoop lists %d tools", len(tools))

	// A structured answer comes back as an object, and it is about the plant
	// this test just built: the device the provisioning made is in it.
	devices := l.mcpClientNode(t, token, `{"server":"mcp","tool":"list_devices"}`)
	e, err = send(t, devices, msg(t, `{}`))
	if err != nil {
		t.Fatalf("list_devices: %v", err)
	}
	out := e.on(0)[0]
	body, _ := out.Payload().(map[string]any)
	found := false
	for _, d := range asSlice(body["devices"]) {
		if m, ok := d.(map[string]any); ok && m["id"] == "flowtest" {
			found = true
		}
	}
	if !found {
		t.Fatalf("list_devices through the node does not show the flowtest device: %#v", out.Payload())
	}
	info, _ := out.Data["mcp"].(map[string]any)
	if info["tool"] != "list_devices" || info["isError"] != false {
		t.Errorf("msg.mcp = %#v", info)
	}

	// The arguments come from the message, and the answer is the live state of
	// an entity, the same state HotLoop's own API gives.
	want := l.state(t, "number.flowtest_sp")
	states := l.mcpClientNode(t, token, `{"server":"mcp","tool":"get_states","arguments":"args","argumentsType":"msg"}`)
	e, err = send(t, states, msg(t, `{"args":{"entity_ids":["number.flowtest_sp"]}}`))
	if err != nil {
		t.Fatalf("get_states: %v", err)
	}
	if got := fmt.Sprint(e.on(0)[0].Payload()); !strings.Contains(got, "number.flowtest_sp") || !strings.Contains(got, want) {
		t.Errorf("get_states through the node = %s, want the entity at state %q", got, want)
	}

	// A tool HotLoop does not have is an error with HotLoop's words, not a message.
	missing := l.mcpClientNode(t, token, `{"server":"mcp","tool":"no_such_tool"}`)
	if e, err = send(t, missing, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "no_such_tool") || e.total() != 0 {
		t.Fatalf("an unknown tool: err = %v, sent %d", err, e.total())
	}
	t.Logf("unknown tool: %v", err)

	// MCP writes, as HotLoop ships it: through the write gate like everybody's.
	// An armed tag takes the value and the register moves.
	write := l.mcpClientNode(t, token, `{"server":"mcp","tool":"write_tag","arguments":"{\"tag\":\"flowtest/sp\",\"value\":50,\"reason\":\"flow test\"}","argumentsType":"json"}`)
	if e, err = send(t, write, msg(t, `{}`)); err != nil {
		t.Fatalf("write_tag to an armed tag through the node: %v", err)
	}
	t.Logf("write_tag: %#v", e.on(0)[0].Payload())
	if got := l.plc(t, "holding", hlSetpointAddr); got != 50.0 {
		t.Errorf("the PLC holds %v after an MCP write of 50", got)
	}

	// A tool that reports an error raises one. A tag nobody marked writable is
	// refused in HotLoop's own words, and the register does not move.
	before := l.plc(t, "holding", hlLockedAddr)
	locked := l.mcpClientNode(t, token, `{"server":"mcp","tool":"write_tag","arguments":"{\"tag\":\"flowtest/locked\",\"value\":9,\"reason\":\"flow test\"}","argumentsType":"json"}`)
	e, err = send(t, locked, msg(t, `{}`))
	if err == nil || !strings.Contains(err.Error(), "not marked writable") {
		t.Fatalf("write_tag to a tag that is not armed: err = %v", err)
	}
	t.Logf("write_tag to a locked tag: %v", err)
	if got := l.plc(t, "holding", hlLockedAddr); got != before {
		t.Errorf("the PLC moved from %v to %v on a refused MCP write", before, got)
	}

	// The wrong token, and no token, are refused by HotLoop and the node says
	// it could not connect, without printing the token.
	for _, bad := range []string{"not-the-token", ""} {
		n := l.mcpClientNode(t, bad, `{"server":"mcp","tool":"get_plant_status"}`)
		_, err := send(t, n, msg(t, `{}`))
		if err == nil || !strings.Contains(err.Error(), "could not connect") {
			t.Errorf("token %q: err = %v", bad, err)
		}
		if bad != "" && err != nil && strings.Contains(err.Error(), bad) {
			t.Errorf("the error repeats the token: %v", err)
		}
	}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
