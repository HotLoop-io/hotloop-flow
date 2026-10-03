package flowtest

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// WriteText prints a report the way a person reads it in a terminal or a CI
// log: one line per test, the reasons under anything that didn't pass, and a
// last line that says whether the file passed. verbose adds the flow's own log
// under every test, not just the ones that failed.
func WriteText(w io.Writer, r *Report, verbose bool) error {
	var b strings.Builder
	for _, t := range r.Tests {
		fmt.Fprintf(&b, "--- %s: %s (%.2fs)\n", strings.ToUpper(string(t.Status)), t.Name, t.Seconds)
		for _, p := range t.Problems {
			fmt.Fprintf(&b, "    %s\n", indent(p.String(), "    "))
		}
		if t.Status != Pass || verbose {
			for _, l := range t.Log {
				fmt.Fprintf(&b, "    log: %s\n", l)
			}
		}
	}
	verdict := "ok"
	if !r.OK() {
		verdict = "FAIL"
	}
	name := r.File
	if name == "" {
		name = "tests"
	}
	fmt.Fprintf(&b, "%s\t%s\t%d passed, %d failed, %d errors (%.2fs)\n",
		verdict, name, r.Passed, r.Failed, r.Errored, r.Seconds)
	_, err := io.WriteString(w, b.String())
	return err
}

func indent(s, by string) string { return strings.ReplaceAll(s, "\n", "\n"+by) }

// JUnit XML, the shape every CI system reads: GitHub's test reporters,
// GitLab, Jenkins. A failure is an assertion that didn't hold; an error is a
// test that couldn't run.
type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Errors   int          `xml:"errors,attr"`
	Time     string       `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Time     string      `xml:"time,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitProblem `xml:"failure,omitempty"`
	Error     *junitProblem `xml:"error,omitempty"`
	SystemOut string        `xml:"system-out,omitempty"`
}

type junitProblem struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// WriteJUnit writes one JUnit document covering every report.
func WriteJUnit(w io.Writer, reports []Report) error {
	doc := junitSuites{}
	var total float64
	for _, r := range reports {
		s := junitSuite{
			Name: r.File, Tests: len(r.Tests), Failures: r.Failed, Errors: r.Errored,
			Time: seconds(r.Seconds),
		}
		for _, t := range r.Tests {
			c := junitCase{Name: t.Name, Classname: r.File, Time: seconds(t.Seconds)}
			if t.Status != Pass {
				lines := make([]string, len(t.Problems))
				for i, p := range t.Problems {
					lines[i] = p.String()
				}
				text := strings.Join(lines, "\n")
				first := text
				if i := strings.IndexByte(first, '\n'); i >= 0 {
					first = first[:i]
				}
				p := &junitProblem{Message: first, Text: text}
				if t.Status == Fail {
					p.Type = "expectation"
					c.Failure = p
				} else {
					p.Type = "error"
					c.Error = p
				}
			}
			if len(t.Log) > 0 {
				c.SystemOut = strings.Join(t.Log, "\n")
			}
			s.Cases = append(s.Cases, c)
		}
		doc.Tests += s.Tests
		doc.Failures += s.Failures
		doc.Errors += s.Errors
		total += r.Seconds
		doc.Suites = append(doc.Suites, s)
	}
	doc.Time = seconds(total)

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func seconds(s float64) string { return fmt.Sprintf("%.3f", s) }
