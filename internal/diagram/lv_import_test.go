package diagram

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/vi"
)

// A VI that came in as a diagram loads and compiles. The only things wrong with
// it are the LabVIEW nodes FlowLab has no meaning for, each named, and nothing
// else: no wire lands on a port that is not there.
func TestAnImportedVIDiagramCompilesAndReportsOnlyItsLabVIEWNodes(t *testing.T) {
	for _, f := range []string{"Get_Grayscale_Value.vi", "Create_Color_Array.vi", "Test_Split_Text_into_Words.vi", "AssessRectangle.vi", "Write_INI_Tokens_and_VI_Tags.vi"} {
		raw, err := os.ReadFile("../vi/testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		in, err := vi.Read(raw, f)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(in.Flows())
		flows, err := engine.ParseFlows(b)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		set := CompileAll(flows)
		if len(set.Order) == 0 {
			t.Fatalf("%s: no diagram came in", f)
		}
		lv := 0
		for _, id := range set.Order {
			d := set.Diagrams[id]
			for _, n := range d.Nodes {
				if n.Type == "dia-lv" {
					lv++
				}
			}
			for _, p := range d.Problems {
				if !strings.Contains(p.Text, "from a LabVIEW VI") {
					t.Errorf("%s: a problem that is not a LabVIEW node: %s", f, p)
				}
			}
			var named int
			for _, p := range d.Problems {
				if p.Node != "" {
					named++
				}
			}
			if named < countType(d, "dia-lv") {
				t.Errorf("%s: %d LabVIEW nodes and %d named problems", f, countType(d, "dia-lv"), named)
			}
		}
		if lv == 0 {
			t.Errorf("%s: no LabVIEW node came across", f)
		}
	}
}

func countType(d *Diagram, typ string) int {
	n := 0
	for _, dn := range d.Nodes {
		if dn.Type == typ {
			n++
		}
	}
	return n
}
