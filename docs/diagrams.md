# FlowLab: diagrams

FlowLab is the part of Flow for test benches and data loggers, and a diagram is its unit: the other way Flow runs a tab. In a flow, a message goes through a node
and any input wakes it. In a diagram, each node runs once per run, when every input
that has a wire holds a value. Two nodes with no wire between them run at the same
time. That is how you build a test bench or a data logger: read a device, parse it,
decide whether it is steady, show it, write it down.

Flows are untouched by any of this. A tab that carries `ew_diagram` is left out of the
message runtime, and every other tab runs exactly as before.

What that means for you: you draw the logic once, press Run, and watch the values
move on the wires. Anyone with the panel address can watch the same run on a phone.

## The file

A diagram is a tab with an `ew_diagram` object. Its nodes are ordinary entries of the
flow file. `wires` names the node a wire goes to, as always, and `ew_in` on the
source node says which input of that target each wire lands on, in the same shape:

```json
{ "id": "a", "type": "dia-const", "z": "d", "value": "1", "x": 100, "y": 80,
  "ew_in": [["a"], []], "wires": [["sum"], []] }
```

The file loads and saves byte for byte with these keys in it. Settings of the tab:

| Key | |
|---|---|
| `autostart` | `true` starts the run when Flow starts and after every deploy, with no browser open. |
| `role` | `"body"` for a diagram that a loop runs. A body is not run by itself. |

Every node also has an optional `after` input and a `done` output. Wire one node's
`done` to another's `after` and the second waits for the first, with nothing else
between them. The order is a wire you can see and diff.

## Running

A diagram runs from what is deployed. In the editor, open a diagram tab and press Run.
The bar over the canvas has:

- **Highlight execution**: every node waits a moment before it runs, so you can follow
  it, and the wire a value crossed lights up. It turns itself off after ten minutes.
- **Pause, Resume, Step into, Step over, Step out**: step goes node by node. Over runs
  a loop's whole body. Into stops at the first node in it.
- **Break on selected**: a breakpoint on the selected node. Double press a wire to
  probe it, which keeps its last 100 values, or to break when a value is put on it.
  A probe never changes a run.
- **Open panel**: the operator's page.

Hover any wire after a run for the last value it carried. The problems button lists
every reason a diagram cannot run, each naming the node and the input.

The same things are an API for scripts and CI:

| | |
|---|---|
| `GET /diagrams`, `GET /diagrams/{id}` | What is deployed, the panel, the last value on every wire. |
| `POST /diagrams/{id}/run`, `/stop`, `/debug` | Needs `diagram.run`. Each is in the audit log with who. |
| `PUT /diagrams/{id}/controls/{node}` | Set a control. Checked against its kind, range and choices. |
| `PUT /diagrams/{id}/breakpoints`, `/probes` | Keys are a node id, `wire:<wire key>` or `diagram:<tab>`. |

Events for a run go out on the same socket as the editor's, as `diagram/<id>/node`,
`token`, `run`, `paused` and `values`.

## Nodes

| Node | |
|---|---|
| `dia-const`, `dia-math`, `dia-compare`, `dia-logic` | Values and arithmetic. Math works one by one on arrays. |
| `dia-wait` | Waits, then passes its input on. |
| `dia-bundle`, `dia-field`, `dia-array` | Records and arrays. |
| `dia-while`, `dia-for` | Loops. The body is a diagram of its own, named by its tab. Carried values start from a wire or from Start values and come out as they were on the last pass. A for loop takes a count or an array. A while loop runs at least once. |
| `dia-body-in`, `dia-body-out` | The two ends of a body. `iter` is the pass number. `stop` ends a while loop. A carried value's name sends it round. |
| `dia-serial-open`, `dia-serial-read`, `dia-serial-write` | A serial port. Reads end at a terminator or a byte count. |
| `dia-parse-reading`, `dia-stable` | Take a number and unit out of a line, and say whether the last few agree. |
| `dia-log-text` | Appends a record as a row of a text file, with a time column. |
| `dia-daq-read` | Reads samples from a data acquisition task on NI hardware, through NI's gRPC Device Server. See Data acquisition below. |
| `dia-control`, `dia-indicator`, `dia-chart` | The panel. |
| `dia-stats` | The mean, RMS, smallest, largest and standard deviation of an array. The standard deviation divides by n minus one, so it needs two readings. |
| `dia-integral` | The area under an array by the trapezoid rule, as one number or as the running area at each sample. |
| `dia-derivative` | The slope at each sample of an array. |
| `dia-filter` | A lowpass, highpass, bandpass or bandstop Butterworth filter of order 1 to 10 with a cutoff in hertz. |

### Filters

The filter node is a Butterworth filter: flat where it passes, no ripple, and 3 dB
down at the cutoff. It is designed from the analog filter by the bilinear transform
with the cutoff prewarped, so the 3 dB point lands on the frequency you typed, and it
runs as a chain of second-order sections so a high order stays accurate. A bandpass or
bandstop of order n has 2n poles. A cutoff has to be above zero and below half the
sample rate, and a design that cannot be built is listed in the problems before the run.
The sample rate is a property, or an input that wins over it. The filter keeps its
memory from call to call inside a run, so an array cut into pieces by a loop comes out
as if it had been filtered whole, and every new run starts from rest.

