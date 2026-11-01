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
| `dia-log-tdms` | Appends a record to a TDMS file, one channel per field. See below. |
| `dia-control`, `dia-indicator`, `dia-chart` | The panel. |

### Log to TDMS

`dia-log-tdms` writes the measurements a test bench takes to a TDMS file, the format
that data acquisition and analysis tools open. Wire a record into `row`. Each field
of the record is a channel in one group, so a record of `pressure`, `ok` and
`operator` makes three channels. A number is stored as a 64 bit float, true or false
as a bool, and anything else as text. A field that holds a list of numbers adds the
whole list, which is how a block of samples goes in. Settings:

| | |
|---|---|
| `path` | The file. A file that is already there is appended to, so one run after another builds one file. |
| `group` | The group the channels go in. |
| `columns` | The fields to log, in order. Empty logs every field, by name. |
| `properties` | A JSON object of properties for the file, such as `{"title": "bench 4", "station": 4}`, written when the run starts. |
| `sync` | On by default: each row reaches the disk before the node finishes. |

Each row is one segment, written in one piece. If the power goes in the middle of
one, the file shows where it was cut, a reader returns every row before it, and the
next run repairs the end of the file and goes on. A field's type is set by the first
row of a run. A later row that doesn't fit, such as text in a number column, goes
to the error wire as a bad input and the file is left alone. A missing number is
written as not-a-number.

To open the file, use any TDMS reader. The library behind the node, `internal/tdms`,
also writes an optional `.tdms_index` beside the file, and reads files without one.
Channel properties such as `unit_string` and the waveform properties `wf_start_time`,
`wf_increment` and `wf_samples` are supported by the library; the node does not set
them yet.

What the reader accepts: little and big endian segments, interleaved and contiguous
data, meta data left out of a segment when it hasn't changed, a new object list, 8, 16,
32 and 64 bit integers, 32 and 64 bit floats, bools, text and timestamps. What it
refuses, by name and without half reading the file: raw data written by a data
acquisition driver, scaled or compressed data, extended precision floats, fixed
point and complex numbers. What the writer writes: little endian, contiguous
data, and those same types. It does not write big endian.

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
