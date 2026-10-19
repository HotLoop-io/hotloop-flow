# Durable state

A historian going down for an hour shouldn't cost you an hour of readings, and a
counter in a flow shouldn't start over because the pod moved. Both of those are a
SQLite file on the volume, and this page is what goes in it, what it guarantees,
what it doesn't, and what it costs a flash card.

One file, `state.db` in the data directory. Pure Go SQLite
([modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite), no cgo), so the
binary is still one static file. Persistent context and the queues behind durable
wires share it, because two stores would be two things to size, back up and keep
in step.

## Context that survives a restart

Flow, global and node context are loaded from the file the first time a scope is
used and live in memory after that, so reads never touch the disk and
`CompareAndSwap`, `Increment` and `Update` are the same code as before, atomic
under the same lock. A restart gets your counter back and it carries on from
there, and a `CompareAndSwap` against a value that came back from disk still
works, including the integer a function node stored.

What you give up is on purpose: writes are behind. A changed value waits
`store.context.flushInterval` (30 seconds) and everything changed in that window
goes to the file in one transaction. Ten thousand increments of one counter in a
window are one row written. A value set to what it already was is no write at
all, which is the flow that stores the same temperature every second and the SD
card that would otherwise hate you for it. A clean stop writes everything first.
A `kill -9` or a power cut loses up to the interval. Set it to `0s` and every
change is written before the call returns, and the card pays for it.

Only things that survive JSON survive a restart: numbers, strings, booleans,
times, byte buffers, lists and objects. Put a function or a socket in context and
the `set` fails, naming the key, instead of storing half of it. A node's context
goes when the node is deleted and a flow's when the tab is, at the deploy, the
same sweep Node-RED does.

## Durable wires

Mark a wire durable on the node it leaves. The key is `ew_durable`, a list of the
node ids that wire goes to, and the flow file keeps loading and saving byte for
byte because other tools ignore it:

```json
{
  "id": "count", "type": "function", "z": "line3", "x": 290, "y": 100,
  "ew_durable": ["influx"],
  "wires": [["influx", "reply"]]
}
```

Every wire from `count` to `influx` is now durable. The wire to `reply` is still
an ordinary one.

What a durable wire does:

- **Writes nothing while the target keeps up.** Messages go from memory straight
  into the target's inbox, the same as an ordinary wire, and the state database
  is not touched. I set that rule after a write-everything-first design measured
  in the hundreds of gigabytes a day on a flash card for a failure that happens a
  few hours a year.
- **Spills when it doesn't.** The wire starts spilling the moment the target's
  handler returns an error, its inbox is full, or it isn't running. From then on
  every message is committed to the file before the sending node's `Send`
  returns, so once the sender has moved on a `kill -9` can't take it. The
  messages already on their way to the target are diverted to disk behind the one
  that failed, in order. In the example above `reply` comes second in the wiring,
  so while the wire is spilling an HTTP 200 means the reading is on disk.
- **Drains in order, one at a time.** A pump hands the oldest message on disk to
  the target and waits until its handler returns. Nothing behind it overtakes it.
  When the queue is empty the wire goes back to memory and stops writing.
- **Retries what the target fails.** The same message goes again after 250 ms,
  then 500 ms, doubling to 5 seconds, for as long as it takes. The flow's Catch
  nodes hear about the first failure and not every retry. A target that is
  stopped, replaced by a deploy or failed to start is waited for, not skipped.
- **Survives a restart.** Whatever is still on disk when the process comes back is
  delivered first.
- **Honors a disk quota.** `store.durable.maxBytes` (256 MiB) covers every
  durable wire together, counting each message's body plus 64 bytes. When it's
  full, the node the wire goes to decides by the overflow policy it already has:
  `block` waits up to `blockTimeout` for space and then errors, `drop-newest`
  refuses the arriving message, `drop-oldest` evicts the oldest message that
  isn't being delivered, `error` raises to a Catch node. Every one is counted and
  announced as a dropped event. The file runs somewhat larger than the quota, by
  SQLite's page overhead and the write-ahead log, and a test fills it, drains it
  and fills it again six times to prove it doesn't keep growing.
- **Shows its depth.** `hotloop_flow_durable_wire_depth{wire="..."}` is the
  messages waiting, with `_bytes`, `_delivered_total`, `_retries_total` and
  `_dropped_total` beside it, and `hotloop_flow_durable_bytes` against
  `hotloop_flow_durable_max_bytes` is the quota.
- **Doesn't restart the node.** Marking a wire durable on a running flow is a
  wiring change, like moving a wire.

A wire that stops being durable, or is deleted, has its queue purged, counted as
dropped and announced. Those messages have nowhere to go, and holding them would
fill the quota with a queue nobody can see.

### What it promises, and what it doesn't

