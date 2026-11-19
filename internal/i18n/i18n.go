// Package i18n holds the words the runtime says to a person in more than one
// language: the text of every node type the editor shows (its help, its field
// labels, placeholders, choices and port names), in English and in twelve more.
//
// The Go descriptors stay English. They are the source and the key: a node type
// declares its text once, in English, where its code is, and this package turns
// that text into keyed strings (Strings), checks a catalog for each language
// against them (the tests), and serves the translated copy on request
// (Localize). Nothing here touches a flow file, a node type name, a property
// name or a port name, because flows depend on those. Only the text a person
// reads changes.
//
// The editor's own words live in the editor, under web/src/i18n, and the
// dashboard's under internal/dashboard/web/i18n. This package checks those too,
// so one test run says whether every language has every string.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Language is one language the editor and the runtime speak.
type Language struct {
	// Code is the BCP 47 tag the catalogs are named by.
	Code string `json:"code"`
	// Name is the language written in itself, which is what the picker shows.
	Name string `json:"name"`
	// RTL is true for a language written right to left.
	RTL bool `json:"rtl,omitempty"`
}

// Languages is every language, English first and then the order Patrick gave.
var Languages = []Language{
	{Code: "en", Name: "English"},
	{Code: "es", Name: "Español"},
	{Code: "fr", Name: "Français"},
	{Code: "it", Name: "Italiano"},
	{Code: "de", Name: "Deutsch"},
	{Code: "nb", Name: "Norsk bokmål"},
	{Code: "bg", Name: "Български"},
	{Code: "el", Name: "Ελληνικά"},
	{Code: "ko", Name: "한국어"},
	{Code: "ja", Name: "日本語"},
	{Code: "vi", Name: "Tiếng Việt"},
	{Code: "hi", Name: "हिन्दी"},
	{Code: "zh-Hans", Name: "简体中文"},
	{Code: "ar", Name: "العربية", RTL: true},
}

//go:embed catalog/nodes.*.json
var catalogFS embed.FS

var (
	loadOnce sync.Once
	catalogs map[string]map[string]string
)

// load reads every catalog once.
func load() {
	catalogs = map[string]map[string]string{}
	for _, l := range Languages {
		b, err := catalogFS.ReadFile("catalog/nodes." + l.Code + ".json")
		if err != nil {
			continue
		}
		m := map[string]string{}
		if err := json.Unmarshal(b, &m); err != nil {
			panic("hotloop-flow: i18n catalog " + l.Code + ": " + err.Error())
		}
		catalogs[l.Code] = m
	}
}

// Normalize turns what a browser or a caller sends (an Accept-Language header,
// "de-AT", "zh-CN", "nb-NO", "no") into the code of the language we have, or
// "en" when there is none. The first language in the list that we speak wins.
func Normalize(header string) string {
	for _, part := range strings.Split(header, ",") {
		tag := strings.TrimSpace(part)
		if i := strings.IndexByte(tag, ';'); i >= 0 {
			tag = tag[:i]
		}
		if code := match(strings.ToLower(strings.TrimSpace(tag))); code != "" {
			return code
		}
	}
	return "en"
}

func match(tag string) string {
	if tag == "" || tag == "*" {
		return ""
	}
	switch {
	case tag == "zh-hans" || strings.HasPrefix(tag, "zh-hans-") || tag == "zh-cn" || tag == "zh-sg" || tag == "zh":
		return "zh-Hans"
	case strings.HasPrefix(tag, "zh-"):
		// Traditional Chinese is not one of the languages. Fall through to the
		// next language the person listed rather than guess at the characters.
		return ""
	case tag == "no" || strings.HasPrefix(tag, "no-") || tag == "nn" || strings.HasPrefix(tag, "nn-"):
		return "nb"
	}
	base := tag
	if i := strings.IndexByte(base, '-'); i >= 0 {
		base = base[:i]
	}
	for _, l := range Languages {
		if strings.EqualFold(l.Code, base) {
			return l.Code
		}
	}
	return ""
}

