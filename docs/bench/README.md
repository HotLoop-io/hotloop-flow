# Benchmarks

Every number in this repository came out of `hotloop-flow bench` or `go test
-bench`, and every one is printed next to the command that made it. Absolute
numbers only. A side-by-side table moves every time the box changes, and it ends
up describing the other thing more than this one.

## The flow

[`bench-flow.json`](bench-flow.json). Five nodes: an HTTP endpoint, three
property edits, a reply. Nothing in it is fast on purpose. It's the shape of a
small real flow, which is what a benchmark of a flow engine should be about.

## Running it

Build the native binary the way CI does, then let the harness launch it:

```bash
cd web && npm ci && npm run build && cd ..
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o hotloop-flow ./cmd/hotloop-flow

W=$(mktemp -d); cp hotloop-flow "$W/"; cp docs/bench/bench-flow.json "$W/flows.json"; cd "$W"
export HOTLOOP_FLOW_DATA_DIR="$W" HOTLOOP_FLOW_ADMIN_USER=admin \
  HOTLOOP_FLOW_ADMIN_PASSWORD_HASH="$(./hotloop-flow hash-password -password benchbench123)" \
  HOTLOOP_FLOW_CREDENTIAL_SECRET=bench-secret HOTLOOP_FLOW_LOG_LEVEL=warn \
  HOTLOOP_FLOW_HOST=127.0.0.1 HOTLOOP_FLOW_PORT=18897

./hotloop-flow bench -mode http -launch ./hotloop-flow -target http://127.0.0.1:18897 \
  -path /bench -duration 30s -warmup 5s -connections 8
./hotloop-flow bench -mode engine -chain 5 -messages 200000
```

`-launch` starts the binary and times how long until the flow answers (cold
start), sleeps two seconds and reads `VmRSS` from `/proc` (idle), then samples it
through the load and keeps the peak (under load). Linux only, since that's where
`/proc` is.

`-target` without `-launch` drives an instance that's already running, a
container or a pod, over HTTP. You get throughput and latency, but not RSS or
cold start, because the harness can't see inside somebody else's process.

The load generator is closed-loop: each client sends the next request when the
last one came back. That measures what the far end can absorb, which is the
question, and it can't produce the misleading coordinated-omission latency an
open-loop generator reports when the target falls behind. The five-second
warm-up is discarded.

## Results

**Measured 2026-10-02.** Linux under WSL2, 24 CPUs (Ryzen 9 7900X), the native
binary built from `main` at `950076b`. Three runs of each command above.

| | Run 1 | Run 2 | Run 3 |
|---|---|---|---|
| Cold start | 0.05 s | 0.05 s | 0.05 s |
| RSS, idle | 16.7 MiB | 16.7 MiB | 16.4 MiB |
| RSS, peak under load | 26.4 MiB | 26.4 MiB | 28.5 MiB |
| HTTP throughput | 15.0k req/s | 15.3k req/s | 13.0k req/s |
| HTTP latency p50 / p95 / p99 | 0.39 / 1.22 / 1.82 ms | 0.39 / 1.20 / 1.78 ms | 0.41 / 1.49 / 4.03 ms |
| Engine, per message | 2.61 µs | 3.11 µs | 2.20 µs |
| Engine, allocated per message | 3,868 bytes | 3,869 bytes | 3,868 bytes |
| Engine, peak heap | 127.2 MiB | 130.0 MiB | 146.2 MiB |

Sizes from the same session: the static linux/amd64 binary is 22,012,066 bytes,
and the published `2.0.5` amd64 image is 25,442,861 bytes as `podman image
inspect` reports it.

### What these numbers are not

**HTTP throughput includes the HTTP stack.** On purpose: it's what a client of
your flow experiences. It is not a measurement of the scheduler. The engine mode
is, with no I/O in the path at all.

**The engine's peak heap is a burst, not a resting state.** It fires 200,000
messages into the chain at once, and with the default `block` policy the bounded
inboxes fill and hold the rest back. Feed it at a sensor's pace and memory sits
where the RSS rows say.

**One flow shape, one payload size, one concurrency.** A flow doing real I/O, an
MQTT publish or a database write, is dominated by the I/O. These numbers are the
runtime's own overhead, not a promise about every workload.

**The tail moves more than the median.** Under saturation everything queues and
queueing owns p99, which is why run 3's p99 is twice the others' while its p50
barely moved. The median is where the cost of handling a request shows up.

**WSL2 is not a Pi.** A Pi 4 and a Zero 2 W get their own numbers, measured on
the boards, in Phase 4 of the [roadmap](../ROADMAP.md).

## Memory

RSS is the process's own resident memory, read from `/proc`. It is not what
`podman stats` reports. That tool shows what the kernel charges to the
container, and on 2026-09-28 the published 2.0.4 image read 2.6 MB there while the
same process had 14.7 MB in `VmRSS`. A figure that moves fivefold depending on
which tool you ask doesn't get printed, which is why this repository used to
carry a 4.8 MB idle number and doesn't any more. Nobody could reproduce it, me
included.

## Re-running after a change

The numbers above are pinned to a commit and a box. If the engine changes,
re-run before editing them, and replace the whole table from one session rather
than patching one row. A number carried over from a different day on a
differently loaded box is exactly what this directory exists to avoid.