**While the wire is healthy there is a loss window.** What is in memory is the
target's inbox and the call it is making, and a `kill -9` or a power cut takes
it. That is the price of writing nothing, and I accepted it on purpose: with an
inbox of 1024 and a fast target it is the messages from about one poll. Once the
wire is spilling, which is when the target is down and the data is at risk, there
is no window. The kill -9 test below is done mid-outage for that reason, with the
queue on disk. I did not kill the runtime while the wire was healthy and count,
because the answer is "a few are gone" and that is the design.

**Delivery is at-least-once.** The target has taken a message when its handler
returns without an error, and the wire marks it done on disk after that. A `kill
-9` between the two means the message goes again after the restart. How many
depends on `store.durable.ackFlush`: marks are written together, so with the
default `100ms` that's the messages finished in the last 100 ms, and with `0s`
each mark is written before the next message is sent, so it's at most the one in
flight.

That sounds like a hole, so here is what closes it for the target this was built
for. A message that crosses a durable wire is stamped `msg._queuedAt` with the
time it entered the wire, and `influxdb out` with no timestamp configured writes
the point at that time. A repeat is the same series at the same time, which
InfluxDB treats as an overwrite, so the row count doesn't move. It also fixes the
other thing that goes wrong with store-and-forward: without it, every reading
held through an outage is stamped when InfluxDB comes back, and an hour of
history lands in one minute. A sink that stamps on arrival, a `now()` in a
Postgres insert for one, would store a repeat twice, and it would put the
replayed hour in the wrong place too. Use `msg._queuedAt`.

**"Taken" is only as strong as the node.** `influxdb out` does its write inside the
handler, so taken means written. A node that hands the work to another goroutine
and returns has promised less, and the wire can't be more durable than what it's
wired to.

**Some properties don't make the trip.** A live handle can't cross a restart and
can't be replied to after the fact, so `msg.req` and `msg.res` (the HTTP In
node's) and anything else that won't serialise is left out of messages on a
durable wire, and the first time that happens on a wire it's logged with the
property's name. The payload is never left out: if it can't be stored the message
is refused with an error naming the path.

**One bad message can hold a wire.** A target that fails the same message forever
gets it forever, with the 5 second pause between tries, and the messages behind
it wait. The retry counter and the depth are how you see it. That's the right
call for a historian that's down. It's the wrong one for a message that's
malformed, so a wire can be given a limit: `"ew_durable_attempts": {"influx": 20}`
on the node it leaves, or the wire dialog in the editor. After the target has
refused one message that many times from the queue, it goes to the flow as an
error from the node that refused it with the message attached, a Catch node can
park it, and the wire moves on. Without the key nothing changes. There is still
no dead-letter queue: a Catch node is where you build it, and
`hotloop_flow_durable_wire_given_up_total` counts what went there.

## Settings

```yaml
store:
  path: ""              # empty is state.db in data.dir
  synchronous: normal   # normal | full
  context:
    persist: true
    flushInterval: 30s  # 0s writes every change at once
  durable:
    maxBytes: 268435456
    ackFlush: 100ms     # 0s marks each message before the next is sent
```

Environment: `HOTLOOP_FLOW_STORE_PATH`, `HOTLOOP_FLOW_STORE_SYNCHRONOUS`,
`HOTLOOP_FLOW_CONTEXT_PERSIST`, `HOTLOOP_FLOW_CONTEXT_FLUSH_INTERVAL`,
`HOTLOOP_FLOW_DURABLE_MAX_BYTES`, `HOTLOOP_FLOW_DURABLE_ACK_FLUSH`. The chart has
the same names under `store:` in `values.yaml`.

`synchronous: normal` runs the file in write-ahead mode without an fsync on every
commit. A commit survives the process being killed, which is the `kill -9` test
below. It doesn't survive a power cut on every filesystem. `full` does, and
`full` is the one that wears the card.

## The proof