// Strings returns every sentence, label and placeholder of a node type that a
// person reads, keyed by where it sits in the descriptor. The key names are the
// contract between the descriptors and the catalogs.
//
//	node.<type>.help              the help text, Markdown
//	node.<type>.compat            how it differs from Node-RED's node
//	node.<type>.in.<n>            the label of input n
//	node.<type>.out.<n>           the label of output n
//	node.<type>.prop.<name>.label, .help, .placeholder
//	node.<type>.prop.<name>.opt.<value>   one choice of a list
//	node.<type>.prop.<name>.field.<sub>.label ...   a row field of a list
//
// The node's own name (its palette label) is not here. A node type name is a
// name, like a protocol or a product, and stays in English.
func Strings(d node.Descriptor) map[string]string {
	out := map[string]string{}
	add := func(key, text string) {
		if strings.TrimSpace(text) != "" {
			out[key] = text
		}
	}
	base := "node." + d.Type + "."
	add(base+"help", d.Help)
	add(base+"compat", d.Compatibility.Notes)
	for i, l := range d.InputLabels {
		add(fmt.Sprintf("%sin.%d", base, i), l)
	}
	for i, l := range d.OutputLabels {
		add(fmt.Sprintf("%sout.%d", base, i), l)
	}
	propStrings(base+"prop.", d.Props, add)
	return out
}

func propStrings(prefix string, props []node.Prop, add func(key, text string)) {
	for _, p := range props {
		k := prefix + p.Name
		if p.Label != "" && p.Label != p.Name {
			add(k+".label", p.Label)
		}
		add(k+".help", p.Help)
		add(k+".placeholder", p.Placeholder)
		for _, o := range p.Options {
			add(k+".opt."+fmt.Sprint(o.Value), o.Label)
		}
		propStrings(k+".field.", p.Fields, add)
	}
}

// AllStrings is Strings for a whole set of descriptors.
func AllStrings(ds []node.Descriptor) map[string]string {
	out := map[string]string{}
	for _, d := range ds {
		for k, v := range Strings(d) {
			out[k] = v
		}
	}
	return out
}

// Keys returns a map's keys, sorted.
func Keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Localize returns the descriptors with their text in the language asked for.
// The input is not changed. A string a catalog does not have stays English, so
// a node type that arrived after the catalog (a plugin's, say) still shows.
func Localize(ds []node.Descriptor, lang string) []node.Descriptor {
	loadOnce.Do(load)
	cat := catalogs[lang]
	if lang == "en" || len(cat) == 0 {
		return ds
	}
	out := make([]node.Descriptor, len(ds))
	for i, d := range ds {
		out[i] = localizeOne(d, cat)
	}
	return out
}

func localizeOne(d node.Descriptor, cat map[string]string) node.Descriptor {
	base := "node." + d.Type + "."
	pick := func(key, english string) string {
		if t, ok := cat[key]; ok && t != "" {
			return t
		}
		return english
	}
	d.Help = pick(base+"help", d.Help)
	d.Compatibility.Notes = pick(base+"compat", d.Compatibility.Notes)
	if len(d.InputLabels) > 0 {
		labels := make([]string, len(d.InputLabels))
		for i, l := range d.InputLabels {
			labels[i] = pick(fmt.Sprintf("%sin.%d", base, i), l)
		}
		d.InputLabels = labels
	}
	if len(d.OutputLabels) > 0 {
		labels := make([]string, len(d.OutputLabels))
		for i, l := range d.OutputLabels {
			labels[i] = pick(fmt.Sprintf("%sout.%d", base, i), l)
		}
		d.OutputLabels = labels
	}
	d.Props = localizeProps(base+"prop.", d.Props, pick)
	return d
}

func localizeProps(prefix string, props []node.Prop, pick func(key, english string) string) []node.Prop {
	if len(props) == 0 {
		return props
	}
	out := make([]node.Prop, len(props))
	for i, p := range props {
		k := prefix + p.Name
		if p.Label != "" && p.Label != p.Name {
			p.Label = pick(k+".label", p.Label)
		}
		p.Help = pick(k+".help", p.Help)
		p.Placeholder = pick(k+".placeholder", p.Placeholder)
		if len(p.Options) > 0 {
			opts := make([]node.Option, len(p.Options))
			for j, o := range p.Options {
				o.Label = pick(k+".opt."+fmt.Sprint(o.Value), o.Label)
				opts[j] = o
			}
			p.Options = opts
		}
		p.Fields = localizeProps(k+".field.", p.Fields, pick)
		out[i] = p
	}
	return out
}
