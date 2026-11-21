package i18n_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/i18n"
)

// The editor's catalogs sit beside its source.
var uiDir = filepath.Join("..", "..", "web", "src", "i18n")
var srcDir = filepath.Join("..", "..", "web", "src")

// The plural forms each language writes for a whole number, from CLDR. English
// and the languages that share its two forms use one and other; Japanese,
// Korean, Vietnamese and Chinese have one form; Arabic has six.
var pluralForms = map[string][]string{
	"en": {"one", "other"}, "es": {"one", "other"}, "fr": {"one", "other"}, "it": {"one", "other"},
	"de": {"one", "other"}, "nb": {"one", "other"}, "bg": {"one", "other"}, "el": {"one", "other"},
	"hi": {"one", "other"}, "ko": {"other"}, "ja": {"other"}, "vi": {"other"}, "zh-Hans": {"other"},
	"ar": {"zero", "one", "two", "few", "many", "other"},
}

var (
	placeholder = regexp.MustCompile(`\{\w+\}`)
	pluralKey   = regexp.MustCompile(`^(.*)_(zero|one|two|few|many|other)$`)
)

func placeholders(s string) string {
	p := placeholder.FindAllString(s, -1)
	sort.Strings(p)
	return strings.Join(p, " ")
}

// Group the keys of a catalog by their base, so a family of plural keys is one entry.
func families(keys []string) map[string][]string {
	out := map[string][]string{}
	for _, k := range keys {
		if m := pluralKey.FindStringSubmatch(k); m != nil {
			out[m[1]] = append(out[m[1]], m[2])
		}
	}
	return out
}

// Every language has every editor string, every {name} where English has it,
// and the plural forms the language needs.
func TestEditorCatalogs(t *testing.T) {
	en := readCatalog(t, filepath.Join(uiDir, "ui.en.json"))
	if len(en) < 400 {
		t.Fatalf("only %d editor strings in ui.en.json", len(en))
	}
	for _, l := range i18n.Languages {
		if l.Code == "en" {
			continue
		}
		t.Run(l.Code, func(t *testing.T) {
			got := readCatalog(t, filepath.Join(uiDir, "ui."+l.Code+".json"))
			var problems []string
			enFam := families(i18n.Keys(en))
			for _, k := range i18n.Keys(en) {
				if m := pluralKey.FindStringSubmatch(k); m != nil {
					continue // a family is checked below, in the language's own forms
				}
				v, ok := got[k]
				switch {
				case !ok:
					problems = append(problems, "missing: "+k)
				case strings.TrimSpace(v) == "":
					problems = append(problems, "empty: "+k)
				case placeholders(v) != placeholders(en[k]):
					problems = append(problems, fmt.Sprintf("placeholders differ: %s (%q)", k, placeholders(v)))
				}
			}
			for base := range enFam {
				var enOther string
				if v, ok := en[base+"_other"]; ok {
					enOther = v
				}
				for _, form := range pluralForms[l.Code] {
					k := base + "_" + form
					v, ok := got[k]
					switch {
					case !ok:
						problems = append(problems, "missing plural form: "+k)
					case strings.TrimSpace(v) == "":
						problems = append(problems, "empty: "+k)
					case !pluralPlaceholdersOK(form, v, enOther):
						problems = append(problems, fmt.Sprintf("placeholders differ: %s (%q)", k, placeholders(v)))
					}
				}
			}
			for _, k := range i18n.Keys(got) {
				if _, ok := en[k]; ok {
					continue
				}
				if m := pluralKey.FindStringSubmatch(k); m != nil {
					if _, fam := enFam[m[1]]; fam {
						allowed := false
						for _, f := range pluralForms[l.Code] {
							if f == m[2] {
								allowed = true
							}
						}
						if allowed {
							continue
						}
					}
				}
				problems = append(problems, "extra: "+k)
			}
			if len(problems) > 0 {
				if len(problems) > 12 {
					problems = append(problems[:12], fmt.Sprintf("... and %d more", len(problems)-12))
				}
				t.Errorf("%s:\n  %s", l.Code, strings.Join(problems, "\n  "))
			}
		})
	}
}

