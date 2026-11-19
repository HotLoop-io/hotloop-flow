# Flow's MCP tools

Flow serves an MCP endpoint at `/mcp` on the admin address, and it reads and
writes. An agent that connects with an API token can look at what the editor
looks at and change what the editor changes, with the token's own permissions and
nothing past them. There is no read only mode: a server an agent can look at and
not touch is a status page.

There are 43 tools of Flow's own, beside the tools each flow serves through its
`mcp tool` nodes. Every one is a route of the admin API. A call goes through the
route's own handler, as the caller the endpoint signed in, so the validation, the
change, the deployment history and the audit row are the route's, and a refusal
comes back in the route's words. Nothing was copied into the MCP server to drift.

Connecting, tokens, rate limits and the audit trail of every call are in the
README's MCP section. A caller needs `mcp.call` and the tool's own permission,
and a tool it may not call is not in its list.

## The flows

| Tool | Route | Permission | Does |
|---|---|---|---|
| `list_flows` | `GET /flows` | `flows.read` | every tab and subflow: id, label, disabled, node count, and the revision |
| `get_flows` | `GET /flows` | `flows.read` | the whole deployed set as the editor reads it, with the revision |
| `get_flow` | `GET /flows` | `flows.read` | one tab or subflow with its nodes, by id |
| `deploy_flows` | `POST /flows` | `flows.write` | **deploy**: replace the whole set, with the revision you read and a note |
| `list_deployments` | `GET /deployments` | `flows.read` | the deployment history, newest first |
| `rollback_deployment` | `POST /deployments/{seq}/rollback` | `flows.write` | **roll back** to an earlier deployment, as a new one |
| `inject_node` | `POST /inject/{id}` | `inject.write` | **press** an inject node, with a message of your own |

`deploy_flows` replaces the whole set, so read `get_flows` first and send it back
changed, with the `rev` it gave you.

## Everything around them

| Tool | Route | Permission | Does |
|---|---|---|---|
| `get_settings` | `GET /settings` | `settings.read` | The settings the editor reads: the version, the admin root, the inbox capacity and overflow policy, whether sign-in, discovery and metrics are on. Secrets and the filesystem layout are not in it. |
| `list_nodes` | `GET /nodes` | `nodes.read` | Every node type this build has, as the editor builds its palette and edit dialogs from it. |
| `list_plugins` | `GET /plugins` | `nodes.read` | The plugins that loaded, and the ones that were refused and why. |
| `list_audit` | `GET /audit` | `audit.read` | The audit log: who deployed, rolled back, pressed an inject, saved tests or called a tool, and when. Narrow it with event, user and since (RFC 3339); limit is 1 to 10000. |
| `get_deployment` | `GET /deployments/{seq}` | `flows.read` | One deployment record: who, when, the note, and what it changed. The number comes from list_deployments. |
| `get_deployment_flows` | `GET /deployments/{seq}/flows` | `flows.read` | The whole flow set as it was at one deployment. |
| `diff_deployments` | `GET /deployments/{from}/diff/{to}` | `flows.read` | What changed between two deployments, node by node, with a text summary. |
| `diff_pending` | `POST /flows/diff` | `flows.read` | What deploying a flow set would change against what is deployed now, without deploying it. The body is the flow document, as deploy_flows takes it. Nothing is changed. |
| `export_flows` | `GET /flows/export` | `flows.read` | The deployed flow set as an export file, the way the editor's Export does it. |
| `check_flow_types` | `POST /flows/types` | `flows.read` | Whether the wires of a flow set agree about what travels on them, with every problem named. The body is the flow document. Nothing is changed. |
| `get_tests` | `GET /tests` | `flows.read` | The flow tests this instance keeps, as the suite document. |
| `put_tests` | `PUT /tests` | `flows.write` | REPLACE the whole flow test suite with the one you send. It is parsed first and refused whole if it does not parse. Read it with get_tests, change it, send it back. |
| `add_test_cases` | `POST /tests/cases` | `flows.write` | Add test cases to the suite without replacing it. Cases whose names are already there are the route's to refuse or replace, in its own words. |
| `run_tests` | `POST /tests/run` | `flows.read` | Run the flow tests against the deployed flows, or against a draft if flows is in the body, and say which passed. Optional body fields: flows, tests (a suite document as a string) and match (a regular expression on test names). |
| `test_from_debug` | `POST /tests/from-debug` | `flows.read` | Draft a test from a message a Debug node showed. It saves nothing: use add_test_cases to keep the draft. The body is {node, msgId}. |
| `list_traces` | `GET /traces` | `flows.read` | The message traces Flow keeps, newest first: each is one message's path through the flows. limit is 1 to 500, 50 when left out. |
| `find_trace` | `GET /traces/find` | `flows.read` | Find the trace a message belongs to, by the message's id and optionally the node it was seen at. |
| `get_trace` | `GET /traces/{id}` | `flows.read` | One trace with every hop. The id comes from list_traces. |
| `trace_to_test` | `POST /traces/{id}/test` | `flows.read` | Draft a flow test from a trace. It saves nothing: use add_test_cases to keep the draft. |
| `replay_trace` | `POST /traces/{id}/replay` | `flows.read` | Put the message that started a trace into the flows (the deployed ones, or a draft if flows is in the body) and compare where it ends up with where it ended up the first time. It runs in a sandbox the way the editor's replay does, and the route says in its own words when this instance cannot replay. |
| `reset_mfa` | `POST /auth/mfa/reset` | `auth.admin` | **Turn off** a user's two-factor sign-in so they can enrol again, for someone who lost their device. The body is `{username}`. |
| `get_runtime_stats` | `GET /runtime/stats` | `status.read` | The running flows' numbers: per-node received, sent, errors, dropped and queue depth. |

