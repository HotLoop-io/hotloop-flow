package i18n_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/i18n"
	"github.com/HotLoop-io/hotloop-flow/internal/node"

	_ "github.com/HotLoop-io/hotloop-flow/internal/diagram"
	_ "github.com/HotLoop-io/hotloop-flow/internal/nodes"
)

var update = flag.Bool("update", false, "rewrite catalog/nodes.en.json from the descriptors")

func readCatalog(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func writeCatalog(t *testing.T, path string, m map[string]string) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("{\n")
	keys := i18n.Keys(m)
	for i, k := range keys {
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		// json.Marshal escapes <, > and & as \u00XX; the catalogs are read by
		// people, so put them back.
		v := strings.NewReplacer(`<`, "<", `>`, ">", `&`, "&").Replace(string(vb))
		sb.WriteString("  " + string(kb) + ": " + v)
		if i < len(keys)-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("}\n")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lowConfidence(t *testing.T, dir, kind, lang string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "lowconfidence."+kind+"."+lang+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatalf("lowconfidence.%s.%s.json: %v", kind, lang, err)
	}
	return keys
}

// The English catalog is the descriptors' own text, so a node type that gains a
// label or a sentence without a catalog entry fails here, with the command that
// fixes it. The other languages are checked against it below.
func TestEnglishNodeCatalogIsTheDescriptors(t *testing.T) {
	want := i18n.AllStrings(node.Default.Descriptors())
	if len(want) < 500 {
		t.Fatalf("only %d node strings found: are the node packages registered?", len(want))
	}
	path := filepath.Join("catalog", "nodes.en.json")
	if *update {
		writeCatalog(t, path, want)
		return
	}
	have := readCatalog(t, path)
	var problems []string
	for _, k := range i18n.Keys(want) {
		h, ok := have[k]
		switch {
		case !ok:
			problems = append(problems, "missing: "+k)
		case h != want[k]:
			problems = append(problems, "changed: "+k)
		}
	}
	for _, k := range i18n.Keys(have) {
		if _, ok := want[k]; !ok {
			problems = append(problems, "stale: "+k)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		if len(problems) > 20 {
			problems = append(problems[:20], fmt.Sprintf("... and %d more", len(problems)-20))
		}
		t.Fatalf("catalog/nodes.en.json is out of step with the node descriptors; run `go test ./internal/i18n -run English -update`, then add the new keys to every language:\n%s", strings.Join(problems, "\n"))
	}
}

var codeSpan = regexp.MustCompile("`[^`]*`")

// words are what a person reads as a word, three letters or more, so a cron line
// or a hostname is not text that needs a translation.
var words = regexp.MustCompile(`\p{L}{3,}`)

func spans(s string) []string {
	out := codeSpan.FindAllString(s, -1)
	sort.Strings(out)
	return out
}

// Every language has every node string, once, in its own words, with the code
// in backticks left exactly as it was.
func TestEveryLanguageHasEveryNodeString(t *testing.T) {
	en := readCatalog(t, filepath.Join("catalog", "nodes.en.json"))
	for _, l := range i18n.Languages {
		if l.Code == "en" {
			continue
		}
		t.Run(l.Code, func(t *testing.T) {
			got := readCatalog(t, filepath.Join("catalog", "nodes."+l.Code+".json"))
			var missing, extra, empty, code []string
			for _, k := range i18n.Keys(en) {
				v, ok := got[k]
				switch {
				case !ok:
					missing = append(missing, k)
				case strings.TrimSpace(v) == "":
					empty = append(empty, k)
				case strings.Join(spans(v), "|") != strings.Join(spans(en[k]), "|"):
					code = append(code, k)
				}
			}
			for _, k := range i18n.Keys(got) {
				if _, ok := en[k]; !ok {
					extra = append(extra, k)
				}
			}
			report := func(what string, keys []string) {
				if len(keys) == 0 {
					return
				}
				if len(keys) > 8 {
					keys = append(keys[:8], fmt.Sprintf("... and %d more", len(keys)-8))
				}
				t.Errorf("%s: %s\n  %s", l.Code, what, strings.Join(keys, "\n  "))
			}
			report("missing", missing)
			report("empty", empty)
			report("extra (not in English)", extra)
			report("the `code` spans differ from English", code)
			// What still reads as English is on a list, so a new string cannot
			// slip through as a copy of the English. The list shrinks as text is
			// translated and is empty when a language is whole.
			listed := map[string]bool{}
			if b, err := os.ReadFile(filepath.Join("catalog", "untranslated.nodes."+l.Code+".json")); err == nil {
				var ks []string
				if err := json.Unmarshal(b, &ks); err != nil {
					t.Fatal(err)
				}
				for _, k := range ks {
					listed[k] = true
					if got[k] != en[k] {
						t.Errorf("%s: %q is listed as untranslated but is translated; take it off the list", l.Code, k)
					}
				}
			}
			var copies []string
			for _, k := range i18n.Keys(en) {
				if got[k] == en[k] && strings.Contains(en[k], " ") && !strings.Contains(en[k], "://") && !protocolPhrases[en[k]] && len(words.FindAllString(en[k], -1)) >= 4 && !listed[k] {
					copies = append(copies, k)
				}
			}
			report("is still English and is not on the untranslated list", copies)
			for _, k := range lowConfidence(t, "catalog", "nodes", l.Code) {
				if _, ok := en[k]; !ok {
					t.Errorf("%s: lowconfidence names %q, which is not a key", l.Code, k)
				}
			}
		})
	}
}

// protocolPhrases are names of wire protocols and modes. They read the same in
// every language, so a copy of the English is right for them.
var protocolPhrases = map[string]bool{"RTU over TCP (telnet)": true}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"":                        "en",
		"de-AT,de;q=0.9,en;q=0.8": "de",
		"zh-CN":                   "zh-Hans",
		"zh-TW,fr;q=0.5":          "fr",
		"nb-NO":                   "nb",
		"no":                      "nb",
		"nn":                      "nb",
		"ar-EG":                   "ar",
		"hi-IN":                   "hi",
		"pt-BR":                   "en",
		"ko":                      "ko",
		"*":                       "en",
	} {
		if got := i18n.Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// Localizing changes the text and nothing a flow depends on.
func TestLocalizeKeepsEverythingButText(t *testing.T) {
	ds := node.Default.Descriptors()
	out := i18n.Localize(ds, "de")
	if len(out) != len(ds) {
		t.Fatalf("%d descriptors in, %d out", len(ds), len(out))
	}
	for i := range ds {
		a, b := ds[i], out[i]
		if a.Type != b.Type || a.Category != b.Category || a.PaletteLabel != b.PaletteLabel || a.Inputs != b.Inputs || a.Outputs != b.Outputs {
			t.Fatalf("%s: a name or a shape changed", a.Type)
		}
		if strings.Join(a.InputPorts, ",") != strings.Join(b.InputPorts, ",") || strings.Join(a.OutputPorts, ",") != strings.Join(b.OutputPorts, ",") {
			t.Fatalf("%s: a port name changed", a.Type)
		}
		checkProps(t, a.Type, a.Props, b.Props)
	}
	if i18n.Localize(ds, "en")[0].Help != ds[0].Help {
		t.Fatal("English changed")
	}
}

func checkProps(t *testing.T, typ string, a, b []node.Prop) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: %d props became %d", typ, len(a), len(b))
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Kind != b[i].Kind || fmt.Sprint(a[i].Default) != fmt.Sprint(b[i].Default) || a[i].TypeProp != b[i].TypeProp {
			t.Fatalf("%s.%s: a name, a kind or a default changed", typ, a[i].Name)
		}
		if len(a[i].Options) != len(b[i].Options) {
			t.Fatalf("%s.%s: options changed in number", typ, a[i].Name)
		}
		for j := range a[i].Options {
			if fmt.Sprint(a[i].Options[j].Value) != fmt.Sprint(b[i].Options[j].Value) {
				t.Fatalf("%s.%s: an option value changed", typ, a[i].Name)
			}
		}
		checkProps(t, typ+"."+a[i].Name, a[i].Fields, b[i].Fields)
	}
}