// Every key a component asks for is in the English catalog, so a typo in a key
// fails the build instead of showing the key on screen.
func TestEveryKeyUsedExists(t *testing.T) {
	en := readCatalog(t, filepath.Join(uiDir, "ui.en.json"))
	use := regexp.MustCompile(`\b(?:tr|trn|trOr)\(\s*'([a-z0-9_.\-]+)'`)
	var missing []string
	filepath.WalkDir(srcDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(p, ".svelte") || strings.HasSuffix(p, ".ts")) {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, m := range use.FindAllStringSubmatch(string(b), -1) {
			k := m[1]
			if _, ok := en[k]; ok {
				continue
			}
			if _, ok := en[k+"_other"]; ok {
				continue
			}
			// A key built from a value at run time ends in a dot or an underscore.
			if strings.HasSuffix(k, ".") || strings.HasSuffix(k, "_") {
				continue
			}
			missing = append(missing, p+": "+k)
		}
		return nil
	})
	if len(missing) > 0 {
		t.Errorf("keys with no English text:\n  %s", strings.Join(missing, "\n  "))
	}
}

// New text goes in the catalogs, not in the markup. This reads every Svelte
// component and fails on a word a person would read that is not behind tr():
// a text node, or a title, aria-label, placeholder or alt attribute. A name that
// is the same in every language is listed here with the reason.
var untranslated = map[string]string{
	"HotLoop Flow": "the product name",
	"FlowLab":      "the module name",
	"JSONata":      "a language name",
	"JSON":         "a format name",
	"MQTT":         "a protocol name",
}

func TestNoHardcodedText(t *testing.T) {
	text := regexp.MustCompile(`>([^<>]*[A-Za-z]{2}[^<>]*)(?:<)`)
	attr := regexp.MustCompile(`\b(title|aria-label|placeholder|alt)="([^"]*[A-Za-z]{2}[^"]*)"`)
	comment := regexp.MustCompile(`(?s)<!--.*?-->`)
	var found []string
	filepath.WalkDir(filepath.Join(srcDir, "components"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".svelte") {
			return nil
		}
		b, _ := os.ReadFile(p)
		s := string(b)
		end := strings.Index(s, "</script>")
		if end < 0 {
			return nil
		}
		markup := s[end+len("</script>"):]
		if i := strings.Index(markup, "<style"); i >= 0 {
			markup = markup[:i]
		}
		markup = comment.ReplaceAllString(markup, "")
		markup = regexp.MustCompile(`(?s)<script.*?</script>`).ReplaceAllString(markup, "")
		// What is inside { } is code (a generic's > included), not words.
		markup = stripBraces(markup)
		for _, m := range attr.FindAllStringSubmatch(markup, -1) {
			if _, ok := untranslated[m[2]]; !ok {
				found = append(found, fmt.Sprintf("%s: %s=%q", p, m[1], m[2]))
			}
		}
		for _, m := range text.FindAllStringSubmatch(markup, -1) {
			core := strings.TrimSpace(m[1])
			if !regexp.MustCompile(`[A-Za-z]{2}`).MatchString(core) {
				continue
			}
			if _, ok := untranslated[core]; ok {
				continue
			}
			found = append(found, fmt.Sprintf("%s: text %q", p, core))
		}
		return nil
	})
	if len(found) > 0 {
		t.Errorf("words in a component that are not in the catalog; use tr('key') and add the English to web/src/i18n/ui.en.json:\n  %s", strings.Join(found, "\n  "))
	}
}

// The dashboard page has a few words of its own. Same rule: every language, every key.
func TestDashboardCatalogs(t *testing.T) {
	dir := filepath.Join("..", "dashboard", "web", "i18n")
	en := readCatalog(t, filepath.Join(dir, "en.json"))
	for _, l := range i18n.Languages {
		got := readCatalog(t, filepath.Join(dir, l.Code+".json"))
		for _, k := range i18n.Keys(en) {
			if strings.TrimSpace(got[k]) == "" {
				t.Errorf("%s: dashboard string %q is missing", l.Code, k)
			}
		}
		for _, k := range i18n.Keys(got) {
			if _, ok := en[k]; !ok {
				t.Errorf("%s: dashboard string %q is not in English", l.Code, k)
			}
		}
	}
}

// stripBraces replaces every { ... } expression, nested ones included, with a
// single placeholder, so the lint reads only the words left in the markup. The
// placeholder keeps a text node that holds an expression from being read as a
// bare word on either side of it.
func stripBraces(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '{':
			if depth == 0 {
				b.WriteRune('\u0001')
			}
			depth++
		case r == '}' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// pluralPlaceholdersOK: a form that names its own number (one, two, zero: "one
// file", "two files", "no files") may leave {n} out; every other placeholder is
// kept, and the forms that count (few, many, other) keep {n} too.
func pluralPlaceholdersOK(form, v, english string) bool {
	if placeholders(v) == placeholders(english) {
		return true
	}
	if form != "zero" && form != "one" && form != "two" {
		return false
	}
	return placeholders(v) == strings.Join(strings.Fields(strings.ReplaceAll(" "+placeholders(english)+" ", " {n} ", " ")), " ")
}