## Diagrams and instruments

What the editor's panel view and the instruments page do, for an agent. A server
without diagrams or instruments has neither the routes nor the tools. A control is
how a person commands what a diagram drives, so setting one, running a diagram and
switching an instrument's writes are in the audit log under the caller's name as
they are for a person, and a diagram's write to equipment still goes through
HotLoop's write gate and an instrument's own switch.

| Tool | Route | Permission | Does |
|---|---|---|---|
| `list_diagrams` | `GET /diagrams` | `status.read` | The diagrams in the flow file: each tab that runs as a diagram, with its id, whether it is running, its problems and its panel. |
| `get_diagram` | `GET /diagrams/{id}` | `status.read` | One diagram: its nodes, wires, panel controls and indicators, and any problems that stop it from running. |
| `get_diagram_trace` | `GET /diagrams/{id}/trace` | `status.read` | What the last run did, in order: every node start and finish and every value on a wire. Not found for a diagram that has not run since it was deployed. |
| `run_diagram` | `POST /diagrams/{id}/run` | `diagram.run` | **Run** a diagram, as the editor's Run button does. Optional body: highlightMs, pause, breakpoints. |
| `stop_diagram` | `POST /diagrams/{id}/stop` | `diagram.run` | **Stop** a running diagram. Nothing it already wrote to equipment is undone. |
| `debug_diagram` | `POST /diagrams/{id}/debug` | `diagram.run` | Drive a running diagram's debugger: pause, resume, step-into, step-over, step-out or highlight. |
| `set_diagram_control` | `PUT /diagrams/{id}/controls/{node}` | `diagram.run` | **Set** a control on the diagram's panel, checked against the control's kind, range and options. The body is `{value}`. |
| `set_diagram_breakpoints` | `PUT /diagrams/{id}/breakpoints` | `diagram.run` | Set the wires a diagram stops at, kept for the next run too. |
| `set_diagram_probes` | `PUT /diagrams/{id}/probes` | `diagram.run` | Choose the wires a diagram records values on, and how many it keeps. |
| `extract_sub_diagram` | `POST /diagrams/{id}/sub` | `status.read` | Make a sub-diagram out of some nodes and answer the new flow file. Nothing is saved or deployed. |
| `list_stations` | `GET /stations` | `flows.read` | The HotLoop Flow installs EmberNet shows as running for this instance's tenant, which are the stations a push goes to. Only where the stations settings are set. |
| `push_to_stations` | `POST /stations/push` | `flows.write` | **Push** a flow file to every station of the tenant, one at a time, each checked, a failed one put back and the push stopped. A push that would restart a running logger changes nothing unless `confirmRestart` is true or a window that holds now is given. |
| `list_instruments` | `GET /instruments` | `status.read` | The instruments the deployed diagrams would write to, and whether each one's writes are on. |
| `set_instrument_writes` | `PUT /instruments/writes` | `instrument.write` | **Turn** one instrument's writes on or off. Until they are on, a diagram's write to it is refused. |

Tools that take a document (`diff_pending`, `check_flow_types`, `put_tests`,
`add_test_cases`, `run_tests`, `test_from_debug`, `replay_trace`) take it as
`body`, the same JSON the route takes. `put_tests` replaces the whole suite, so
read `get_tests` first. `diff_pending`, `check_flow_types`, `test_from_debug` and
`trace_to_test` only ask: nothing is deployed or saved.

## Left out on purpose

These are a person's own sign-in, and a tool for them would only be a way round it:
`/auth/token` is the sign-in itself (a username, a password and a code answered
with a session), `/auth/revoke` ends the session that sends it, and `/auth/mfa`
with its setup, confirm and disable belong to whoever is signed in, for their own
second factor. `reset_mfa` is the administrator's side of that and is a tool,
with the `auth.admin` permission the route asks for.

`/auth/mode`, `/health`, `/ready` and `/metrics` are the probes and need no token.
`/panel/{id}` is a redirect to the editor's panel page. `/comms` is the editor's live socket, a stream and not a call; what it carries
(the Debug messages and the node status) is in the traces, `get_trace` and
`list_traces`. `/mcp` is this server.
