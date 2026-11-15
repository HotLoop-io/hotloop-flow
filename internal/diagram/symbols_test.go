package diagram

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

// B4: every node type of a diagram has a symbol drawn by us. The editor draws
// the symbol its type's icon names from web/src/lib/symbols.ts, so a type that
// names one that is not there would be drawn bare.
func TestEveryDiagramNodeTypeHasADrawnSymbol(t *testing.T) {
	src, err := os.ReadFile("../../web/src/lib/symbols.ts")
	if err != nil {
		t.Fatal(err)
	}
	drawn := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+([a-z]+): '`).FindAllStringSubmatch(string(src), -1) {
		drawn[m[1]] = true
	}
	var missing []string
	for _, name := range Kinds() {
		k := kinds[name]
		if len(name) > 9 && name[:9] == "dia-test-" {
			continue
		}
		if !drawn[k.Descriptor.Icon] {
			missing = append(missing, name+" ("+k.Descriptor.Icon+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("diagram node types with no symbol drawn for their icon: %v", missing)
	}
}
