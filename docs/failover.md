# Failover: a warm standby with a lease

Two installs of Flow can share the job of one. One of them is the writer: it is the only one that sends anything to equipment. The other is the standby: it runs the same flows, reads, subscribes and keeps its context warm, but holds every equipment write until it takes the writer's place. If the writer dies, hangs or loses its link, the standby takes over in about the length of a lease, and the one that was the writer cannot write again until it wins the lease back.

This is for the case a restart is not fast enough, and a second writer is worse than a short wait. If a restart is fast enough, a single install and a Kubernetes reschedule is simpler and is what the chart does by default.

## How it works

The lease is one row in a PostgreSQL both installs can reach. The writer renews it every few seconds. The standby tries for it on the same schedule and gets it only after it has run out. Each time the holder changes, the fencing number on the row goes up, and Flow reports it.

Every write to equipment asks the same question just before it sends: do I hold the lease, with more of it left than a write needs? If not, the write is refused with a counted error that reaches the flow's Catch node, and nothing is sent, not even a connection. That check, made at the moment of each write, is what keeps two installs from writing at once:

- The holder counts its lease from the moment it asked for it, never from the moment the answer came back, so it never thinks it has longer than the database does.
- The holder stops writing when it has less lease left than a write needs (`writeMargin`), which is before the lease runs out. The standby can only take the lease after it has run out. So there is a gap, and no overlap.
- A holder that is frozen and thawed, or cut off and reconnected, finds its lease gone, refuses its writes, and asks for the lease like anyone else.
- The database's clock decides when the lease ends, so two boxes whose clocks disagree still agree on who holds it.

The writes that ask are the ones that can command equipment: `modbus write`, `s7 write`, `opcua write` and `ethernetip write`, whether direct or through HotLoop's write gate, the `hotloop script` and `hotloop recipe` nodes, and in a FlowLab diagram the serial write, an instrument command that writes (alongside that instrument's own write switch) and the HotLoop write. Sending a message to an MQTT broker, a database or a notification is not fenced: it is not a command to equipment, and a repeat of it is harmless where a repeated setpoint may not be. A flow test stands in for the outside world and is never held.

## What it costs and what it does not give you

- **Takeover time** is about the lease length. With the defaults (15 second lease, renewed every 5 seconds, a 5 second margin) a standby took over 14.7 seconds after the writer was killed. With a 6 second lease, 5.7 seconds. A writer that stops cleanly gives the lease up, and the standby took over in 0.35 seconds.
- **A write that takes longer than `writeMargin`** can outlive the lease. Keep the protocol timeouts of the nodes that write under it. A write already in flight when a process is frozen has already passed its check and can still arrive.
- **At least once.** A PLC that took a write whose acknowledgment was lost can see the new writer send it again. That is safe for a setpoint and not for a pulse or an increment, so write setpoints, not deltas.
- **Queues stay local.** Durable wires and context are on each install's own disk. What was queued on a writer that died is not on the standby.
- **The database is a dependency.** A writer that cannot reach it cannot renew, and stops writing when its lease runs short. Put the lease on a database that is at least as available as the equipment it protects.

## Setting it up

Install Flow twice, as two Helm releases or two Quadlet units, each with its own volume, and point both at the same lease. Give the second the same flow file as the first and keep it there with a push to stations ([stations.md](stations.md)), because Flow does not copy flows between the two.

| Setting | Environment | What it is |
|---|---|---|
| `failover.urlFile` | `HOTLOOP_FLOW_FAILOVER_URL_FILE` | A file holding the PostgreSQL URL of the lease database. Setting it turns failover on. |
| `failover.name` | `HOTLOOP_FLOW_FAILOVER_NAME` | The lease. The same on both. Default `flow`. |
| `failover.holder` | `HOTLOOP_FLOW_FAILOVER_HOLDER` | This install's own name. It has to differ between the two, and defaults to the host name. |
| `failover.ttl` | `HOTLOOP_FLOW_FAILOVER_TTL` | How long a grant lasts, and so about the takeover time. Default `15s`. |
| `failover.renewEvery` | `HOTLOOP_FLOW_FAILOVER_RENEW_EVERY` | How often the holder asks again. Default a third of the ttl. |
| `failover.writeMargin` | `HOTLOOP_FLOW_FAILOVER_WRITE_MARGIN` | How much lease a write needs left. Default `5s`. |

Flow refuses to start when the settings would make the holder dip under its margin between renewals, for example a margin as long as the ttl.

In the chart these are `failover.urlSecret` (a Secret whose key holds the URL, mounted as a file and never put in the ConfigMap), `failover.name`, `failover.ttl`, `failover.renewEvery` and `failover.writeMargin`. With Quadlet they are the environment variables above, and the URL is a Podman secret mounted as a file:

```ini
Environment=HOTLOOP_FLOW_FAILOVER_URL_FILE=/run/secrets/flow-lease-url
Environment=HOTLOOP_FLOW_FAILOVER_NAME=line3
Secret=hotloop-flow-lease-url,type=mount,target=flow-lease-url
```

## Seeing it

- `GET /failover` (and the MCP tool `get_failover`) says whether this install is `active`, `standby` or `lost`, who holds the lease, the fencing number it holds, and how many writes it has held back, by what was writing. It needs `status.read` and is only there when failover is set up.
- The editor shows a banner on a standby and on a holder that has lost its lease.
- The metrics endpoint has `hotloop_flow_failover_active`, `hotloop_flow_failover_epoch`, `hotloop_flow_failover_takeovers_total` and `hotloop_flow_failover_refused_total{op}`.
- Both installs log when they take the lease and when it is taken from them.

## What is proven, and what is not

Proven on two real Flow processes against a real PostgreSQL and a Modbus TCP server that logs every write, one writing a tagged value every 200 ms from each install: with the writer killed mid stream, the standby took over after one lease, and no write from the standby came before the last write of the writer or after it (0 and 0); with the writer frozen for longer than the lease and thawed, it did not write again; with the writer stopped cleanly, the standby took over in 0.35 seconds. A test for each write path above shows a standby's or a stale holder's write is refused before anything is sent, and shows the same write is sent when the gate is open.

Not proven: a real PLC's behavior on a repeated write, a database failover underneath the lease, and a network that is cut in one direction only. The Modbus server is a simulator that speaks the protocol. Failover is not tried on a cluster, and the chart values and Quadlet lines are rendered and checked, not installed. The strings in the editor banner have had no native-speaker review.