Measured 2026-10-19 on Linux under WSL2, with the native binary built from this
branch and a real InfluxDB 2.9.1 (`docker.io/library/influxdb:2.9.1`, the newest
2.x on Docker Hub that day) in Podman. Nothing is mocked. The script is
[store-and-forward.sh](store-and-forward.sh), and it is the whole experiment:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o hotloop-flow ./cmd/hotloop-flow
BIN=./hotloop-flow RATE=20 OUTAGE=600 bash docs/durable/store-and-forward.sh
```

A producer posts a numbered reading to the flow in [flow.json](flow.json) twenty
times a second. The flow counts it in flow context and sends it down a durable
wire to `influxdb out`. The reading carries no timestamp, so a message delivered
twice would be two rows. A small pass-through between the flow and InfluxDB logs
every write InfluxDB accepted, because InfluxDB treats a point written twice at
the same time as one point and rows alone can't show a repeat.

| Time | What happened |
|---|---|
| 00:13:53 | Flow started, 20 readings a second. |
| 00:14:53 | `podman kill` on InfluxDB. It stays dead for ten minutes. The wire spills to disk. |
| 00:19:54 | Five minutes in, queue depth 5,989 on disk. `kill -9` on the runtime. |
| 00:19:59 | Runtime started again, InfluxDB still down. It finds 5,992 messages on disk and keeps taking new ones. |
| 00:24:59 | InfluxDB started. Queue depth 11,989. |
| 00:25:25 | Draining, queue depth 5,987. `kill -9` on the runtime again, mid-queue. |
| 00:25:30 | Runtime started again with 5,948 on disk. |
| 00:25:53 | Backlog drained. Ninety more seconds of steady input, then a count. |

```
deliveries      15987 writes InfluxDB answered 204 to
delivered twice 5 [7829, 7830, 7831, 7832, 7833]
sent            16185
accepted (200)  15982
failed          203
rows stored     15982
distinct seqs   15982
missing         0 []
doubled         0 []
unacked stored  0 []
```

Every reading the flow answered 200 for is in InfluxDB, once. **None missing, none
doubled.** The 203 that failed were requests that arrived while the runtime was
dead, which is the producer being told no, and none of them were stored.

Five messages were handed to InfluxDB twice. They are the ones the second kill
caught between InfluxDB taking them and the "done" mark reaching the disk, inside
the `ackFlush` window. Each carried the same `_queuedAt`, so the second write
overwrote the first and the row count never moved. That's what
[at-least-once](#what-it-promises-and-what-it-doesnt) looks like when it works.
It is also what a sink that stamps on arrival would have stored twice.

What this does not cover: a `kill -9` while the wire is healthy. The readings in
memory at that moment are gone, by design, and I did not count them.

## What it writes to disk

`write_bytes` from `/proc/<pid>/io` is what the kernel was asked to send to
storage for the process, cancelled writes taken off. That is the number a flash
card cares about. Each row is 300 seconds of one scenario, after 30 seconds to
settle, from [disk-writes.sh](disk-writes.sh). The flow is the proof flow with
InfluxDB behind it, so the load is real.

```bash
BIN=./hotloop-flow SECS=300 bash docs/durable/disk-writes.sh
```

| Scenario | Rate | Context flush | Written per hour | Per day |
|---|---|---|---|---|
| Idle, a durable wire in the flow | none | 5 s | 0 bytes | 0 |
| Healthy durable wire, steady load | 20 msg/s | 30 s | 1.7 MiB | 0.04 GB |
| Healthy durable wire, steady load | 50 msg/s | 30 s | 1.2 MiB | 0.03 GB |
| Same, context flushed every 5 s | 20 msg/s | 5 s | 5.1 MiB | 0.12 GB |
| Same, context flushed every 5 s | 50 msg/s | 5 s | 4.2 MiB | 0.10 GB |
| Context only, ordinary wire | 20 msg/s | 30 s | 1.2 MiB | 0.03 GB |
| Wire spilling, InfluxDB down | 20 msg/s | 5 s | 728 MiB | 17 GB |
| Wire spilling, `synchronous: full` | 20 msg/s | 5 s | 1,011 MiB | 24 GB |

Read it in three parts.

**Idle writes nothing.** Zero bytes in five minutes.

**A healthy wire writes nothing of its own.** The healthy rows are persistent
context, a counter bumped on every message, and nothing else. The context-only
row with an ordinary wire is within noise of them. The context flush default was
five seconds until this table: at twenty messages a second that was 5.1 MiB an
hour, 0.12 GB a day, and thirty seconds takes it to 1.7 MiB, which is why the
default is thirty. A kill -9 then loses up to thirty seconds of context.

**A spilling wire is expensive, and only while it spills.** Every message is its
own commit, about 10 KB of writes each, because the sender is waiting on the
disk. Ten minutes of outage at twenty a second is about 120 MiB, a day-long
outage about 17 GB. That is the cost of "on disk before the sender moves on",
paid only while the historian is down. Group commit shares a transaction between
concurrent senders, but one sender makes one commit.

One more number, because it caught me out. At 100 messages a second this harness
didn't keep the wire healthy: the target stopped keeping up, the wire spilled and
stayed spilling for the whole run, 3,548 MiB an hour. I didn't chase down whether
the pass-through or InfluxDB was the slow part. It is the design working as
written, because "backed up" counts as not keeping up, and it means a target that
is slower than its input puts a durable wire on the disk. At 50 messages a second
it stayed healthy.

The design before this one wrote every message to disk first. On the first schema
(4 KiB pages, a table and an index) that measured 632 MiB an hour at 20 messages
a second even with InfluxDB healthy, and 1,163 MiB with each "done" mark written
at once. That is where "disk only during an outage" came from.

### On a Pi

UNVERIFIED. These numbers are from an x86 machine under WSL2, not a Pi 4 or a
Zero 2 W, and I don't have the boards. Bytes written is a property of the
code and the page size and should carry over. What a card does with them
(write amplification, wear) does not, and neither do memory, start time and
sustained messages per second. The Pi item in the roadmap stays open.
