# Node-RED node types that run natively

A plant's flows lean on a handful of community nodes: Modbus, the dashboard,
PostgreSQL, e-mail. Flow runs those types itself, in Go, under
Node-RED's own type names and property names, so a flow file that uses them
loads, runs and saves unchanged. Nothing is installed, and nothing needs Node.js.

This page says what runs, how, and where it differs. The line-by-line table is
[compatibility.md](compatibility.md), generated from the registry.

## What runs

| Family | Types | Where |
|---|---|---|
| Modbus | `modbus-client`, `modbus-read`, `modbus-getter`, `modbus-flex-getter`, `modbus-write`, `modbus-flex-write`, `modbus-server`, `modbustcp-server` | [Modbus](#modbus) |
| Dashboard | `ui_base`, `ui_tab`, `ui_group`, `ui_link`, `ui_spacer`, `ui_text`, `ui_numeric`, `ui_button`, `ui_switch`, `ui_text_input`, `ui_led`, `ui_gauge`, `ui_chart`, `ui_template`, and the Dashboard 2 configuration nodes, accepted | [The dashboard](#the-dashboard) |
| PostgreSQL | `postgreSQLConfig`, `postgresql` | [PostgreSQL](#postgresql) |
| E-mail | `e-mail` (sending) | [E-mail](#e-mail) |
| Function | `setInterval`, `setTimeout`, their clear functions, and `Buffer` | [The function node](#the-function-node) |

A flow with other community nodes still starts, and each node Flow does not know
fails by itself with a message that names its type. The rest of the flow runs.

## Modbus

The client is a config node (`modbus-client`) for one device over TCP, RTU over
TCP or a serial port. The nodes that point at it take turns on one connection.

- `modbus-getter` reads on a message from its own address, quantity and data
  type. `modbus-read` does the same on a timer (`rate` and `rateUnit`) and on a
  message. `modbus-flex-getter` takes `msg.payload = {fc, unitid, address,
  quantity}`.
- `modbus-write` writes `msg.payload` to coils or holding registers.
  `modbus-flex-write` takes `{value, fc, unitid, address, quantity}` with
  function code 5, 6, 15 or 16.
- The output is what the community nodes send: `msg.payload` is the list of
  values (numbers for registers, booleans for bits), `msg.responseBuffer` is
  `{data, buffer}` with the raw bytes, big endian, `msg.input` is the message
  that asked, and `msg.modbusRequest` says what was asked. A failure goes out the
  second output with `msg.error`, and out the first as an empty message when
  `emptyMsgOnFail` is on.
- `modbus-server` is a Modbus TCP server over buffers you size, for a simulator
  or a bridge. A message sets values and a client's write comes out as a message.
  With a network interface set (below) its listening socket is bound to that
  interface before it listens, so a box with a VLAN for each machine can run one
  server on each, all at the same address and port.

What is not provided: the I/O file (`useIOFile`; a node that asks for one does
not start, and says so), the command queue and delay settings (requests take
turns on the connection, so they have nothing to do), and serial ASCII.

Proven against a real Modbus TCP server (pymodbus, the simulator CI runs) and an
in-process one: reads and writes of coils and registers, the unit id, polling,
the start delay, error paths, and the float a plant builds from two registers.

## The dashboard

The `ui_` nodes draw a page, served from the binary at `/ui` (set
`HOTLOOP_FLOW_UI_PATH` to move it). No sign-in, like an HTTP In node: put it
where you would put one.

- `ui_tab` is a page in the menu, `ui_group` is a card on it (`width` columns
  wide), and the widgets sit in the group's grid. `ui_link` is a menu entry to
  another address. Tabs that share a name are told apart by the flow tab their
  widgets are on, so a file of twelve machines does not show "Main Screen" twelve
  times.
- Widgets show the last message they got, and a page that opens late starts from
  that. A chart keeps its points for `removeOlder` times its unit.
- What the operator does comes out of the node as a message: a button press, a
  switch, a number, typed text, `scope.send` from a template. `passthru` and
  `decouple` do what they do in the community dashboard.
- `ui_template` is HTML with a small AngularJS-style layer: `{{ }}`, `ng-click`,
  `ng-class`, `ng-init`, `ng-show`, `ng-hide`, `ng-if`, `ng-bind`, `scope.msg`,
  `scope.$watch`, `scope.send`. It runs in the operator's browser with the page's
  permissions, so it is as trusted as whoever wrote the flow.

The page is plain HTML, CSS and JavaScript in the binary. Nothing is fetched
from outside the plant. It works at a phone width and follows the dashboard's
dark or light theme.

What is not provided: Dashboard 2 widgets (its configuration nodes load and
nothing draws them), Angular directives beyond the list above, icons, and the
pie, polar and radar charts, which draw as a line.

Proven in a real browser against the real binary (`web/e2e/dashboard.spec.ts`):
live updates, the late-opening page, every operator action coming back as a
message, a redeploy changing the open page, a phone width, and a cross-site post
being refused.

## PostgreSQL

`postgreSQLConfig` and `postgresql` run SQL against PostgreSQL or TimescaleDB.
Put values in with `$1`, `$2` and `msg.params`, so the database does the quoting;
`{{{msg.x}}}` puts text straight into the query and is only for names you trust.
`msg.payload` is the rows and `msg.pgsql` is `{command, rowCount}`. With `split`
on, rows go out `rowsPerMsg` at a time with `msg.parts`.

The connection opens on the first query, not at deploy, so a database that is
not up yet does not stop the flow. Proven against a real PostgreSQL 17.

## E-mail

`e-mail` sends over SMTP: a secure connection from the start, or STARTTLS when the
server offers it, with `tls` as the switch for only trusting a valid certificate.
Basic sign-in (PLAIN, LOGIN) and XOAUTH2 with the token read from a message
property. `msg.payload` is the body, `msg.topic` the subject, `msg.to`, `msg.cc`,
`msg.bcc`, `msg.html` and `msg.attachments` are honored, and a line break in a
header value is refused. Sending only.

## The function node

Three things a plant's functions use that Flow's sandbox did not have:

- **`Buffer`**: `alloc`, `from`, `concat`, `isBuffer`, `byteLength`, the read and
  write methods for 8, 16 and 32 bit integers, floats and doubles in both byte
  orders, `slice`, `toString` (utf8, hex, base64) and `toJSON`. A byte slice in a
  message arrives as a byte array, so wrap it with `Buffer.from`.
- **`setInterval` and `setTimeout`**, with `clearInterval` and `clearTimeout`. A
  function that names one runs on a runtime of its own, so a callback lives in the
  runtime that made it. The timers follow the node's clock, a callback has the
  same CPU limit as a message, `node.send` in a callback goes out as usual, and
  every timer is cleared when the flow stops or is redeployed. A function that
  names none of them runs as it always did.
- `require` and npm modules are still not there, and cannot be.

## Binding to a network interface

A box whose one port carries a VLAN for each machine, every VLAN with the same
subnet and the same device addresses, has one route to `192.168.1.5` for each
VLAN. A socket bound to `enp2s0.14` with `SO_BINDTODEVICE` routes and resolves
addresses on that interface and no other, so the same address reaches the right
device.

Every network client takes a **Network interface** setting: `modbus-client` and
the Modbus nodes (the `modbus-server` too, for its listening socket), `postgreSQLConfig` and `postgresql`, `e-mail`, `tcp request`, `udp out`, `http request`, and Flow's own
Modbus and PostgreSQL configs. A node uses, in this order:

1. its own **Network interface**,
2. the `FLOW_NETWORK_INTERFACE` environment variable of its tab (the tab's
   environment variables in the editor), of its subflow, or of the process,
3. its config node's **Network interface**,
4. the host's choice.

So one flow file, with one `modbus-client` and a tab for each dryer, needs one
variable on each tab and no edit to any node. A client keeps a connection for
each interface its nodes use, and each has its own lock, so a device that is
silent on one VLAN does not hold up another.

The interface has to be there. Naming one that is not gives an error that says
so, and the connection is not made some other way. A link that goes down fails
the reads on that interface with a message that names it, leaves the others
alone, and reads again when it comes back, with nothing done to the flow.

**The pod needs the host's network.** In the chart that is `network.mode: host`.
On Linux 5.7 and later an unprivileged process may bind a socket that is not yet
bound to a device, so no capability is needed. Before 5.7 it takes `CAP_NET_RAW`:
set `bindToInterface.netRaw: true` and the chart adds that one capability, and
nothing else (every other capability stays dropped). With Podman, use
`Network=host`, and `AddCapability=NET_RAW` on an old kernel.

Proven on The Beast with two namespaces that carry the same `192.168.1.0/24`
and the same device address, a Modbus simulator in each holding different values
(`testdata/vlan/lab.sh`): one Flow, two tabs, each tab bound to its interface by
its tab variable, 30 rounds of reads that each came back from the right device,
a node on one tab overridden to read the other VLAN, the same flow with no
interface set reading only one of the two (so the binding is what makes the
difference), and a link taken down and up with the read failing and then
recovering. The same lab runs two `modbus-server` nodes on `192.168.1.9:10502`, one
bound to each interface by its tab variable: a client in each VLAN reads its own
server for 20 rounds with no crosstalk, and the same two servers with no
interface set collide ("address already in use"). Not proven: a kernel older than 5.7 and 802.1Q tagging itself, which
is the switch's and the VLAN subinterfaces' job.

## Node-RED's own example flows

The Node-RED project ships example flows for its core nodes. They are the
flows people have most likely already imported somewhere, so they are the test:
`testdata/nodered/examples` holds 71 of them (Apache-2.0, from
node-red/node-red, with NOTICE), and every one imports and deploys. Each was also
run in a real Node-RED 5.0.10 with every Inject pressed, and what its Debug nodes
printed is recorded in `testdata/nodered/oracle.json` by a harness that lives in the
repo (`testdata/nodered/oracle`, with its README): it runs each flow twice on real
Node-RED and keeps what does not depend on a clock. 62 flows are recorded; all 62 give
exactly the same output here, and a test fails when one stops. Two
more flows print the machine the oracle ran on and are kept out as `environment`.

Three things in those files used to make an imported flow quietly do the wrong
thing, and are fixed: a Debug node written with `complete: "false"` (the old
spelling of "show the payload") showed `(undefined)`, an Inject written with a
bare `payload` row sent an empty payload, and a CSV node with no columns listed
refused to start. Concatenate mode of Batch with one topic listed twice panicked,
and a Switch with "recreate message sequences" on did not rebuild its sequences.

All 62 recorded flows now give Node-RED's output here, with two kinds of difference written down and
checked by the test. What the test leaves out of a message on purpose: Node-RED's `msg._error` (the Error object
itself) and the `stack` in `msg.error` name Node-RED's own source files, and Flow's `msg._errorDepth` is the hop
count that breaks an error loop, so none of them is compared; a flow that reads `msg.error.stack` gets nothing
from Flow. And one flow where Flow differs on purpose: "Link across tabs" has a Link Out naming a Link In that is
not in the file, which Node-RED drops without a word and Flow raises as an error a Catch node can see
(`link out` in docs/compatibility.md). What closed on the last pass: a Function node's `await`, a promise its body
returns and a promise its Setup returns (the first message waits for it), `node.linkcall` to a Link In by id or
name, a node's error that no Catch node took (the oracle now records those as `[error]` lines, and Flow's own error
event carries `handled`, so the debug sidebar and the test agree on which ones are uncaught), and a Trigger whose
timeout answer comes a second in (the test waits for the recorded output instead of a fixed 1.5 s). The Delay's
reset and flush, a Join in automatic mode behind a CSV or a Switch, the CSV node's `msg.columns` and `msg.reset`,
and Batch's concatenate behind a Switch give Node-RED's output on their examples too. Also partial, from the earlier list: a Delay or Rate limit per topic, a
Trigger's units, a Function node's `node.log` and `external modules`, a Join node's reduce and timeout modes, and
timing-based Batch. Flow does
not hide any of it: `hotloop-flow import` lists what a file uses that is partial.

## Flows from the Node-RED library

The library at flows.nodered.org holds what people actually built. Of the first 900 flows it lists, 35 use
only Node-RED's own nodes (no MQTT broker, no HTTP, no dashboard, no node from npm) and have an Inject and a
Debug, so they can be run on both sides. They were run on Node-RED 5.0.10 by the same harness as the examples and
the output of each compared with Flow's (`HOTLOOP_NODERED_LIBRARY=<folder> go test ./cmd/hotloop-flow -run
PublishedLibraryFlows`; the library states no license that lets this repository keep a copy, so the flows are
fetched, not committed, and the folder holds the oracle's `oracle.json` beside them). Of the 24 whose output does
not depend on a clock, 22 give what Node-RED gives, after three things were fixed that the examples had not shown: a
Function body that declares its own `const context` (or `flow`, `global`, `env`, `util`) did not compile, which
silenced the whole flow; the CSV node did not set `msg.columns` on what it sends, so a Change that adds a column and
a CSV node that writes the header from it lost every column; and a CSV node's own column list yielded to
`msg.columns`, when in Node-RED it comes first. A Function node with a syntax error is reported as a node that did
not start, which is what Node-RED logs. Two differ and are written down: one needs an npm module in its Function node
(`node-dualshock-controller`), which Flow cannot load (see `function` in compatibility.md), and one prints a prompt
longer than a Debug node's Length setting, where Flow cuts the text at the length and Node-RED cuts each long string
and array inside the message, so the two sidebars show a long message differently.
