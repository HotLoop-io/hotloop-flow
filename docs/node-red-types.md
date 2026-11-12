# Node-RED node types that run natively

A plant's flows lean on a handful of community nodes: Modbus, the dashboard,
PostgreSQL, MongoDB, e-mail. Flow runs those types itself, in Go, under
Node-RED's own type names and property names, so a flow file that uses them
loads, runs and saves unchanged. Nothing is installed, and nothing needs Node.js.

This page says what runs, how, and where it differs. The line-by-line table is
[compatibility.md](compatibility.md), generated from the registry.

## What runs

| Family | Types | Where |
|---|---|---|
| Modbus | `modbus-client`, `modbus-read`, `modbus-getter`, `modbus-flex-getter`, `modbus-write`, `modbus-flex-write`, `modbus-server`, `modbustcp-server` | [Modbus](#modbus) |
| Dashboard | `ui_base`, `ui_tab`, `ui_group`, `ui_link`, `ui_spacer`, `ui_text`, `ui_numeric`, `ui_button`, `ui_switch`, `ui_text_input`, `ui_led`, `ui_gauge`, `ui_chart`, `ui_template`, and the Dashboard 2 configuration nodes, accepted | [The dashboard](#the-dashboard) |
| PostgreSQL | `postgreSQLConfig`, `postgresql` | [Databases](#databases) |
| MongoDB | `mongodb4-client`, `mongodb4` | [Databases](#databases) |
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

## Databases

`postgreSQLConfig` and `postgresql` run SQL against PostgreSQL or TimescaleDB.
Put values in with `$1`, `$2` and `msg.params`, so the database does the quoting;
`{{{msg.x}}}` puts text straight into the query and is only for names you trust.
`msg.payload` is the rows and `msg.pgsql` is `{command, rowCount}`. With `split`
on, rows go out `rowsPerMsg` at a time with `msg.parts`.

`mongodb4-client` and `mongodb4` run `insertOne`, `insertMany`, `find`, `findOne`,
`updateOne`, `updateMany`, `replaceOne`, `deleteOne`, `deleteMany`,
`countDocuments` and `aggregate` on a collection. `find` sends one message with
the list, one per document, or the count. Documents come out as plain JSON.
Database-level operations are not provided, and a node set to one does not start.

Both open their connection on the first query, not at deploy, so a database that
is not up yet does not stop the flow. Proven against a real PostgreSQL 17 and a
real MongoDB 8.

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
the Modbus nodes, `postgreSQLConfig` and `postgresql`, `mongodb4-client` and
`mongodb4`, `e-mail`, `tcp request`, `udp out`, `http request`, and Flow's own
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
recovering. Not proven: a kernel older than 5.7 and 802.1Q tagging itself, which
is the switch's and the VLAN subinterfaces' job.
