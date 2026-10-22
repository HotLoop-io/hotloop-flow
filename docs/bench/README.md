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

**WSL2 is not a Pi.** The Pi 5 numbers are in the next section. A Pi 4 and a
Zero 2 W still get their own, measured on the boards, in Phase 4 of the
[roadmap](../ROADMAP.md), and I haven't had those two in front of me.

## Results on a Raspberry Pi 5

**Measured 2026-10-22.** The same flow and the same commands as above, on one
Raspberry Pi 5 Model B Rev 1.1 with 8 GB (`MemTotal` 8,122,272 kB), four
Cortex-A76 cores, Ubuntu 26.04.1 LTS, kernel `7.0.0-1020-raspi`, root on NVMe.
The clock is capped at 2.0 GHz (`arm_freq=2000`, the Pi 5 default is 2.4 GHz)
because that board's power supply is under the official 27 W one, and the
governor is `ondemand`. The house notes say none of the cluster's Pis has an Active
Cooler yet, and the node shows no fan among its cooling devices, so this is a bare
board.

It ran as a pod on a k3s node, with nothing else on that node but the cluster's
own DaemonSets (monitoring exporters, the load balancer shim, the reboot
daemon). The node read 52m CPU before I started. The image was
`ghcr.io/hotloop-io/hotloop-flow:2.2.0`, the arm64 manifest
`sha256:657360c102afa69765c9f0e0dd9b01d085b81b4542ee4bedf1dceb41bb0d1115`
(read anonymously from ghcr.io), which containerd on the node reported as image
ID `sha256:de115f6214f23975c7b58bd0f2d89ff838ccaec15f1359e79c51292b5edf76bd`.
Instead of building a native binary I ran the image's own binary:
`hotloop-flow bench -mode http -launch /usr/local/bin/hotloop-flow ...` as the
container's command, with the same arguments as above, over the pod's loopback,
data on an `emptyDir` seeded with `bench-flow.json`. The load generator and the
flow share the same four cores, as they do in the x86 run. Each run was a fresh
pod.

| | Run 1 | Run 2 | Run 3 |
|---|---|---|---|
| Cold start | 0.46 s | 0.15 s | 0.10 s |
| RSS, idle | 88.3 MiB | 88.0 MiB | 88.1 MiB |
| RSS, peak under load | 40.3 MiB | 40.5 MiB | 40.3 MiB |
| HTTP throughput | 13.2k req/s | 13.4k req/s | 13.3k req/s |
| HTTP latency p50 / p95 / p99 | 0.48 / 1.45 / 2.78 ms | 0.48 / 1.42 / 2.72 ms | 0.48 / 1.44 / 2.74 ms |
| HTTP latency, max | 12.00 ms | 7.82 ms | 10.92 ms |
| Engine, per message | 8.15 µs | 7.74 µs | 7.89 µs |
| Engine, allocated per message | 4,026 bytes | 4,025 bytes | 4,025 bytes |
| Engine, peak heap | 170.4 MiB | 172.3 MiB | 184.2 MiB |

Run 1's cold start is the first run after the image was pulled, and the file
cache was cold. I didn't run a fourth to see if that's the whole story.

### Sustained, and what the board did

Thirty seconds doesn't show a heat problem, so I ran the HTTP command once more
with `-duration 600s` and everything else the same, while sampling
`vcgencmd get_throttled`, `measure_temp` and `measure_clock arm` on the node
every five seconds.

| 600 seconds, one run | |
|---|---|
| Requests | 6.86M |
| HTTP throughput | 11.4k req/s |
| HTTP latency p50 / p95 / p99 | 0.58 / 1.61 / 3.00 ms |
| HTTP latency, max | 21.90 ms |
| RSS, idle / peak under load | 87.9 MiB / 41.7 MiB |
| SoC temperature | 69.2 to 86.2 C |
| ARM clock | 1,500 to 2,000 MHz, mean 1,684 MHz |

The board throttled. The soft temperature limit bit was set from 45 seconds
in, and the frequency-capped and throttled bits from about two minutes in. 110
of the 119 samples during the run had at least one of them set, and for the last
nine minutes the temperature sat between 83 and 86 C with the clock hovering
between 1.5 and 1.7 GHz instead of the 2.0 GHz cap. The under-voltage bits were
never set. So the sustained figure is 11.4k req/s against 13.2k to 13.4k for the
30-second runs, and p50 moved from 0.48 to 0.58 ms. Those happened together. I
didn't sample throughput per minute, so I can't tell you how much of the drop is
the clock and how much is anything else. After the load stopped the board went
back to about 55 C within ten minutes. The peak, 86.2 C, is above the 85 C line I
use as the stop for a memory test on these boards, and nothing on the node
failed or froze, though I haven't tested its memory, and the sticky "has occurred" bits stay
set until reboot, as they're meant to.

The three 30-second runs were short enough that the sampler caught the soft
limit bit during run 3 and nothing before it.

### What these numbers are not

**RSS idle is bigger than the x86 figure, and I don't know why.** 88 MiB
against 16.7 MiB. A separate pod running the same image idle held `VmRSS`
89,648 kB (69,704 kB anonymous, 19,944 kB file-backed) flat from one second
to two minutes after start, read from `/proc` on the node. The harness's "under
load" number is lower than its idle number, because it samples only after the
warm-up and the process's resident set was lower by then. I measured both and
haven't found out what is holding the memory at start, so I'm not offering a
cause.

**One board, one afternoon, a capped clock.** At 2.4 GHz, with a cooler that
keeps it out of the soft limit, this would read differently, in both
directions. These numbers are for the board as it sits.

**This is the bench flow, not durable wires.** Nothing here measures the SQLite
store or SD card writes.

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
