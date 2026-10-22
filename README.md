# HotLoop Flow

**Wire the plant. Review it like code.**

HotLoop Flow is a visual flow engine for the plant floor, written in Go. You wire
sensors, brokers, PLCs and databases together on a canvas, and it runs those
wires in one static binary small enough for a Pi, with a scheduler that doesn't
fall over when a sensor starts talking faster than the thing reading it.

[![CI](https://github.com/HotLoop-io/hotloop-flow/actions/workflows/ci.yml/badge.svg)](https://github.com/HotLoop-io/hotloop-flow/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-BUSL--1.1-blue)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8)](go.mod)

What that means on a bad night:

- **Every queue has a ceiling, and every message that doesn't fit is counted.**
  A fast sensor in front of a slow historian backs up, blocks or drops by the
  policy you picked, and it shows up on a graph before it shows up as an OOM
  kill with nothing in the log.
- **A historian going down for an hour doesn't cost you an hour.** Mark a wire
  durable and when the node behind it stops taking messages they go to disk in
  order, and they're still there after a `kill -9`. While it's healthy, nothing is
  written. [Store and forward](#store-and-forward)
  was proven against a real InfluxDB that I killed for ten minutes.
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

Current release is `2.2.0`. The image is `ghcr.io/hotloop-io/hotloop-flow:2.2.0`
for amd64 and arm64, the Helm repo is `https://hotloop.io/hotloop-flow/`, and
[docs/deploying.md](docs/deploying.md) has Kubernetes, Podman and Quadlet. About
36,000 lines of Go, more than a third of it tests, 79 node types, and the race
detector comes back clean on every package. Home and lab use is free, so is use by schools, nonprofits and government, and
business use is free through EmberNET, under HotLoop's Business Source License.

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
| Image, `2.2.0` amd64, as `podman image inspect` reports it | **39.1 MB** |
| Start to answering its first request | **0.05 s** |
| RSS, idle | **16.4 to 16.7 MiB** |
| RSS, peak under the HTTP load below | **26.4 to 28.5 MiB** |
| HTTP, five-node flow, 8 connections | **13.0k to 15.3k req/s** |
| HTTP latency p50 / p99 | **0.39 to 0.41 ms / 1.78 to 4.03 ms** |
| Engine, five-node chain, no I/O | **2.20 to 3.11 µs a message**, 3.9 KB allocated |

The image row is the published `2.2.0` image, read on 2026-10-20 with
`podman image inspect ghcr.io/hotloop-io/hotloop-flow:2.2.0 --format '{{.Size}}'`
(39,095,482 bytes). It was 32.1 MB at `2.1.0` and 25.4 MB at `2.0.5`. Part of
that is the license text in `/usr/share/doc/hotloop-flow/`, and 2.2.0 also added
the plant nodes, tracing and the SQLite state store. I did not split the growth
between those, so don't read all of it as any one of them. The other rows are
still the 2026-10-02 build.

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

### On a Raspberry Pi 5

The same flow and the same commands, 2026-10-22, on a Raspberry Pi 5 Model B
(8 GB, four Cortex-A76 cores, Ubuntu 26.04.1, kernel `7.0.0-1020-raspi`) with
its clock capped at 2.0 GHz, running the published `2.2.0` arm64 image as a pod
with nothing else on the node but cluster DaemonSets. Three runs, range printed.

| | |
|---|---|
| Start to answering its first request | **0.10 to 0.46 s** |
| RSS, idle | **88.0 to 88.3 MiB** |
| RSS, peak under the HTTP load | **40.3 to 40.5 MiB** |
| HTTP, five-node flow, 8 connections, 30 s | **13.2k to 13.4k req/s** |
| HTTP latency p50 / p99 | **0.48 ms / 2.72 to 2.78 ms** |
| Engine, five-node chain, no I/O | **7.74 to 8.15 µs a message**, 4.0 KB allocated |
| HTTP, the same command held for 600 s | **11.4k req/s**, p50 0.58 ms, p99 3.00 ms |

The last row is the honest one for a box that stays loaded. The board hit its
soft temperature limit under a minute in, throttled from about two minutes in,
and sat at 83 to 86 C with the clock at 1.5 to 1.7 GHz for the rest. The
idle RSS is about five times the x86 figure above and I don't know why yet.
The process, the board, the throttle readings, and what I didn't establish are
in [docs/bench/](docs/bench/README.md#results-on-a-raspberry-pi-5).

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

And it survives a restart. Flow, global and node context live in a SQLite file
on the volume, loaded when a scope is first used and written behind, so a counter
or a latch in a flow is where you left it when the pod comes back. The atomic
operations are the same code as before. Changes are batched into one write every
thirty seconds and a value set to what it already was writes nothing, because I
care what this does to an SD card more than I care about being clever. A clean
stop writes everything; a `kill -9` loses up to those thirty seconds, and
`store.context.flushInterval` is the knob. Details, and what it costs a card, are
in [docs/durable](docs/durable/README.md).

## Store and forward

Mark a wire durable and, while the node on the other end keeps up, it is an
ordinary wire and writes nothing to disk. The moment that node stops taking
messages, the wire spills to the same SQLite file before the sender moves on, and
a pump delivers them oldest first, one at a time, until the target takes each
one. So the historian goes down, the readings pile up on disk instead of blocking
the flow or getting dropped, and they go in, in order, when it comes back. The
price of writing nothing while it's healthy is that a `kill -9` then loses what
was in memory, about one poll's worth, and I'd rather say that here than have you
find out.

```json
{ "id": "count", "type": "function", "ew_durable": ["influx"], "wires": [["influx", "reply"]] }
```

You don't have to write the key by hand. Double-click the wire in the editor and
tick *Durable*.

It survives a restart, keeps order, honors a disk quota (256 MiB by default, and
the overflow policy of the node on the other end decides what gives when it's
full), and shows its depth as `hotloop_flow_durable_wire_depth`. It's
at-least-once, not exactly-once, and I'd rather tell you that here than have you
find out in a postmortem: the exact words, and how `influxdb out` stays correct
anyway, are in [docs/durable](docs/durable/README.md).

The proof is `docs/durable/store-and-forward.sh`: a real InfluxDB 2.9.1 in Podman
under twenty readings a second, killed for ten minutes, brought back, with the
runtime killed with `kill -9` once mid-outage and once mid-drain. 15,982 readings
accepted, 15,982 rows stored, none missing, none doubled. A healthy durable wire
writes nothing of its own, and with context flushed every thirty seconds the
whole process wrote 1.7 MiB an hour at twenty messages a second (0.04 GB a day).
The tables and the commands are in [docs/durable](docs/durable/README.md).

### A message the target refuses forever

By default a wire keeps retrying a message its target fails, with a pause that
grows to five seconds, and nothing behind that message moves. For a historian
that's down that's exactly right. For one malformed reading the historian will
never accept it's a wire stuck for good, so there's a way out, and it's off
until you ask for it.

```json
{ "id": "count", "type": "function", "ew_durable": ["influx"], "ew_durable_attempts": { "influx": 20 }, "wires": [["influx", "reply"]] }
```

Give the wire a number of attempts, in the editor's wire dialog or as above, and
after the target has refused one message that many times from the queue the wire
raises it to the flow as an error from the node that refused it, with the message
attached the way any node error carries it, and moves on to the next. A Catch
node on that flow gets it and can park it, alert on it or write it somewhere.
With no Catch the error still shows in the debug sidebar and the message is gone,
so wire one. The first refusal, in memory, reaches the Catch as it always did, so
a Catch sees the same message twice: once when it first failed and once when the
wire gave up. The count is per message and in memory, so a restart starts it
over. `hotloop_flow_durable_wire_given_up_total` counts them, beside the depth.
A limit that isn't a whole number of 1 or more is reported and the wire carries
on retrying, because ignoring it quietly would leave a wire stuck that somebody
had told to stop.

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

**Copy, cut and paste work like you'd expect, and they speak the flow file.**
Select nodes, Ctrl+C, and what's on your clipboard is the same JSON every other
flow tool reads, so you can paste it into a chat, a ticket, or another box. Paste
puts it where your pointer is, with new ids every time, so pasting twice gives
you two copies and never a collision. A wire to something you didn't copy is cut
rather than left pointing at nothing, and a configuration node the nodes use
comes along. If that configuration node is already in the flow you are pasting into, the copy
shares it instead of opening a second connection to something that counts them.
Credentials never go on the clipboard. A password in a paste buffer is how it ends
up in a bug report.

**Import and Export** do the same for a selection, one flow, or everything, with
a box to copy from, a Download button, and a file picker on the way in. Garbage
gets a reason ("the id a appears twice", "entry 1 has no id") and the dialog
stays open. A node type this runtime doesn't have comes in grey and dashed, and
the import tells you which ones, because a silently dropped node is worse than a
loud one.

**Subflows are edited on a canvas of their own.** Press *+ subflow*, and it opens
with its input and output ports on it. Wire them to the nodes inside like
anything else. *Subflow properties* sets the name, the colour, how many outputs
it has, and the properties an instance can set. Instances on flows follow when
the shape changes, and the palette lists every subflow so you can drop one.
Double-click an instance to give it its own name and its own value for each
property. A subflow can't be placed inside itself or inside one that holds it,
and one still in use can't be deleted. The editor says so rather than letting the
next deploy find out.

**Selection to subflow.** Select nodes and press *Selection to subflow*. They
move into a subflow of their own and an instance takes their place. Every wire
that crossed the edge of the selection becomes a port: the one way in is the
subflow's input, and each output of a selected node that went somewhere else is
an output of the subflow, so the flow does what it did. A subflow has one input,
so if the selection is wired into two different nodes it says so and changes
nothing, because feeding both from both would be a different flow. Select a
group and it comes along with everything in it. A configuration node that
belongs to the flow and that only the selection uses goes into the subflow with
it, along with whatever it points at. One that something left behind also uses
goes global instead, so the two ends keep sharing the one connection, because a
second broker connection on a box that counts them is the kind of surprise I do
not ship. Environment variables the moved nodes read (typed as env, as `${NAME}`
or `$(NAME)` in a string, or `env.get`) and that the flow declares become
properties of the subflow that forward to the flow, so the value still comes
from where it did and the instance can override it. Ctrl+Z undoes the lot.

**Flows.** Double-click a tab, or press *Flow properties*, to rename it or
delete it. Deleting asks, takes every node on the flow with it, and cuts the
wires and links other flows had into it so the file isn't left pointing at
nothing. It takes the deleted nodes out of any Catch, Status or Complete scope
that named them, too. A Catch or Status whose whole list was deleted keeps its
last id, since an empty list means the whole flow and I won't flip a node from
watching one thing to watching everything behind your back. Ctrl+Z brings it
back until you deploy. The last flow can't be deleted, because a runtime with no
flow has nowhere to put a node.

**Configuration nodes** are in the palette under *configuration*. They sit on no
canvas, so you double-click the type, fill in the dialog, and it's added to the
list under *configuration in use*, where a double-click edits it and the cross
deletes it. One a node still uses won't delete, and says how many do. That's
where "Add one from the palette first" in a node's dialog sends you.

**Deleting a wire.** Alt-click it. A plain click does nothing, so brushing past
a wire while you select can't break a flow. Ctrl+Z puts it back.

**Durable wires.** Double-click a wire. Tick *Durable* and it's drawn heavier and
blue, and the node it leaves gets `ew_durable` in the file. The mark is on the
pair of nodes, so it holds for every wire between them. It's wiring to the
runtime, so marking one never restarts the node.

**Groups.** Ctrl+G wraps the selection, Ctrl+Shift+G takes it apart. Double-click
the title bar to name and colour it. Drag a group and everything in it goes with
it. Drag a node into one and it joins, drag it out and it leaves. Groups nest.
Deleting a group deletes what's in it, and Ctrl+Z puts it all back.

**Search** (Ctrl+F, or the button) looks in every flow and subflow at once: names,
types, ids, and every setting, including a line in the middle of a function body.
A hit takes you to the right tab with the node selected and in the middle of the
view. Configuration nodes show up too, and their hit opens their dialog, since
they have no place on a canvas.

**Debug filtering.** Narrow the sidebar to one flow, to the nodes you've got
selected, to a word, or to just the errors. It applies to what's already in the
log, not only to what arrives next, so narrowing it never makes the line you were
looking at disappear.

All of it is clicked through in a real browser on every pull request, against the
real binary with the editor embedded. That includes every node dialog the runtime
offers: the list comes from `GET /nodes`, so a node added next month is in the
test next month without anyone remembering to add it. Playwright is a dev
dependency of `web/e2e` only and never reaches the bundle. Writing those tests
found two things that had never worked in Chromium: double-clicking a node to open
its dialog, and dragging a wire onto a node. The canvas holds the pointer for the
whole drag, so the event's target was always the canvas and a wire let go over a
node never found the node. Both about as embarrassing as bugs get in a visual
editor. Both fixed.

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

## Tests next to the flows

"Did my change break the line?" should get answered before the deploy, by the
person making the change, not at 3 AM by the person on call. So a flow can have
tests, and they're YAML that lives next to the flow file and goes through the
same review:

```yaml
# line3.test.yaml
flows: line3.json            # relative to this file; flows.json if you leave it out
tests:
  - name: a high reading raises the alarm and nothing else
    inject:
      - node: reading                  # by name or by id
        msg: {topic: line3/temp, payload: 92.5}
    expect:
      - node: over limit?              # the Switch
        port: 1
        msg: {payload: 92.5}
        within: 500ms
      - node: over limit?
        port: 2
        nothing: true
      - node: alarm out                # a Debug: checked on what it received
        msg: {payload: HIGH}

  - name: a negative reading is caught, not alarmed on
    inject:
      - node: reading
        msg: {payload: -4}
    expect:
      - node: errors                   # a Catch node
        error: "out of range"
```

```bash
hotloop-flow test -junit report.xml line3.test.yaml
```

```
--- PASS: a high reading raises the alarm and nothing else (0.00s)
--- PASS: a negative reading is caught, not alarmed on (0.00s)
ok	line3.test.yaml	2 passed, 0 failed, 0 errors (0.00s)
```

That's the real runtime and the real nodes, in the process, with no server and
no editor. Every test gets a runtime of its own, so nothing one test leaves in a
Delay or a context can leak into the next.

- **`inject`** puts messages in, in order, each one handled before the next goes
  in. A node with an input gets it there. A source like an Inject or an MQTT In
  sends it out of its first output as if it had produced it, and an Inject node
  given no message gets its button pressed.
- **`expect`** watches a node's output, counting ports from 1 the way the editor
  does. A node with no outputs, like a Debug, is checked on what it received. A
  subflow instance is watched on its own outputs, wherever inside it they come
  from.
- **`msg`** is what the message has to contain. Every property you give has to be
  there with that value, all the way down, and anything else on the message is
  ignored, so a test about the payload doesn't break when somebody starts setting
  `msg.topic`. **`assert`** is a JSONata expression that has to come out true,
  for the checks a literal can't make. **`error`** is text the error a Catch node
  caught has to contain.
- **`within`** is a deadline. A message that turns up late fails, and the failure
  says how late. **`nothing: true`** and **`count: N`** are checked once the flow
  has gone quiet, which is the only time "nothing came out" means anything. Two
  expectations on one port are two messages, in that order.

A test ends as soon as everything it waits for has happened and the flow has
nothing left to do. The `timeout` is 5 seconds unless you say, on the test's
own clock, which is the next section.

A failure says what came out instead and where it differs:

```
--- FAIL: the alarm says LOW (0.00s)
    expect 1: debug "alarm out" (alarm-out): 1 message(s) received and none matched; the first one differs at msg.payload: wanted "LOW", got "HIGH"
      at 0s: {"payload":"HIGH","topic":"line3/temp"}
```

A test that can't run is an error, not a pass: a node it names that isn't in the
flow, a name two nodes share, a port the node hasn't got, or a node in the flow
that won't start. So is a misspelt key. `expcet:` is refused when the file is
read, because a typo that was quietly skipped would leave a test with nothing to
check, and a test with nothing to check passes. Exit status 1 for anything
short of all passing, and `-junit` writes the report every CI system reads.

### On the test's clock

Nobody runs a test suite that waits out a five-minute Delay, so a test doesn't
wait. It keeps its own clock and moves it straight to the next thing anything in
the flow is waiting for: a Delay letting go, a Trigger's second message, an
Inject on an interval or a crontab, a Join or a Batch timing out, a Link Call
giving up. The timer fires, the flow handles whatever it sent, and only then does
the clock look for what's due next, the same order the real minutes would have
taken. Five minutes of Delay is one step and about a millisecond.

```yaml
  - name: a reading at 4m pushes the watchdog's off out to 9m
    clock: 2026-10-05T06:00:00Z     # when the test starts; the real time if you leave it out
    timeout: 20m
    inject:
      - {node: watchdog, msg: {payload: reading}}
      - {node: watchdog, msg: {payload: reading}, at: 4m}   # at: on the test's clock
    expect:
      - {node: watchdog, msg: {payload: "on"}}
      - {node: watchdog, msg: {payload: "off"}, within: 9m}

  - name: and not a moment before
    timeout: 8m59s
    inject:
      - {node: watchdog, msg: {payload: reading}}
      - {node: watchdog, msg: {payload: reading}, at: 4m}
    expect:
      - {node: watchdog, count: 1}  # the "on", and nothing else yet
```

**`at`** puts an injection at a moment on the test's clock, and everything due
before it happens first. **`within`** and **`timeout`** are on that clock too, so
`timeout: 1h` on a flow with an Inject every ten minutes is six ticks and a few
milliseconds. **`clock`** sets when the test starts, for a flow that cares what
time it is: a crontab that fires at half past six fires when the test's clock
says half past six. A Function node's `Date`, an Inject's timestamp, a Change
node setting a date and a cookie's `Expires` all read the same clock, so a flow
that stamps and compares times agrees with itself under a test.

The suite proves every timing node in the palette against it, to the millisecond:
Delay in its fixed, random, rate and timed modes, Trigger with extend, Inject on
an interval, once at startup and on a crontab, Join and Batch timeouts and a Link
Call that never gets its answer. All of it, an hour of plant time included, runs
in well under a second. What a test can't move is the CPU: a flow still busy
after a minute of real time fails the test rather than hanging the suite. JSONata
`$now()` and `$millis()` still read the wall clock.

### Tests from the editor

Nobody writes a test for a flow that works. They write one the day it stops
working, which is a day late. So the editor makes one out of what the flow just
did, in two clicks.

Every message in the debug sidebar has a **Test** button. Click it and the
editor drafts a test from that very message: what came in at the source it
entered by, put in there again, and what this Debug node showed, expected
here. Change anything you like, because a reading that carries a timestamp is
better checked with an `assert` than pinned to the millisecond. Click **Add to
tests** and it's in the suite beside the flow file, at the end, with whatever
was already written there left exactly as it was, comments included. A name the
suite already has gets a number on the end rather than replacing anything.

How the editor knows what came in: the runtime keeps the last 64 messages that
entered the flow at a source and the last 64 that reached a Debug node, under
the id a message carries from one node to the next. That's a ring with a size
cap, and a message over 64 KB isn't kept at all, so it can never be the thing
that grows a pod. A message that's been pushed out, or that a node replaced
with a new one on the way, can't be drafted, and the editor says so instead of
guessing what went in.

The **Tests** panel runs the suite against the working copy, deployed or not,
which is the point: you find out before the deploy. Every node a passing test
went through gets a tick, every node a failure is about gets a cross with the
reason on hover, and clicking a failure takes you to its node. Edit anything and
the marks go, because they describe a flow that no longer exists. **Edit** opens
the whole suite as YAML. A deploy the [gate](#the-deploy-gate) refuses lands in
the same panel, failures on the nodes.

All of it runs in a real browser against the real binary on every pull request.

### The deploy gate

Tests on somebody's laptop prove the flow they had on their laptop. The gate
proves the one being deployed. Keep the suite on the instance, next to the flow
file it tests (`data/flows.test.yaml` beside `data/flows.json`, through
`PUT /tests` or the editor), turn on `tests.gate`, and every deploy runs it
against the flows it's about to put in place. One that doesn't pass is refused,
a 422 with the whole report:

```
$ hotloop-flow deploy -file flows.json -note "raise the alarm at 95"
1 changed

Tab "Line 3" (t1)
  ~ switch "over limit?" (chk)
      rules[0].v: "80" -> "95"
hotloop-flow: the deploy was refused: 1 of 2 flow tests didn't pass, and the flows already running are untouched; "a high reading raises the alarm and nothing else": expect 1: switch "over limit?" (chk): nothing sent from port 1

--- FAIL: a high reading raises the alarm and nothing else (0.00s)
    expect 1: switch "over limit?" (chk): nothing sent from port 1
    expect 2: switch "over limit?" (chk): expected nothing, and 1 message(s) sent from port 2
      at 0s: {"payload":92.5,"topic":"line3/temp"}
    expect 3: debug "alarm out" (alarm-out): nothing received
--- PASS: a negative reading is caught, not alarmed on (0.00s)
FAIL	flows.test.yaml	1 passed, 1 failed, 0 errors (0.00s)
```

That's a real run: the binary with the gate on, the suite from the top of this
section kept beside its flow file, and a deploy token.

**The flows already running don't notice.** The gate runs before a byte is
written: no flow file, no credentials, no deployment record, no restart. The
refusal goes in the audit trail as a `deploy.refused` that says the tests failed.
And the tests don't run in the server process at all. They run in a child, this
same binary as `hotloop-flow test -stdin`, handed the candidate flows and the
suite on stdin, because a test starts every node in the flow and some of them
register where the whole process shares them: a Link In by its id, an HTTP route
by its path. Run in here, the candidate's Link In would answer for the running
one's the moment it started. In a child it can't touch anything the line uses, a
node that panics on a goroutine of its own takes down the test and not the
plant, and the memory goes back when it exits. The test proves it the hard way:
the refused candidate has the same Link In, and after the refusal a message
through the running flow's link still arrives.

Three decisions worth knowing. **It's off by default**, because a gate an
operator didn't ask for is a deploy that fails at 3 AM for a reason nobody on
shift has heard of. **Tests that can't run don't pass**: a child that won't
start or a run longer than `tests.timeout` refuses the deploy and says which.
And **rollbacks aren't gated.** A rollback is how somebody gets out of a bad
state, to one that already ran, and holding it up on today's tests is how an
outage gets longer. With the gate on and no suite there's nothing to fail, so
the deploy goes and the log says why it wasn't checked.

### Context and credentials

Every test starts with empty context, whatever the test before it left behind. A
counter one test pushed to 42 is not where the next one starts, because a suite
where the second test only passes when the first ran first is a suite that lies
the day somebody runs one test on its own. So a test that needs a counter at 41
says so, and says what context has to hold when it's done:

```yaml
  - name: picks up where the line left off
    context:                        # what the test starts with
      global: {limit: 90}
      flow: {Line 3: {count: 41}}   # flow context, by tab label or id
    credentials:                    # by node name or id; never the real ones
      historian: {token: test-token}
      plc api: {password: s3cret}
    inject: [{node: count, msg: {payload: 95}}]
    expect:
      - {node: out, msg: {count: 42}}
      - context:                    # what it holds at the end
          global: {alarm: true}
          flow: {Line 3: {count: 42}}
          node: {count: {last: 95}} # node context, by node name or id
```

`context` in an expectation is matched like `msg`, and `null` means the key must
not be set. A failure says what the scope does hold, since the usual answer is a
typo: `flow context of "Line 3" (t1): cuont: not set; it holds count`.

A test never sees the real credentials. It runs with the ones it names, which
is what makes a test safe to commit, and they reach a node exactly as the real
ones would: an HTTP request's password shows up in the `Authorization` header it
would have sent, and an InfluxDB configuration that won't start without a token
starts with the test's. A node that reads a password from a mounted file through
`ew_credentialFiles` gets the test's credential for that field instead, because
a test reads nothing off the disk, and an empty one when the test names none.

Context lives in memory for a test, never in the state database, so a test can't
read what the running flows left behind or leave anything for them. A fixture is
YAML in the test, not a file pretending to be a database.

### Nothing leaves the test

A test that ran your MQTT Out for real would publish to the plant's broker, and
at that point it isn't a test, it's a deploy. So under a test, every node that
talks to the world outside the process gets a stand-in instead: MQTT in and out,
HTTP in, response and request, PostgreSQL, InfluxDB, TCP and UDP in, out and
request, WebSocket in and out and the listener and client behind them, the file
nodes, `exec`, `scan` and `netinfo`.

The node still does its whole job right up to the wire. The MQTT Out still works
out the topic, the QoS and the bytes; the HTTP Request still renders the URL,
builds the body and follows redirects; the PostgreSQL node still builds the SQL
and its parameters. Then it hands that to the stand-in, which records it, and
answers with whatever the test scripted. Unscripted, the answer is nothing and
success: no rows, an empty 200, a command that exited 0. The sources (MQTT In,
HTTP In, TCP and UDP In, WebSocket In, Watch) don't subscribe, listen or poll,
so what arrives is what the test injects.

```yaml
  - name: the limit comes from the database and the alarm goes to the broker
    inject:
      - node: reading
        msg: {topic: "3", payload: 92.5}
    replies:
      - node: lookup limit                   # a PostgreSQL query
        reply: {rows: [{limit_c: 90}]}
      - node: maintenance api                # an HTTP Request
        reply: {statusCode: 503, payload: {error: down for maintenance}}
      - node: historian                      # InfluxDB, and it's down
        error: connection refused
    expect:
      - node: lookup limit
        sent: {query: "select limit_c from limits where line = $1", params: ["3"]}
      - node: to the plant                   # an MQTT Out
        sent: {topic: line3/alarm, payload: HIGH, qos: 1, retain: true}
      - node: errors
        error: connection refused
```

**`sent`** is what the node would have sent, matched like `msg`. **`replies`**
script what comes back: the first reply listed for a node answers its first
call, the next its second, and the last keeps answering, so "it answered, then
it stopped" is two lines. **`error`** makes the call fail the way the real
thing does, so the path your flow takes when the historian is down gets tested
before the historian is.

| Node | `sent` | `reply` |
|---|---|---|
| MQTT Out | `topic`, `payload`, `qos`, `retain` | |
| HTTP Request | `method`, `url`, `headers`, `payload` | `statusCode` (200), `headers`, `payload` |
| HTTP Response | `statusCode`, `headers`, `payload` | |
| PostgreSQL | `query`, `params` | `rows` for a query, `rowCount` for an insert |
| InfluxDB | `line`, the line protocol | |
| TCP Out, UDP Out | `host`, `port`, `payload` (`session` when replying) | |
| TCP Request | `host`, `port`, `payload` | `payload`, read the way the node reads a real reply |
| WebSocket Out | `payload` (`session` when replying) | |
| File | `filename`, `action`, `payload` | |
| File In | `filename` | `payload`, the contents |
| exec | `command`, `args` | `stdout`, `stderr`, `code` (0) |
| scan | `range`, `ports` | `devices` |
| netinfo | | `interfaces` |

A few things a test doesn't check, on purpose, because they belong to the box
and not the flow: the `exec` allowlist, the path and discovery scopes, and the
certificates in a `tls-config`. Nothing runs, opens, probes or dials under a
test, so there's nothing for them to fence in or encrypt, and the deploy still
answers to all of them on the box it lands on.

The proof is a test that points every one of those nodes at a TCP port, a UDP
port and a directory the test holds, runs a reading through all of them, checks
what each would have sent byte for byte, and then counts: zero connections, zero
datagrams, no files written, nothing executed. A node that reaches outside and
has no stand-in is refused by name before anything runs, and the palette test
fails if anybody adds one.

## Tracing and replay

A value goes wrong at 2 AM and the debug sidebar tells you the value. It does
not tell you which of the nine nodes it went through changed it, how long the
slow one sat on it, or what it looked like going in. You find that out by
sprinkling Debug nodes through the flow, deploying, waiting for it to happen
again, and taking them out. I've done that on a live line. Never again.

So every message that enters the flow gets a trace. Every debug message in the
sidebar has a **Trace** button, and one click lights the path that message took
on the canvas: the nodes it went through, the wires it travelled, and
everything it never touched dimmed. Each node on the path says how long it had
the message (`8.3 µs`, or `8.3 µs ×3` when it handled three pieces of it), and
the Trace panel beside it lists every hop in order with the message as that
node was handed it. Click a row and you're at the node. A hop that failed is
red, with the error under it.

It follows the message where it actually goes:

- **Through a split.** One reading becomes three, and all three are on the
  trace, each its own hop at every node after the split.
- **Through a join.** The hop after a join has every message the join took in
  for it as a parent, not just the one that happened to arrive last. The three
  branches that fed it are all lit.
- **Through a subflow.** The nodes inside run as copies with ids of their own,
  and the hops are written down under those ids. The canvas lights the subflow
  instance, because that's the node on the tab you're looking at.
- **Into a Catch.** The node that failed has the error on its hop, and the Catch
  that got it is the next hop, marked as reached by an error.

The trace rides on the message but isn't in it. No flow can read it or break it
with `msg = {...}`, and it never shows up in a Debug node, a test or a payload on
the wire.

### It can't grow

This runs on the line it's watching, so every direction is bounded, and the
limits are `trace:` in the [configuration](#configuration):

| | Default |
|---|---|
| Traces kept, newest | 256 |
| Hops one trace keeps, so a loop can't write itself down forever | 512 |
| Biggest message a hop keeps a copy of | 16 KiB |
| Everything together, roughly counted, oldest forgotten first | 16 MiB |
| New traces started in a second | 1,000 |

A message over 16 KiB keeps its timing and is marked as too big to show, and can't
be replayed or turned into a test. Past 1,000 new traces a second the rest of
that second's messages go untraced, because a sensor talking a hundred thousand
times a second would otherwise pay for a trace on every one of them. A message
that was skipped, or whose trace was pushed out while it was still on its way,
stays untraced for the rest of its trip; starting a new trace halfway would only
fill the buffer with the tails of journeys whose heads are gone. All of it is
counted on `/metrics`, so you can see when it's happening.

The soak: **a million messages through five nodes, 2026-10-18**, same machine as
[the numbers above](#the-numbers). Heap after a forced GC, sampled every hundred
thousand messages:

| | Heap, every checkpoint | Messages a second |
|---|---|---|
| Tracing off | **1.3 MiB** | 192,596 |
| Tracing every message (`perSecond: 0`) | **2.7 MiB** | 115,770 |
| Default limits | **4.0 MiB** | 195,199 |

It's the same number at all ten checkpoints. The tracer held 808,448 bytes in 256
traces from the first checkpoint to the last, forgot 999,744 of the million as it
went, and never went near its 16 MiB. Tracing every message does cost: it took
this chain from 192k a second to 116k. That's why the default limit exists. At
1,000 traces a second the cost is lost in the noise, and below that a trace is
about 3 KB. If you want every message anyway, set `perSecond: 0` and
read the first row of that table before you decide. Reproduce it with:

```bash
HOTLOOP_FLOW_SOAK_MESSAGES=1000000 go test ./internal/runtime/ \
  -run TestTraceMemoryStaysFlatUnderASoak -v
```

It runs a smaller soak in every `go test`, and CI runs the full million on every
pull request. If the heap stops being flat the test fails.

### Replay it into a draft

You found the bad value, you think you know the fix, and you've changed a node
on the canvas. Don't deploy to find out. **Replay into draft** takes the message
that started the trace, puts it into the flow as it is on your canvas right now,
deployed or not, and tells you where it ends up compared with where it ended up
the first time.

"Ends up" means the places the message stopped: a Debug node, an MQTT Out, a
Switch that matched nothing, a Join still waiting. For each one the comparison
says it's the same, or which properties changed (`payload: "HIGH" -> "CRITICAL"`),
or that the original ended there and the replay didn't, or the other way round.
The replay's own path is lit on the canvas, so you can see where the draft
sent it. **Back to the original trace** puts the first one back.

It runs the way a [flow test](#tests-next-to-the-flows) runs, in a child process
with the real runtime and the real nodes, and for the same reason: your draft
starts every node in it and nothing in it can touch the plant. That means a node
that would have called a web service answers with nothing, so a flow whose
output depends on an answer shows up as a difference. That's the honest result.
It will not pretend to have talked to a PLC it never talked to.

### A trace becomes a test

**Make test** in the Trace panel drafts a [flow test](#tests-from-the-editor)
from the whole trace: the message that started it, put in at the source it came
in at, and every place it ended that a test can watch, with what that node was
handed expected there. It's the same two clicks as a test from a debug message,
and it's better for the awkward flows, because a trace remembers every end a
message reached. A split into three Debug nodes is three expectations, where one
debug message was one.

It says why when it can't. A trace that started inside a subflow, from a
source's second output, or from a message too big to keep can't be put back in
the way a test needs. And a message that was dropped by a Switch ends nowhere a
test can watch, so it's refused rather than written as a test with nothing in it,
because a test with nothing to check passes.

### OpenTelemetry

Point it at a collector and every hop goes out as a span:

```yaml
trace:
  otlp:
    endpoint: http://collector:4318   # OTLP over HTTP; /v1/traces is added
    headers: {x-api-key: ...}         # if the collector wants one
    serviceName: hotloop-flow
```

A trace is an OpenTelemetry trace and a hop is a span, with the hop that sent
the message as its parent. A join's span has the first as its parent and the rest
as links. Each span carries the node id, the node type, the message id, how many
messages the node sent on, and the output port it was reached by, and an error
is a span with an error status. **The messages never leave.** A payload is a
plant's data and a collector is somebody else's server, so what goes out is the
shape of the journey and its timing.

It speaks OTLP/HTTP with the JSON encoding the spec defines, written out by hand
in one file, rather than pulling in the OpenTelemetry SDK with its protobuf and
gRPC to do the same single POST. The binary has a size budget and I'm not
spending it on that. A hand-written client has to be proved the only way that
counts, so CI
starts the stock `otel/opentelemetry-collector` image, a pinned release, sends
real traces through real splits, joins, subflows and a failing Function, and
reads back what the collector says it received. Every span id, parent, link,
name, attribute and error status has to match. There's no mock in that test
anywhere. If the collector is down, the export drops spans and counts them and
the flow never notices; `hotloop_flow_otlp_spans_failed_total` going above zero
is how you find out.

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

**Send the token in a header, not a URL.** A browser can't put an `Authorization`
header on a WebSocket handshake, so the editor's event stream used to carry its
token in the query string, which is exactly where access logs, proxies and
browser history keep things. It now offers the token as a second WebSocket
subprotocol beside `hotloop-flow`, `hotloop-flow.bearer.<token>`, a header
nothing logs by habit, and the server answers with `hotloop-flow`. A script that
still sends `?access_token=` keeps working, because breaking it for an upgrade
would be worse, and the server logs a warning on every such connection that says
what to send instead. Flow never writes the URL, or the token in it, to its own
log or the audit trail.

**Everything that matters leaves a trail.** Logins, failed logins (with whether
the name even exists, which a client never gets told but an operator should),
logouts, deploys, refused deploys, rollbacks, injects, MCP tool calls and saves
of the flow tests go to
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
license to run anything. The allowlist matches the **resolved absolute path**,
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

**Secrets get their own scope, and it isn't the file nodes'.** A tls-config reads
its certificate, key and CA through `secrets.allowedPaths`: the data directory
plus whatever you list. Mount the Secret cert-manager made there and the TLS
config uses the key while no flow can read it, because anything read through
that scope ends up in a handshake, never in a message. Put the same key under
`files.allowedPaths` instead and any flow with a File In node can pick it up and
mail it to whoever it likes.

**A password can live in a Secret instead of the flow.** Any node's credential
can name a file instead of holding the value:

```json
{"id": "b1", "type": "mqtt-broker", "broker": "plc-broker", "user": "line3",
 "ew_credentialFiles": {"password": "/etc/hotloop-flow/secrets/plc-broker/password"}}
```

The file is read through the same secret scope when the node starts. One
trailing newline comes off, because that's what `echo` leaves behind and a
password with a newline on the end fails a login with nothing in the log to say
why. The flow file, the deployment log, the diff and a git mirror carry the path
and never the password, so the flow can go in git and the password stays where
whoever runs the cluster already keeps passwords. A file that's missing or
outside the scope fails that node's deploy instead of letting it connect with no
password. The chart mounts Secrets for you under `secrets.mounts`. A rotated
Secret takes effect on the next deploy or restart.

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

**79 node types.**

| Category | Nodes |
|---|---|
| Common | inject, debug, complete, catch, status, link in, link out, comment, junction |
| Function | function, switch, change, range, template, delay, trigger, exec, rbe |
| Network | mqtt in/out, http in, http response, http request, websocket in/out, tcp in/out, tcp request, udp in/out, modbus read/write, s7 read/write, ethernetip read/write, opcua read/write/subscribe, sparkplug in/out/command, hotloop in/state/script/recipe, mcp tool/result/client |
| Sequence | split, join, sort, batch |
| Parser | csv, html, json, xml, yaml |
| Storage | file, file in, watch, influxdb out, postgres |
| Discover | scan, netinfo, for inventorying an OT segment |
| Config | mqtt-broker, websocket-listener, websocket-client, influxdb, postgres, modbus, s7, ethernetip, opcua, sparkplug, hotloop, mcp-server |

`scan` sweeps a CIDR range and identifies Modbus and EtherNet/IP endpoints, and
`netinfo` reports the interfaces the runtime can actually see, which in macvlan
mode is how a flow discovers its own address on the OT VLAN. Modbus TCP, S7,
EtherNet/IP, OPC UA and Sparkplug B nodes can talk to what `scan` finds now, and
they're the next section.

One Go descriptor per node type declares its properties, ports, defaults and
help text, and the editor renders the dialog from it, so a node can't ship with
a dialog that disagrees with its runtime.

---

## The plant floor

`scan` could find a PLC and nothing in the palette could talk to it, which is a
funny thing to ship in a product for plants. Modbus TCP, S7, EtherNet/IP, OPC UA
and Sparkplug B are in now.
Each one is a config node that holds the connection to the device and action
nodes that point at it, same shape as the InfluxDB node, and the editor builds
every dialog from the descriptor like everything else.

They sit on open source libraries: [simonvetter/modbus](https://github.com/simonvetter/modbus)
v1.6.4 (MIT), [gopcua/opcua](https://github.com/gopcua/opcua) v0.9.1 (MIT),
[robinson/gos7](https://github.com/robinson/gos7) (BSD-3-Clause, no tagged
releases, so it's pinned to the latest commit, and carried in `third_party/gos7`
with one line changed, `third_party/gos7/HOTLOOP_PATCH.txt` says which) and
[danomagnum/gologix](https://github.com/danomagnum/gologix) v0.44.0-beta (MIT).
Nothing is copied out of HotLoop's drivers, which are a different license and stay
that way.

**Modbus TCP** is `modbus read` and `modbus write` on a `hotloop-flow-modbus`
server. Holding registers, input registers, coils and discrete inputs. Registers
read as uint16, int16, uint32, int32, float32, uint64, int64 or float64, with the
byte order and word order as switches, because the two devices on your line will
not agree on either and the manual will not say. A quantity is values, not
registers, so two float32 is four registers on the wire. Addresses are zero
based as they go on the wire, and the dialog and the error both say out loud that
register 40001 in a manual is address 0.

**S7** is `s7 read` and `s7 write` on a `hotloop-flow-s7` server, over ISO on TCP.
gos7 is a Go implementation of the protocol and has none of Snap7's C library in
it, which matters because Snap7 is LGPL and I'm not linking that into a product I ship. You
write the address the way Siemens does: `DB1.DBX0.0`, `DB1.DBW4`,
`M10.2`, `MW20`, `I0.0`, `QB4`, with `E` and `A` for the German spellings. The
size letter has to agree with the data type, so `DB1.DBW0` read as a float32 is
refused before it goes anywhere, with the address you meant. A quantity of 3 on a
bit is three bits on from the one you named. A string is an S7 STRING: the write
reads its maximum length first and refuses text that won't fit instead of
writing past it. A bit is written with the protocol's own bit write, which
changes that bit and leaves the other seven in the byte alone. An S7-1200 or
S7-1500 says no to all of it until PUT/GET is permitted and the data block's
Optimized block access is off, and the errors say so, because that is the thing
everybody trips on.

**EtherNet/IP** is `ethernetip read` and `ethernetip write` on a
`hotloop-flow-ethernetip` controller, tag access on ControlLogix, CompactLogix
and Micro800. You name the tag the way Logix spells it, with members
(`Pump1.Speed`), array elements (`Samples[4]`) and bits (`Status.3`), and the data
type it holds. A quantity above 1 moves that many array elements from the one
named. A tag read as the wrong type is refused and the error says to use the
tag's own. The path to the CPU is `1,0` unless you change it, and empty for a
controller you reach directly. PLC-5, SLC and MicroLogix speak PCCC and not CIP
tags, and they're out. Yes, the library is tagged beta. It's the newest release
there is, and I'd rather tell you than leave it out.

**OPC UA** is `opcua read`, `opcua write` and `opcua subscribe` on a
`hotloop-flow-opcua` server. Security mode None, Sign or SignAndEncrypt with the
Basic256Sha256 or Aes128_Sha256_RsaOaep policy, anonymous or a username and
password. A client certificate comes from a `tls-config` (it needs an
application URI in the subject alternative name), and without one the node makes
a throwaway certificate each time the flow starts and logs a warning, because
the server then has to be told to trust a new one every time. If you set a
server thumbprint the node refuses any other certificate. If you don't, it
accepts whatever the server shows, which encrypts the conversation and proves
nothing about who you're talking to. Pin it.

**Sparkplug B** is `sparkplug out`, `sparkplug in` and `sparkplug command` on a
`hotloop-flow-sparkplug` connection, which is either an edge node or a host
application, because each has a will message and an MQTT connection has one. As
an edge node, the connection is born when it connects, publishes each
`sparkplug out` node as the edge node's own metrics or as one device's, dies
with its will if it's cut off (with a new bdSeq in the next birth, so a host can
tell the sessions apart), says NDEATH before a clean disconnect, and answers a
rebirth request. Every metric has to be declared on the node, because a birth
lists everything before any data is sent. `sparkplug in` on an edge node is how a
flow acts on a command. As a host application it publishes STATE the way the
spec asks, with the will, the retain flag and the timestamp that has to match,
then follows every edge node it can see: names and aliases from the births,
datatypes from the births, sequence numbers checked, a death marking what the
birth listed as stale, and a rebirth request when a gap isn't filled inside the
reorder timeout or data shows up from a node it never saw born. `sparkplug
command` sends an NCMD or a DCMD, typed from the birth and by alias when the
birth gave one. MQTT 3.1.1 through paho.mqtt.golang. The wire format is written
by hand from the Eclipse schema and the 3.0 specification, and tested against
that schema compiled with Google's protobuf, which is a different implementation
from mine. The schema is EPL-2.0, so it isn't in this repository: CI fetches it
pinned to a commit and a hash. It was also run against a real Mosquitto, and by
hand against pysparkplug, in both directions, edge node and host.

An edge node can name a **primary host application**. It subscribes to that
host's STATE and says nothing, no NBIRTH and no DBIRTH, until the host says it
is online. A death certificate older than the last online STATE is some old
session's and gets ignored. A current one gets an NDEATH, a disconnect and a
start over. A list of **other brokers** is where it starts over: one at a time,
round and round, moving on when a broker can't be reached, when the host is
offline there, and, with more than one broker, when the host never published
there at all. With one broker an offline host just keeps the edge node waiting,
connected, until it isn't. The edge node is on one broker at a time, as the spec
says. The array types, **DataSet**, **Template** and a metric's **PropertySet**
are carried both ways: arrays in the packed little endian form the spec gives,
a DataSet as `{columns, types, rows}`, a Template as a definition or an instance
with its parameters and members, and properties on any metric as
`{"engUnit": "degC", "Quality": {"type": "Int32", "value": 192}}`. A Template
instance whose definition isn't in the NBIRTH is refused at birth with what to
declare. The Eclipse schema reads every one of them off the wire, and its own
bytes read back the other way. Not done: a metric's metadata (files and multi-part
bytes), a host application that talks to more than one broker, and MQTT 5. It
hasn't met Ignition's MQTT Engine.

**When the device goes quiet.** This is what a plant floor does all day, so it
is tested the ugly way, against the real simulators with a line in between that
swallows the traffic (a hung PLC) or slams the door (a switched-off one). A
read or write that gets no answer fails inside the timeout, says so in words
(`no answer from 10.20.1.15:502 within 3s. The device is silent. Check that it
is powered...`), drops the connection and opens a fresh one on the next message.
An OPC UA subscription notices on its own, turns its badge red, raises one error
for a Catch node per outage and not one per retry, waits 1 second and then
longer up to 30, and picks the subscription back up when the server returns. No
redeploy. Errors name the thing to change: the Modbus exception code and what it
usually means, the OPC UA status code and which setting it points at, the S7
refusal that means PUT/GET is off, a refused connection versus a silent one. An
OPC UA server that takes the connection and never answers endpoint discovery costs
the node that asked its timeout and nobody else anything: the other nodes on that
server, and a flow that is being stopped, don't queue behind it.

**HotLoop** is `hotloop in`, `hotloop state`, `hotloop script` and `hotloop
recipe` on a `hotloop-flow-hotloop` connection, which is a URL and an account.
Flow runs next to HotLoop and calls its HTTP API from the outside, nothing
else, and a flow that never picks a HotLoop never makes a call. The connection
signs in with `POST /api/auth/login`, sends the bearer token HotLoop hands back,
and signs in again when HotLoop says the session is gone. An operator account
is enough for all four nodes and for the gated write below. Leave the username
empty only for a HotLoop that runs with no login at all.

`hotloop in` follows HotLoop's live feed (`GET /api/stream`) and sends a message
for each entity that changes state: `msg.topic` is the entity id, `msg.payload`
the new state, and `msg.hotloop` the entity id, the state it had before, its
attributes, its quality and its timestamps. It follows the entity ids and
patterns you list (`sensor.tank_*`, or `*`). The picture HotLoop sends when the
node connects isn't a change, so it isn't sent, unless you turn on "Send the
current state when it connects". An attribute that moved without the state
changing isn't a change either. If the link drops, the badge goes red, one error
reaches a Catch node for the outage and not one per retry, the wait grows from 1
second to 30, and when it is back an entity that changed in the meantime is sent
and one that didn't isn't. HotLoop sends every state as text, so "Numbers as
numbers" turns `12.5` into a number and leaves `on` and `unknown` alone.

`hotloop state` reads one entity when a message arrives
(`GET /api/states/{entity_id}`) and sends the message on with the same fields.
An entity that doesn't exist, or is disabled, raises an error with HotLoop's own
words, which say which.

`hotloop script` runs a script (`POST /api/scripts/{id}/run`) with the fields
you give it, and `hotloop recipe` applies a recipe
(`POST /api/recipes/{id}/apply`) with its parameters and, if you want one, an
older version on purpose. Both are HotLoop's own: it checks every field and
parameter and never moves a value to fit, runs every write through its write
gate, and keeps the record. Flow sends the reason `flow:<node id>`, with your
own words after it if the node has a reason, and HotLoop records that beside the
run or the application, in its write audit and in its logbook, under the account
Flow signed in as. A script waits for its run to end unless you turn that off,
and the message goes on with the run, trace included, only if it succeeded. A
recipe goes on only when every target was written, with each value's
`verification`. Anything else arrives at a Catch node with HotLoop's reason and
its status: a script that is busy, a field out of range, a run that failed at
step 2, a recipe refused whole because one target isn't armed, or one that
stopped halfway, which says how many targets were written. Nothing that was
written is undone, because that is HotLoop's rule too.

**Writes change equipment, and there is no ceremony, unless HotLoop is there.**
I looked for a confirmation pattern in Flow to follow and there isn't one for
equipment. The guards that exist bound what a node may reach
(`discovery.allowedCIDRs` for `scan`, the file and secret scopes), and nothing
asks "are you sure" before a message does something. So the write nodes don't
either. A write node is a deliberate thing someone placed on a canvas and put
through the same deploy record as everything else, and the message arriving at
its output means the device acknowledged it.

When HotLoop is on the network, the equipment is its to guard. Every equipment
write node (`modbus write`, `opcua write`, `s7 write` and `ethernetip write`)
has **Write through HotLoop**, empty by default, and **HotLoop tag**, the tag in
HotLoop this write is for, written `device/tag`. With a HotLoop picked, the
value goes to `POST /api/write` and not to the PLC. HotLoop's write gate decides,
in its order: that writes are switched on, that the tag is armed, that the value
is in the tag's range. HotLoop's own address, data type and scale for that tag
apply, so the address and data type on the Flow node are not used. The node's
equipment connection stays picked, so clearing the HotLoop puts the node back
exactly as it was. A write the gate refuses never reaches the equipment and
arrives at a Catch node with the gate's words (`HotLoop's gate refused the write
to plc-1/sp: value is outside the tag's configured range: 120 is outside 0..95
(HTTP 422)`). A HotLoop that can't be reached fails the write too. It never falls
back to writing directly, because that would be a write the gate never saw. The
gate takes one bool, number or text per write, so a list of registers is
refused, with a message saying so.

**HotLoop is tested against the real thing.** The published HotLoop IoT 4.18.10
image runs in CI with a login and writes on, next to the Modbus server below.
The tests make a device, tags, a script, a recipe and an operator account
through HotLoop's own API, then check both ends: what HotLoop said, and what
the simulated PLC holds afterwards. A write through the gate lands in the
register. One outside the tag's range, one to a tag that isn't armed, one to a
tag HotLoop doesn't have, and one with HotLoop unreachable all fail with the
gate's words and leave the register where it was. The script and the recipe run
and write through the gate, a field out of range is refused, `hotloop state`
reads what HotLoop holds, and `hotloop in` sends the change a gated write made,
with the state it had before. HotLoop's write audit and logbook were checked for
the account and the reason. Signing in again after HotLoop forgets the session is
tested, and so is a refused login. The reconnect and the diffing of the feed are
tested against a small stand-in for HotLoop that answers in the documented
shapes, because dropping a real HotLoop's connection on cue isn't something I
can do. Not done: HotLoop records the account Flow signs in as, and `flow:<node
id>` goes in as the reason, because the API gives a client no way to name its own
actor. The connection trusts the system's certificate roots and nothing else, so
an `https` HotLoop with a private CA isn't covered. No node calls a HotLoop
service directly, like `switch.turn_on`: a script or a recipe does that, with
HotLoop's checks.

**Tested against** Microsoft's OPC PLC (`opc-plc` 2.15.11) with anonymous and
username logins across None, Sign and SignAndEncrypt, and against pymodbus
through `oitc/modbus-server` 2.3.0, with real exception codes coming back. S7 is
tested against the server in python-snap7 3.2.1 (MIT), written in pure Python and
a different implementation of the protocol from gos7. EtherNet/IP is tested
against `ab_server` from libplctag 2.7.2, the simulator libplctag runs its own
tests on, and the values Flow wrote were read back with libplctag's own client as
well. Both are built from `testdata/plant`, run as separate containers, and
nothing from either is linked into Flow (libplctag is MPL-2.0 or LGPL). CI starts
all four and fails if a test skips instead of running.

Neither the S7 nor the EtherNet/IP nodes have met a Siemens or Rockwell box yet.
The python-snap7 server doesn't do a bit write the way a CPU does (it
overwrites the whole byte), so the bit write is checked on the wire, request by
request, and not by reading the byte back. `ab_server` matches tag names with
their case and Logix doesn't, and gologix sends them lower case, so the
simulator's tags are lower case.

**Aes256_Sha256_RsaPss isn't in the OPC UA dropdown, and the fault is in
gopcua.** v0.9.1 pads RSA-OAEP-SHA256 with 130 bytes where the number is 66
(two hash lengths of 32, plus 2), so the secure channel request is cut into
blocks the .NET stack under OPC PLC doesn't expect, and it answers
`BadSecurityChecksFailed`. I changed that one constant in a copy of gopcua and
the same client connected to the same OPC PLC in Sign and in SignAndEncrypt. The
fix is two open pull requests on gopcua (#915 and #931) and no release has it, so
the policy stays out, and a test fails the day a gopcua update carries it.
Basic256 and Basic128Rsa15 are deprecated and the simulator doesn't offer them,
so they're out too. A setting I can't test doesn't get a dropdown entry.

Not done: no Modbus RTU, no Modbus over TLS, no OPC UA events, history or method
calls, no browse, no S7 timers or counters, no PCCC controllers, and 64 bit
integers past 2^53 come out as a string of their exact digits because a flow's
numbers can't hold them.

---

## MCP, in both directions

An agent that speaks the Model Context Protocol can call a flow as a tool, and a
flow can call a tool on any MCP server, HotLoop's own included. Three nodes and
one config node do it: `mcp tool` and `mcp result` serve a flow, `mcp client` on
an `hotloop-flow-mcp-server` connection calls out. If you only ever wire
sensors to dashboards you can skip all of this: a flow that uses none of these
nodes never makes an MCP call, and Flow serves no tools it wasn't given.

The protocol is the Go SDK's (`github.com/modelcontextprotocol/go-sdk` v1.8.0,
Apache-2.0 with some files still MIT while the project finishes relicensing,
maintained by the MCP project). It speaks the current revision of the
specification, 2026-07-28, which has no handshake and carries the version on
every request, and the handshake revisions through 2025-11-25, on the same
endpoint. A client gets whichever it opens with, so nothing needs configuring.

### Flows as tools

An `mcp tool` node declares a tool: a name, a description for the model that
reads it, and an input schema. While the flow is deployed Flow serves it at
`/mcp` (under the admin root, if you moved it). A call becomes a message out of
the node, `msg.payload` is the call's arguments, `msg.topic` is the tool name,
and `msg.mcp` holds `tool`, `caller` and `callId`. An `mcp result` node
somewhere downstream sends the answer back, the way `http response` answers
`http in`. Everything between them is a flow like any other.

```json
[
  {"id": "t1", "type": "tab", "label": "Line"},
  {"id": "tool", "type": "mcp tool", "z": "t1", "toolName": "line_status",
   "description": "Status of a production line: its state and speed.",
   "schema": "{\"type\":\"object\",\"properties\":{\"line\":{\"type\":\"string\",\"enum\":[\"L1\",\"L2\",\"L3\"]}},\"required\":[\"line\"],\"additionalProperties\":false}",
   "x": 100, "y": 100, "wires": [["status"]]},
  {"id": "status", "type": "function", "z": "t1", "name": "status", "outputs": 1,
   "func": "msg.payload = {line: msg.payload.line, state: 'running', askedBy: msg.mcp.caller};\nreturn msg;",
   "x": 300, "y": 100, "wires": [["answer"]]},
  {"id": "answer", "type": "mcp result", "z": "t1", "x": 500, "y": 100, "wires": []}
]
```

**The schema is checked before the flow runs.** It is JSON Schema, read the way
the `json` node reads one (draft 2020-12 unless the schema names another, and a
`$ref` to another URL is an error). A schema that is not valid, a root that is
not an object, a tool name MCP doesn't allow, or two nodes serving the same name
fail that node's deploy, with the reason. A call whose arguments don't fit is
answered as a tool error that names the bad field, `arguments.line: value must
be one of "L1", "L2", "L3"`, so the model can fix it and try again, and the flow
never sees it. An object payload goes back as an object and as its JSON text,
text goes back as text, and an `mcp result` fed by a Catch node, or with "The
work failed" ticked, answers as a tool error with the error's own words. A call
nothing answers is told so after the reply timeout, 30 seconds unless you set
it, and the error goes to a Catch node. A redeploy waits for calls in flight.

**An agent is a caller like any other.** `/mcp` takes the same tokens the admin
API takes, a session token or an API token, and nothing else. There is no
anonymous call: no token is a 401, a token Flow doesn't know is a 401, and a
token with no `mcp.call` permission is a 403, before the protocol sees the
request. An instance running with `HOTLOOP_FLOW_INSECURE` answers 403 to every
`/mcp` request, because a tool call has to have a caller to put in the audit
log. A token for an agent is a token like any other:

```yaml
auth:
  tokens:
    - name: agent
      hash: sha256:...          # from: hotloop-flow token -name agent
      permissions: ["mcp.call"]
      rateLimit: {calls: 60, per: 1m}   # optional; no rateLimit, no limit
```

The call runs with that identity's rights. Every caller needs `mcp.call` (or
`mcp.*`, or `*`). Put another permission in a tool's "Extra permission" box, say
`mcp.line3`, and a caller also needs that one, and doesn't see the tool in the
list without it. Two agents can share a Flow and see different tools.

**A token can be held to a number of calls.** `rateLimit` on an API token in
`auth.tokens` is `calls` in a window, `per`, one minute if you leave it out. The
window starts with the token's first call, a call over the number is refused
until it ends, and then the count starts again. A call that is refused never
reaches the flow, and is answered as a tool error that says how many seconds to
wait, so an agent in a loop is told to slow down in words it reads. The
specification names no error for this, so it is the tool error it already
defines for a call that can't be done. A token with no `rateLimit` is never
throttled, and a session token isn't limited either, because it is a person's.
Each token counts on its own, and the count lives in memory, so a restart
starts it over. A refusal is an `mcp.refused` row with the reason `rate limit
exceeded`, which means a flood is in the audit log too.

**Every call is in the audit log, under the caller's name.** `mcp.call` is
written before the flow runs, with the tool, the node and a call id, and
`mcp.result` after it, with the outcome and the milliseconds. `mcp.refused` is a
token that is wrong, one that lacks the permission, a tool it may not call, or
arguments that didn't fit. The arguments themselves are never written. A
request with no token at all isn't written either, because nobody is on the
other end to name. `GET /audit?event=mcp.` reads all of it, and `?user=token:agent`
narrows it to one agent.

Try it with the MCP project's own inspector, which needs nothing from Flow:

```bash
npx @modelcontextprotocol/inspector@2.12.0 --cli http://localhost:1880/mcp \
  --transport http --header "Authorization: Bearer $TOKEN" \
  --method tools/call --tool-name line_status --tool-arg line=L3
```

A flow test puts the message a call would have made where the `mcp tool` node
sits, and `mcp result` hands what it would have answered to the test as
`{isError, text, structuredContent}`, so the flow is tested without a server.

### Calling tools on other servers

An `mcp client` node calls a tool when a message arrives, on an
`hotloop-flow-mcp-server` connection: the server's MCP address, a bearer token
if it takes one, and a timeout. The tool and its arguments are typed inputs, so
either can come from the message. `msg.payload` is the tool's structured answer
when it has one, the text when everything it said was text, and the content as
it came when it was anything else, and `msg.mcp` has the server, the tool and
the whole answer. The other operation, "List the tools", puts every tool the
server has in `msg.payload` with its description and input schema.

A tool that reports an error raises one, with the server's words, so a Catch
node sees what the model would have seen. **A call is never sent twice.** A tool
may move equipment, and a dropped connection doesn't say whether the server ran
it, so the error says the connection failed and the next message connects
again. The token goes to the address you gave and nowhere else, a redirect isn't
followed, and an error never repeats it. The connection trusts the system's
certificate roots and nothing else.

For HotLoop it is the address of its `/mcp` and the `mcp-token` from its release
Secret, and every tool in HotLoop's MCP docs is there:

```json
{"id": "plant", "type": "mcp client", "z": "t1", "server": "hotloop",
 "tool": "get_plant_status", "toolType": "str", "arguments": "{}", "argumentsType": "json",
 "x": 300, "y": 100, "wires": [["use"]]}
```

HotLoop sees the call as an MCP client's, under its own rules: writes from MCP
are off unless HotLoop was told otherwise, and a `write_tag` it refuses comes
back as the node's error in HotLoop's words. To write through HotLoop's gate
from a flow without MCP, use the HotLoop nodes above.

### What it was tested against

**The server** is tested with the Go SDK's own client over real HTTP, in both
eras: list, call, arguments refused, a permission refused, a token refused, the
audit rows, a tool that goes away when its node stops, and the whole admin
server in front of it with real API and session tokens and the audit log on
disk. The MCP project's Inspector CLI 2.12.0 listed a running Flow's tools,
called them, and was refused without a token and with a made-up one. The
`mcp client` node was run against a real HotLoop 4.18.10 (the published IoT
image): it listed all 27 of its tools, called `get_plant_status`, `list_devices`
and `get_states` and used the answers, was refused with a wrong token and with
none, got HotLoop's own error for a tool that doesn't exist and for a write MCP
isn't allowed to make, and a Flow tool called by the Inspector ran a flow that
called HotLoop's MCP and answered with what it said. CI starts that HotLoop and
runs all of it, and fails if any of it skips.

**Not done:** OAuth (a bearer token is the only way in, either direction),
stdio servers, resources, prompts, sampling, elicitation and progress
notifications, and a tool's output schema. HotLoop 4.18.10 speaks the handshake
revisions, so the node meets it that way; against a server of the current
revision it is tested only with Flow's own. A handshake server that forgets a
session (a restart) fails the first call after it, once, and the next one
connects again. Rate limiting is per API token and off until you set it, so a token
with no `rateLimit` is bounded only by the queues, which already have a ceiling,
and by the request size, 4 MiB.

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

A flow runs a WASM guest through a plugin: a signed module with a descriptor the
editor draws its dialog from, which gets no host access unless it asks and you
say yes. Memory is capped per node, a message gets a timeout, and a guest that
blows past either traps, lands at a Catch node with the reason, and takes nothing
else down. All of it is in [docs/plugins.md](docs/plugins.md), including how
you trust a publisher's key and what the refusals say. What it means for you:
the thing Node-RED needs npm and a whole Node.js install for, Flow runs as a
sandboxed file you can verify before it loads.

A plugin can be written in Rust, TinyGo, Zig or AssemblyScript instead of
JavaScript, so the signal processing an OT flow actually wants runs compiled,
with a memory ceiling it can't argue with. There are two worked examples built in
CI, [one in Rust](examples/plugins/moving-average) and [one in
TinyGo](examples/plugins/stamp).

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
| Memory ceiling | none enforceable | 16 MiB a node unless the plugin says otherwise, 64 MiB at most unless you raise `plugins.maxMemoryMiB`, hard |
| Max output | 16 MiB | 8 MiB |

Both runtimes are pure Go with no cgo. That's what keeps the build
`CGO_ENABLED=0` and the image distroless, and the WASM host is in the binary. Pull in one cgo dependency and the static binary
and the distroless image go with it, and CI fails the build to tell you so.

---

## Typed wires

A message with the wrong shape is the failure I least want on a live line. A
temperature that arrives as the string `"hot"`, a missing `id`, a number where a
list was supposed to be: nothing crashes, the flow keeps going, and something
downstream quietly does the wrong thing. Flow takes JSON Schema to that problem
in three steps.

### Schema on the `json` node

Give the `json` node a schema in its settings and every value it handles is
checked against it. A value that matches goes on. A value that doesn't goes to a
Catch node, doesn't continue down the wire, and the error names every bad field:

```
does not match the schema: payload.temp: got string, want number; payload.id: missing property "id"
```

`msg.schemaError` carries the same thing as a list of `{path, keyword, message}`
objects, for a Catch node that wants to route on it. Paths read the way a Change
node writes them: `payload.sensors[2].temp`.

If the node has no schema in its settings and a message carries `msg.schema`, the
node validates against that instead, the way Node-RED's `json` node does. A
message that passes has `msg.schema` removed. One that fails keeps it. The
schema in the settings wins when both are there.

- The schema is JSON Schema draft 2020-12 unless it names another draft with
  `$schema`. Drafts 4, 6, 7 and 2019-09 are accepted.
- An invalid schema in the settings fails the deploy with the validator's reason.
  An invalid `msg.schema` is an error on that message.
- A schema is self-contained. A `$ref` to another URL or a file is refused, not
  fetched, so an edge box with no route out behaves like any other.
- `format` is checked for the formats the validator knows and ignored for the
  rest, the same as most validators.
- A flow with no schema anywhere behaves exactly as it did before.

The validator is `internal/schema`, a thin wrapper over
[santhosh-tekuri/jsonschema](https://github.com/santhosh-tekuri/jsonschema) v6
(Apache-2.0, pure Go). Anything else in Flow that needs to check a value against
a schema uses the same package.

### Schemas on ports

Any node can declare what it accepts and what each output port promises. Two
optional keys on the node, both in the flow file, both ignored by every other
tool that reads one, so a flow that carries them still round-trips byte for
byte:

```json
{"id":"sensor","type":"function","outputs":2,"wires":[["limit"],[]],
 "ew_outSchemas":[{"type":"object","properties":{"payload":{"type":"object",
   "required":["temp"],"properties":{"temp":{"type":"number"}}}}}, null]},
{"id":"limit","type":"switch","wires":[[],[]],
 "ew_inSchema":{"type":"object","required":["topic"]}}
```

`ew_inSchema` is what the node takes. `ew_outSchemas` is a list with one schema
or `null` for each output port. A schema describes the whole message, so the
payload is `properties.payload`. Keys that start with an underscore, `_msgid`
and the runtime's other bookkeeping, are left out of what it sees, so a closed
schema doesn't have to name them. They're JSON Schema like the `json` node's,
and the same package checks them.

**A mismatch the editor can see is refused at deploy.** When both ends of a wire
declare a schema and no message could satisfy both, the deploy is refused before
a byte is written, the same as the test gate: a 422, nothing restarted, nothing
saved, and a record in the audit trail as `deploy.refused` with the reason
`typed wires`.

```
the deploy was refused because of 1 typed wire problem, and the flows already running are untouched; junction "sensor" (sensor) output 1 -> "limit" (limit): payload.temp: the sender sends string and the receiver takes number
```

It only refuses what it can prove. Different types on the same field, a value
outside the other end's `enum` or range, a field the receiver requires that the
sender's closed schema rules out, a field the sender always sends that the
receiver's closed schema refuses. A sender that just doesn't mention a field the
receiver requires is not refused, because the message might well carry it, and
a schema that uses `$ref`, `allOf`, `anyOf`, `oneOf`, `not` or `if` isn't
second-guessed in that part. A false refusal stops a deploy for nothing, and
anything it can't prove is still caught at run time. A schema that doesn't
compile, or an `ew_outSchemas` longer than the node has outputs, is refused the
same way. Rollbacks aren't checked, for the reason they aren't gated by tests.

**An Inject node's literals count as a type.** An Inject has no schema of its
own, but what it sets for certain is as good as one. When an Inject is wired
straight to a node with an `ew_inSchema`, the literal payload, and any other
literal property it sets, is checked against that schema at deploy, and a
literal the receiver can never take is refused the same way, with the same 422
and the same `deploy.refused` in the audit trail:

```
inject "reading" (i1) output 1 -> "limit" (l1): payload: the inject sets "hot", which the receiver does not take: got string, want number
```

Only a literal is checked: a string, a number, a boolean or JSON. A timestamp,
an environment variable, flow or global context, another message property,
JSONata, a buffer or a regular expression isn't known until the node fires, so it
is skipped, never guessed. A required property the Inject doesn't set isn't a
clash either, because a value that isn't static may set it. Only the wire from
the Inject to the node is judged: a literal that goes through another node first
is left to the run-time check. The editor draws the same clash on that wire.

**One it can't see goes to a Catch node and to a metric.** A message arriving at
a node that breaks its `ew_inSchema` never reaches the node. A message leaving a
port that breaks its schema is stopped at the sender and never delivered. Either
way it's raised as an error from the node that held the schema, and the Catch
node gets it with the path of every bad field in `msg.error.message` and as a
list in `msg.error.violations`, `{path, keyword, message}` each:

```
the message does not match the schema of output 1: payload.temp: got string, want number
```

`hotloop_flow_node_schema_mismatches_total` counts them per node, and counts the
`json` node's mismatches too. A node with no schemas costs one nil check, and a
flow with none behaves exactly as it did before.

### Types on the wires, in the editor

A wire with a type at either end carries it. The editor writes the type on the
wire, at the middle of the curve: the sender's, as a few characters
(`payload {temp: number}`), and the whole story when you hover the wire.

- **Both ends declare a type and they agree:** green outline, the sender's type.
- **Only one end declares one:** dashed and muted, so you can see where a wire is
  still untyped.
- **Both declare one and they can never agree:** the wire and its label go red and
  say which field, `≠ payload.temp`, and the tooltip has the runtime's reason. The
  deploy would refuse it, and now you can see it before you press Deploy.

The red isn't the editor guessing. It asks the runtime, `POST /flows/types`, which
runs the same check the deploy refuses with, so the canvas and the deploy can't
disagree. The editor only asks when something on the canvas has a schema, a
little after the last edit. A flow with no schemas makes no call and draws
nothing new.

Set the types in a node's dialog, under **Types**: an input schema, and one for
each output port. Empty means no type, and a node you never typed doesn't grow
the keys. Writing something that isn't JSON, or isn't an object, is refused in
the dialog.

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
  ghcr.io/hotloop-io/hotloop-flow:2.2.0
```

Generate the hash with `podman run --rm ghcr.io/hotloop-io/hotloop-flow:2.2.0
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
| `hotloop-flow plugin keygen`, `sign`, `verify` | Makes a publisher key, signs a [plugin](docs/plugins.md), and runs every check a server runs when it loads one, saying why if it would refuse. |
| `hotloop-flow test <tests.yaml>` | Runs [flow tests](#tests-next-to-the-flows) in process. `-junit` for CI. |
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
  dir: /data                # the PVC. Flows, credentials, the deployment log and state.db live here
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
      # rateLimit: {calls: 60, per: 1m}   # MCP tool calls per window; unset is no limit

runtime:
  inboxCapacity: 1024
  overflow: block           # block | drop-newest | drop-oldest | error
  blockTimeout: 30s
  closeTimeout: 15s

store:
  path: ""                  # empty is state.db in data.dir: persistent context and durable wires
  synchronous: normal       # normal | full. full fsyncs every commit, and wears the card
  context:
    persist: true           # false is memory only
    flushInterval: 30s      # a changed value waits this long; a kill -9 loses it. 0s writes at once
  durable:
    maxBytes: 268435456     # disk quota for every durable wire together
    ackFlush: 100ms         # how long a delivered message waits to be marked done; 0s marks each one first

discovery:
  enabled: false
  allowedCIDRs: []          # enabled with this empty is a startup error

exec:
  enabled: false
  allowedCommands: []       # enabled with this empty is a startup error

files:
  allowedPaths: []          # extra trees on top of data.dir, which is always allowed

plugins:
  dir: ""                   # empty means plugins/ inside data.dir
  trustedKeys: []           # publisher keys: ed25519:<base64>, a PEM public key, or a file holding one. Empty refuses every plugin
  grants: {}                # plugin name to the host access it may have: clock, random, context.flow, context.global
  maxMemoryMiB: 64          # the most memory any one plugin node can be given
  maxTimeout: 5s            # the longest any one plugin node can be given for a message

secrets:
  allowedPaths: []          # where a tls-config may read a certificate or key, on top of data.dir

logging:
  level: info               # error | warn | info | debug | trace
  format: text              # text for a terminal, json for a cluster

metrics:
  enabled: true
  path: /metrics

audit:
  maxBytes: 16777216        # data.dir/audit.log rotates at 16 MiB
  keep: 4                   # rotated files kept beside it

tests:
  gate: false               # refuse a deploy that fails data.dir/flows.test.yaml
  timeout: 2m               # wall clock for one run of the flow tests

trace:                      # where every message went, kept in memory, bounded
  enabled: true
  traces: 256               # journeys kept, newest
  maxHops: 512              # hops one journey keeps
  maxMessageBytes: 16384    # the biggest message a hop keeps a copy of
  maxBytes: 16777216        # everything together, oldest forgotten first
  perSecond: 1000           # new traces a second; 0 traces every message
  otlp:
    endpoint: ""            # http://collector:4318 sends every hop as a span
    serviceName: hotloop-flow
    interval: 2s            # the longest a finished hop waits to be sent
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
| `HOTLOOP_FLOW_STORE_PATH`, `HOTLOOP_FLOW_STORE_SYNCHRONOUS` | The state database: where it is, and whether every commit is fsynced. |
| `HOTLOOP_FLOW_CONTEXT_PERSIST`, `HOTLOOP_FLOW_CONTEXT_FLUSH_INTERVAL` | Persistent context. `false` turns it off; the interval is a duration such as `30s`. |
| `HOTLOOP_FLOW_TRACE_OTLP_HEADERS` | Headers for the collector, `Name=value` pairs separated by commas, added to any in the file. For an API key that has no business in a ConfigMap. |
| `HOTLOOP_FLOW_DURABLE_MAX_BYTES`, `HOTLOOP_FLOW_DURABLE_ACK_FLUSH` | The disk quota for durable wires, and how long a delivered message waits to be marked done. |
| `HOTLOOP_FLOW_TESTS_GATE` | `true` turns on the [deploy gate](#the-deploy-gate). |
| `HOTLOOP_FLOW_LOG_LEVEL`, `HOTLOOP_FLOW_LOG_FORMAT` | Logging. |
| `HOTLOOP_FLOW_DISCOVERY_ENABLED`, `HOTLOOP_FLOW_DISCOVERY_CIDRS` | Discovery nodes. Comma-separated CIDRs. |
| `HOTLOOP_FLOW_EXEC_ENABLED`, `HOTLOOP_FLOW_EXEC_ALLOWED_COMMANDS` | The exec node. Comma-separated commands. |
| `HOTLOOP_FLOW_PLUGINS_DIR`, `HOTLOOP_FLOW_PLUGIN_TRUSTED_KEYS` | Where plugins are, and the publisher keys to trust. Comma-separated keys. |
| `HOTLOOP_FLOW_PLUGIN_GRANTS` | What plugins may use: `name=cap,cap;name=cap`. |
| `HOTLOOP_FLOW_PLUGIN_MAX_MEMORY_MIB`, `HOTLOOP_FLOW_PLUGIN_MAX_TIMEOUT` | The ceilings on a plugin node's memory and per-message time. |
| `HOTLOOP_FLOW_FILE_ALLOWED_PATHS` | Extra file node roots. Comma-separated. |
| `HOTLOOP_FLOW_SECRET_ALLOWED_PATHS` | Extra roots a secret file may be read from. Comma-separated. |
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
| `GET /plugins` | `nodes.read` | The plugins that loaded, what each asks for and has been granted, and the ones that were refused with the reason. |
| `GET /flows` | `flows.read` | |
| `GET /flows/export` | `flows.read` | The flow file exactly as it sits on disk, for git. |
| `POST /flows` | `flows.write` | Deploy. Takes an optional note for the deployment log. |
| `GET /deployments` | `flows.read` | The deployment log, newest first, without the payloads. `?limit=N` bounds it. |
| `GET /audit` | `audit.read` | Who did what from where, newest first. `?event=`, `?user=`, `?since=`, `?limit=`. |
| `GET /deployments/{seq}` | `flows.read` | One record, with its flows parsed. Never its credentials. |
| `GET /deployments/{seq}/flows` | `flows.read` | That deployment's flow file, byte for byte. |
| `GET /deployments/{from}/diff/{to}` | `flows.read` | Node-by-node diff between two deployments, structured and as text. |
| `POST /flows/diff` | `flows.read` | The diff from what's live to the document in the body. Deploys nothing. |
| `POST /flows/types` | `flows.read` | The typed-wire check a deploy makes, on the document in the body: `{"wires": [...]}`, empty when nothing is wrong. The editor draws the answer. Deploys nothing. |
| `POST /deployments/{seq}/rollback` | `flows.write` | Deploys that record's flows and credentials again as a new deployment. Takes the rev header and an optional `{"note": ...}`. |
| `GET /tests` | `flows.read` | The flow test suite kept beside the flow file, its file name, and whether deploys are gated on it. |
| `PUT /tests` | `flows.write` | Replaces the suite. The body is the YAML; one that doesn't parse is a 400 with the reason. |
| `POST /tests/run` | `flows.read` | Runs the suite, or `"tests"` from the body, against `"flows"` from the body or what's running. Returns the report. Deploys nothing. |
| `POST /tests/from-debug` | `flows.read` | Drafts a test from `{"node", "msgId"}`, a message a Debug node showed. Saves nothing. |
| `POST /tests/cases` | `flows.write` | Adds the tests in the body, a test file of their own, to the end of the suite. |
| `GET /traces` | `flows.read` | The traces kept, newest first, with what the buffer holds and its limits. `?limit=N`. 404 with `trace.enabled: false`. |
| `GET /traces/find` | `flows.read` | The trace a message was in, from `?msgId=` and `?node=`, the id and Debug node the sidebar shows. Returns every hop and the path to the one asked about. |
| `GET /traces/{id}` | `flows.read` | One trace, every hop with its timing and the message as the node was handed it. |
| `POST /traces/{id}/test` | `flows.read` | Drafts a flow test from the trace. Saves nothing. |
| `POST /traces/{id}/replay` | `flows.read` | Puts the message that started the trace into `"flows"` from the body, or what's running, in a child process, and returns where it ends up against where it ended up. Deploys nothing. |
| `GET /runtime/stats` | `status.read` | |
| `POST /inject/{id}` | `inject.write` | Fire an Inject node. |
| `GET /comms` | `status.read` | The editor's status and debug websocket. Send the token as the subprotocol `hotloop-flow.bearer.<token>`, offered beside `hotloop-flow`. That is what the editor does. `?access_token=<token>` in the URL still works for scripts that already use it, and logs a warning for every connection that does, because a URL ends up in proxy and access logs. |
| `POST /mcp` (and `GET` and `DELETE`, for the older revisions of the protocol) | `mcp.call` | The [MCP endpoint](#mcp-in-both-directions), where flows are served as tools. A session or API token like any other. The tools a caller is offered, and may call, are the ones its permissions reach. 403 with authentication switched off. |

`/metrics` and `/health` being unauthenticated is a decision rather than an
oversight. A Prometheus scraper carries no bearer token, so requiring one means
either handing a credential to your monitoring stack or having no monitoring, and
I have watched people pick the second one.

A user with two-factor sign-in on sends `"code"` with the username and password
to `POST /auth/token`. Without it the answer is a 401 with `"mfa": "required"`.

Permissions are `"*"` for everything, an exact string such as `flows.read`, or a
prefix grant such as `flows.*`. An agent calling flows as MCP tools needs `mcp.call`. A read-only account for a dashboard that just
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
| `hotloop_flow_node_schema_mismatches_total` | counter | Messages that broke a JSON Schema at a node: on its input port, on an output port, or in a `json` node. Each one went to a Catch node with the path of every bad field. |
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
| `hotloop_flow_durable_wire_depth` | gauge | Messages waiting on a durable wire, on disk. Label `wire`. |
| `hotloop_flow_durable_wire_bytes` | gauge | Quota charged to that wire's waiting messages. |
| `hotloop_flow_durable_wire_delivered_total` | counter | Messages the wire handed over and the target took. |
| `hotloop_flow_durable_wire_retries_total` | counter | Times the wire sent a message again because its target failed it. A target that is down climbs this. |
| `hotloop_flow_durable_wire_given_up_total` | counter | Messages the wire stopped retrying because the node's `ew_durable_attempts` ran out. Each went to the flow as an error with the message attached. |
| `hotloop_flow_durable_wire_dropped_total` | counter | Messages the quota policy refused or evicted, and queues purged when a wire stopped being durable. |
| `hotloop_flow_durable_bytes`, `hotloop_flow_durable_max_bytes` | gauge | Quota used by every durable wire together, and the quota. |
| `hotloop_flow_context_flushes_total`, `hotloop_flow_context_rows_written_total` | counter | Transactions that wrote context, and rows they actually changed. |
| `hotloop_flow_context_flush_failures_total` | counter | Context flushes that failed. The changes are kept and tried again. Should be zero. |
| `hotloop_flow_audit_write_failures_total` | counter | Audit entries that couldn't be written. Should be zero forever. |
| `hotloop_flow_traces_kept` | gauge | Message traces in the buffer right now. |
| `hotloop_flow_trace_bytes` | gauge | Roughly what they hold. Bounded by `trace.maxBytes`. |
| `hotloop_flow_traces_started_total` | counter | Message traces started. |
| `hotloop_flow_traces_forgotten_total` | counter | Traces pushed out to make room. |
| `hotloop_flow_traces_skipped_total` | counter | Messages not traced because `trace.perSecond` was reached. |
| `hotloop_flow_trace_hops_dropped_total` | counter | Hops not written down: their trace was full or already forgotten. |
| `hotloop_flow_otlp_spans_exported_total` | counter | Spans the OpenTelemetry collector accepted. Only with an endpoint. |
| `hotloop_flow_otlp_spans_dropped_total` | counter | Spans dropped because the export queue was full. |
| `hotloop_flow_otlp_spans_failed_total` | counter | Spans the collector didn't take after a retry. Above zero means it isn't getting them. |

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

A node can also point at a config node that is not in the file. Delete a flow and
its own MQTT broker goes with it, while a subflow that used that broker keeps
pointing at an id that names nothing. A hand edit or a half-copied export does
the same thing. I used to find that out when the first message hit the node, and
the nodes that only look their server up then said nothing at all. Now the import
report has a section for it that names every node and the id it can't find, the
editor puts the same lines in the log when it loads, and a deploy answers with
them and marks those nodes as failed to start, with the property that holds the
bad id. The rest of the flow keeps running. A node you disabled, and a TLS
setting that is blank or says `true` or `false`, are not reported, because
those are not mistakes.

An id that exists is not the same as an id that works. I missed that the first
time. If the setting points at a tab, a subflow, a group or a plain node, or at
a config node of the wrong kind, like a TLS setting aimed at an MQTT broker,
it gets the same report and the same failed start, and the line says what the
id actually is and which kind the setting wants. The editor only offers you the
right kind, so this only comes from a hand edit or a pasted id, which is
exactly where it bites.

By compatibility level the 79 node types are 12 full, 21 partial, 14 deliberately
divergent, and 32 of Flow's own. Every node declares how it relates to
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
WASM plugins are Flow's answer to that, and they run today: see
[docs/plugins.md](docs/plugins.md).

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
credentials and context. The chart deploys it in all three network modes and the
whole publish chain resolves.

**Verified against real infrastructure**, not just against my own encoders:
mosquitto, InfluxDB 2.7 and PostgreSQL 16, in podman, on 2026-08-07. The
InfluxDB tag value came back out of a real database as `press 01,west` with the
space and the comma intact. That's the escaping bug that otherwise fragments a
series in silence and costs somebody a day to find. That run predates the
rename, and the only change to those nodes since is what they're called.

**Race detector clean**, every package, on Linux with cgo, on every push to
`main` and every pull request.

**Cookies and multipart answer the way Node-RED's own libraries do**, because
they were checked against them. Express, cookie-parser, form-data, multer and
tough-cookie, at the versions Node-RED pins, ran over a hundred cases with
Node-RED's node code copied in between, and the answers went into a golden file
the tests replay through real sockets. That includes the ugly ones: a UTF-8 filename
comes out garbled exactly as multer garbles it, and a cookie named `value`
loses its value in `msg.responseCookies`, same as there. Matching the bugs is
the point. A flow that leans on one breaks the day it stops being there.

**Not done yet**, roughly in the order it bothers me:

- **The S7 and EtherNet/IP nodes have only met simulators.** They are tested
  against real servers that speak the protocols, and they have not talked to a
  Siemens CPU or a Logix controller. Phase 5 stays that honest until they have.
- **Durable wires have never been on a Pi.** Store and forward and the disk
  writes were measured on an x86 box under WSL. The Pi 4 and Zero 2 W numbers
  are still to do, and I won't print a card figure I didn't measure on a card.
- **Plugins have never run on a Pi.** The traps and the refusals are tested
  against guests built from Rust, TinyGo and Go, and a signed plugin has run in a
  real cluster, but the per-call numbers above are from an x86 box.
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
    flowtest/        flow tests: the YAML, the runner, JUnit; replay and tests from a trace
    otlp/            the OpenTelemetry export: spans over OTLP/HTTP
    store/           context (memory and SQLite), durable wire queues, flow file, credentials
    js/              goja host
    wasmhost/        wazero host: memory ceiling, timeout, capabilities
    plugin/          plugin format, signatures, trust, grants, and the node each plugin becomes
    api/             admin REST and the editor websocket
    config/          the YAML surface and the refusals
    metrics/         the Prometheus exposition
    flowhttp/        the route table a flow's HTTP nodes register into
    shell/           the exec node's command allowlist
    filescope/       the file nodes' path scope
    discover/        network discovery
  web/               the editor
  charts/            the Helm chart
  docs/              deploying, plugins, compatibility, the roadmap, and the benchmark method
  examples/plugins/  a plugin in Rust and one in TinyGo, built in CI
```

## Building

```bash
cd web && npm install && npm run build && cd ..
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o hotloop-flow ./cmd/hotloop-flow
```

Static, no cgo, runs on distroless. goja is pure Go, and so is wazero, which runs
the plugins, and that is the only reason that sentence is true. CI fails
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
refuses anonymous clients, and the editor clicked through in a real browser, every node dialog included.
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
B). All four are out as 4.18.10, each with its own image and chart, and they pull
with no login. [hotloop.io/products](https://hotloop.io/products/) has the
lineup.

Flow talks to them, and it will never be built into them. It runs next to them
and calls HotLoop's API from the outside: the HotLoop nodes read entity states
and changes, run scripts and apply recipes, and the equipment write nodes can
send their writes through HotLoop's write gate. All of that is under
[The plant floor](#the-plant-floor). A Flow with no HotLoop picked shares nothing
with them but a broker, if you give them one.

Flow is under the same license as those four: HotLoop's Business Source License
1.1. Home and lab use is free, and so is use by accredited schools and educational institutions, nonprofit organizations and government bodies. Business use is free when it runs through
[EmberNET](https://embernet.ai), and there is no other business path. Hosting
Flow as a service for other people, reselling it and embedding it in a product
you sell are not granted. Unlike the others, Flow's source is public, so you can
read every line you run.

## License

HotLoop's Business Source License 1.1 (`BUSL-1.1`). It is not an open source
license, and we won't pretend it is. Each version converts to GPL-3.0 or later four years
after it is first made publicly available. [LICENSE](LICENSE) is the legal text
and the [Terms of Service](https://hotloop.io/terms) cover the rest. A few
pieces are ported from Node-RED and from the libraries its
nodes use, because an imported flow has to get exactly the answer it got before.
[NOTICE](NOTICE) lists every one of them, with its copyright and license. See
[docs/compatibility.md](docs/compatibility.md) for every deliberate divergence.
