# HotLoop Flow

**Node-RED's idea. My runtime.** A visual flow engine for the plant floor,
written in Go. One static binary, an editor that belongs to us, and a scheduler
that doesn't fall over when a sensor starts talking faster than the thing
reading it.

[![CI](https://github.com/HotLoop-io/hotloop-flow/actions/workflows/ci.yml/badge.svg)](https://github.com/HotLoop-io/hotloop-flow/actions/workflows/ci.yml)
[![Licence](https://img.shields.io/badge/licence-Apache--2.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8)](go.mod)

We shipped Node-RED in our own App Store for a year. It works, and that's the
trap, because "it works" is where everybody stops looking. So look. The image is
717 MB. One single-threaded event loop carries the runtime, the editor, the
websockets and every node's I/O, so one heavy Function node freezes all of it,
including the editor you opened to find out why. The queue between a fast sensor
and a slow database has no ceiling, so it grows until the kubelet kills the pod,
and the log doesn't tell you a damn thing. I read that image size off a registry
listing at 3 AM and opened a Go file instead of going to bed.

This is what came out. **25.1 MB.** Every queue has a ceiling, and every message
that doesn't fit is counted where Prometheus can see it. It refuses to start
without authentication. And your flows still load.

**Flow files stay Node-RED v1.** Point it at your `flows.json` and it runs, and
saving it without touching anything hands back the exact same bytes. Before you
trust that with a production line, run `hotloop-flow import flows.json`. It
sorts every node type in the file into runs, partly runs (with what's missing
spelled out) and doesn't run, so you find out at your desk instead of when a
line stops. Nobody loses a year of flows because I had opinions at three in the
morning. Everything else was fair game, and I took nearly all of it.

Current release is `2.0.5`, and `2.0.0` was the first under the HotLoop Flow
name. The image is `ghcr.io/hotloop-io/hotloop-flow:2.0.5` for amd64 and arm64,
the Helm repo is `https://hotloop.io/hotloop-flow/`. Just under 35,000 lines of
Go, a third of it tests, 51 node types, and the race detector comes back clean
on every package. Apache 2.0, the same terms for a business as for anyone, and
nothing to sign up for.

Before the rename it was Emberwire, and `v0.1.0` is still where it was, at
`ghcr.io/embernet-ai/emberwire:0.1.0`. 2.x reads nothing the old name wrote: the
`EMBERWIRE_*` variables, the two `emberwire-` database node types, a WASM module
built against the old exports and the 0.1.0 credentials file all have to be
redone. That's a clean break on purpose. Nobody had built on 0.1.0, and a shim
for zero users is just more code to get wrong. The credentials file at least
fails like an adult now. 2.0.0 died on it with a JSON parsing error. Since 2.0.1
it names the file, says Emberwire wrote it, and tells you to move it aside and
enter the credentials again.

---

## The numbers

Both runtimes in rootless podman on one box, the same five-node flow file
deployed to each **unchanged**, driven by the same load generator in the same
sitting on 2026-08-08. Linux, 12 CPUs, `nodered/node-red:latest`, 8 connections,
30 seconds.

| | HotLoop Flow | Node-RED | |
|---|---|---|---|
| Image size | **25.1 MB** | 717 MB | 29× |
| Throughput | **3,460 req/s** | 1,290 req/s | 2.7× |
| Latency p50 | **1.03 ms** | 4.61 ms | 4.5× |
| Latency p99 | **16.72 ms** | 23.14 ms | 1.4× |
| Cold start | **2.1–2.3 s** | 4.6–5.5 s | ~2.3× |

Now I walk my own numbers back, because a benchmark table with no caveats under
it is marketing in a lab coat. Three things that table does not say.

**Cold start is not really 2.3×.** About two seconds of each row is podman
bringing up a container, and both runtimes pay it. What the two programs
actually differ by is the gap between the rows, 2.3 to 3.4 seconds, not the
ratio.

**Throughput is not a scheduler benchmark.** It runs through the whole HTTP stack
on purpose, because that's the only surface both runtimes present identically.
It tells you what a client of your flow sees. It does not tell you whose
scheduler is faster, and I won't let anybody quote it that way.

**The tail is not 4.5× better.** p99 is 1.4×. Under saturation both runtimes
queue, and queueing owns the tail. The median is the one latency column where
the cost of handling a request shows up instead of the time spent waiting in
line, and that's where the gap is.

Memory used to be in that table too: 4.8 MB idle, 12.3 MB under load. I pulled
both. They came off `podman stats`, which reports what the kernel charges to the
container, not what the process holds, and nobody could reproduce 4.8 MB, me
included. On 2026-09-28 the same 2.0.4 container read 2.6 MB in `podman stats`
while the process had 14.7 MB in `VmRSS`. A number that moves more than fivefold
depending on which tool you ask doesn't get printed next to a ratio. Here's what
the process actually holds, measured on the native binary with no container:

| | HotLoop Flow |
|---|---|
| RSS, idle | **16.6 MiB** (16.5 to 16.7 over three runs) |
| RSS, peak under load | **26.6 MiB** (26.2 to 27.5 over three runs) |

That's `hotloop-flow bench -mode http -launch ./hotloop-flow -target
http://127.0.0.1:18897 -path /bench -duration 30s -warmup 5s -connections 8`,
serving [the bench flow](docs/bench/bench-flow.json), set up exactly as in
[docs/bench/](docs/bench/README.md#memory). It reads `VmRSS` two seconds after
the flow first answers, then samples it through the load and keeps the peak.
Built from `main` with `CGO_ENABLED=0 -trimpath -ldflags="-s -w"`, run on Linux
under WSL2, 24 CPUs, 2026-09-28. There's no Node-RED column because nobody has
run Node-RED through `-launch` on the same box yet, and dividing by a number
that came from a different method is exactly the mistake I just undid.

Every figure in this section came out of `hotloop-flow bench`, which ships in
this repository, so go disagree with me on your own hardware. Method, flow file,
exact commands and the rest of the caveats are in [docs/bench/](docs/bench/).
**Not one figure here came off a forum post**, and nothing comparative goes into
this file until that command produced it on one box in one run.

---

## Why I did not just keep running Node-RED

Four problems, and not one of them can be fixed with configuration. If one
could, this would be a values file, not a runtime.

**There is no back-pressure.** Node-RED puts a `setImmediate` between every wire
hop, and that queue has no ceiling. Put a fast source in front of a slow sink and
the queue grows until the pod gets OOM-killed, with nothing in the log explaining
itself. Here every inbox has a ceiling and a policy per node: block the sender,
drop the newest, drop the oldest, or raise it to a Catch node and let the flow
decide. Whichever you pick, it's counted, and a queue creeping toward its
ceiling shows up on a graph before anything gets dropped.

**It is single-threaded.** One event loop carries the runtime, the editor API,
the websocket fan-out and every node's I/O. One CPU-heavy Function node stalls
all of it, including the editor you're using to find out why it stalled. Here
every node instance gets its own goroutine, ordered within a node and parallel
across nodes, so a heavy Function node costs you a core instead of the editor
and every other flow on the box.

**The Function node's sandbox is not a boundary.** That's not my accusation,
it's Node's own documentation: [*"The `node:vm` module is not a security
mechanism. Do not use it to run untrusted code."*](https://nodejs.org/api/vm.html)
Node-RED's real trust model is that anyone who can deploy a flow already owns
the box, and honestly, fair, the `exec` node is right there in the palette. That
holds on a Pi in a workshop. It does not hold on a customer's plant floor, and
it isn't hypothetical. Pilz shipped its IndustrialPI 4 with Node-RED on it and
authentication never set up, and that became
[CVE-2025-41656](https://certvde.com/en/advisories/VDE-2025-045/): anyone who
could reach it could run commands on the device with high privileges, CVSS 10.
That's a CVE in Pilz's firmware, not in Node-RED, and I'm not going to pretend
otherwise. But no auth is Node-RED's default, a vendor shipped the default, and
it scored a perfect 10. That's why Flow won't start without auth, and why its
`exec` node ships switched off.

**Credentials are AES-256-CTR keyed by a raw SHA-256 of your secret.** CTR has
no MAC, so anyone who can write `flows_cred.json` on a shared PVC can flip bits
in the plaintext without the key, and nothing notices. If they know or can guess
what a field says, they can rewrite it to whatever they like. And plain SHA-256
is not a KDF. It's fast by design, which is exactly backwards for the one thing
standing between a stolen file and a weak passphrase, because it lets an
attacker guess as fast as their hardware can hash. No CVE on either. Both are
still real. Flow uses AES-256-GCM, which refuses a tampered file outright, and
Argon2id, which makes every single guess cost 64 MiB of memory.

---

## What is different, concretely

| | Node-RED | HotLoop Flow |
|---|---|---|
| Runtime | Node.js, one event loop | Go, goroutine per node |
| Back-pressure | none, unbounded queue | bounded inbox, four policies |
| Message cloning | first recipient aliases the sender | every recipient gets a copy |
| Function sandbox | `node:vm`, explicitly not a boundary | goja with no host bindings. A WASM host with a hard memory ceiling is built and tested, but no node uses it yet |
| `exec` node | any command, through a shell | disabled until allowlisted, and no shell at all |
| File nodes | any path the process can reach | scoped to the PVC, symlinks resolved |
| Credentials | AES-256-CTR, SHA-256 as the key | AES-256-GCM, Argon2id |
| Context API | get, set | get, set, **CompareAndSwap, Increment, Update** |
| Flow file writes | in place | temp, fsync, rename, fsync dir, three backups deep |
| Config | `settings.js`, executable JavaScript | declarative YAML |
| Auth | off by default | refuses to start without it |
| Metrics | none | Prometheus, per node |
| Editor | ~40k lines of jQuery and D3 | vanilla TypeScript, native SVG, our theme, 35.1 kB of JS and 16.7 kB of CSS, minified |
| Node definition | a `.js` plus a hand-written `.html` twin | one Go descriptor |

### Everything unbounded over there is bounded here, visibly

This table is the whole project. Node-RED's signature failure is a pod that
quietly inflates until the kubelet kills it, and the log it leaves explains
nothing to whoever is holding the pager. Every row is one of the ways that
happens, with a ceiling on it.

| | Node-RED | HotLoop Flow |
|---|---|---|
| Node inbox | unbounded | bounded, four overflow policies |
| Delay and rate-limit queue | unbounded | bounded, refused to a Catch node past the limit |
| Trigger timers | unbounded | bounded |
| `exec` output | buffered without limit | capped per stream, truncation raises an error |
| `exec` concurrency | one process per message | bounded, refused past the limit |
| File read | whole file into memory | capped, with per-line and chunked modes offered |
| HTTP request body | unbounded | capped |
| HTTP response wait | forever | 504 after a timeout |
| TCP connections and frame size | unbounded | bounded |
| WebSocket send queue | unbounded | bounded, slow client disconnected |

Every one of those is a written, documented divergence in
[docs/compatibility.md](docs/compatibility.md), not a cap I picked and never
mentioned. And nothing gets thrown away quietly: every dropped message is
counted, pushed to the editor as an event, and exported as a metric you can
alert on. When a limit bites, you find out from a graph, not from an operator.

### Cloning, and why I broke compatibility on purpose

Node-RED hands the **first** recipient on a wire the original message object and
clones only for the ones after it. It's a documented memory optimisation. It's
also how two branches that each think they own their message end up quietly
corrupting each other, and why one branch behaves differently from its
identical-looking siblings for no reason except the order you wired them in.
That's a bug that reproduces on Tuesdays.

Every recipient here gets its own copy, and that isn't free. Copying a small
message (a reading and a topic) costs 411 to 453 ns and 680 bytes, and a
five-node chain moves a message every 1.14 to 1.40 µs, copies included. Where
copying really hurts is a big binary payload: a 1 MiB buffer costs 128 to
156 µs and another megabyte on the heap for every recipient. So large binary
payloads travel as `ImmutableBytes`, which shares the buffer instead of copying
it: **250 to 279 ns and 360 bytes for the same 1 MiB message.** The file and
HTTP nodes already hand binary payloads out that way, so you get it without
asking.

Those are `go test -run=XXX -bench='Clone|ChainThroughput' -benchmem -count=5
./internal/engine/ ./internal/runtime/`, Linux under WSL2 on a 24-CPU Ryzen 9
7900X, Go 1.27.1, `main` at `3ca2ea4`, 2026-09-29.

### Atomic context operations

Node-RED's context API is get and set, and those two verbs are why a Node-RED
flow [can't run as more than one
instance](https://flowfuse.com/blog/2023/05/bringing-high-availability-to-node-red/):
two copies doing get-modify-set on a shared counter race each other, and the API
has nothing you could fix it with even if you caught it happening. Flow adds
`CompareAndSwap`, `Increment` and `Update`. The test throws 10,000 concurrent
increments at one counter under the race detector and loses none of them.

Know what that buys you today, though. The only store is in memory. Flow and
global context survive a deploy and are gone on every restart, and they aren't
shared between instances. So these are the primitives a second instance would
need, not a second instance, and a counter or a latch in your flow resets every
time the pod moves. Node-RED ships a file-backed store and Flow doesn't have one
yet. It's Phase 4 of the [roadmap](docs/ROADMAP.md).

---

## Security posture

These are the places where "anyone who can edit a flow" stops meaning "anyone
who owns the box".

**It won't start without authentication.** Not a warning in a log nobody reads,
not a default you're trusted to change: a startup error with the fix printed
next to it. CVE-2025-41656 happened because a device maker shipped Node-RED with
authentication at its default, which is off. Flow doesn't have that default, so
there's nothing to forget. Turning it off takes `HOTLOOP_FLOW_INSECURE=true` in
the environment, on purpose, because a config file can't do it on its own: the
file is where a copy-paste lands. Do it and the log warns on every boot and the
editor wears a **no login** badge, so the next person knows the door is open.

**The `exec` node ships disabled.** An operator names the commands a flow may
run, and an enabled node with an empty allowlist is a startup error, not a
licence to run anything. The allowlist matches the **resolved absolute path**,
not the string the flow typed. Match on strings and a flow asks for `curl` and
gets whichever `curl` sits first on a `PATH` that a Function node can read and a
sidecar can influence. And there's no shell. The command line is split on
quoting rules only, and an unquoted shell metacharacter (`|`, `&`, `;`, `$` and
friends) is refused rather than passed through as a literal and hoped about. So
`ping -c1 10.0.0.1; rm -rf /data` is an error, not two commands.

**The file nodes are scoped to the PVC.** Symlinks are resolved over the longest
existing prefix of the path, which closes the obvious hole: put a symlink under
the PVC that points at `/`, then read straight through it. A plain prefix check
waves that through without blinking, and now a flow can read anything the
process can.

**Credentials are AES-256-GCM with Argon2id**, so a tampered file fails to
decrypt instead of decrypting to something an attacker picked. The flow file is
written temp, fsync, rename, fsync the directory, three backups deep, so a power
cut mid-save leaves you the old file or the new one, never half of each. If it
ever reads a corrupt one, it falls back to a backup and says so out loud.

**The discovery nodes are off by default** and bounded by an operator-configured
CIDR allowlist. A hostname that resolves to one in-scope address and one
out-of-scope address is refused outright, because otherwise DNS is just the way
around the allowlist.

---

## The palette

**51 node types.** Everything Node-RED ships that is not a community node.

| Category | Nodes |
|---|---|
| Common | inject, debug, complete, catch, status, link in, link out, comment, junction |
| Function | function, switch, change, range, template, delay, trigger, exec, rbe |
| Network | mqtt in/out, http in, http response, http request, websocket in/out, tcp in/out, tcp request, udp in/out |
| Sequence | split, join, sort, batch |
| Parser | csv, html, json, xml, yaml |
| Storage | file, file in, watch, influxdb out, postgres |
| Discover | scan, netinfo, both ours, for inventorying an OT segment |
| Config | mqtt-broker, websocket-listener, websocket-client, influxdb, postgres |

By compatibility level: 10 full, 26 partial, 9 deliberately divergent, and 6 with
no Node-RED counterpart at all. Every node declares how it relates to its
Node-RED equivalent, and **a test fails the build if a node claims partial
compatibility without stating in writing what is missing.** A node that is 90%
compatible and silent about the other 10% is more dangerous than one that is
obviously absent, because the first one lets a flow appear to work. Full matrix:
[docs/compatibility.md](docs/compatibility.md), which is generated from the
registry rather than maintained by hand, because a hand-maintained compatibility
document is a lie with a timestamp.

Two nodes are ours. `scan` sweeps a CIDR range and identifies Modbus and
EtherNet/IP endpoints, and `netinfo` reports the interfaces the runtime can
actually see, which in macvlan mode is how a flow discovers its own address on
the OT VLAN.

**Node-RED community nodes do not work here.** They are npm packages that need
Node.js. There is no future version of this where they suddenly do, and I am not
going to imply otherwise to make the table look nicer. That is the trade you make
for the footprint and the sandbox, stated plainly so you can decide against it.

---

## Two sandboxes, and only one of them runs flows yet

The Function node runs on [goja](https://github.com/dop251/goja), a JavaScript
interpreter in pure Go with no host bindings unless somebody adds them, and
nobody did. No `require`, no `process`, no `Buffer`, nothing to walk out to, and
a test runs the classic constructor-chain escape to prove it reaches nothing.
That's a real boundary, which `node:vm` is documented not to be. A call costs
11.6 to 13.7 µs.

It is not a boundary against memory. A function that allocates in a loop grows
the Go heap until the pod dies, and all goja can do about it is a wall-clock
timeout. So there's a second sandbox: a WebAssembly host on
[wazero](https://wazero.io/), in `internal/wasmhost`, where linear memory has a
hard ceiling and a guest that allocates past it gets a trap while the host
carries on unbothered. That costs 184 to 196 µs a call, fifteen to sixteen
times a goja call. Worth paying exactly when you're running code you don't fully trust,
and not otherwise.

Both figures are `go test -run=XXX -bench='FunctionSimple|WasmCall' -benchmem
-count=5 ./internal/nodes/ ./internal/wasmhost/`, on the same box, commit and
day as the cloning numbers above. The WASM benchmark needs the test guest built
first, exactly as CI builds it.

**Here's the catch: no node uses the WASM host yet.** It's real, it's tested, and CI
fails if those tests quietly skip. But nothing in the palette calls it and it
isn't compiled into the binary, so today you cannot run a WASM guest in a flow.
The Function node is goja and only goja. A `wasm` node on this host is Phase 6
of the [roadmap](docs/ROADMAP.md), and until it ships, everything below
describes the host, not something you can drag onto a canvas.

When it does ship, a node can be written in Rust, TinyGo, Zig or AssemblyScript
instead of JavaScript, so the signal processing an OT flow actually wants runs
compiled, with a memory ceiling it can't argue with.

The guest ABI is three exports, two of them allocator hooks, and it's that small
on purpose. Every export is attack surface, and one more thing every guest
author has to get right.

| Export | Signature | Required |
|---|---|---|
| `hotloop_flow_process` | `(ptr i32, len i32) -> i64` | yes |
| `hotloop_flow_alloc` | `(size i32) -> i32` | yes |
| `hotloop_flow_free` | `(ptr i32, size i32)` | no |

The `i64` result packs an offset in the high 32 bits and a length in the low 32,
pointing at a JSON response in the guest's own memory. Everything crosses as
JSON, so a guest in any language can produce it without a shared schema
compiler.

The limits are small on purpose. This runs on edge hardware next to the flows
that actually matter, and a node that needs more should have to say so:

| | goja | WASM |
|---|---|---|
| Timeout per call | 5 s | 5 s |
| Memory ceiling | none enforceable | 64 MiB, hard |
| Max output | 16 MiB | 8 MiB |

Both runtimes are pure Go with no cgo. That's what keeps the build
`CGO_ENABLED=0` and the image distroless today, and keeps both true when the
WASM host goes into the binary. Pull in one cgo dependency and the static binary
and the distroless image go with it, and CI fails the build to tell you so.

---

## Subflows

Each instance gets its own copy of the template's nodes, its own flow context,
and its own resolved properties. The same template renders `4.2 bar` in one
instance and `4.2 kPa` in the next, verified end to end through a real deploy
rather than asserted in a unit test.

Expansion produces a **separate graph** rather than rewriting the parsed flows,
and that is the whole design decision. The flow file is never touched, so the
byte-identical round-trip below still holds, and the expanded graph is an
ordinary graph, so the scheduler, the Catch routing, the metrics, and the
editor's status events all work inside a subflow with no special cases anywhere.

Nesting works. An error nobody catches inside a subflow walks out to the Catch
node on the calling tab, because otherwise a subflow is just a place where errors
go to disappear. A configuration node declared inside a template is **shared** by
every instance rather than copied, since an MQTT broker inside a subflow is an
author saying "share this", and copying it would open one connection per instance
against something that is almost certainly counting them.

---

## Compatibility

`flows.json` v1 loads and saves **byte for byte**. Load a file Node-RED wrote,
save it without editing anything, and you get identical bytes back: same key
order, same spacing, no helpfully rewritten escapes. Edit one property and the
diff is one line rather than the entire node.

That is harder in Go than it sounds and I very nearly did not bother. A
JavaScript object preserves key insertion order, so Node-RED gets this for free
from `JSON.parse` and `JSON.stringify` without anybody thinking about it. A Go
map has no order at all and `encoding/json` deliberately sorts keys. Rather than
force an order-preserving map through every read path in the codebase, each
entry's original bytes are kept and re-emitted when the parsed form is unchanged,
then re-encoded against the original key order when it is not. `json.Compact` and
`json.Indent` are byte-level transforms, so they re-indent without reordering
anything.

It matters for two reasons, both boring and both real. Your flow file lives on a
PVC and in git, and it should not churn just because a pod restarted. And when an
operator reviews a deploy diff before pushing it to a line, they should see what
changed and absolutely nothing else.

A node type this build has never heard of survives a load-and-save with every
property intact, so it is safe to run a Node-RED-authored flow here and hand it
back afterwards. Node-RED's `flows_cred.json` imports read-only, and anything
read that way is re-encrypted under GCM on the next save.

Before you deploy anything, ask it what is going to happen:

```bash
hotloop-flow import flows.json
```

It reports every node type in the file, including the ones inside subflows, split
into supported, partially supported with the gap spelled out, and not supported
at all. Subflow internals are counted against the expanded graph rather than the
file, so an instance never gets reported as "supported" while staying silent
about what is inside it. That is the difference between finding out now and
finding out when a line stops.

---

## Quick start

From source:

```bash
cd web && npm install && npm run build && cd ..
go build -o hotloop-flow ./cmd/hotloop-flow

export HOTLOOP_FLOW_DATA_DIR=./data
export HOTLOOP_FLOW_ADMIN_USER=admin
export HOTLOOP_FLOW_ADMIN_PASSWORD_HASH="$(./hotloop-flow hash-password -password 'something-long')"
export HOTLOOP_FLOW_CREDENTIAL_SECRET="$(openssl rand -hex 32)"

./hotloop-flow
```

The editor build comes first and is not optional. The bundle is embedded with
`go:embed`, so a Go build without it fails on a missing pattern rather than
producing a binary with no editor in it.

Or skip all of that:

```bash
podman run --rm -p 1880:1880 -v hotloop-flow-data:/data \
  -e HOTLOOP_FLOW_ADMIN_USER=admin \
  -e HOTLOOP_FLOW_ADMIN_PASSWORD_HASH='<bcrypt hash>' \
  -e HOTLOOP_FLOW_CREDENTIAL_SECRET="$(openssl rand -hex 32)" \
  ghcr.io/hotloop-io/hotloop-flow:2.0.5
```

Generate the hash with `podman run --rm ghcr.io/hotloop-io/hotloop-flow:2.0.5
hash-password -password 'something-long'`. The image is distroless nonroot with
no shell in it, so there is nothing to `exec` into and nothing for anybody who
gets code execution to pivot with.

Then open <http://localhost:1880>. Drop a `flows.json` into the data directory
and restart, or just build the flow in the editor.

### The commands

| Command | What it does |
|---|---|
| `hotloop-flow` | Serves the runtime, the admin API, and the editor. The default. |
| `hotloop-flow -config <path>` | Same, from a YAML file. Also reads `HOTLOOP_FLOW_CONFIG`. |
| `hotloop-flow hash-password` | bcrypt hash for a password. Takes `-password` or `HOTLOOP_FLOW_PASSWORD`. Refuses anything under 8 characters. |
| `hotloop-flow import <flows.json>` | Reports what would happen before you deploy it. |
| `hotloop-flow bench` | The benchmark harness that produced the table above. |
| `hotloop-flow version` | The version. |

---

## Configuration

Declarative YAML with environment overrides, because `settings.js` is executable
JavaScript in which `adminAuth` can be a function, `https` can be a function
returning cert options, and `storageModule` can be a `require()`. You cannot
validate that, diff it, template it out of a ConfigMap, or reason about it
without running it first. Every field below is optional and shown at its default.

```yaml
server:
  host: 0.0.0.0
  port: 1880
  adminRoot: /              # where the editor and admin API live
  httpRoot: /               # where a flow's HTTP In nodes live
  readTimeout: 60s          # generous: a big deploy over a slow edge link is legitimate
  writeTimeout: 0s          # zero on purpose, the comms websocket is long-lived
  shutdownTimeout: 20s
  maxRequestBytes: 33554432 # 32 MiB, bounds a flow deploy

data:
  dir: /data                # the PVC. Flows and credentials live here. Context doesn't, it's memory only
  flowFile: flows.json
  credentialsFile: credentials.json
  credentialSecret: ""      # empty means plaintext, which is refused by default
  allowPlaintextCredentials: false
  backupGenerations: 3

auth:
  enabled: true             # off refuses to start unless HOTLOOP_FLOW_INSECURE=true
  sessionTTL: 168h
  users:
    - username: admin
      passwordHash: "$2a$10$..."   # bcrypt only, never plaintext
      permissions: ["*"]

runtime:
  inboxCapacity: 1024
  overflow: block           # block | drop-newest | drop-oldest | error
  blockTimeout: 30s
  closeTimeout: 15s

discovery:
  enabled: false
  allowedCIDRs: []          # enabled with this empty is a startup error

exec:
  enabled: false
  allowedCommands: []       # enabled with this empty is a startup error

files:
  allowedPaths: []          # extra trees on top of data.dir, which is always allowed

logging:
  level: info               # error | warn | info | debug | trace
  format: text              # text for a terminal, json for a cluster

metrics:
  enabled: true
  path: /metrics
```

A typo in that file is a startup failure rather than a setting that silently does
nothing, because unknown fields are rejected. An operator who misspells
`credentialSecret` should find out immediately, not six months later when they
notice the credential file is plaintext.

### The overflow policies

`runtime.overflow` sets the default and any node can override it. This is the
setting that does not exist over there at all.

| Policy | Behaviour |
|---|---|
| `block` | The sender waits for space, up to `blockTimeout`. Back-pressure propagates upstream, which is the correct answer almost always. |
| `drop-newest` | Discard the arriving message. Counted and announced. |
| `drop-oldest` | Discard the head of the queue to make room. Counted and announced. For "only the latest reading matters" flows. |
| `error` | Refuse the send and raise it to a Catch node, letting the flow decide. |

### Environment overrides

Environment beats the file, which is the only workable split on Kubernetes: a
Helm chart puts the boring settings in a ConfigMap and injects the secrets from a
Secret.

| Variable | Sets |
|---|---|
| `HOTLOOP_FLOW_CONFIG` | Path to the YAML file. |
| `HOTLOOP_FLOW_HOST`, `HOTLOOP_FLOW_PORT` | Listener. |
| `HOTLOOP_FLOW_ADMIN_ROOT`, `HOTLOOP_FLOW_HTTP_ROOT` | Path prefixes. |
| `HOTLOOP_FLOW_DATA_DIR`, `HOTLOOP_FLOW_FLOW_FILE` | Where state lives. |
| `HOTLOOP_FLOW_CREDENTIAL_SECRET` | Credential encryption secret. |
| `HOTLOOP_FLOW_ADMIN_USER`, `HOTLOOP_FLOW_ADMIN_PASSWORD_HASH` | A single admin account with full permissions, which is what makes a first-run container usable without mounting a file. Both must be set. |
| `HOTLOOP_FLOW_INBOX_CAPACITY`, `HOTLOOP_FLOW_OVERFLOW` | Scheduler defaults. |
| `HOTLOOP_FLOW_LOG_LEVEL`, `HOTLOOP_FLOW_LOG_FORMAT` | Logging. |
| `HOTLOOP_FLOW_DISCOVERY_ENABLED`, `HOTLOOP_FLOW_DISCOVERY_CIDRS` | Discovery nodes. Comma-separated CIDRs. |
| `HOTLOOP_FLOW_EXEC_ENABLED`, `HOTLOOP_FLOW_EXEC_ALLOWED_COMMANDS` | The exec node. Comma-separated commands. |
| `HOTLOOP_FLOW_FILE_ALLOWED_PATHS` | Extra file node roots. Comma-separated. |
| `HOTLOOP_FLOW_INSECURE` | `true` runs with authentication off, and nothing else does. Only `1`, `true`, `yes` and `on` count, so `false` or `0` can't turn it off by accident. For an isolated network you've decided to own. It doesn't waive the credential secret. |
| `HOTLOOP_FLOW_ALLOW_PLAINTEXT_CREDENTIALS` | Permits unencrypted credentials at rest. |

The on/off variables accept only `1`, `true`, `yes` and `on`, in any case.
Anything else is off, so `HOTLOOP_FLOW_ALLOW_PLAINTEXT_CREDENTIALS=0` means no,
not "it's set, so yes". A safety switch that flips on because somebody typed
`=false` is exactly the kind of thing that ends up in an incident report.

Anything not in that table is file-only, and that is deliberate: session TTL,
backup generations, request size limits, and the server timeouts are decisions
somebody should be reviewing in a ConfigMap, not typing into a shell at 3 AM
while a line is down. I have been that person. Do not let that person edit
timeouts.

---

## The admin API

Same shape as Node-RED's where a client already expects it, with real permissions
on top.

| Method and path | Permission | Notes |
|---|---|---|
| `GET /health` | none | Liveness. The kubelet carries no token, and auth on this route would restart-loop the pod forever. |
| `GET /ready` | none | Readiness, reported separately, so a runtime that failed to start leaves the Service without the kubelet killing the pod. |
| `POST /auth/token` | none | Log in. |
| `POST /auth/revoke` | none | Log out. |
| `GET /metrics` | none | Prometheus. Counts and node ids only, never message contents or configuration. |
| `GET /settings` | `settings.read` | |
| `GET /nodes` | `nodes.read` | The registry, which is what drives the editor's palette and its dialogs. |
| `GET /flows` | `flows.read` | |
| `POST /flows` | `flows.write` | Deploy. |
| `GET /runtime/stats` | `status.read` | |
| `POST /inject/{id}` | `inject.write` | Fire an Inject node. |
| `GET /comms` | `status.read` | The editor's status and debug websocket. |

`/metrics` and `/health` being unauthenticated is a decision rather than an
oversight. A Prometheus scraper carries no bearer token, so requiring one means
either handing a credential to your monitoring stack or having no monitoring, and
I have watched people pick the second one.

Permissions are `"*"` for everything, an exact string such as `flows.read`, or a
prefix grant such as `flows.*`. A read-only account for a dashboard that just
wants to render flow status is `["flows.read", "status.read"]` and nothing more.

Deploys take an optional `HotLoop-Flow-Deployment-Rev` header. Send the revision you
last read and a deploy racing another editor is rejected instead of silently
overwriting somebody's work, which is the failure mode you only find out about
from whoever lost their afternoon.

---

## Metrics

Prometheus at `/metrics`, per node, with `node` and `type` labels on everything
per-node. No exporter sidecar, because an edge box does not have room for one and
you should not need a second container to find out that your inbox is full.

| Metric | Type | What it tells you |
|---|---|---|
| `hotloop_flow_build_info` | gauge | Always 1. The version rides in the label. |
| `hotloop_flow_uptime_seconds` | gauge | Seconds since the runtime started. |
| `hotloop_flow_nodes_running` | gauge | Node instances currently running. |
| `hotloop_flow_node_messages_received_total` | counter | Messages delivered to a node. |
| `hotloop_flow_node_messages_sent_total` | counter | Messages a node has emitted. |
| `hotloop_flow_node_errors_total` | counter | Errors a node raised. |
| `hotloop_flow_node_messages_dropped_total` | counter | Messages discarded because an inbox was full. Node-RED cannot report this, because it has no bound to overflow. |
| `hotloop_flow_node_sends_blocked_total` | counter | Times a sender waited for space. Sustained back-pressure, which is the real signal that a flow cannot keep up. |
| `hotloop_flow_node_queue_length` | gauge | Messages waiting right now. |
| `hotloop_flow_node_queue_capacity` | gauge | Where the overflow policy starts applying. |
| `hotloop_flow_node_queue_high_water` | gauge | The deepest that inbox has ever been. |
| `hotloop_flow_goroutines` | gauge | Roughly one per node plus the I/O each holds. |
| `hotloop_flow_memory_heap_bytes` | gauge | Heap currently allocated. |
| `hotloop_flow_memory_sys_bytes` | gauge | Bytes taken from the OS. |
| `hotloop_flow_gc_cycles_total` | counter | Completed GC cycles. |

The one to alert on is `hotloop_flow_node_queue_high_water` against
`hotloop_flow_node_queue_capacity`. High water is the early warning that a flow is
approaching its ceiling, which arrives before anything is dropped and long before
anyone is awake. That alert is the whole reason I built the bounded inbox, and it
is the metric Node-RED structurally cannot give you: you cannot report how close
you are to a limit that does not exist.

---

## Deploying

Kubernetes, Podman with Quadlet, or a plain `podman run` to kick the tires.
[docs/deploying.md](docs/deploying.md) has all three end to end, plus backups
and the one secret you must never lose. The short version:

```bash
helm repo add hotloop-flow https://hotloop.io/hotloop-flow/
helm install line3-flows hotloop-flow/hotloop-flow
```

That's the whole install, on any cluster. On a box with no cluster, a Quadlet
unit hands the container to systemd, so it starts at boot, restarts when it dies
and logs to the journal. The unit in the doc was run exactly as written against
the published image.

Two things bite people, so they're here and not only there.

**One instance is one pod.** The chart pins `replicaCount: 1` on purpose. Flow
holds open connections to brokers and PLCs, so two pods behind one Service would
both subscribe and both write, and your historian would quietly get everything
twice. Scaling out means a second release with its own flows.

**The credential secret is forever.** It encrypts every password your flows
hold. The chart generates it on the first install and reads it back off the
existing Secret on every upgrade, because if it ever regenerates, every
credential on that volume is unreadable, and it looks like a clean upgrade until
MQTT stops authenticating. On Podman it lives in a Podman secret for the same
reason.

The pod runs distroless nonroot with a read-only root filesystem and every
capability dropped, and the chart's three network modes (`cluster`, `host`, and
`macvlan` for its own MAC and IP on the OT VLAN) are in the doc.

---

## When it refuses to start

Past the plain config errors (a YAML typo, a port out of range), it refuses to
start for five reasons, all on purpose. Every one prints the fix instead of a
stack trace, because whoever reads it is staring at a CrashLoopBackOff and
doesn't need my Go paths.

| Refusal | Fix |
|---|---|
| Authentication is disabled | Configure a user. Or, if the network really is isolated and you've decided to own that, set `HOTLOOP_FLOW_INSECURE=true` in the environment. `auth.enabled: false` in the file isn't enough on its own. (Up to and including 2.0.4 the variable did nothing and you got this refusal back regardless. Fixed in 2.0.5.) |
| Authentication is on with no users | Set `auth.users`, or `HOTLOOP_FLOW_ADMIN_USER` and `HOTLOOP_FLOW_ADMIN_PASSWORD_HASH`. |
| A `passwordHash` that is not bcrypt | Run `hotloop-flow hash-password`. This check exists so a plaintext password can never end up in a ConfigMap by accident. |
| No credential secret | Set `HOTLOOP_FLOW_CREDENTIAL_SECRET`. Or `HOTLOOP_FLOW_ALLOW_PLAINTEXT_CREDENTIALS=true` if this instance holds no secrets at all. |
| `discovery` or `exec` enabled with an empty allowlist | List what is permitted, or turn the thing off. An empty allowlist read permissively is exactly how a narrow capability becomes a shell. |

What it will *not* refuse to start over is one bad node. A flow with a single
broken node runs every other node and logs the failure, because taking a line
down over one typo in one dialog is worse than running 40 nodes out of 41 and
saying so. Same with an exec allowlist that names something not on the `PATH`
yet: it warns at boot and resolves it again when a flow uses it, since an init
container or a mounted volume can legitimately bring it in after start-up.

If the flow file won't parse, it tries the backups newest first, keeps the
corrupt file next to it as `.corrupt`, and logs exactly which backup it fell
back to. If none of them parse either, it stops there, rather than come up empty
and look healthy while every flow you had is gone. And a deploy writes the flow
file *before* it stops the old runtime, so if the save fails, your previous
flows keep running instead of the line going down over a bad save. That order is
not the obvious one, and it is not an accident.

---

## Where it stands

**It runs.** It starts, serves the API and the editor, loads flows off the PVC,
moves messages, and comes back after a restart with its flows and credentials.
Not its context (see below). The chart deploys it in all three network modes and
the whole publish chain resolves.

**Verified against real infrastructure**, not just against my own encoders:
mosquitto, InfluxDB 2.7 and PostgreSQL 16, in podman, on 2026-08-07. The
InfluxDB tag value came back out of a real database as `press 01,west` with the
space and the comma intact. That's the escaping bug that otherwise fragments a
series in silence and costs somebody a day to find. That run predates the
rename, and the only change to those nodes since is what they're called.

**Race detector clean**, every package, on Linux with cgo, on every push to
`main` and every pull request.

**Not done yet**, roughly in the order it bothers me:

- **Partial deploy.** A deploy stops and restarts every node, not just the ones
  you changed. Every MQTT connection drops and reconnects, and a Delay node
  holding messages lets them go early rather than lose them. Node-RED can
  restart only what changed. It's one of the two big runtime gaps, and until it
  closes, you'll learn not to deploy mid-shift.
- **Persistent context.** The other one. Context lives in memory only, so flow
  and global context are gone every time the process restarts, and a counter or
  a latch in a flow resets when the pod moves. Node-RED has a file-backed store.
  This has nothing yet.
- **A node that uses the WASM host.** The host is built and tested. Nothing in
  the palette calls it, so WASM guests can't run in a flow today.
- **Link Call, and Link Out's "return" mode.** Both are refused with an error
  rather than silently doing nothing. That's the right behaviour while they
  don't exist, and it's still a gap.
- **JSONata.** Every `jsonata`-typed property is refused, so a node that uses
  one errors on every message. That beats returning the expression text and
  letting a flow route on a literal string, but it's also the single most
  common thing an imported flow trips over.
- **Cron-style Inject scheduling.** Interval and on-startup injection work. "At
  06:00 on weekdays" doesn't: `crontab` is ignored, so a cron-scheduled Inject
  loads fine and never fires on its schedule. `hotloop-flow import` warns you.
  The runtime doesn't.
- **Editor click-through for the newer nodes.** The dialogs are built from each
  node's descriptor, so they render, but nobody has clicked through the HTTP,
  WebSocket, TCP or UDP ones by hand yet.
- **Multipart uploads on HTTP In**, and cookies on HTTP Response. A multipart
  body arrives as raw bytes, not `msg.files`, and `msg.cookies` does nothing,
  so set a `Set-Cookie` header instead.
- **It has never run on a real plant floor.** Shepherd Boy Farms is the intended
  first site. Everything past the deploy line is unproven in the field, and I'm
  not calling it production-hardened until a plant has had a real go at
  breaking it.

What gets built next, and in what order, is in [docs/ROADMAP.md](docs/ROADMAP.md).
Flow waits its turn behind HotLoop's own roadmap, so if one of those gaps blocks
you, plan around the gap, not around a date.

---

## Layout

```
hotloop-flow/
  cmd/hotloop-flow/     the binary, the import checker, and the benchmark harness
  internal/
    engine/          messages, property expressions, the v1 graph, subflow expansion
    node/            what a node type is: Descriptor, registry, contracts
    nodes/           the built-in palette
    runtime/         the scheduler, delivery, error and status routing
    store/           context, flow file, credentials
    js/              goja host
    wasmhost/        wazero host, tested, not used by any node yet
    api/             admin REST and the editor websocket
    config/          the YAML surface and the refusals
    metrics/         the Prometheus exposition
    flowhttp/        the route table a flow's HTTP nodes register into
    shell/           the exec node's command allowlist
    filescope/       the file nodes' path scope
    discover/        network discovery
  web/               the editor
  charts/            the Helm chart
  docs/bench/        the benchmark method and results
```

## Building

```bash
cd web && npm install && npm run build && cd ..
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o hotloop-flow ./cmd/hotloop-flow
```

Static, no cgo, runs on distroless. goja is pure Go, and so is wazero for when
the WASM host goes in, which is the only reason that sentence is true. CI fails
the build if the binary exceeds 40MiB or turns out to be dynamically linked,
because both of those are things you discover on a plant floor otherwise.

The race detector needs cgo, so run it where a C compiler exists:

```bash
CGO_ENABLED=1 go test -race -count=1 -timeout 900s ./...
```

`docs/compatibility.md` is generated and a test fails when it drifts. Regenerate
it rather than editing it:

```bash
HOTLOOP_FLOW_UPDATE_DOCS=1 go test ./internal/nodes/
```

Integration tests are skipped unless the environment points at a real broker and
a real database. The exact variables are in the header of
`internal/nodes/integration_test.go`, and the point of them is that "it works
against my own encoder" is not a claim worth making.

Run `gofmt -w ./internal ./cmd ./web` before committing. CI does not check
formatting, so that one is on you.

What CI does check, beyond vet and the race detector, is the set of things I have
personally been burned by: that `go mod tidy` is committed, that the sandbox
tests genuinely ran rather than silently skipping, that the editor bundle is
actually inside the binary, that the binary is under 40MiB and statically linked,
and a 60-second fuzz run against the property expression parser. Every one of
those exists because the alternative was a green checkmark over something broken,
and a green checkmark is worse than a red one.

Images and charts publish from **version tags only**, and both workflows refuse
anything else twice: once in the trigger, once in a step that re-checks the ref.
An artefact built from an untagged commit is one nobody can name out loud, and
something on a plant floor can end up running it. Pushing to `main` runs CI and
publishes nothing.

## Adding a node

One Go descriptor per node type, not a `.js` and a hand-written `.html` twin that
drift apart the moment somebody is in a hurry. The descriptor declares the
properties, the ports, the defaults, and the compatibility level, and the editor
renders the dialog from it, so a node cannot ship with a UI that disagrees with
its runtime.

Two rules that are enforced rather than requested. Anything less than fully
compatible must state in writing what is missing, and a test fails the build if
it does not. And nothing may silently discard data: if your node drops something,
count it, announce it, and let it show up in the metrics above. Every place where
this codebase deliberately diverges from Node-RED has a comment explaining why,
not what. Keep that up.

## The rest of HotLoop

Flow is its own product. The rest of HotLoop is one codebase shipped as four
more: **IoT** (the automation base, built for OT), **Edge** (IoT plus the
machine layer), the **Gateway** (everything, plus fleet, multi-site and reports)
and the **Edge Relay** (headless, forwards over Sparkplug B). Gateway and Edge
Relay 4.16.0 are out and pull with no login. IoT and Edge arrive in an upcoming
release. [hotloop.io/products](https://hotloop.io/products/) has the lineup.

Flow doesn't talk to any of them yet. HotLoop nodes, where every command a flow
sends goes through HotLoop's write gate and lands in its logbook, are Phase 5 of
the [roadmap](docs/ROADMAP.md). Until then they run side by side and share
nothing but a broker, if you give them one.

Those four are under the HotLoop Community License, where business use goes
through EmberNET. Flow isn't. It's Apache 2.0, the same for a business as for
anyone else, with no EmberNET sign-up, and nothing about the lineup's license
touches it. Run it at work, ship it in a product, fork it. That's what Apache
2.0 is for.

## Licence

Apache 2.0. HotLoop Flow is an independent implementation and contains no Node-RED
source. See [NOTICE](NOTICE) for the attribution, and
[docs/compatibility.md](docs/compatibility.md) for every deliberate divergence.
