# Deploying HotLoop Flow

Three ways in, all from the same image, `ghcr.io/hotloop-io/hotloop-flow`, built
for amd64 and arm64:

| You have | Use |
|---|---|
| A Kubernetes cluster, k3s on a Pi included | [Helm](#kubernetes) |
| One Linux box running systemd | [Podman with Quadlet](#podman-with-quadlet) |
| Ten minutes and curiosity | [`podman run`](#podman-run) |

No Docker Compose file, and there won't be one. Quadlet hands the container to
systemd, which already knows how to restart things, order them after the network
and keep their logs. A second supervisor on top of that is just one more thing
that can be down when you need it.

## What every install needs

Whichever way you go, Flow won't start without three things, and every one of
those refusals is on purpose:

- **An admin account.** A username and a **bcrypt hash** of its password, never
  the password itself. `hotloop-flow hash-password -password '...'` makes one,
  and the image can run it for you. Anything that isn't a bcrypt hash is a
  startup error, so a plaintext password can't end up in a config file by
  accident.
- **A credential secret.** It encrypts every broker password and database login
  your flows hold. **Lose it and those credentials are gone**, and it looks like
  a perfectly clean start until MQTT stops authenticating. Generate it once,
  store it somewhere that outlives the box, and never let anything regenerate it.
- **Somewhere for `/data` to live.** Flows, credentials and three generations of
  backups go there. A container with no volume works right up until it restarts.

Context (flow, global and node) is kept in a SQLite file, `state.db`, on that same
volume, and survives a restart whichever way you deploy. So do the queues behind
durable wires. A container with no volume loses the file with everything else, so
it keeps context only until the container is replaced. What gets written, how
often, and what it costs a flash card is in [durable/README.md](durable/README.md).

---

## Kubernetes

```bash
helm repo add hotloop-flow https://hotloop.io/hotloop-flow/
helm install line3-flows hotloop-flow/hotloop-flow
```

That's the whole install, on any cluster. The chart generates the admin password
and the credential secret on the first install and stores both in a Secret named
after the release. Read the password back with:

```bash
kubectl get secret line3-flows-auth -o jsonpath='{.data.admin-password}' | base64 -d
```

**On upgrade it reads both back off that Secret instead of making new ones.**
That's the line between an upgrade and losing every credential on the PVC. (The
generated password didn't work at all before 2.0.2: the hash the app checked was
made from a different random password than the one the chart stored. Fixed, and
a release installed with the bug is fixed in place by upgrading, without changing
the password or the credential secret.)

To choose your own instead:

```bash
helm install line3-flows hotloop-flow/hotloop-flow \
  --set auth.passwordHash="$(podman run --rm ghcr.io/hotloop-io/hotloop-flow:2.6.0 hash-password -password 'something-long')" \
  --set hotloopFlow.credentialSecret="$(openssl rand -hex 32)"
```

`auth.password` takes a plaintext password and hashes it at install time, if
you'd rather. `auth.passwordHash` wins when both are set.

### One instance, one pod

Multi-instance means multiple **releases**, not multiple replicas, and the chart
pins `replicaCount: 1` on purpose. Flow holds flow state and open connections to
brokers and PLCs, so two pods behind one Service would both subscribe and both
write, and your historian would quietly get everything twice. Need more? Install
a second release with its own flows.

### Resources

Presets, not raw numbers. `resources.preset` is `small` (the default), `medium`,
`large` or `custom`:

| Preset | Requests | Limits |
|---|---|---|
| `small` | 10m CPU, 64Mi | 500m CPU, 256Mi |
| `medium` | 50m CPU, 128Mi | 1 CPU, 512Mi |
| `large` | 200m CPU, 512Mi | 2 CPU, 2Gi |

Idle, the process holds about 16.6 MiB resident (method and command in
[docs/bench/](bench/README.md#memory)), so `small` schedules on an edge node with
64Mi to spare.

### Network modes

This is why Flow earns a place on a plant floor at all. A flow engine that can
only see what the cluster routes to it can't inventory an OT segment.

| `network.mode` | What it gets | Instances per node |
|---|---|---|
| `cluster` | ClusterIP. Whatever the cluster routes. The default, and the right answer unless you need L2. | unlimited |
| `host` | The node's interfaces, ARP table and broadcast domain. | one, the port is the node's |
| `macvlan` | Its **own MAC and IP** on the target VLAN, right on the segment with the PLCs. Needs Multus. | unlimited |

`macvlan` is the interesting one: safe to run several of on one node **and** on
the OT segment, with no port collisions because every instance has its own
address. Set `network.macvlan.master` to the node's interface on that VLAN and
fill in `network.macvlan.ipam`.

### What the pod is allowed to do

Distroless nonroot as uid 65532, read-only root filesystem, no privilege
escalation, `RuntimeDefault` seccomp, every capability dropped. Discovery
included: `scan` only makes TCP connections, which need no capability, and
finishes the handshake instead of leaving half-open connections on a PLC.

### Signing in from the EmberCORE dashboard

When the dashboard deploys Flow from the App Store it hands the chart its public key (`embernetSignin.publicKey`), and from then on the dashboard is the way in. It puts a token it signed in `X-Embernet-Assertion` on every request it proxies, the editor's websocket included, and Flow checks it with that key: the signature (ES256), the issuer (`embernet-dashboard`), that it was made for this app in this tenant (the audience is `<tenant>/<release>`, so a token for another app opens nothing here), and its expiry (about a minute, so there is nothing to revoke and a role changed in the dashboard lands on the next request).

The permission comes from the tenant's role map in the dashboard: `*` is full access, `read` is read-only (the flows, the palette, the settings and the status, and nothing that changes them, no deployment log and no inject). A role that is neither, or none, gets no access. Someone signed in this way shows in the deployment log and the audit trail as `dashboard:<email>`.

Off unless the key is set. With no key Flow ignores the header and behaves as it always did. With a key, a bad token is refused outright and nothing else on the request rescues it. The admin login and bearer tokens keep working beside it, which is the break-glass for staff and automation, and the editor shows "Open this from the EmberCORE dashboard" instead of its own sign-in form (`embernetSignin.hideLoginForm`).

On a pod you run yourself, mount the PEM and set `HOTLOOP_FLOW_ASSERTION_PUBLIC_KEY_FILE` and `HOTLOOP_FLOW_ASSERTION_AUDIENCE`; Flow refuses to start with a key it cannot read or no audience.

### The values you'll actually touch

| Value | Default | What it's for |
|---|---|---|
| `image.tag` | the chart's app version | Pin it. The explicit tag is the update mechanism. |
| `persistence.size`, `persistence.storageClass` | `2Gi`, cluster default | Where `/data` lives. |
| `store.context.persist`, `store.context.flushInterval` | on, `30s` | Context across restarts, and how long a changed value waits before it's written. A kill -9 loses that much. |
| `store.durable.maxBytes`, `store.durable.ackFlush` | 256 MiB, `100ms` | The disk quota for durable wires, and how long a delivered message waits to be marked done. |
| `store.synchronous` | `normal` | `full` fsyncs every commit. Stronger against a power cut, harder on the card. |
| `trace.enabled`, `trace.perSecond` | on, `1000` | Message tracing, the thing behind clicking a debug message to see where it went. `perSecond` caps new traces a second so a fast line isn't taxed for it; `0` traces every message. |
| `trace.traces`, `trace.maxHops`, `trace.maxMessageBytes`, `trace.maxBytes` | `256`, `512`, `16384`, `16777216` | How much tracing keeps, in memory, and never more. A restart starts the list over. |
| `trace.otlp.endpoint` | empty | An OpenTelemetry collector's OTLP/HTTP address, like `http://collector:4318`. Empty means no export. `trace.otlp.serviceName` and `trace.otlp.interval` go with it. |
| `trace.otlp.headersSecret.name`, `.key` | empty, `headers` | A Secret holding the collector's headers as `Name=value` pairs separated by commas, for a hosted collector's API key. It reaches the pod as an environment variable and never touches the ConfigMap. |
| `history.retain` | `100` | How many deployment records are kept under `/data/deployments`. `0` keeps every one, which is a decision about disk, since each record is a copy of the flow file. |
| `history.git.url` | empty | Mirror every deploy to a git repository as a commit from whoever deployed. Empty is off. `history.git.branch`, `.path`, `.emailDomain` and `.username` go with it, and left empty they get the binary's own defaults. |
| `history.git.passwordSecret.name`, `.key` | empty, `password` | A Secret holding the access token for that repository. It reaches the pod as `HOTLOOP_FLOW_GIT_PASSWORD` and never touches the ConfigMap. |
| `audit.maxBytes`, `audit.keep` | 16 MiB, `4` | When `/data/audit.log` rotates, and how many rotated files stay beside it. |
| `tests.gate`, `tests.timeout` | off, `2m` | `true` refuses a deploy whose flows fail `/data/flows.test.yaml`. Off by default, because a gate nobody asked for fails a deploy at 3 AM for a reason nobody on shift has heard of. |
| `discovery.enabled`, `discovery.allowedCIDRs` | off, empty | The scan nodes. Enabled with an empty list refuses to start. |
| `exec.enabled`, `exec.allowedCommands` | off, empty | The exec node. Same rule. |
| `files.allowedPaths` | empty | Extra trees the file nodes may reach beyond `/data`. |
| `secrets.mounts` | empty | Secrets to mount read-only, each `{secretName, mountPath}`, for a tls-config's files and `ew_credentialFiles`. Each path joins the secret scope and never the file nodes'. |
| `plugins.enabled` | off | [Plugins](plugins.md). Off renders nothing. On, with `plugins.trustedKeys` empty, every plugin is refused. |
| `plugins.trustedKeys`, `plugins.grants` | empty | The publisher keys to believe, and what each plugin may use by name (`clock`, `random`, `context.flow`, `context.global`). A flow using a plugin that asks for more fails the deploy. |
| `plugins.maxMemoryMiB`, `plugins.maxTimeout` | `64`, `5s` | The most memory and the longest time any one plugin node can be given. |
| `plugins.volume`, `plugins.mountPath`, `plugins.dir` | empty, `/etc/hotloop-flow/plugins`, empty | How the plugin files arrive. No volume: copy them into `plugins/` on the data volume. A volume is any Kubernetes volume source (an existing claim, a ConfigMap, a Secret), mounted read-only at `mountPath`, and `dir` follows it. |
| `metrics.serviceMonitor.enabled` | off | Prometheus Operator scraping. |
| `ingress.enabled` | off | If you want it reachable from outside the cluster. |

Every `trace.*`, `store.*`, `history.*`, `audit.*` and `tests.*` value has to land in the field of the same name in
`config.yaml`, and CI proves it: it renders the chart, takes the config out of the
ConfigMap and loads it with the binary's own loader, which refuses keys it doesn't
know. A default render has to equal the binary's own defaults too. I got tired of
settings that say one thing in `values.yaml` and do another in the process.

The Service listens on 8080 and forwards to the binary's 1880. Everything else is
documented next to the value in
[`charts/hotloop-flow/values.yaml`](../charts/hotloop-flow/values.yaml).

---

## Podman with Quadlet

For a single box that isn't running Kubernetes: an industrial PC, a gateway, a
VM next to the historian. Quadlet turns a short unit file into a systemd service
that runs the container, so Flow starts at boot, restarts when it dies and logs
to the journal like everything else on the box.

You need a Podman that ships Quadlet (`/usr/libexec/podman/quadlet` exists). The
steps below were run exactly as written, as root, on Podman 4.9.3 with the 2.0.5
image: it started, took a login, took a deploy, and came back after
`systemctl restart` with the flows and the encrypted credentials intact.

**1. Keep the two secrets out of the unit file.** Podman secrets live outside it,
so the unit can be copied around, committed and reviewed without carrying
anything that matters.

```bash
printf '%s' "$(podman run --rm ghcr.io/hotloop-io/hotloop-flow:2.6.0 \
  hash-password -password 'something-long')" | podman secret create hotloop-flow-admin-hash -
printf '%s' "$(openssl rand -hex 32)" | podman secret create hotloop-flow-credential-secret -
```

The `printf '%s' "$(...)"` strips the trailing newline. Without it the newline
becomes part of the secret, and a hash with a newline on the end doesn't match
anything.

**2. The unit.** `/etc/containers/systemd/hotloop-flow.container`:

```ini
[Unit]
Description=HotLoop Flow
Wants=network-online.target
After=network-online.target

[Container]
ContainerName=hotloop-flow
Image=ghcr.io/hotloop-io/hotloop-flow:2.6.0
PublishPort=1880:1880
Volume=hotloop-flow-data:/data
Environment=HOTLOOP_FLOW_ADMIN_USER=admin
Environment=HOTLOOP_FLOW_LOG_FORMAT=json
Secret=hotloop-flow-admin-hash,type=env,target=HOTLOOP_FLOW_ADMIN_PASSWORD_HASH
Secret=hotloop-flow-credential-secret,type=env,target=HOTLOOP_FLOW_CREDENTIAL_SECRET
ReadOnly=true
NoNewPrivileges=true
DropCapability=ALL

[Service]
Restart=always
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
```

**3. Start it.**

```bash
systemctl daemon-reload
systemctl start hotloop-flow.service
journalctl -u hotloop-flow -f
```

Quadlet generates the service on `daemon-reload`, and the `[Install]` section is
what starts it at boot. There's no `systemctl enable` step: Podman's own docs say
a generated unit can't be enabled, and the generator applies `[Install]` itself.

Rootless works the same way with the file in `~/.config/containers/systemd/`,
`systemctl --user` in place of `systemctl` and `WantedBy=default.target`. Run
`loginctl enable-linger <user>` too, or systemd stops that user's services when
they log out, and Flow goes with them. That's
[Podman's](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html)
and systemd's documented behavior. The rootful unit above is the one I ran.

**Upgrading** is changing the tag in `Image=`, then `daemon-reload` and
`restart`. Flows, credentials and the secret stay where they were. Pin the tag.
`:latest` on a plant floor means you find out what changed when it breaks.

**Turning things on** is more `Environment=` lines: `HOTLOOP_FLOW_EXEC_ENABLED`
and `HOTLOOP_FLOW_EXEC_ALLOWED_COMMANDS`, `HOTLOOP_FLOW_DISCOVERY_ENABLED` and
`HOTLOOP_FLOW_DISCOVERY_CIDRS`, and the rest of the table in the
[README](../README.md#environment-overrides). For the file-only settings (session
TTL, backup generations, timeouts), mount a YAML file and point
`HOTLOOP_FLOW_CONFIG` at it.

For discovery on an OT segment, `Network=host` gives the container the box's own
interfaces. Same trade as `host` mode on Kubernetes: one instance per port.

---

## `podman run`

To try it, not to run a line on:

```bash
podman run --rm -p 1880:1880 -v hotloop-flow-data:/data \
  -e HOTLOOP_FLOW_ADMIN_USER=admin \
  -e HOTLOOP_FLOW_ADMIN_PASSWORD_HASH="$(podman run --rm ghcr.io/hotloop-io/hotloop-flow:2.6.0 hash-password -password 'something-long')" \
  -e HOTLOOP_FLOW_CREDENTIAL_SECRET="$(openssl rand -hex 32)" \
  ghcr.io/hotloop-io/hotloop-flow:2.6.0
```

Then open <http://localhost:1880>. That credential secret is new every time you
run the command, so a credential saved in one run can't be read in the next. Fine
for a look around. That's exactly why the Quadlet install keeps it in a Podman
secret.

The image is distroless nonroot with no shell in it, so there's nothing to
`exec` into, and nothing for anyone who gets code execution to pivot with.

---

## Backups

Everything worth keeping is in `/data`: `flows.json`, `credentials.json`,
`flows.json.bak.1` to `.bak.3`, the deployment log in `deployments/` (one file
per deploy), the audit trail in `audit.log`, two-factor enrollments in
`mfa.json`, and sign-in sessions in `sessions.json`. The log's records and the
enrollments are encrypted under the same secret as the credentials, and the
sessions are hashes. `git-mirror/` is only a clone, rebuilt from the repository
if it's lost. Back up the directory and the credential secret,
**separately**. The credential file is useless without the secret, which is the
point, and a backup that holds both in one place is a backup that hands both to
whoever steals it.
