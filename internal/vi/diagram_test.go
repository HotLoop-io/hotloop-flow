package vi

import (
	"math"
	"testing"
)

func count(d *Diagram) (nodes, wires, bodies int) {
	nodes, wires = len(d.Nodes), len(d.Wires)
	for _, n := range d.Nodes {
		for _, b := range n.Bodies {
			bodies++
			bn, bw, bb := count(b)
			nodes, wires, bodies = nodes+bn, wires+bw, bodies+bb
		}
	}
	return
}

// The wire counts are what pylabview (MIT) reads from the same files as
// signal objects of the block diagram heap, counted from its XML export of each VI.
func TestEveryWireOfARealDiagramIsRead(t *testing.T) {
	for f, want := range map[string]struct{ wires, bodies int }{
		"Const_Temp_Coordinate_4_points.vi": {1, 0},
		"Create_Color_Array.vi":             {10, 0},
		"Get_Grayscale_Value.vi":            {12, 0},
		"AssessRectangle.vi":                {21, 2},
		"Test_Split_Text_into_Words.vi":     {14, 1},
		"Write_INI_Tokens_and_VI_Tags.vi":   {90, 4},
	} {
		in, err := Read(load(t, f), f)
		if err != nil || in.Diagram == nil {
			t.Fatalf("%s: %v %s", f, err, in.Why)
		}
		_, w, b := count(in.Diagram)
		if w != want.wires {
			t.Errorf("%s: %d wires, the file has %d", f, w, want.wires)
		}
		if b != want.bodies {
			t.Errorf("%s: %d structure bodies, want %d", f, b, want.bodies)
		}
		for _, wi := range allWires(in.Diagram) {
			if !wi.Known || wi.From.Node == 0 {
				t.Errorf("%s: a wire has no source: %+v", f, wi)
			}
			for _, r := range wi.To {
				if r.Node == 0 {
					t.Errorf("%s: a wire ends on a terminal no node owns: %+v", f, wi)
				}
			}
		}
	}
}

func allWires(d *Diagram) []Wire {
	out := append([]Wire{}, d.Wires...)
	for _, n := range d.Nodes {
		for _, b := range n.Bodies {
			out = append(out, allWires(b)...)
		}
	}
	return out
}

// The luminosity weights of Get Grayscale Value, as the VI stores them: three
// single precision constants. They read as the HDTV weights the VI's own
// description names, which is the check that the type of each constant is read right.
func TestConstantsAreReadByTheirType(t *testing.T) {
	in, err := Read(load(t, "Get_Grayscale_Value.vi"), "x")
	if err != nil || in.Diagram == nil {
		t.Fatal(err, in.Why)
	}
	var got []float64
	for _, n := range in.Diagram.Nodes {
		if n.HasConst {
			f, _ := n.Const.(float64)
			got = append(got, f)
		}
	}
	want := []float64{0.2126, 0.7152, 0.0722}
	if len(got) != 3 {
		t.Fatalf("constants %v", got)
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-6 {
			t.Errorf("constant %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPanelTerminalsAreNamedAndToldControlFromIndicator(t *testing.T) {
	in, _ := Read(load(t, "Get_Grayscale_Value.vi"), "x")
	got := map[string]string{}
	for _, n := range in.Diagram.Nodes {
		if n.Panel != "" {
			got[n.Label] = n.Panel + " " + n.PanelKind
		}
	}
	if got["input color"] != "control stdColorNum" || got["grayscale value"] != "indicator stdNum" {
		t.Errorf("panel = %v", got)
	}
}

func TestACallAndAPrimitiveAreNamedAndWired(t *testing.T) {
	in, _ := Read(load(t, "Get_Grayscale_Value.vi"), "x")
	var call *Node
	prims := 0
	for _, n := range in.Diagram.Nodes {
		if n.Class == "iUse" {
			call = n
		}
		if n.Class == "prim" {
			prims++
		}
	}
	if call == nil || call.Label != "Color to RGB.vi" || len(call.Terms) != 5 {
		t.Fatalf("call = %+v", call)
	}
	if prims != 4 {
		t.Errorf("%d primitives, want 4", prims)
	}
}

func TestALoopKeepsItsBodyAndItsTunnels(t *testing.T) {
	in, _ := Read(load(t, "Test_Split_Text_into_Words.vi"), "x")
	var loop *Node
	for _, n := range in.Diagram.Nodes {
		if n.Class == "forLoop" {
			loop = n
		}
	}
	if loop == nil || len(loop.Bodies) != 1 || len(loop.Bodies[0].Nodes) == 0 {
		t.Fatalf("loop = %+v", loop)
	}
}
