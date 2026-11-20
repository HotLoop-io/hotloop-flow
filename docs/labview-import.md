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
* the names of the VIs and controls the block diagram calls,
* the other readable text of the diagram: free labels and comments.

They arrive as one flow, with a comment on it that says all of this, so the VI is
never mistaken for a diagram that came in.

## What does not

Said in the dialog, every time, because a diagram that looks imported and has lost
its wires is worse than none:

* the block diagram's wires, primitives and structures (loops, cases, math,
  comparisons) and their settings,
* which panel items are controls and which are indicators, and each one's type
  and default value,
* the front panel's layout, fonts and colors, the icon and the connector pane's
  pattern,
* the contents of the VIs it calls. Each is another file: import it too.

## Why the diagram is not read

NI does not publish the VI format. What is read here is the container, which the
LabVIEW community has described, and the plain data blocks in it. The panel and
the diagram are stored as a compressed tree of tagged objects, and what each tag
number means is in LabVIEW's own tables, not in the file. Without them a wire is a
pair of numbers and a primitive is a number with no name. A reader that guessed
would make a diagram that runs and is wrong, and a test bench that is wrong while
it runs is the failure this product exists to prevent. When the tag table is
available from a source we may use, the diagram can be read, and this page will
say so.

The reader was written from the file's bytes and the community's description of
the container, and shares no code with other readers. A file that is not a VI, or
is cut short, is refused with the reason.

## Tested on

The VIs in `internal/vi/testdata` are from
[ni/labview-icon-editor](https://github.com/ni/labview-icon-editor) (MIT), saved by
LabVIEW 20.0. They are NI's, with their license beside them.
