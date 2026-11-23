package diagram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/vi"
)

func wiresOf(d *vi.Diagram) (wires, bad int) {
	for _, w := range d.Wires {
		wires++
		if !w.Known || w.From.Node == 0 {
			bad++
		}
		for _, r := range w.To {
			if r.Node == 0 {
				bad++
			}
		}
	}
	for _, n := range d.Nodes {
		for _, b := range n.Bodies {
			w, x := wiresOf(b)
			wires, bad = wires+w, bad+x
		}
	}
	return
}

// Every VI of NI's labview-icon-editor (MIT, 425 of them, saved by LabVIEW) is read, and read the way
// pylabview reads it: the wires of each equal the number of signal objects pylabview finds in the VI's
// block diagram (testdata/icon-editor-wires.tsv, made by running pylabview's readRSRC -x on each file and
// counting class="signal"), every wire has a source and ends on a node's terminal, and the diagram the
// import builds compiles with no problem but the LabVIEW nodes it names.
//
// The corpus is 25 MB and is not in this repository. Set HOTLOOP_LABVIEW_CORPUS to a checkout of
// github.com/ni/labview-icon-editor to run it.
func TestEveryVIOfNIsIconEditorIsReadAsPylabviewReadsIt(t *testing.T) {
	dir := os.Getenv("HOTLOOP_LABVIEW_CORPUS")
	if dir == "" {
		t.Skip("HOTLOOP_LABVIEW_CORPUS is not set")
	}
	tsv, err := os.ReadFile("../vi/testdata/icon-editor-wires.tsv")
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, line := range strings.Split(strings.TrimSpace(string(tsv)), "\n") {
		rel, n, ok := strings.Cut(line, "\t")
		want, err := strconv.Atoi(n)
		if !ok || err != nil {
			t.Fatalf("bad line %q", line)
		}
		raw, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		in, err := vi.Read(raw, filepath.Base(rel))
		if err != nil || in.Diagram == nil {
			t.Errorf("%s: not read: %v %s", rel, err, in.Why)
			continue
		}
		read++
		got, bad := wiresOf(in.Diagram)
		if got != want {
			t.Errorf("%s: %d wires, pylabview counts %d", rel, got, want)
		}
		if bad != 0 {
			t.Errorf("%s: %d wire ends with no source or no owning node", rel, bad)
		}
		b, _ := json.Marshal(in.Flows())
		flows, err := engine.ParseFlows(b)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		set := CompileAll(flows)
		for _, id := range set.Order {
			for _, p := range set.Diagrams[id].Problems {
				if !strings.Contains(p.Text, "from a LabVIEW VI") {
					t.Errorf("%s: a problem that is not a LabVIEW node: %s", rel, p)
				}
			}
		}
	}
	t.Logf("%d VIs read of %d", read, len(strings.Split(strings.TrimSpace(string(tsv)), "\n")))
}
