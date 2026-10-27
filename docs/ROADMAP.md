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

- [x] The WASM sandbox is real and tested, but no node uses it and it isn't in
      the binary. Say so until Phase 6 makes it true
- [x] Two code comments point at a bbolt-backed context store that doesn't exist.
      Delete them
- [x] Re-measure idle and loaded memory the same way on the same box, publish the
      command with the number, and drop any figure nobody can reproduce
- [x] CVE-2025-41656 is a vendor shipping another flow engine with auth off, not
      a CVE in that engine. Fix the attribution in the README and in the `exec`
      row of `docs/compatibility.md`
- [x] Rewrite the top of the README around what Flow does. Importing existing
      flows moves to its own section further down, with the byte-exact round
      trip and `hotloop-flow import` as the evidence
- [x] Deployment docs talk about Kubernetes, Podman and Quadlet, not one
      company's App Store
- [x] Tests for the packages that have none: `api` (login, permissions, the 409
      on a stale deploy), `config` (every startup refusal the README lists) and
      `cmd` (deploy writes the flow file before it stops the old runtime), plus
      `js`, the Function node's sandbox, which had none either

### Phase 1: history, diff, rollback

Every deploy is a record: who, when, a note, the exact bytes, and what changed.
"What changed, who changed it, put it back" is the first thing anybody asks after
a line stops, and today the answer is three `.bak` files.

- [x] Deployment log: every deploy is an immutable record with the user, the
      time, an optional note, the flow bytes and the encrypted credentials as
      they stood. Retention is a setting. `GET /deployments`
- [x] Semantic diff: node by node, property by property. Added, removed,
      changed, rewired. Dragging a node around shows up as layout, never as a
      logic change. Over the API and as `hotloop-flow diff a.json b.json`
- [x] The diff works as a git difftool, so a pull request on a flow reads
      "rule 2 threshold 7.5 to 8.0" instead of a JSON hunk
- [x] Rollback: redeploys an old record as a new one, credentials included, so a
      node you deleted comes back with its broker password. History stays
      append-only
- [x] Editor: history panel, the diff drawn on the canvas, "review and deploy"
      with a note, a rollback button. A deploy conflict shows what the other
      person changed instead of a yes/no box
- [x] Flows as code: `hotloop-flow deploy` and `hotloop-flow export` for CI,
      with a deploy-only token so a pipeline never holds an admin password
- [x] Optional git mirror: every deploy pushed as a commit authored by whoever
      deployed it. Pure Go, and the binary size check still passes
- [x] Audit log: logins, failed logins, deploys, rollbacks and injects, each
      with who and from where. The deployment log is its first table
- [x] MFA (TOTP), free and on for anyone who wants it. Nothing about signing in
      safely gets gated, ever
- [x] Login rate limiting and lockout, sessions that survive a restart, and the
      websocket token sent as a subprotocol instead of in the URL. A token in
      the URL still works for existing scripts and logs a warning, because
      breaking them on an upgrade was worse than the leak; removing it waits for
      a major release

### Phase 2: flow tests

"Did my change break the line?" gets answered before the deploy, not after.
Tests are YAML next to the flows, so they diff and review the same way.

- [x] `hotloop-flow test`: the real runtime and the real nodes, in process.
      Inject a message, expect what comes out of a port within a deadline,
      expect nothing, expect an error to reach a Catch. JUnit output for CI
- [x] Virtualized I/O: an MQTT out, a database write or an HTTP request in a test
      records what it would have sent and never touches the network
- [x] Virtual clock: a five-minute Delay runs in milliseconds, and every timing
      node is proven against it
- [x] Context and credential fixtures, and assertions on context afterwards.
      One test can't lean on what another left behind
- [x] Deploy gate: with it on, a deploy that fails its tests is refused with the
      failing assertions, and the flows already running don't notice
- [x] Editor: turn a captured debug message into a test in two clicks, run the
      suite from the editor, pass and fail on the nodes

### Phase 3: tracing and replay

- [x] Click a debug message and the canvas lights up the path it took, with
      timing and the payload at every hop, through splits, joins and subflows
- [x] Bounded trace buffer. Memory stays flat under a million-message soak, and
      the number is published
- [x] Replay a captured message into a draft of the flow and compare the output
- [x] A captured trace becomes a Phase 2 test
- [x] OpenTelemetry export, proven against a real collector

### Phase 4: durable state

A historian going down for an hour shouldn't cost you an hour of readings.

