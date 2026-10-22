# Plugins without npm

A Node-RED community node is an npm package. It needs Node.js on the box, and
once it's installed it runs inside the runtime's own process with everything that
process can reach. Flow doesn't run those and never will, so here is what it does
instead: a plugin is a WebAssembly module in a sandbox with a memory ceiling and a
timeout, it comes with a descriptor the editor draws its dialog from, it's signed,
and it gets no host access unless it asks for it and you grant it.

What that means for you: you can read a plugin's whole contract in one JSON file
before it runs, the box refuses anything that isn't signed by a key you chose, and
the worst a buggy plugin can do is fail its own messages.

## What a plugin is

A directory under `<data.dir>/plugins/` (or `plugins.dir`), named for the plugin,
with three files:

```
plugins/
  moving-average/
    plugin.wasm     the module
    plugin.json     the descriptor: name, dialog, capabilities, limits
    plugin.sig      an ed25519 signature over both
```

Each plugin becomes one node type, `wasm-<name>`, in the Function section of the
palette. Plugins are read when Flow starts, so adding one or changing one is a
restart. Nothing is read at message time.

### The module

The ABI is the one in the [README](../README.md#two-sandboxes-and-only-one-of-them-runs-flows-yet):
`hotloop_flow_process` and `hotloop_flow_alloc` are required, `hotloop_flow_free`
is optional, and everything crosses as JSON. A guest gets
`{"msg": {...}, "config": {...}}` and returns
`{"send": [[msgs for output 0], [msgs for output 1]], "status": {...}, "error": "...", "logs": [...]}`.
A message property that isn't plain JSON, such as the response handle an HTTP In
node attaches or a binary buffer, isn't given to the guest. The node holds it and
puts it back on every message the guest sends, unless the guest set that property
itself, so a plugin can sit between an HTTP In and an HTTP Response.
`config` is what the node's dialog holds. An `error` fails the message to a Catch
node, and whatever it sent before that is still sent.

The module may import WASI and, if it declared a context capability, the two
context functions below. Anything else it imports is refused when the plugin
loads, and the refusal names the import.

### The descriptor

`plugin.json` is read strictly. A field Flow doesn't know is a refusal, so a
misspelled `"capabilites"` is caught instead of becoming a plugin that quietly
asked for nothing.

| Field | |
|---|---|
| `format` | `1`. Required. |
| `name` | Lowercase letters, digits and hyphens, up to 40, starting with a letter. Must match the directory name, because it's what `plugins.grants` and the flow file use. |
| `version` | Required. Shown in the node's help and in `GET /plugins`. |
| `label`, `description`, `help` | The palette label, one line, and Markdown for the info sidebar. |
| `inputs`, `outputs` | Inputs must be 1. Outputs 0 to 16, default 1. |
| `props` | The dialog. The same property declarations the built-in nodes use, with the kinds `string`, `text`, `number`, `bool`, `select`, `json` and `list`. Credentials, config-node references and the code-editor kinds are refused: a plugin has no business with them. |
| `capabilities` | The host access it asks for. |
| `limits` | `memoryMiB` and `timeoutMs` it wants by default. |

The editor renders the dialog from `props` with no code of the plugin's in the
browser. Every plugin node also gets a name, a memory limit and a timeout field.
Those three names are reserved.

## Limits, and what a trap looks like

Every plugin node has its own memory ceiling and its own timeout. The plugin's
descriptor sets the defaults (16 MiB and one second when it says nothing), a node
can change them in its dialog, and `plugins.maxMemoryMiB` (64) and
`plugins.maxTimeout` (5s) are what the box will allow at all. A node asking for
more than the box allows fails the deploy and says which setting to raise.

A guest that grows past its ceiling traps. A guest still running at its timeout
is stopped. Either way the message goes to the nearest Catch node, the instance is
thrown away so the next message starts clean, and no other node is touched. What
the Catch node sees in `msg.error.message`:

```
plugin hog: wasm guest exceeded its memory limit (calling hotloop_flow_process): wasm error: out of bounds memory access
plugin spin: wasm guest timed out while calling hotloop_flow_process
```

The ceiling is held up front: a node with a 64 MiB ceiling accounts 64 MiB of the
Go heap while its instance exists, whether the guest touches it or not, because
growing a buffer by copying it would let the host briefly hold several times the
ceiling. The test that proves it measures what a call allocated in all (256 MiB
for a 256 MiB ceiling) and that the heap is back where it started after the trap.
So set ceilings to what a plugin needs, not to the biggest number you can type.

## Capabilities

No capability, no access. Each one is wired in only when the descriptor asks for
it and `plugins.grants` gives it to that plugin by name.

| Capability | What it gives | Without it |
|---|---|---|
| `clock` | The real wall and monotonic clocks. | The guest sees a stand-in clock that starts at 2022-01-01 and has nothing to do with the plant's. |
| `random` | The operating system's random bytes. | The same fixed sequence every time. |
| `context.flow` | Read and write the flow context of the node's own flow. | The context functions don't exist for it. |
| `context.global` | Read and write the global context. | Same. |

```yaml
plugins:
  trustedKeys: ["ed25519:..."]
  grants:
    stamp: [clock]
    remember: [context.flow]
```

Asking for something that isn't granted does not fail at message time. It fails
the deploy: the flow is refused with a 422 before anything is written, the running
flows never notice, and the message names the node and the line to add:

```
the deploy was refused because of its plugins: node stamp (n1) is a wasm-stamp: the plugin asks for host access the operator has not granted: clock. Add it to plugins.grants.stamp, or remove the capability from the plugin
```

A rollback is a deploy, so a rollback to flows that use a plugin whose grant has
since been taken away is refused the same way. A plugin that is installed but not
granted still shows in the palette, with its capabilities in its help.

### The context functions

A guest that declares a context capability may import these from the module
`hotloop_flow`. Values cross as JSON.

```
context_get(scope i32, keyPtr i32, keyLen i32, outPtr i32, outCap i32) -> i32
context_set(scope i32, keyPtr i32, keyLen i32, valPtr i32, valLen i32) -> i32
```

Scope is `0` for flow and `1` for global. `context_get` returns the length of the
JSON and writes it only if it fits in `outCap`, so a guest that gets back more than
it offered retries with a bigger buffer. A key that isn't there is `-1`, and `-2`
means denied: the scope wasn't granted, the key or the JSON was bad, or the store
said no. A guest can't read a scope it wasn't granted by trying.

## Signing, and trusting a publisher

A plugin is signed with an ed25519 key. The signature covers both files, not just
the module, because the descriptor is where a plugin asks for access. Sign only
the module and anyone with write access to the directory could add a capability to
a signed plugin. The signed message is:

```
hotloop-flow-plugin-v1
module sha256 <hex of plugin.wasm>
descriptor sha256 <hex of plugin.json>
```

Nothing is looked up when a plugin loads: no network, no transparency log, no
certificate chain. The check is the signature, the key it names, and your list of
keys. That's a deliberate choice for a box on a plant network that may have no
route out. It also means there's no revocation service, so revoking a key is
removing it from `plugins.trustedKeys` and restarting.

### As a publisher

```bash
hotloop-flow plugin keygen -out acme            # acme.key (private) and acme.pub
hotloop-flow plugin sign -key acme.key plugins/moving-average
hotloop-flow plugin verify -trust acme.pub plugins/moving-average
```

`keygen` writes a PKCS #8 private key and a PKIX public key as PEM, the same files
`openssl genpkey -algorithm ed25519` and `openssl pkey -pubout` make, and refuses
to overwrite either. It prints the key id and the one-line form of the public key.
Keep the private key out of git. The examples in this repo are signed in CI with a
key made for that run and thrown away with the runner. No key is committed.

### As an operator

On the Helm chart these are `plugins.trustedKeys`, `plugins.grants` and
`plugins.volume`, in [deploying.md](deploying.md).

You trust a publisher by putting their public key in `plugins.trustedKeys`, or in
`HOTLOOP_FLOW_PLUGIN_TRUSTED_KEYS`, comma separated. Each entry is the
`ed25519:<base64>` line, a PEM public key, or the path of a file holding one, which
is how a mounted Secret or ConfigMap gets in:

```yaml
plugins:
  trustedKeys:
    - ed25519:/iYdLE7lAGb2YFglPYjhoPItgdiQ9xEPVMqQNrD8Hqg=
    - /etc/hotloop-flow/keys/acme.pub
```

With no trusted keys, every plugin is refused. A key you can't parse is a startup
error. Trust is per key and not per plugin, so trusting a publisher is trusting
everything they sign. What a plugin may then use is a separate decision, made in
`plugins.grants` by name.

## When it refuses

A plugin that doesn't load is logged at startup, listed with its reason in
`GET /plugins`, absent from the palette, and named in the refusal when a flow that
uses it is deployed. One bad plugin never stops the rest of Flow from starting.
`hotloop-flow plugin verify` runs the same checks, so a publisher sees what an
operator would.

| What's wrong | What it says |
|---|---|
| No `plugin.sig` | `unsigned: plugin.sig is missing` |
| Module or descriptor edited after signing | `signature does not match: plugin.wasm or plugin.json was changed after it was signed with key <id>, or the signature is damaged` |
| Signed by a key you don't trust | `signing key is not trusted: it was signed with key <id>, which is not in plugins.trustedKeys` |
| No keys configured at all | `signing key is not trusted: no trusted keys are configured, so no plugin can load (plugins.trustedKeys)` |
| Damaged signature file | `signature file is malformed: ...` |
| Descriptor with an unknown field, bad kind or bad capability | `plugin.json is not valid: ...` naming the field |
| Module imports something the host doesn't provide | `the module is not usable: guest imports env.reach_out, which the host does not provide` |
| Module uses the context functions without declaring a capability | `the module is not usable: guest imports hotloop_flow.context_set but was not given a context capability` |
| Directory name and `name` differ | `the directory is called "x" and plugin.json says the plugin is "y"; they have to match` |

The signature is checked before the descriptor is parsed and before the module is
compiled, so unsigned code is never compiled at all.

## The two examples

Both are in `examples/plugins` and both are built and loaded in CI.

* [`moving-average`](../examples/plugins/moving-average) is Rust. It averages a
  reading over the last N messages and keeps its window in its own memory. It asks
  for nothing. `cargo build --locked --release --target wasm32-wasip1`, then
  `plugin.wasm` is the `moving_average.wasm` it builds.
* [`stamp`](../examples/plugins/stamp) is TinyGo. It adds the time to each message,
  so it asks for `clock`. It builds with TinyGo's own container, using Podman:
  `podman run --rm -v "$PWD/stamp:/src:ro" -v "$PWD/out:/out" -w /src docker.io/tinygo/tinygo:0.43.0 tinygo build -target=wasip1 -buildmode=c-shared -o /out/plugin.wasm .`

CI pins Rust 1.99.1 and TinyGo 0.43.0 by digest, builds both, signs them with a
throwaway key, verifies them, tampers with one and checks that it's refused, and
fails if the tests that load them were skipped.

Flow is a Go house. The host, the signing, the CLI and every test are Go, and
the TinyGo example is Go. The Rust example is there because a plugin format that
only worked for the language the host is written in wouldn't be a plugin format,
and because Rust is what a signal processing library is most likely to already be
written in. That's the written reason for the one non-Go example. The crates it
builds with are `serde` and `serde_json`, both MIT or Apache-2.0, compiled into
the example module and not into Flow.

## What isn't proven

* Plugins have run on x86 only. No Pi.
* On Kubernetes the plugin files arrive through `plugins.volume`, any volume
  source. A ConfigMap holds a plugin only up to its 1 MiB limit, so a plugin the
  size of the TinyGo example wants a claim.
* There's no network capability. A plugin can't make a request, and that's on
  purpose until someone has a reason with a host allowlist to go with it.
* Flow tests run plugins for real, in the same child process as every other node.
  A plugin's capabilities are the same ones the deploy would give it.
* `hotloop-flow test` run by hand from a shell doesn't load plugins. The deploy
  gate and the editor's test runs do.
