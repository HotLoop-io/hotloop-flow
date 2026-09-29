# Roadmap

What HotLoop Flow is going to be, in the order it gets built. If it isn't on this
list it isn't getting done, so put it on the list.

**None of this starts until HotLoop's own roadmap is done.** Flow waits its turn.

## What Flow is for

A visual flow engine for the plant floor. Every deploy is diffed, tested and one
click from rollback. When something goes sideways at 3 AM you can see exactly
which path a message took and what it looked like at every hop. It runs in one
static binary small enough for a Pi, and it doesn't fall over when a sensor gets
chatty.

It also reads your existing `flows.json` unchanged. That's a feature, not the
point.

The rules every item below ships under:

- No stubs. If it isn't built, tested against the real thing and working, it
  isn't ticked.
- Anything that can command equipment goes through a gate that can say no, and
  says why.
- Nothing is thrown away quietly. Every drop, refusal and overflow is counted and
  shows up in the metrics.
- The flow file keeps loading and saving byte for byte. New data goes in keys
  that other tools ignore, and the round-trip test covers files that carry it.

---

### Phase 0: tell the truth

The README makes claims the code doesn't back. That gets fixed before anything
new is promised, because a README that lies once gets read like it lies every
time.

- [ ] The WASM sandbox is real and tested, but no node uses it and it isn't in
      the binary. Say so until Phase 6 makes it true
- [ ] Two code comments point at a bbolt-backed context store that doesn't exist.
      Delete them
- [ ] Re-measure idle and loaded memory the same way on the same box, publish the
      command with the number, and drop any figure nobody can reproduce
- [ ] CVE-2025-41656 is a vendor shipping another flow engine with auth off, not
      a CVE in that engine. Fix the attribution in the README and in the `exec`
      row of `docs/compatibility.md`
- [ ] Rewrite the top of the README around what Flow does. Importing existing
      flows moves to its own section further down, with the byte-exact round
      trip and `hotloop-flow import` as the evidence
- [ ] Deployment docs talk about Kubernetes, Podman and Quadlet, not one
      company's App Store
- [ ] Tests for the packages that have none: `api` (login, permissions, the 409
      on a stale deploy), `config` (every startup refusal the README lists) and
      `cmd` (deploy writes the flow file before it stops the old runtime)

### Phase 1: history, diff, rollback

Every deploy is a record: who, when, a note, the exact bytes, and what changed.
"What changed, who changed it, put it back" is the first thing anybody asks after
a line stops, and today the answer is three `.bak` files.

- [ ] Deployment log: every deploy is an immutable record with the user, the
      time, an optional note, the flow bytes and the encrypted credentials as
      they stood. Retention is a setting. `GET /deployments`
- [ ] Semantic diff: node by node, property by property. Added, removed,
      changed, rewired. Dragging a node around shows up as layout, never as a
      logic change. Over the API and as `hotloop-flow diff a.json b.json`
- [ ] The diff works as a git difftool, so a pull request on a flow reads
      "rule 2 threshold 7.5 to 8.0" instead of a JSON hunk
- [ ] Rollback: redeploys an old record as a new one, credentials included, so a
      node you deleted comes back with its broker password. History stays
      append-only
- [ ] Editor: history panel, the diff drawn on the canvas, "review and deploy"
      with a note, a rollback button. A deploy conflict shows what the other
      person changed instead of a yes/no box
- [ ] Flows as code: `hotloop-flow deploy` and `hotloop-flow export` for CI,
      with a deploy-only token so a pipeline never holds an admin password
- [ ] Optional git mirror: every deploy pushed as a commit authored by whoever
      deployed it. Pure Go, and the binary size check still passes
- [ ] Audit log: logins, failed logins, deploys, rollbacks and injects, each
      with who and from where. The deployment log is its first table
- [ ] MFA (TOTP), free and on for anyone who wants it. Nothing about signing in
      safely gets gated, ever
- [ ] Login rate limiting and lockout, sessions that survive a restart, and no
      more tokens in query strings where access logs can keep them

### Phase 2: flow tests

"Did my change break the line?" gets answered before the deploy, not after.
Tests are YAML next to the flows, so they diff and review the same way.

- [ ] `hotloop-flow test`: the real runtime and the real nodes, in process.
      Inject a message, expect what comes out of a port within a deadline,
      expect nothing, expect an error to reach a Catch. JUnit output for CI
- [ ] Virtualized I/O: an MQTT out, a database write or an HTTP request in a test
      records what it would have sent and never touches the network
- [ ] Virtual clock: a five-minute Delay runs in milliseconds, and every timing
      node is proven against it
- [ ] Context and credential fixtures, and assertions on context afterwards.
      One test can't lean on what another left behind
- [ ] Deploy gate: with it on, a deploy that fails its tests is refused with the
      failing assertions, and the flows already running don't notice
- [ ] Editor: turn a captured debug message into a test in two clicks, run the
      suite from the editor, pass and fail on the nodes

### Phase 3: tracing and replay

- [ ] Click a debug message and the canvas lights up the path it took, with
      timing and the payload at every hop, through splits, joins and subflows
- [ ] Bounded trace buffer. Memory stays flat under a million-message soak, and
      the number is published
- [ ] Replay a captured message into a draft of the flow and compare the output
- [ ] A captured trace becomes a Phase 2 test
- [ ] OpenTelemetry export, proven against a real collector

### Phase 4: durable state

