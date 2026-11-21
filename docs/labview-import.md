# Bringing a LabVIEW VI

You have LabVIEW programs, and FlowLab runs the same kind of test bench. Import
the VI and FlowLab opens it, tells you what is in it, and tells you what it could
not take, so you start your diagram from the VI's own names and not from a blank
page.

## Use it

In the editor, **Import**, choose the `.vi` file. The dialog shows what did not
come across, in a list, and nothing is added until you press **Import**. Through
the API it is `POST /flows/import-vi` with `{"name": "...", "data": "<the VI, base64>"}`,
and an agent has it as the `import_vi` tool. Reading a VI saves and deploys nothing.

## What comes across

* the VI's description (the text from its properties),
* the version of LabVIEW that saved it,
* the names on the front panel, in the order the file holds them,
* the block diagram, as a FlowLab diagram tab with the VI's own wires:
  * constants, with their values read by the type the file gives them,
  * each front panel terminal, named, as a control or an indicator (a control
    starts a wire and an indicator ends one) of kind number, boolean or text,
  * every other node in its place with its terminals and its wires: a call of
    another VI (named), a primitive (by its number), a compound arithmetic node,
    a loop or a case structure,
  * the diagram inside each structure, as a tab of its own, with the structure's
    tunnels where its wires end,
* the other readable text of the diagram: free labels and comments.

They arrive as flows, with a comment on the first that says all of this, so the
VI is never mistaken for a diagram that came in.

## What does not

Said in the dialog, every time:

* what a primitive does. LabVIEW numbers its primitives, the table of what each
  number is belongs to LabVIEW, and it is not in the file. A primitive arrives as a
  "LabVIEW node" named by its number, wired as it was, and a diagram that holds one
  does not run: the error list names the node until you replace it with the node
  that does the same. A diagram that ran with a guess in that place would give
  numbers that are wrong while they look right.
* the settings inside nodes (a compound arithmetic node's operation, a case
  structure's selector values, a loop's conditions),
* the front panel controls' default values, ranges and number formats, and the
  kinds FlowLab has no control for,
* the front panel's layout, fonts and colors, the icon and the connector pane's
  pattern,
* the contents of the VIs it calls. Each is another file: import it too.

## How the diagram is read

The VI is a resource file whose front panel and block diagram are stored as a
compressed tree of tagged objects. The community project
[pylabview](https://github.com/mefistotelis/pylabview) (MIT) documents the layout
of that tree and the number of every tag and object class, and this reader is
written in Go from that description (NOTICE has the credit). The tree gives the
nodes of a diagram, the terminals of each, and its wires as lists of terminals.
A wire's first terminal is its source. Each constant's type comes from the VI's
type table, so a stored four byte constant reads as the single precision number
it is: Get Grayscale Value's three constants read 0.2126, 0.7152 and 0.0722, the
weights its own description names.

The reader was checked against real VIs: for each of the seven test VIs the number
of wires read equals the number of signal objects pylabview finds in the same
file, every wire has a source and ends on a terminal that a node owns, and the
loop and case structures give their bodies.

A file that is not a VI, or is cut short, is refused with the reason.

## Tested on

The VIs in `internal/vi/testdata` (seven of them) are from
[ni/labview-icon-editor](https://github.com/ni/labview-icon-editor) (MIT), saved by
LabVIEW 20.0. They are NI's, with their license beside them.
