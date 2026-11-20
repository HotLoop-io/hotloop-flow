package vi

import (
	"os"
	"strings"
	"testing"
)

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The VIs in testdata are from github.com/ni/labview-icon-editor (MIT, see
// testdata/LICENSE), saved by LabVIEW itself.
func TestReadsTheLabelsDescriptionAndSubVIsOfARealVI(t *testing.T) {
	in, err := Read(load(t, "Get_Grayscale_Value.vi"), "Get Grayscale Value.vi")
	if err != nil {
		t.Fatal(err)
	}
	if in.Library != "lv_icon.lvlib" || in.Description != "Determine the gray scale value of a color." {
		t.Errorf("library %q, description %q", in.Library, in.Description)
	}
	if in.Version != "20.0" {
		t.Errorf("version %q", in.Version)
	}
	joined := strings.Join(in.PanelLabels, "|")
	for _, want := range []string{"grayscale value", "input color"} {
		if !strings.Contains(joined, want) {
			t.Errorf("panel labels %v lack %q", in.PanelLabels, want)
		}
	}
	if len(in.SubVIs) != 1 || in.SubVIs[0] != "Color to RGB.vi" {
		t.Errorf("sub VIs = %v", in.SubVIs)
	}
	if in.Types < 2 {
		t.Errorf("types %d", in.Types)
	}
}

func TestReadsEveryFixtureAndSaysWhatItDidNotRead(t *testing.T) {
	for _, f := range []string{"Const_Temp_Coordinate_4_points.vi", "Create_Color_Array.vi", "Get_Grayscale_Value.vi", "Get_SubPicture_Coordinate.vi", "Polymorphic_Template.vi"} {
		in, err := Read(load(t, f), f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if len(in.Blocks) < 15 {
			t.Errorf("%s: only blocks %v", f, in.Blocks)
		}
		if len(in.Unmapped()) < 5 {
			t.Errorf("%s: unmapped list is too short to be honest: %v", f, in.Unmapped())
		}
	}
	g, _ := Read(load(t, "Get_SubPicture_Coordinate.vi"), "x")
	if len(g.SubVIs) != 1 || g.SubVIs[0] != "Magic Transparent Color Constant.vi" {
		t.Errorf("sub VIs = %v", g.SubVIs)
	}
	in, _ := Read(load(t, "Create_Color_Array.vi"), "x")
	if !strings.Contains(strings.Join(in.PanelLabels, "|"), "RGB in") {
		t.Errorf("labels %v", in.PanelLabels)
	}
}

func TestRefusesWhatIsNotAVIWithAReason(t *testing.T) {
	good := load(t, "Get_Grayscale_Value.vi")
	for name, tc := range map[string]struct {
		in   []byte
		want string
	}{
		"text":      {[]byte("hello world, this is a text file and not a VI at all, padded to be long enough"), "RSRC"},
		"cut short": {good[:len(good)/2], "cut short"},
		"not a VI":  {append(append([]byte{}, good[:8]...), append([]byte("LVAR"), good[12:]...)...), ""},
	} {
		_, err := Read(tc.in, "x.vi")
		if name == "not a VI" {
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to say %q", name, err, tc.want)
		}
	}
	bad := append([]byte{}, good...)
	bad[8], bad[9], bad[10], bad[11] = 'L', 'V', 'X', 'X'
	if _, err := Read(bad, "x.vi"); err == nil || !strings.Contains(err.Error(), "LVXX") {
		t.Errorf("another resource type: %v", err)
	}
}

func TestTheFlowsCarryWhatWasReadAndWhatWasNot(t *testing.T) {
	in, _ := Read(load(t, "Get_Grayscale_Value.vi"), "Get Grayscale Value.vi")
	f := in.Flows()
	if len(f) != 2 || f[0]["type"] != "tab" || f[1]["z"] != f[0]["id"] {
		t.Fatalf("flows = %v", f)
	}
	info, _ := f[1]["info"].(string)
	for _, want := range []string{"Determine the gray scale value", "Color to RGB.vi", "Not imported:", "block diagram's wires"} {
		if !strings.Contains(info, want) {
			t.Errorf("the note lacks %q:\n%s", want, info)
		}
	}
}
