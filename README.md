# HotLoop Flow

**Wire the plant. Review it like code.**

HotLoop Flow is a visual flow engine for the plant floor, written in Go. You wire
sensors, brokers, PLCs and databases together on a canvas, and it runs those
wires in one static binary small enough for a Pi, with a scheduler that doesn't
fall over when a sensor starts talking faster than the thing reading it.

[![CI](https://github.com/HotLoop-io/hotloop-flow/actions/workflows/ci.yml/badge.svg)](https://github.com/HotLoop-io/hotloop-flow/actions/workflows/ci.yml)
[![Licence](https://img.shields.io/badge/licence-Apache--2.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8)](go.mod)

What that means on a bad night:

- **Every queue has a ceiling, and every message that doesn't fit is counted.**
  A fast sensor in front of a slow historian backs up, blocks or drops by the
  policy you picked, and it shows up on a graph before it shows up as an OOM
  kill with nothing in the log.
- **Every node runs on its own.** One goroutine per node, so a Function node
  chewing on a big payload costs you a core, not the editor and every other flow
  on the box.
- **The flow file is something you can review.** It loads and saves byte for
  byte, an edit to one property is a one-line diff, and every save is atomic with
  three backups behind it. Every deploy is [a record](#every-deploy-is-a-record):
  who, when, why and the exact bytes, and any two of them
  [diff node by node](#the-diff-reads-like-a-review), in the API, on the
  command line and inside `git diff`. Any of them [rolls back](#put-it-back),
  credentials included.
- **It won't start unlocked.** No authentication is a startup error, not a
  default. The `exec` node ships switched off, the file nodes are fenced into the
  data directory, and credentials are AES-256-GCM under an Argon2id key.

It runs your existing flows unchanged, too. That's [further down](#bring-your-flows),
because it's a feature, not the point.

Current release is `2.0.5`. The image is `ghcr.io/hotloop-io/hotloop-flow:2.0.5`
for amd64 and arm64, the Helm repo is `https://hotloop.io/hotloop-flow/`, and
[docs/deploying.md](docs/deploying.md) has Kubernetes, Podman and Quadlet. About
36,000 lines of Go, more than a third of it tests, 51 node types, and the race
detector comes back clean on every package. Apache 2.0, the same terms for a
business as for anyone, and nothing to sign up for.

---

## The numbers

Absolute numbers only, each with the command that made it, so you can go
disagree with me on your own hardware. Linux under WSL2, 24 CPUs (Ryzen 9 7900X),
2026-10-02, the native binary built from `main` at `950076b` with
`CGO_ENABLED=0 -trimpath -ldflags="-s -w"`. Three runs each, and the range is
what's printed.

| | |
|---|---|
| Binary, linux/amd64, static | **22.0 MB** |
| Image, `2.0.5` amd64, as `podman image inspect` reports it | **25.4 MB** |
| Start to answering its first request | **0.05 s** |
| RSS, idle | **16.4 to 16.7 MiB** |
| RSS, peak under the HTTP load below | **26.4 to 28.5 MiB** |
| HTTP, five-node flow, 8 connections | **13.0k to 15.3k req/s** |
| HTTP latency p50 / p99 | **0.39 to 0.41 ms / 1.78 to 4.03 ms** |
| Engine, five-node chain, no I/O | **2.20 to 3.11 µs a message**, 3.9 KB allocated |

The start, memory and HTTP rows are one command against
[the bench flow](docs/bench/bench-flow.json), set up exactly as in
[docs/bench/](docs/bench/README.md#running-it):

```bash
hotloop-flow bench -mode http -launch ./hotloop-flow -target http://127.0.0.1:18897 \
  -path /bench -duration 30s -warmup 5s -connections 8
```

It starts the binary, times how long until the flow answers, reads `VmRSS` two
seconds later, then holds the load for 30 seconds sampling RSS and keeps the
peak. The engine row is `hotloop-flow bench -mode engine -chain 5 -messages
200000`, which pushes messages through five nodes with nothing else in the path.
That one peaks at 127 to 146 MiB of heap, and that's not a leak: it fires 200,000
messages in one burst, and the bounded inbox is doing its job under `block`. Feed
it at a sensor's pace and it sits where the RSS rows say.

Know what these aren't. The HTTP number runs through the whole HTTP stack on
purpose, because that's what a client of your flow sees. It isn't a scheduler
benchmark and I won't let anybody quote it as one. And under saturation every
runtime queues and queueing owns the tail, which is why p99 wanders more than
p50 between runs.

`hotloop-flow bench` ships in this repository. The method and the rest of the
caveats are in [docs/bench/](docs/bench/).

---

## Back-pressure, visibly

This is the whole project. The signature failure of a flow engine on an edge box
is a pod that quietly inflates until the kubelet kills it, and the log it leaves
explains nothing to whoever is holding the pager. So every place a message or a
byte can pile up has a ceiling, and every ceiling is a written, documented
decision in [docs/compatibility.md](docs/compatibility.md), not a cap I picked and
never mentioned.

| What can pile up | What stops it |
|---|---|
| A node's inbox | A ceiling (1024 by default) and one of four overflow policies |
| Delay and rate-limit queues | A ceiling, refused to a Catch node past it |
| Trigger timers | A ceiling |
| `exec` output | Capped per stream; truncation raises an error |
| `exec` processes | A concurrency limit, refused past it |
| File reads | Capped, with per-line and chunked modes |
| HTTP request bodies | Capped |
| Waiting on an HTTP response | A 504 after a timeout |
| TCP connections and frames | Bounded |
| WebSocket send queues | Bounded; a slow client is disconnected |

The overflow policies, set globally and overridable per node:

| Policy | What happens |
|---|---|
| `block` | The sender waits for space, up to `blockTimeout`. Back-pressure goes upstream, which is the right answer almost every time. |
| `drop-newest` | The arriving message is discarded. Counted and announced. |
| `drop-oldest` | The head of the queue goes to make room. Counted and announced. For "only the latest reading matters." |
| `error` | The send is refused and raised to a Catch node, so the flow decides. |

Nothing gets thrown away quietly. Every drop is counted, pushed to the editor as
an event, and exported as a metric you can alert on. When a limit bites, you find
out from a graph, not from an operator.

### One goroutine per node

Every node instance gets its own goroutine and its own inbox. Ordered within a
node, parallel across nodes. A Function node doing real work occupies one core
while the editor, the API, the websocket and every other flow keep moving.

### Every recipient gets its own copy

When a wire fans out, every node on the other end gets its own copy of the
message, so two branches that each think they own it can't corrupt each other,
and a branch never behaves differently from its identical-looking siblings
because of the order you wired them in. That's a bug that reproduces on Tuesdays,
and it doesn't exist here.

It isn't free. Copying a small message (a reading and a topic) costs 411 to
453 ns and 680 bytes, and a five-node chain moves a message every 1.14 to
1.40 µs, copies included. Where copying hurts is a big binary payload: a 1 MiB
buffer costs 128 to 156 µs and another megabyte on the heap for every recipient.
So large binary payloads travel as `ImmutableBytes`, which shares the buffer
instead of copying it: **250 to 279 ns and 360 bytes for the same 1 MiB
message.** The file and HTTP nodes hand binary payloads out that way already, so
you get it without asking.

Those are `go test -run=XXX -bench='Clone|ChainThroughput' -benchmem -count=5
./internal/engine/ ./internal/runtime/`, same box, Go 1.27.1, `main` at
`3ca2ea4`, 2026-09-29.

### Context that can count

Flow and global context have `get` and `set`, and they also have
`CompareAndSwap`, `Increment` and `Update`. Two branches doing get-modify-set on
one counter race each other and lose updates, and there's nothing you can do
about it with get and set alone. With `Increment` there's nothing to race. The
test throws 10,000 concurrent increments at one counter under the race detector
and loses none.

Know what that buys you today, though. The only store is in memory. Context
survives a deploy and is gone on every restart, so a counter or a latch in a flow
resets when the pod moves. A SQLite store that survives restarts is Phase 4 of
the [roadmap](docs/ROADMAP.md).

---

## Every deploy is a record

"What changed, who changed it, put it back" is the first thing anybody asks
after a line stops. Three `.bak` files answer none of it. They don't know who,
they roll over after three saves, and the one you need is always the fourth.

So every deploy writes an immutable record under `data/deployments/`: who
deployed it (from their token, never from anything the client claims), from
where, when, the note they left, the revision it replaced, the flow file byte
for byte, and the credentials as they stood after the deploy, encrypted exactly
like the credential file. Records are append-only. The log keeps the last 100 by
default (`history.retain`, and 0 keeps all of them), and it never prunes the
newest, because the newest is what's running.

```bash
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:1880/deployments
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:1880/deployments/42/flows > flows-42.json
```

A note goes in with the deploy, as `"note"` in the wrapped document
(`{"rev": ..., "flows": [...], "note": "..."}`) or as a
`HotLoop-Flow-Deployment-Note` header. The document form is there because a
browser won't put anything outside Latin-1 in a header, and people write notes
in their own language.

Two things it does that you'd only miss once they bit you:

- **A hand edit gets caught.** If the flow file on disk isn't the last thing the
  log recorded, because somebody edited it while the process was down, startup
  records it as a `baseline` entry that says so. Same on the first start with a
  flow file already there. Otherwise a hand edit quietly becomes a past nobody
  wrote down.
- **A log that can't be written doesn't stop the line.** The flow file is
  already saved by then, so refusing the deploy would leave the disk and the
  running flows disagreeing until the next restart. The deploy goes ahead, and
  the response and the log both say, in words, that this one has no record.

The API never hands out the credentials in a record, encrypted or not. They're
kept for one job, putting them back, and a history endpoint isn't a way to walk
off with them.

### The diff reads like a review

A line diff of a flow file is honest and useless. Drag a node twenty pixels and
it's a change. Change a threshold and it's a change. The reviewer can't tell
which one is about to stop a press. So Flow diffs node by node:

```
1 changed, 2 moved

Tab "Line 3" (t1)
  ~ switch "Pressure check" (sw1)
      rules[1].v: "7.5" -> "8.0"
      wire added: port 1 -> change "set alarm" (alarm1)

Moved on the canvas only
  mqtt in "press 01" (in1)
  debug "ok" (ok1)
```

Added, removed and changed, property by property with the old and new value,
which wires moved by output port, and **layout kept apart from logic**: moving,
resizing or hiding a label is "moved on the canvas", never a change, so a
tidy-up reads as a tidy-up. A credential reads as "set" or "changed", never with
its value. Edits inside a subflow show up on the subflow, and dragging one of its
ports is layout too.

Same answer everywhere, from one engine package:

- `GET /deployments/{from}/diff/{to}` compares two deployments.
- `POST /flows/diff` compares what's live with a document nobody has deployed
  yet, which is what you look at before pressing deploy.
- `hotloop-flow diff old.json new.json` on the command line. `-json` for a
  program. Exits 1 when something differs, like `diff`.
- **Inside git.** Make it git's diff driver for flow files and `git diff`,
  `git log -p` and `git show` print the semantic diff instead of a JSON hunk:

  ```bash
  git config diff.hotloop-flow.command 'hotloop-flow diff'
  echo 'flows.json diff=hotloop-flow' >> .gitattributes
  ```

  Or as a difftool:
  `git config difftool.hotloop-flow.cmd 'hotloop-flow diff "$LOCAL" "$REMOTE"'`,
  then `git difftool -t hotloop-flow`.

A flow diffed against itself is no change at all, for any document the parser
accepts. CI fuzzes that on every pull request, because a diff that invents a
change is worse than no diff.

### Put it back

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  -H "HotLoop-Flow-Deployment-Rev: $CURRENT_REV" \
  -d '{"note":"42 broke the label printer"}' \
  http://localhost:1880/deployments/41/rollback
```

A rollback deploys an old record again **as a new record**. History stays
append-only: deployment 41 isn't touched, and the rollback is its own entry that
says "rollback to deployment 41", who did it, and why. The flow file comes back
byte for byte, so it's the same revision it was then.

**The credentials come back too.** Every record carries them as they stood, so a
node you deleted returns with its broker password instead of returning and
failing to log in. That's tested against a real Mosquitto that refuses anonymous
clients: delete the subscriber, deploy, roll back, and it logs in again and
messages flow. CI runs that broker on every pull request.

It refuses three ways, before anything is written. A stale
`HotLoop-Flow-Deployment-Rev` gets the same 409 a deploy would, because a
rollback racing somebody's deploy is still a race. A deployment that retention
already removed is a 404. And a record whose credentials can't be decrypted with
the current credential secret is a 422, because flows that can't log in to
anything are a rollback that only looks like it worked.

### In the editor

**Deploy doesn't deploy.** It opens a review: the engine's diff against what's
running, drawn on the canvas and listed in words, with a box for the note. Green
is added, amber is changed, blue is only moved, and red ghosts sit where removed
nodes and wires used to be. A tab with anything changed on it gets a dot. The
second Deploy button is the one that deploys, and the note goes in the log. Edit
anything while the review is open and it closes, because a review of something
you've since changed is a lie about what you're about to push.

**The history panel** lists every deployment with who, when and the note.
*Changes* opens that deployment on a read-only canvas with what it changed drawn
on it, over the top of your working copy, which stays exactly as you left it.
*Roll back* asks why, warns you if you're about to throw away unsaved edits, and
deploys it.

**A deploy conflict shows what the other person did.** It used to be a browser
`confirm()` asking you to overwrite theirs or lose yours without saying what
theirs was. Now it says who deployed, when, their note, and their change node by
node, then lets you load theirs, overwrite with yours, or keep editing.

All of it is clicked through in a real browser on every pull request, against the
real binary with the editor embedded. Playwright is a dev dependency of
`web/e2e` only and never reaches the bundle. Writing those tests found that
double-clicking a node to open its dialog had never worked in Chromium, which is
about as embarrassing as a bug gets in a visual editor. Fixed.

### Flows as code

The repository is where flows get reviewed, so the repository should be able to
deploy them. Keep the flow file in git, set up the diff driver above so the pull
request reads like a review, and let the merge deploy it:

```bash
hotloop-flow export -o flows.json          # the live file, byte for byte, to commit
hotloop-flow deploy -file flows.json -note "PR #12: line 3 pressure check"
```

`deploy` prints the diff against what's running, the same text as everywhere
else, then deploys against the revision that diff was made from. If somebody
deploys in between, that's a 409 and a red pipeline, not a quiet overwrite.
`-expect-rev` pins it to the revision a reviewed plan was made against, and
`-dry-run` prints the diff and deploys nothing. A file with nothing to change
deploys nothing and adds nothing to the history, so a merge that didn't touch
the flows doesn't litter the log. A node that fails to start deploys and still
fails the job, because the rest of the flow is running and somebody needs to
look. `export -deployment 41` gets any deployment back out of the history.

**A pipeline never holds an admin password.** It gets an API token:

```bash
hotloop-flow token -name ci
```

prints a token, once, for the CI secret store, and the hash to give the
runtime: `HOTLOOP_FLOW_DEPLOY_TOKEN_HASH` for a deploy token, or `auth.tokens`
in the config file for anything else. The runtime only ever holds the hash, same
as a password. A deploy token can read the flows and deploy, which includes
rolling back, and nothing else: no settings, no inject, no editor. Both commands
read the token from `HOTLOOP_FLOW_TOKEN` (or `-token-file`) and the instance
from `HOTLOOP_FLOW_URL` (or `-url`), never a token on the command line, where
it would land in the process list and every CI log that echoes the command. The
history shows the token as the deployer, `token:ci`, with the note the pipeline
passed.

Credentials never travel this way. They aren't in the flow file, so a deploy
from git keeps every broker password exactly where it was.

### A git mirror, if you want one

The deployment log lives on the box. A repository is where everybody else
already looks: it has a web view, blame, backups and people who know how to read
it. Point Flow at one and every deployment becomes a commit of the flow file on
a branch only Flow writes to, **authored by whoever deployed it**, at the time
they deployed it, with their note as the message:

```yaml
history:
  git:
    url: https://git.plant.example/line3/flows.git
    branch: main              # one nobody else pushes to
    path: flows.json
    emailDomain: plant.example  # dana deploys, the commit is from dana@plant.example
```

The password is an access token from the git server, and it goes in
`HOTLOOP_FLOW_GIT_PASSWORD` from a Secret, never the file (`HOTLOOP_FLOW_GIT_URL`
and `HOTLOOP_FLOW_GIT_USERNAME` work too). A URL with a password in it is refused
at startup, so the URL can always be logged. `http://` and `https://` only: the
image has no git binary for `file://` and no keys for `ssh://`, and a mirror that
fails on every push is worse than one that refuses to start. Rollbacks and
startup baselines say what they are in the commit, and every commit carries a
`Flow-Deployment: N` trailer, which is how a restarted process, or one whose
clone got wiped, picks up exactly where the repository says it got to, without a
duplicate.

**It never holds up a deploy.** Pushing happens on its own goroutine. A git
server that is down, slow or refusing the push costs a deploy nothing: the
commits wait in a local clone under `data/git-mirror` and go out on the next
push that works, retried every minute. `GET /deployments` says how far the
mirror got and the last error, and `hotloop_flow_git_mirror_behind_deployments`
is the number to alert on. Tested against git's own smart-HTTP server behind
basic auth: three deploys make three commits by three authors, a push the server
refuses waits and catches up when it stops refusing, and a deploy with a git
server that takes ten seconds to say no returns in milliseconds.

It's pure Go (go-git), so the binary stays static. It costs about 3.8 MB of
binary, which is why it's measured and not assumed, and why the 40 MiB ceiling
in CI still has room.

---

## Security posture

The rule underneath all of this: anyone who can deploy a flow can make the box do
things, because that's what flows are for. So "who can deploy a flow" gets
answered by the operator, on purpose, and never by a default.

**It won't start without authentication.** Not a warning in a log nobody reads,
not a default you're trusted to change: a startup error with the fix printed
next to it. Turning it off takes `HOTLOOP_FLOW_INSECURE=true` in the environment,
on purpose, because a config file can't do it on its own: the file is where a
copy-paste lands. Do it and the log warns on every boot and the editor wears a
**no login** badge, so the next person knows the door is open.

**Two-factor sign-in is free and built in.** Any user turns it on from the
editor's Two-factor button: scan the QR code (or type the key) into any
authenticator app, type one code back, and from then on signing in takes the
password and the six digits. Nothing changes until that first code proves the
phone has it, so a setup abandoned half way can't lock anybody out. A code works
once: the step it came from is remembered, so a code read over somebody's
shoulder is dead the moment they use it, and so is any code older than it.
Thirty seconds of clock skew either way is allowed. Turning it off takes a code
too, so a session left open on a shared screen isn't enough to strip it off an
account. A lost phone is an account with `auth.admin` calling
`POST /auth/mfa/reset`, which lands in the audit trail with who did it to whom.
Enrollments are encrypted with the credential secret, the same way node
credentials are. Nothing about signing in safely is a paid tier here or ever
will be.

**Guessing passwords goes nowhere.** Five wrong passwords (or right passwords
with wrong codes) for one account from one address lock that account out from
that address for fifteen minutes, and twenty failures from one address across
any accounts lock the address. The account lock is per address on purpose: lock
the account everywhere and anybody who can reach the port can lock the real
operator out from across the plant. Locked attempts get a 429 with
`Retry-After` before the password is even checked, and the lock and every
attempt it turns away are in the audit trail. Behind a reverse proxy every
request has the proxy's address, so the per-address limit then applies to
everybody together; that's the safe way round to be wrong, and the numbers are
in `auth.lockout`. There's no setting that turns it off.

**Sessions survive a restart.** They used to live in memory, so every pod
reschedule and every upgrade signed everybody out, and the person who needed to
change a flow right then went looking for a password instead. They're in
`data/sessions.json` now, as SHA-256 hashes only, so the file proves a token
when one is presented and is useless to whoever copies it. A session remembers
who, not what they may do: take a permission or a user out of the config and
the next request knows.

**No token in a URL, ever.** A browser can't put an `Authorization` header on a
WebSocket handshake, so the editor's event stream used to carry its token in the
query string, which is exactly where access logs, proxies and browser history
keep things. It now offers the token as a second WebSocket subprotocol beside
`hotloop-flow`, a header nothing logs by habit, and the server answers with
`hotloop-flow`. A token in a query string doesn't count anywhere.

**Everything that matters leaves a trail.** Logins, failed logins (with whether
the name even exists, which a client never gets told but an operator should),
logouts, deploys, refused deploys, rollbacks and injects go to
`data/audit.log`, one JSON line each, synced to disk before the request is
answered. Each says who, from which address, and what: a deploy points at its
deployment record, an inject names the node. `X-Forwarded-For` is kept beside
the socket's address and never instead of it, because anybody can send that
header. `GET /audit` reads it newest first, filtered by `?event=` (`login.` for
every kind of login), `?user=`, `?since=` and `?limit=`, and it takes its own
`audit.read` permission, because being allowed to deploy doesn't mean being
allowed to read who else logged in. It rotates by size (`audit.maxBytes`,
`audit.keep`) so a busy instance can't fill its volume, and a trail that can't
be written never stops the action it was recording: it's logged and counted in
`hotloop_flow_audit_write_failures_total`, which should be zero forever.

**The `exec` node ships disabled.** An operator names the commands a flow may
run, and an enabled node with an empty allowlist is a startup error, not a
licence to run anything. The allowlist matches the **resolved absolute path**,
not the string the flow typed. Match on strings and a flow asks for `curl` and
gets whichever `curl` sits first on a `PATH` that a Function node can read and a
sidecar can influence. And there's no shell. The command line is split on
quoting rules only, and an unquoted shell metacharacter (`|`, `&`, `;`, `$` and
friends) is refused rather than passed through as a literal and hoped about. So
`ping -c1 10.0.0.1; rm -rf /data` is an error, not two commands.

**The file nodes are scoped to the data directory.** Symlinks are resolved over
the longest existing prefix of the path, which closes the obvious hole: put a
symlink under the volume that points at `/`, then read straight through it. A
plain prefix check waves that through without blinking, and now a flow can read
anything the process can.

**Credentials are AES-256-GCM, keyed with Argon2id.** GCM refuses a tampered file
outright instead of decrypting it to something an attacker picked, and Argon2id
makes every guess at a weak secret cost 64 MiB of memory. The flow file is written
temp, fsync, rename, fsync the directory, three backups deep, so a power cut
mid-save leaves you the old file or the new one, never half of each. If it ever
reads a corrupt one, it falls back to a backup and says so out loud.

**The discovery nodes are off by default** and bounded by an operator-configured
CIDR allowlist. A hostname that resolves to one in-scope address and one
out-of-scope address is refused outright, because otherwise DNS is just the way
around the allowlist.

**The Function node has nothing to reach.** It runs on
[goja](https://github.com/dop251/goja), a JavaScript interpreter in pure Go with
no host bindings unless somebody adds them, and nobody did. No `require`, no
`process`, no `Buffer`, no filesystem, no network, and a test runs the classic
constructor-chain escape to prove it reaches nothing.

---

## The palette

**51 node types.**

| Category | Nodes |
|---|---|
| Common | inject, debug, complete, catch, status, link in, link out, comment, junction |
| Function | function, switch, change, range, template, delay, trigger, exec, rbe |
| Network | mqtt in/out, http in, http response, http request, websocket in/out, tcp in/out, tcp request, udp in/out |
| Sequence | split, join, sort, batch |
| Parser | csv, html, json, xml, yaml |
| Storage | file, file in, watch, influxdb out, postgres |
| Discover | scan, netinfo, for inventorying an OT segment |
| Config | mqtt-broker, websocket-listener, websocket-client, influxdb, postgres |

`scan` sweeps a CIDR range and identifies Modbus and EtherNet/IP endpoints, and
`netinfo` reports the interfaces the runtime can actually see, which in macvlan
mode is how a flow discovers its own address on the OT VLAN. Nothing in the
palette can talk to the PLC `scan` just found yet. Modbus, OPC UA, S7, Sparkplug
B and EtherNet/IP nodes are Phase 5 of the [roadmap](docs/ROADMAP.md), and that's
the biggest hole in a product built for plants.

One Go descriptor per node type declares its properties, ports, defaults and
help text, and the editor renders the dialog from it, so a node can't ship with
a dialog that disagrees with its runtime.

---

## Two sandboxes, and only one of them runs flows yet

The Function node runs on goja (see [Security posture](#security-posture)). A call
costs 11.6 to 13.7 µs. It is not a boundary against memory: a function that
allocates in a loop grows the Go heap until the pod dies, and all goja can do
about it is a wall-clock timeout.

So there's a second sandbox: a WebAssembly host on
[wazero](https://wazero.io/), in `internal/wasmhost`, where linear memory has a
hard ceiling and a guest that allocates past it gets a trap while the host
carries on unbothered. That costs 184 to 196 µs a call, fifteen to sixteen times
a goja call. Worth paying exactly when you're running code you don't fully trust,
and not otherwise.

Both figures are `go test -run=XXX -bench='FunctionSimple|WasmCall' -benchmem
-count=5 ./internal/nodes/ ./internal/wasmhost/`, on the same box, commit and day
as the cloning numbers above. The WASM benchmark needs the test guest built first,
exactly as CI builds it.

**Here's the catch: no node uses the WASM host yet.** It's real, it's tested, and
CI fails if those tests quietly skip. But nothing in the palette calls it and it
isn't compiled into the binary, so today you cannot run a WASM guest in a flow.
The Function node is goja and only goja. A `wasm` node on this host is Phase 6 of
the [roadmap](docs/ROADMAP.md), and until it ships, everything below describes the
host, not something you can drag onto a canvas.

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
byte-identical round trip still holds, and the expanded graph is an ordinary
graph, so the scheduler, the Catch routing, the metrics, and the editor's status
events all work inside a subflow with no special cases anywhere.

Nesting works. An error nobody catches inside a subflow walks out to the Catch
node on the calling tab, because otherwise a subflow is just a place where errors
go to disappear. A configuration node declared inside a template is **shared** by
every instance rather than copied, since an MQTT broker inside a subflow is an
author saying "share this", and copying it would open one connection per instance
against something that is almost certainly counting them.

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
and restart, or just build the flow in the editor. The editor is vanilla
TypeScript and native SVG, 48.5 kB of JS and 20.1 kB of CSS minified.

### The commands

| Command | What it does |
|---|---|
| `hotloop-flow` | Serves the runtime, the admin API, and the editor. The default. |
| `hotloop-flow -config <path>` | Same, from a YAML file. Also reads `HOTLOOP_FLOW_CONFIG`. |
| `hotloop-flow hash-password` | bcrypt hash for a password. Takes `-password` or `HOTLOOP_FLOW_PASSWORD`. Refuses anything under 8 characters. |
| `hotloop-flow import <flows.json>` | Reports what would happen before you deploy it. |
| `hotloop-flow diff <old> <new>` | Node-by-node diff of two flow files. Also git's diff driver and difftool. |
| `hotloop-flow deploy -file <flows.json>` | Shows the diff against a running instance, then deploys with a note. For CI. |
| `hotloop-flow export` | The running flow file, byte for byte, or `-deployment N` from the history. |
| `hotloop-flow token` | A new API token and the hash to configure for it. |
| `hotloop-flow bench` | The benchmark harness that produced [the numbers](#the-numbers). |
| `hotloop-flow version` | The version. |

---

## Configuration

Declarative YAML with environment overrides. You can validate it, diff it,
template it out of a ConfigMap and reason about it without running it, which is
everything a config file should let you do and nothing more. Every field below is
optional and shown at its default.

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
  dir: /data                # the PVC. Flows, credentials and the deployment log live here. Context doesn't, it's memory only
  flowFile: flows.json
  credentialsFile: credentials.json
  credentialSecret: ""      # empty means plaintext, which is refused by default
  allowPlaintextCredentials: false
  backupGenerations: 3

history:
  retain: 100               # deployment records kept under data.dir/deployments; 0 keeps all
  git:
    url: ""                 # empty is off; see "A git mirror"
    branch: main
    path: flows.json
    emailDomain: flow.invalid

auth:
  enabled: true             # off refuses to start unless HOTLOOP_FLOW_INSECURE=true
  sessionTTL: 168h          # sessions live in data.dir/sessions.json, hashed, and survive restarts
  lockout:
    attempts: 5             # failures for one account from one address before it locks there
    perAddress: 20          # failures from one address, any account, before the address locks
    window: 15m
    duration: 15m
  users:
    - username: admin
      passwordHash: "$2a$10$..."   # bcrypt only, never plaintext
      permissions: ["*"]
  tokens:                   # API tokens for machines; the hash only, from: hotloop-flow token
    - name: ci
      hash: "sha256:..."
      permissions: ["flows.read", "flows.write"]

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

audit:
  maxBytes: 16777216        # data.dir/audit.log rotates at 16 MiB
  keep: 4                   # rotated files kept beside it
```

A typo in that file is a startup failure rather than a setting that silently does
nothing, because unknown fields are rejected. An operator who misspells
`credentialSecret` should find out immediately, not six months later when they
notice the credential file is plaintext.

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
| `HOTLOOP_FLOW_DEPLOY_TOKEN_HASH` | A deploy token called `deploy`, for CI: `flows.read` and `flows.write`, nothing else. The hash, never the token. |
| `HOTLOOP_FLOW_GIT_URL`, `HOTLOOP_FLOW_GIT_USERNAME`, `HOTLOOP_FLOW_GIT_PASSWORD` | The git mirror. The password (an access token) is environment only. |
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

| Method and path | Permission | Notes |
|---|---|---|
| `GET /health` | none | Liveness. The kubelet carries no token, and auth on this route would restart-loop the pod forever. |
| `GET /ready` | none | Readiness, reported separately, so a runtime that failed to start leaves the Service without the kubelet killing the pod. |
| `POST /auth/token` | none | Log in. A 429 with `Retry-After` while locked out. |
| `POST /auth/revoke` | none | Log out. |
| `GET /auth/mfa`, `POST /auth/mfa/setup`, `/confirm`, `/disable` | a signed-in user | Your own two-factor sign-in. Setup returns the key, the `otpauth://` link and the QR code. Confirm and disable take `{"code": ...}`. |
| `POST /auth/mfa/reset` | `auth.admin` | Turns off somebody else's two-factor sign-in: `{"username": ...}`. |
| `GET /metrics` | none | Prometheus. Counts and node ids only, never message contents or configuration. |
| `GET /settings` | `settings.read` | |
| `GET /nodes` | `nodes.read` | The registry, which is what drives the editor's palette and its dialogs. |
| `GET /flows` | `flows.read` | |
| `GET /flows/export` | `flows.read` | The flow file exactly as it sits on disk, for git. |
| `POST /flows` | `flows.write` | Deploy. Takes an optional note for the deployment log. |
| `GET /deployments` | `flows.read` | The deployment log, newest first, without the payloads. `?limit=N` bounds it. |
| `GET /audit` | `audit.read` | Who did what from where, newest first. `?event=`, `?user=`, `?since=`, `?limit=`. |
| `GET /deployments/{seq}` | `flows.read` | One record, with its flows parsed. Never its credentials. |
| `GET /deployments/{seq}/flows` | `flows.read` | That deployment's flow file, byte for byte. |
| `GET /deployments/{from}/diff/{to}` | `flows.read` | Node-by-node diff between two deployments, structured and as text. |
| `POST /flows/diff` | `flows.read` | The diff from what's live to the document in the body. Deploys nothing. |
| `POST /deployments/{seq}/rollback` | `flows.write` | Deploys that record's flows and credentials again as a new deployment. Takes the rev header and an optional `{"note": ...}`. |
| `GET /runtime/stats` | `status.read` | |
| `POST /inject/{id}` | `inject.write` | Fire an Inject node. |
| `GET /comms` | `status.read` | The editor's status and debug websocket. The token rides as the subprotocol `hotloop-flow.bearer.<token>`, offered beside `hotloop-flow`. |

`/metrics` and `/health` being unauthenticated is a decision rather than an
oversight. A Prometheus scraper carries no bearer token, so requiring one means
either handing a credential to your monitoring stack or having no monitoring, and
I have watched people pick the second one.

A user with two-factor sign-in on sends `"code"` with the username and password
to `POST /auth/token`. Without it the answer is a 401 with `"mfa": "required"`.

Permissions are `"*"` for everything, an exact string such as `flows.read`, or a
prefix grant such as `flows.*`. A read-only account for a dashboard that just
wants to render flow status is `["flows.read", "status.read"]` and nothing more.

Deploys take an optional `HotLoop-Flow-Deployment-Rev` header. Send the revision you
last read and a deploy racing another editor is rejected with a 409 instead of
silently overwriting somebody's work, which is the failure mode you only find out
about from whoever lost their afternoon.

A deploy restarts only what you changed. A node stays up when nothing it leans
on moved: its settings, its tab, its group, its environment, its credentials,
and every config node it names. Dragging it across the canvas or rewiring it
doesn't count, because new wires go onto the running node. So fixing a typo in
one function node no longer drops every MQTT session on the box, and a Delay
node sitting on ten minutes of readings keeps sitting on them. That's the whole
reason it exists: when every save bounces the plant, operators stop saving
during a shift, and the fix waits for Saturday.

Pick how much restarts with a `HotLoop-Flow-Deployment-Type` header. Node-RED's
`Node-RED-Deployment-Type` works too, so a deploy script you already have gets
the restart it asks for.

| Type | What restarts |
|---|---|
| `nodes` (the default) | The nodes you changed, plus anything that names a config node you changed. |
| `flows` | Every node on any flow that has a change in it. |
| `full` | Everything. Same as restarting the process. |

The response says what it did: `type`, `started`, `restarted`, `stopped` and
`unchanged`. Nothing in flight gets lost on the way. Work already queued at a
node being replaced finishes on the old instance, anything that shows up during
the swap waits for the new one, and a message headed for a node you deleted is
counted and shows up as a drop instead of vanishing.

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
| `hotloop_flow_node_messages_dropped_total` | counter | Messages discarded because an inbox was full. |
| `hotloop_flow_node_sends_blocked_total` | counter | Times a sender waited for space. Sustained back-pressure, which is the real signal that a flow cannot keep up. |
| `hotloop_flow_node_queue_length` | gauge | Messages waiting right now. |
| `hotloop_flow_node_queue_capacity` | gauge | Where the overflow policy starts applying. |
| `hotloop_flow_node_queue_high_water` | gauge | The deepest that inbox has ever been. |
| `hotloop_flow_goroutines` | gauge | Roughly one per node plus the I/O each holds. |
| `hotloop_flow_memory_heap_bytes` | gauge | Heap currently allocated. |
| `hotloop_flow_memory_sys_bytes` | gauge | Bytes taken from the OS. |
| `hotloop_flow_gc_cycles_total` | counter | Completed GC cycles. |
| `hotloop_flow_git_mirror_behind_deployments` | gauge | Deployments the git mirror's remote doesn't have yet. Only with a mirror. |
| `hotloop_flow_git_mirror_push_failures_total` | counter | Pushes to the mirror that failed and will be retried. |
| `hotloop_flow_git_mirror_pushes_total` | counter | Pushes to the mirror that succeeded. |
| `hotloop_flow_audit_write_failures_total` | counter | Audit entries that couldn't be written. Should be zero forever. |

The one to alert on is `hotloop_flow_node_queue_high_water` against
`hotloop_flow_node_queue_capacity`. High water is the early warning that a flow is
approaching its ceiling, which arrives before anything is dropped and long before
anyone is awake. That alert is the whole reason I built the bounded inbox. You
can't report how close you are to a limit that doesn't exist, so the limit had to
exist first.

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
not the obvious one, it is not an accident, and a test fails if anybody swaps it.

---

## Bring your flows

Flow reads Node-RED's v1 `flows.json` format, the flat JSON array that every
Node-RED export and flow library entry is written in. Point it at your file and
it runs.

**It loads and saves byte for byte.** Load a file, save it without editing
anything, and you get identical bytes back: same key order, same spacing, no
helpfully rewritten escapes. Edit one property and the diff is one line rather
than the entire node.

That is harder in Go than it sounds and I very nearly did not bother. A
JavaScript object preserves key insertion order, so a JavaScript runtime gets this
for free from `JSON.parse` and `JSON.stringify` without anybody thinking about it.
A Go map has no order at all and `encoding/json` deliberately sorts keys. Rather
than force an order-preserving map through every read path in the codebase, each
entry's original bytes are kept and re-emitted when the parsed form is unchanged,
then re-encoded against the original key order when it is not. `json.Compact` and
`json.Indent` are byte-level transforms, so they re-indent without reordering
anything.

It matters for two reasons, both boring and both real. Your flow file lives on a
volume and in git, and it should not churn just because a pod restarted. And when
an operator reviews a deploy diff before pushing it to a line, they should see
what changed and absolutely nothing else. That rule stays, even as tests and
types land in the file: new data goes in keys other tools ignore, and the
round-trip test covers files that carry them.

A node type this build has never heard of survives a load-and-save with every
property intact, so it's safe to run a flow here and hand it back afterwards.
Node-RED's `flows_cred.json`, which is AES-256-CTR, imports read-only, and
anything read that way is re-encrypted under GCM on the next save.

**Before you deploy anything, ask it what is going to happen:**

```bash
hotloop-flow import flows.json
```

It reports every node type in the file, including the ones inside subflows, split
into supported, partially supported with the gap spelled out, and not supported
at all. Subflow internals are counted against the expanded graph rather than the
file, so an instance never gets reported as "supported" while staying silent
about what is inside it. That is the difference between finding out at your desk
and finding out when a line stops.

By compatibility level the 51 node types are 10 full, 26 partial, 9 deliberately
divergent, and 6 of Flow's own. Every node declares how it relates to
Node-RED's node of the same name, and **a test fails the build if a node claims
partial compatibility without stating in writing what is missing.** A node
that is 90% compatible and silent about the other 10% is more dangerous than one
that is obviously absent, because the first one lets a flow appear to work. The
full matrix is [docs/compatibility.md](docs/compatibility.md), generated from the
registry rather than maintained by hand, because a hand-maintained compatibility
document is a lie with a timestamp.

The divergences are deliberate and each one is written down there: every
recipient on a fan-out gets its own copy, the `exec` node has an allowlist and no
shell, the file nodes are scoped, and everything listed under
[Back-pressure, visibly](#back-pressure-visibly) has a ceiling.

**Node-RED community nodes do not work here.** They are npm packages that need
Node.js. There is no future version of this where they suddenly do, and I am not going to
imply otherwise to make a table look nicer. That is the trade you make for the
footprint and the sandbox, stated plainly so you can decide against it. Signed
WASM plugins are Flow's answer to that, Phase 6 of the roadmap.

JSONata used to top the list of what an imported flow trips over. It runs now:
every `jsonata`-typed property evaluates, with Node-RED's `$flowContext`,
`$globalContext`, `$env` and `$clone` bound in, and CI runs it against the
published jsonata-js test suite on every pull request. 1673 of the 1686 cases
pass. The 13 that don't are pinned in a test with the reason for each, and ten
of those still raise an error, just with a different code. `$moment` is the one
Node-RED function you'll miss, and it tells you what to use instead of failing
quietly.

Cron-scheduled Inject nodes were next on that list. They fire now, and on the
same minute Node-RED's would: the crontab is read by a port of cronosjs 1.7.1,
the scheduler inside Node-RED's own Inject node, and checked against 22,620
firings cronosjs itself produced across six time zones, either side of real
clock changes. That matters at 2 AM. A shift start that lands in the hour that
goes missing in spring runs the moment the clocks jump, and the hour that
repeats in autumn runs once, not twice. The schedule also rereads the wall
clock every minute, so a box that boots with the wrong time and gets fixed by
NTP later starts firing on the right one instead of hours late.

Link Call was the last of the three. It works now, Link Out's return mode with
it: one piece of logic, "look up this batch", "check this interlock", written
once on its own tab and called from every line, with the way back kept on the
message as a stack, so a called flow can make calls of its own. A call that
never comes back raises an error after its timeout instead of hanging. A return
that turns up late still goes out, as it does in Node-RED. And a return with
nothing to return to is an error a Catch node sees, instead of the warning
Node-RED logs and nobody reads.

---

## Where it stands

**It runs.** It starts, serves the API and the editor, loads flows off the
volume, moves messages, and comes back after a restart with its flows and
credentials. Not its context (see below). The chart deploys it in all three
network modes and the whole publish chain resolves.

**Verified against real infrastructure**, not just against my own encoders:
mosquitto, InfluxDB 2.7 and PostgreSQL 16, in podman, on 2026-08-07. The
InfluxDB tag value came back out of a real database as `press 01,west` with the
space and the comma intact. That's the escaping bug that otherwise fragments a
series in silence and costs somebody a day to find. That run predates the
rename, and the only change to those nodes since is what they're called.

**Race detector clean**, every package, on Linux with cgo, on every push to
`main` and every pull request.

**Not done yet**, roughly in the order it bothers me:

- **No industrial protocol nodes.** `scan` finds a Modbus or EtherNet/IP device
  and nothing in the palette can then talk to it. Phase 5.
- **Persistent context.** Context lives in memory only, so flow and global
  context are gone every time the process restarts, and a counter or a latch in a
  flow resets when the pod moves. SQLite, Phase 4.
- **A node that uses the WASM host.** The host is built and tested. Nothing in
  the palette calls it, so WASM guests can't run in a flow today.
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

### Before it was HotLoop Flow

It was Emberwire, and `v0.1.0` is still where it was, at
`ghcr.io/embernet-ai/emberwire:0.1.0`. `2.0.0` was the first release under this
name, and 2.x reads nothing the old name wrote: the `EMBERWIRE_*` variables, the
two `emberwire-` database node types, a WASM module built against the old exports
and the 0.1.0 credentials file all have to be redone. That's a clean break on
purpose. Nobody had built on 0.1.0, and a shim for zero users is just more code
to get wrong. The credentials file at least fails like an adult now. 2.0.0 died
on it with a JSON parsing error. Since 2.0.1 it names the file, says Emberwire
wrote it, and tells you to move it aside and enter the credentials again.

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
  docs/              deploying, compatibility, the roadmap, and the benchmark method
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
a 60-second fuzz run against the property expression parser, a 30-second one
proving a flow never differs from itself, a rollback against a real broker that
refuses anonymous clients, and the editor clicked through in a real browser.
Every one of those exists because the alternative was a green checkmark over
something broken, and a green checkmark is worse than a red one.

Images and charts publish from **version tags only**, and both workflows refuse
anything else twice: once in the trigger, once in a step that re-checks the ref.
An artefact built from an untagged commit is one nobody can name out loud, and
something on a plant floor can end up running it. Pushing to `main` runs CI and
publishes nothing.

## Adding a node

One Go descriptor per node type, not a script and a hand-written dialog twin that
drift apart the moment somebody is in a hurry. The descriptor declares the
properties, the ports, the defaults, and the compatibility level, and the editor
renders the dialog from it, so a node cannot ship with a UI that disagrees with
its runtime.

Two rules that are enforced rather than requested. Anything less than fully
compatible must state in writing what is missing, and a test fails the build if
it does not. And nothing may silently discard data: if your node drops something,
count it, announce it, and let it show up in the metrics above. Every place where
this codebase deliberately diverges from Node-RED's behaviour has a comment
explaining why, not what. Keep that up.

## The rest of HotLoop

Flow is its own product, and it stays that way. The rest of HotLoop is one
codebase shipped as four more: **IoT** (the automation base, built for OT),
**Edge** (IoT plus the machine layer), the **Gateway** (everything, plus fleet,
multi-site and reports) and the **Edge Relay** (headless, forwards over Sparkplug
B). Gateway and Edge Relay 4.16.0 are out and pull with no login. IoT and Edge
arrive in an upcoming release. [hotloop.io/products](https://hotloop.io/products/)
has the lineup.

Flow doesn't talk to any of them yet, and it will never be built into them. It
runs next to them. HotLoop nodes, where Flow calls HotLoop's API from the outside
and every command a flow sends goes through HotLoop's write gate and lands in its
logbook, are Phase 5 of the [roadmap](docs/ROADMAP.md). Until then they run side
by side and share nothing but a broker, if you give them one.

Those four are under the HotLoop Community License, where business use goes
through EmberNET. Flow isn't. It's Apache 2.0, the same for a business as for
anyone else, with no EmberNET sign-up, and nothing about the lineup's license
touches it. Run it at work, ship it in a product, fork it. That's what Apache
2.0 is for.

## Licence

Apache 2.0. HotLoop Flow is an independent implementation and contains no
Node-RED source. See [NOTICE](NOTICE) for the attribution, and
[docs/compatibility.md](docs/compatibility.md) for every deliberate divergence.