What it can't do: it is not a linear-phase filter, so it delays different frequencies
by different amounts, and it has no zero-phase mode that filters forward and back. Where
the timing between two signals has to line up, or a waveform's shape has to survive,
that is the wrong filter. Integral and derivative take the time between samples from a
`dt` input in seconds, a `rate` input in hertz, or the Samples per second property.

### The error wire

Nodes that talk to the world have `err` in and `err` out. An error is a record: a
flag, a numeric code, the source text, and the node it came from. A node with an
error coming in does no work and hands the error on, so one fault at the start of a
chain skips everything after it and shows up once at the end. Wire an error to a
loop's `err` body out and the loop stops on it, with the error on the loop's error
out. An error indicator on the panel goes red on a fault and clears when the next
value arrives with none.

### Serial

A port that goes away, such as a pulled cable or an unplugged adapter, is a fault on
the read's error wire. The next read tries to open it again, once a second, and counts
the loss and the return. Flow control lines are not offered.

### Data acquisition

`dia-daq-read` reads a task from NI hardware, such as a CompactDAQ chassis, through
NI's own open gRPC Device Server. Flow talks to that server over the network, so the
hardware can be on a different machine from Flow, and Flow needs no NI driver and no
vendor library of its own. It stays one static binary.

**On the machine the hardware is attached to**, install NI's DAQmx driver and NI's
gRPC Device Server (https://github.com/ni/grpc-device, MIT licensed). The server
listens on that machine only (`localhost`, port 31763) until its configuration file
says otherwise. To reach it from Flow on another machine, set `address` in its
`server_config.json` to `[::]` or the interface you want, and turn on TLS by setting
`server_cert`, `server_key`, and, for mutual TLS, `root_cert`, with the files in the
`certs` folder beside the configuration. NI's README has the details for the version
you run. Do not leave the server's default, unsecured port open on a plant network:
anyone who can reach it can run the hardware.

**In the node**, set:

* `address` to `host:port` of the server.
* `tls` to `off` (only for a server on the same machine), `on` (Flow checks the
  server's certificate against `caFile`, or the system's authorities if that is
  empty), or `mutual` (Flow also presents `certFile` and `keyFile`, which the server
  checks against its `root_cert`). `serverName` is the name to look for in the
  server's certificate when it is not the host in `address`.
* `device` and `channels` to name the hardware, such as `cDAQ1Mod1` and `ai0:3`, or a
  list like `ai0, ai2`. `kind` is `voltage`, `current`, or `thermocouple`, with `min`
  and `max` for the range you expect and `terminal` for the wiring. Thermocouples take
  a `tcType` and a `unit` and use the module's own cold junction compensation.
* `mode`, `rate`, and `samples`. Finite mode takes one acquisition of `samples` per
  channel at `rate` on every call, starting it again each time. Continuous mode starts
  the task on the first call and each call reads the next `samples`, so a node in a
  loop body reads a stream block by block. `trigger` starts the task on a digital edge
  at a terminal such as `/cDAQ1/PFI0`.

`y` is the samples as an array for one channel, or an array of arrays, one per channel
in the order you named them. `dt` is the seconds between samples, one over the rate, so
a block's length in seconds is its sample count times `dt`: 1,000 samples at 1,000
samples a second cover 1.000 s. The device's own clock times the samples, as it always
does. Flow only moves the data. A refusal from the server or the driver comes out on
`err` with the driver's own code and text, and a server that does not answer in
`callTimeoutSec` is a timeout. The task lives for the run and is cleared on the server
when the run ends, so the hardware is free again.

What is done: analog voltage in, analog current in, and thermocouple temperature in,
finite or continuous, with a sample clock and an optional digital start trigger. What
is not: digital in and out and counters use different read calls and are not built, and
neither are writes, other trigger types, or scaling beyond what the module does.

**This has not been run against NI's server.** There is no NI hardware, driver, or
server in our lab. Every test runs against a stand-in server we wrote, which speaks
the same calls and answers with a sine wave. What backs the wire format is a test that
reads NI's own protocol files, kept in `third_party/ni-grpc-device/`, and checks that
every message the client sends and reads has the same field numbers, names, and types.
That shows we say the right words. It does not show NI's server behaves the way the
stand-in does.

## The panel

`/panel/<diagram id>` on a runtime opens a page of the diagram's controls,
indicators and charts, signed in the same way as the editor. It is built to be used
with a thumb: nothing on it is smaller than a finger. A chart keeps its last 1,024
points unless you change it, and says how many it has let go of. A graph replaces its
data with each array. An XY chart plots x against y and keeps history too.

Several people can watch one run. Setting a control from the browser needs
`diagram.run`, is checked on the server, and is written to the audit log.

## What this is not

A diagram runs in a Go process on a general purpose operating system. Garbage
collection and the scheduler can delay a node by milliseconds, so a node can't promise
a deadline. Work that has to meet one belongs in a PLC or a runtime built for it. The
timing of samples from a data acquisition device is not affected by any of this: the
device's own clock does it, and Flow only moves the data.