- [x] Persistent context in SQLite, pure Go with no cgo, with the atomic
      operations (compare-and-swap, increment, update). One store for context now
      and durable queues next, not two. Today flow and global context are gone on
      every restart, which is the gap most likely to bite somebody first.
      Written behind in one transaction per thirty seconds, and a value set to what
      it already was writes nothing
- [x] Durable wires: mark a wire durable and its queue spills to that same store
      instead of blocking or dropping. Survives a restart, keeps order, honors a
      disk quota, and the depth is a metric. Marked in the flow file with
      `ew_durable`, so the round trip stays byte for byte. Double-click a wire in
      the editor to mark it
- [x] Store-and-forward proven the ugly way: kill the real InfluxDB for ten
      minutes under steady input, bring it back, count rows. None missing, none
      doubled. Then kill -9 the runtime mid-queue and count again. The script, the
      counts and the disk writes per hour are in [durable/](durable/README.md)
- [x] Proven on a real Pi 5: memory, start time, and sustained throughput, in
      [docs/bench](bench/README.md#results-on-a-raspberry-pi-5), with the
      throttling it showed
- [ ] Proven on a Pi 4 and a Zero 2 W, and SD card writes per hour on a card,
      because a context store that chews through an SD card is a bug

### Phase 5: the plant floor

`scan` finds a PLC today and nothing in the palette can talk to it. That's the
biggest hole in a product built for plants.

- [x] Modbus TCP read and write
- [x] OPC UA read, subscribe and write
- [x] S7 read and write
- [x] Sparkplug B, as an edge node and as a host
- [x] EtherNet/IP read and write
- [x] Every one of them tested against a real device or the real simulator,
      including what happens when the device goes silent and when it comes back.
      Modbus TCP is done against pymodbus, OPC UA against Microsoft's OPC PLC,
      silent and switched-off both, Sparkplug B against a real Mosquitto with
      the edge node cut off and the host restarted, S7 against python-snap7's
      server, and EtherNet/IP against libplctag's `ab_server`. All simulators.
      No Siemens CPU or Logix controller has been in front of any of it
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

### Phase 8: the editor, rewritten in Astro

Done, and moved up. It was meant to go last, and I moved it ahead of Flow's first
screenshots, because nobody should see the first ones in an editor that looks like
it came from a different company.

Every HotLoop project is getting one modern UI, and this editor was the one that
didn't match. It was hand-built vanilla TypeScript on native SVG, which was the
right call when the goal was a small static binary with nothing to audit. It is now
Astro with Svelte islands, the same stack as Cascade's editor, on the same HotLoop
tokens, light and dark.

- [x] A design pass first, before a line of Astro. What the editor should look
      and feel like next to the rest of HotLoop, settled on paper and in the
      shared tokens, so the rewrite is building a thing somebody already decided
      on instead of deciding as it goes
- [x] The editor rebuilt in Astro with the canvas, dialogs, history, review,
      tests, tracing, search and groups all carried over, and none of them
      allowed to get worse. The browser tests in `web/e2e` are the bar: they pass
      against the new editor unchanged
- [x] Still embedded in the one static binary, still no network needed on an
      edge box, still inside the binary size budget, and the license gate still
      covers everything that ships in the bundle

The canvas and the graph under it did not get rewritten. They moved into `web/src/lib`
as the TypeScript they were, with a new skin, so wiring, dragging, undo and the byte
for byte flow file round trip are the code that was already tested. Everything
around them is Svelte.

Flow stays its own product through all of this. It shares the look with HotLoop,
not the code, and it is never embedded in HotLoop.

---

## Everyday gaps

These run alongside the phases. Nobody picks a flow engine because it has them,
but plenty of people drop one that doesn't.

- [x] Partial deploy: only changed nodes and their wires restart. Today every
      save drops every MQTT session and every in-flight message, which teaches
      operators not to deploy during a shift
- [x] JSONata, proven against the published examples, since it's the most common
      thing an imported flow trips over
- [x] Cron-style inject
- [x] Link Call and Link Out's return mode
- [x] Join by timeout and reduce, batch by time, split streaming
- [x] MQTT v5 and last will
- [x] TLS on TCP, cookies and multipart on HTTP
- [x] Editor: copy and paste, import and export a selection, subflow and group
      editing, search across flows, debug filtering
- [x] Browser tests for the editor in CI, and a click-through of every node
      dialog
- [x] Editor gaps the first pass left: add a configuration node from the
      palette (the dialog told you to and you couldn't), turn a selection into a
      subflow, rename and delete a flow, and mark a wire durable by
      double-clicking it
- [x] A way out for a message a durable wire's target refuses forever: an opt-in
      attempt limit per wire. After that many refusals the message goes to the
      flow's Catch as an error with the message attached and the wire moves on.
      Off by default, so a wire still holds on and retries exactly as before
- [x] Helm values for `trace.*`, checked in CI by loading the rendered config
      with the binary's own loader. `store.*` was already there from the
      durable work and now has the same check
- [x] The gaps those left, filled: Alt-click deletes a wire (the click never
      reached it because the canvas holds the pointer, same as dragging one did),
      the chart has `history.*`, `audit.*` and `tests.*` and CI proves they reach
      the config, selection to subflow takes the flow's own configuration nodes
      and the environment variables its nodes read, and deleting a flow or a node
      takes its id out of the Catch, Status and Complete scopes that named it
- [x] A node pointing at a config node that isn't in the file (a flow deleted
      with its own broker while a subflow, a hand edit or an import still names
      it) is reported by `hotloop-flow import`, in the editor on load and on
      deploy, and in the deploy answer, and the node is a start failure instead
      of a node that quietly never works
- [x] The gaps that one left: a config reference that names a tab, a subflow, a
      group, a plain node or the wrong kind of config node is reported and
      fails to start like one that names nothing, a rollback puts the file's
      warnings in the editor log once and not twice, and `hotloop-flow import`
      writes its report to a writer like `deploy` does, so its tests stop
      swapping out standard output
- [x] gos7 wrote a raw timeout line to Go's global logger on every network
      error (`tcpclient.go`, the `log.Printf` in `Send`) and had no way to turn
      it off. Flow carries gos7 in `third_party/gos7` now, with that one line
      sent through the handler's `Logger`, which Flow leaves unset. Nothing else
      in the library is touched, and `third_party/gos7/HOTLOOP_PATCH.txt` says
      when the copy can go. Upstream still has the line; I didn't ask them to
      take the change
- [x] The durable wire purge test could count one more dropped than it had
      sampled as queued. The test was wrong, not the queue: it waited for 28 of
      30 messages on disk and then held the purge to exactly what it had seen,
      while the spool was still counting the last ones. How many of the 30 are
      on disk when the wire goes depends on timing, so it holds the purge to
      what it saw as a floor and to the 30 sent as a ceiling now
- [x] The durable wire kill test passed locally and failed on a slower runner.
      The queue counted a message as waiting before it was on disk, so a copy
      taken the moment the count hit 100 could be one short. It counts after the
      commit now
- [x] Secrets by reference: a node reads a password from a mounted Kubernetes
      Secret or file, so it never lives in the flow file at all
- [ ] Bring your flows: `hotloop-flow import` keeps reporting exactly what will
      and won't run before anything is deployed

## Later

- [ ] Active/standby: a lease, context in Postgres, and a failover that is
      proven not to double-write to a PLC. Not before durable wires and
      persistent context, because a split brain writing to equipment is worse
      than a restart

## Decided

Every question this list used to carry is answered. They stay here with the
answers, because the reasons are what stops somebody reopening them.

Decided 2026-10-03:

- **Industrial nodes get built in Flow,** on the same open source libraries the
  HotLoop drivers use. HotLoop's drivers stay HotLoop's. Flow keeps its own license, which was
  Apache-2.0 when this was decided and is HotLoop's Business Source License 1.1
  from 2.2.0 on.
- **Flow stays its own product and is never embedded in HotLoop.** It runs next
  to HotLoop and talks to HotLoop's API from the outside, which is what the
  Phase 5 HotLoop nodes do, and that's the whole relationship.
- **The README leads with what Flow does.** "Wire the plant. Review it like
  code." Importing existing flows lives further down in "Bring your flows", and
  nothing at the top compares Flow to anything else. Flow stands on its own.
- **Persistent context is SQLite from the start,** pure Go with no cgo. One
  store for context now and durable queues later, not a file store first and a
  second store after it.
- **The workflow automation product is its own thing** and doesn't share Flow's
  engine. History, diff and the rest are still engine packages here, because the
  API, the CLI, the editor and git all have to get the same answer.

Decided before Phase 1 started:

- **MFA and the audit log ship in Phase 1,** ahead of the fun stuff. Both did.
- **The byte-exact round trip of existing flow files stays a hard rule,** even
  with types and tests in the file. New data goes in keys other tools ignore,
  and the round-trip test covers files that carry it.
- **A browser test harness for the editor is fine as a CI-only dev
  dependency.** It's Playwright in `web/e2e`, and it found a bug on its first
  run.
- **Benchmarks are absolute numbers only,** each with the command that made it.
  A side-by-side table moves every time the box changes, and it ends up
  describing the other engine more than this one.
