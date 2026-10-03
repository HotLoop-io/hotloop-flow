package cron

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// The reference. testdata/cronosjs-1.7.1.json is what cronosjs 1.7.1 itself
// answered, run the way Node-RED runs it, for every crontab in its own test
// suite, every shape Node-RED's Inject dialog writes, and the rest of the
// extended syntax, from starts either side of real daylight saving changes in
// six zones. testdata/golden.mjs regenerates it. Every answer has to match to
// the second.
type golden struct {
	Cronosjs string                         `json:"cronosjs"`
	N        int                            `json:"n"`
	Zones    map[string][][]json.RawMessage `json:"zones"`
	Parse    map[string]bool                `json:"parse"`
}

func loadGolden(t *testing.T) golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/cronosjs-1.7.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if g.Cronosjs != "1.7.1" {
		t.Fatalf("the golden file came from cronosjs %s", g.Cronosjs)
	}
	return g
}

func TestMatchesCronosjs(t *testing.T) {
	g := loadGolden(t)
	compared := 0
	for zone, cases := range g.Zones {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatalf("zone %s: %v", zone, err)
		}
		for _, c := range cases {
			var expr string
			var fromMs int64
			var want []int64
			if err := json.Unmarshal(c[0], &expr); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(c[1], &fromMs); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(c[2], &want); err != nil {
				t.Fatal(err)
			}
			e, err := Parse(expr)
			if err != nil {
				t.Errorf("%s: %v", expr, err)
				continue
			}
			from := time.UnixMilli(fromMs)
			base := from.Unix()
			var got []int64
			at := from
			for range g.N {
				next, ok := e.Next(at, loc)
				if !ok {
					break
				}
				got = append(got, next.Unix()-base)
				at = next
			}
			compared += len(want)
			if !slices.Equal(got, want) {
				t.Errorf("%s in %s from %s:\n  cronosjs %s\n  here     %s", expr, zone,
					from.In(loc).Format(time.RFC3339), show(base, want, loc), show(base, got, loc))
			}
		}
	}
	t.Logf("%d firings across %d zones match cronosjs %s", compared, len(g.Zones), g.Cronosjs)
}

func show(base int64, deltas []int64, loc *time.Location) string {
	var parts []string
	for _, d := range deltas {
		parts = append(parts, time.Unix(base+d, 0).In(loc).Format("2006-01-02 15:04:05 MST"))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func TestParsesWhatCronosjsParses(t *testing.T) {
	g := loadGolden(t)
	for expr, ok := range g.Parse {
		_, err := Parse(expr)
		if ok && err != nil {
			t.Errorf("%q: cronosjs accepts it, this refuses it: %v", expr, err)
		}
		if !ok && err == nil {
			t.Errorf("%q: cronosjs refuses it, this accepts it", expr)
		}
	}
}

// The two rules a shift schedule lives or dies by, spelt out with the
// examples cronosjs documents, rather than buried in the golden file.
func TestDaylightSaving(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, expr, from string
		want             []string
	}{
		{
			// 01:05, 01:25 and 01:45 never happen on 31 March 2019: the
			// clocks go from 01:00 to 02:00. The run happens at the jump.
			"a run in the missing hour happens when the clocks jump",
			"5/20 1 * * *", "2019-03-30T23:00:00Z",
			[]string{"2019-03-31T01:00:00Z", "2019-04-01T00:05:00Z", "2019-04-01T00:25:00Z"},
		},
		{
			// 01:00 to 01:59 happens twice on 27 October 2019. Once is
			// enough.
			"the repeated hour runs once",
			"*/20 1 * * *", "2019-10-26T23:00:00Z",
			[]string{"2019-10-27T00:00:00Z", "2019-10-27T00:20:00Z", "2019-10-27T00:40:00Z", "2019-10-28T01:00:00Z"},
		},
		{
			"starting inside the repeated hour skips the rest of it",
			"*/20 1 * * *", "2019-10-27T01:00:00Z",
			[]string{"2019-10-28T01:00:00Z", "2019-10-28T01:20:00Z"},
		},
	}
	for _, c := range cases {
		e, err := Parse(c.expr)
		if err != nil {
			t.Fatal(err)
		}
		at, _ := time.Parse(time.RFC3339, c.from)
		for i, w := range c.want {
			next, ok := e.Next(at, london)
			if !ok || next.UTC().Format(time.RFC3339) != w {
				t.Errorf("%s: firing %d = %s, want %s", c.name, i, next.UTC().Format(time.RFC3339), w)
				break
			}
			at = next
		}
	}
}

// cronosjs searches forever for a date that cannot exist and hangs whatever
// called it. Here the search ends and says there is no next time.
func TestImpossibleDatesEnd(t *testing.T) {
	for _, expr := range []string{"0 0 30 2 *", "0 0 31 4,6,9,11 *", "0 0 0 1 1 * 2019"} {
		e, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if next, ok := e.Next(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), time.UTC); ok {
			t.Errorf("%s: next = %s, want none", expr, next)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("%s: took %s to give up", expr, took)
		}
	}
}

func TestParseErrorsSayWhichField(t *testing.T) {
	for expr, want := range map[string]string{
		"* * * *":       "5 to 7 fields",
		"* 24 * * *":    "hours field",
		"* * 32 * *":    "day field",
		"* * * 13 *":    "month field",
		"60 * * * * *":  "seconds field",
		"0 0 * * Mon#6": "day field",
	} {
		_, err := Parse(expr)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v, want an error naming the %s", expr, err, want)
		}
	}
}