A historian going down for an hour shouldn't cost you an hour of readings.

- [ ] Persistent context: a file store first, then Postgres, with the atomic
      operations (compare-and-swap, increment, update) on both. Today flow and
      global context are gone on every restart, which is the gap most likely to
      bite somebody first
- [ ] Durable wires: mark a wire durable and its queue spills to disk instead of
      blocking or dropping. Survives a restart, keeps order, honors a disk quota,
      and the depth is a metric
- [ ] Store-and-forward proven the ugly way: kill the real InfluxDB for ten
      minutes under steady input, bring it back, count rows. None missing, none
      doubled. Then kill -9 the runtime mid-queue and count again
- [ ] Proven on a real Pi 4 and a Zero 2 W: memory, start time, sustained
      messages per second, and SD card writes per hour, because a context store
      that chews through an SD card is a bug

### Phase 5: the plant floor

`scan` finds a PLC today and nothing in the palette can talk to it. That's the
biggest hole in a product built for plants.

- [ ] Modbus TCP read and write
- [ ] OPC UA read, subscribe and write
- [ ] S7 read and write
- [ ] Sparkplug B, as an edge node and as a host
- [ ] EtherNet/IP read and write
- [ ] Every one of them tested against a real device or the real simulator,
      including what happens when the device goes silent and when it comes back
- [ ] HotLoop nodes: entity changes in, entity state, call a script, apply a
      recipe. Every command goes through HotLoop's write gate and lands in its
      logbook as `flow:<node id>`, and a write the gate refuses arrives at a
      Catch node with the gate's reason
- [ ] When HotLoop is there, Flow's equipment writes go through HotLoop's gate
      instead of straight to the PLC

### Phase 6: plugins without npm

- [ ] A `wasm` node on the host that already exists: hard memory ceiling, a
      timeout, and a guest that blows past either traps without taking anything
      else down
- [ ] Plugin format: the module, a descriptor so the editor renders its dialog,
      and a signature. Unsigned or tampered plugins refuse to load and say why
- [ ] Capabilities: a plugin gets no host access unless its manifest asks and
      the operator grants it. Asking for something not granted fails the deploy
- [ ] A worked example in Rust and one in TinyGo, built in CI

### Phase 7: typed wires and MCP

- [ ] Schema validation on the `json` node, the first slice
- [ ] Optional schemas on ports. A mismatch the editor can see is refused at
      deploy; one it can't goes to a Catch node with the path of the bad field,
      and to a metric
- [ ] Types drawn on the wires in the editor
- [ ] Flows as MCP tools: a tool node declares a name, a description and an
      input schema, and Flow serves `/mcp`. An agent calling a flow gets the
      same permissions and the same audit log as a person
- [ ] An MCP client node, so a flow can call tools on other servers, HotLoop's
      included

---

## Everyday gaps

These run alongside the phases. Nobody picks a flow engine because it has them,
but plenty of people drop one that doesn't.

- [ ] Partial deploy: only changed nodes and their wires restart. Today every
      save drops every MQTT session and every in-flight message, which teaches
      operators not to deploy during a shift
- [ ] JSONata, proven against the published examples, since it's the most common
      thing an imported flow trips over
- [ ] Cron-style inject
- [ ] Link Call and Link Out's return mode
- [ ] Join by timeout and reduce, batch by time, split streaming
- [ ] MQTT v5 and last will
- [ ] TLS on TCP, cookies and multipart on HTTP
- [ ] Editor: copy and paste, import and export a selection, subflow and group
      editing, search across flows, debug filtering
- [ ] Browser tests for the editor in CI, and a click-through of every node
      dialog
- [ ] Secrets by reference: a node reads a password from a mounted Kubernetes
      Secret or file, so it never lives in the flow file at all
- [ ] Bring your flows: `hotloop-flow import` keeps reporting exactly what will
      and won't run before anything is deployed

## Later

- [ ] Active/standby: a lease, context in Postgres, and a failover that is
      proven not to double-write to a PLC. Not before durable wires and
      persistent context, because a split brain writing to equipment is worse
      than a restart

## Open decisions

Asked when this work starts, not before. Each has the answer I'd pick today.

- **Industrial nodes: build them in Flow on the same open source libraries the
  HotLoop drivers use, or move HotLoop's drivers into a shared Apache module?**
  Build them in Flow. Flow stays Apache and HotLoop's drivers stay HotLoop's.
- **Flow inside HotLoop IoT, Edge and Gateway, or next to it?** Next to it first,
  talking to HotLoop's API. Embedding is legally fine later (Apache code can go
  into HotLoop with its NOTICE), but it's a bigger call.
- **Is Flow's engine the shared engine for the workflow automation product?**
  Yes in principle. History, tests and tracing get built as engine packages, not
  editor features, so the other product gets them for free.
- **MFA and the audit log in Phase 1, ahead of the fun stuff?** Yes.
- **Byte-exact round trip of existing flow files stays a hard rule even with
  types and tests in the file?** Yes.
- **A browser test harness for the editor, as a CI-only dev dependency?** Yes.
  The editor has no tests, and Phases 1 and 2 put real UI in it.
- **Persistent context on a Pi: a file store or SQLite?** File store first,
  SQLite when durable wires need a real queue, and then both share it.
- **Benchmarks: absolute numbers, or side by side with another engine?**
  Absolute numbers only, each with the command that made it. A side-by-side
  table moves every time the box changes, and it ends up describing the other
  engine more than this one.
