// Ported from cronosjs 1.7.1 src/parser.ts, src/expression.ts and src/date.ts
// (ISC), Copyright (c) 2019, James Clarke. Modified. The ISC permission notice
// is in NOTICE.

// Package cron reads the crontab an Inject node carries and says when it fires
// next.
//
// It is a port of cronosjs 1.7.1, the library Node-RED 5 schedules Inject nodes
// with, and it follows that library rather than any other cron dialect, because
// an imported flow has to fire when it fired before. That means its syntax: five
// to seven fields with optional seconds and years, names for months and days,
// wrap-around ranges like Fri-Mon, L, W, # and the @daily family. And it means
// its daylight saving behaviour with the defaults Node-RED leaves in place: a
// run scheduled in the hour that goes missing in spring happens at the moment
// the clocks jump, and the hour that repeats in autumn is run once, not twice.
// On a line that starts a shift at 02:00 that is the difference between the
// shift starting and not.
//
// Times are local to whatever zone the caller passes, which for an Inject node
// is the process's own, same as Node-RED.
package cron

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var predefined = map[string]string{
	"@yearly":   "0 0 0 1 1 * *",
	"@annually": "0 0 0 1 1 * *",
	"@monthly":  "0 0 0 1 * * *",
	"@weekly":   "0 0 0 * * 0 *",
	"@daily":    "0 0 0 * * * *",
	"@midnight": "0 0 0 * * * *",
	"@hourly":   "0 0 * * * * *",
}

var (
	monthNames = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	dayNames   = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
	monthRe    = regexp.MustCompile(strings.Join(monthNames, "|"))
	dayRe      = regexp.MustCompile(strings.Join(dayNames, "|"))
	itemRe     = regexp.MustCompile(`^(?:(\*)|([0-9]+)|([0-9]+-[0-9]+))(?:/([1-9][0-9]*))?$`)
	nthRe      = regexp.MustCompile(`^[1-5]$`)
	fieldSplit = regexp.MustCompile(`\s+`)
)

// maxYear is the largest full year a JavaScript Date can hold, which is where
// cronosjs stops.
const maxYear = 275759

// Expr is a parsed crontab.
type Expr struct {
	src     string
	seconds []int
	minutes []int
	hours   []int
	days    daysValues
	months  []int
	years   []item
}

// String returns the crontab as it was given.
func (e *Expr) String() string { return e.src }

// Parse reads a crontab: five fields (minute hour day month weekday), six with
// a leading seconds field, seven with a trailing year, or one of @yearly,
// @annually, @monthly, @weekly, @daily, @midnight and @hourly.
func Parse(spec string) (*Expr, error) {
	expr := strings.ToLower(strings.TrimSpace(spec))
	if p, ok := predefined[expr]; ok {
		expr = p
	}
	fields := fieldSplit.Split(expr, -1)
	if len(fields) < 5 || len(fields) > 7 {
		return nil, fmt.Errorf("crontab %q: a crontab has 5 to 7 fields, this one has %d", spec, len(fields))
	}
	switch len(fields) {
	case 5:
		fields = append([]string{"0"}, fields...)
		fields = append(fields, "*")
	case 6:
		fields = append(fields, "*")
	}

	e := &Expr{src: spec}
	var err error
	wrap := func(name string, err error) error {
		return fmt.Errorf("crontab %q: %s field %w", spec, name, err)
	}
	if e.seconds, err = fieldValues(fields[0], 0, 59, nil); err != nil {
		return nil, wrap("seconds", err)
	}
	if e.minutes, err = fieldValues(fields[1], 0, 59, nil); err != nil {
		return nil, wrap("minutes", err)
	}
	if e.hours, err = fieldValues(fields[2], 0, 23, nil); err != nil {
		return nil, wrap("hours", err)
	}
	if e.days, err = parseDays(fields[3], fields[5]); err != nil {
		return nil, wrap("day", err)
	}
	months := monthRe.ReplaceAllStringFunc(fields[4], func(m string) string {
		return strconv.Itoa(slices.Index(monthNames, m) + 1)
	})
	if e.months, err = fieldValues(months, 1, 12, nil); err != nil {
		return nil, wrap("month", err)
	}
	for _, s := range strings.Split(fields[6], ",") {
		it, err := parseItem(s, 0, maxYear, false, nil)
		if err != nil {
			return nil, wrap("year", err)
		}
		e.years = append(e.years, it)
	}
	return e, nil
}

// item is one comma-separated part of a field: *, a value, a range, any of
// those with a /step.
type item struct {
	hasRange bool
	from     int
	to       int
	hasTo    bool
	step     int
}

func (it item) any() bool    { return !it.hasRange && it.step == 1 }
func (it item) single() bool { return it.hasRange && it.hasTo && it.from == it.to }

func parseItem(s string, first, last int, cyclic bool, transform func(int) int) (item, error) {
	m := itemRe.FindStringSubmatch(s)
	if m == nil {
		return item{}, fmt.Errorf("item %q is not a value, a range or *, with an optional /step", s)
	}
	it := item{step: 1}
	if m[4] != "" {
		it.step, _ = strconv.Atoi(m[4])
	}
	num := func(t string) (int, error) {
		n, err := strconv.Atoi(t)
		if err != nil {
			return 0, fmt.Errorf("item %q is out of range", s)
		}
		if transform != nil {
			n = transform(n)
		}
		return n, nil
	}
	switch {
	case m[2] != "":
		n, err := num(m[2])
		if err != nil {
			return item{}, err
		}
		if n < first || n > last {
			return item{}, fmt.Errorf("item %q is outside %d-%d", s, first, last)
		}
		it.hasRange, it.from = true, n
		if m[4] == "" {
			it.to, it.hasTo = n, true
		}
	case m[3] != "":
		ends := strings.SplitN(m[3], "-", 2)
		a, err := num(ends[0])
		if err != nil {
			return item{}, err
		}
		b, err := num(ends[1])
		if err != nil {
			return item{}, err
		}
		if a < first || a > last || b < first || b > last || (b < a && !cyclic) {
			return item{}, fmt.Errorf("range %q is outside %d-%d or runs backwards where that is not allowed", s, first, last)
		}
		it.hasRange, it.from, it.to, it.hasTo = true, a, b, true
	}
	return it, nil
}

func (it item) rangeLength(first, last int) int {
	start, end := first, last
	if it.hasRange {
		start = it.from
		if it.hasTo {
			end = it.to
		}
	}
	if end < start {
		return (last - start) + (end - first) + 1
	}
	return end - start
}

// values lists what the item selects. A wrap-around range counts on past the
// end of the field and comes round to the start, which is how Fri-Mon is Friday
// to Monday.
func (it item) values(first, last int) []int {
	start := first
	if it.hasRange {
		start = it.from
	}
	n := it.rangeLength(first, last)/it.step + 1
	out := make([]int, n)
	for i := range n {
		out[i] = first + (start-first+it.step*i)%(last-first+1)
	}
	return out
}

func unionValues(items []item, first, last int) []int {
	var out []int
	for _, it := range items {
		for _, v := range it.values(first, last) {
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	slices.Sort(out)
	return out
}

// fieldValues parses an ordinary field. Ranges in these fields may wrap.
func fieldValues(field string, first, last int, transform func(int) int) ([]int, error) {
	var items []item
	for _, s := range strings.Split(field, ",") {
		it, err := parseItem(s, first, last, true, transform)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return unionValues(items, first, last), nil
}

// daysValues is the day-of-month and day-of-week fields together, because
// together they decide which days a month has in it.
type daysValues struct {
	lastDay        bool
	lastWeekday    bool
	days           []int
	nearestWeekday []int
	daysOfWeek     []int
	lastDaysOfWeek []int
	nthDaysOfWeek  [][2]int // weekday, which one in the month
}

func parseDays(dom, dow string) (daysValues, error) {
	var dv daysValues
	var dayItems, nearestItems, dowItems, lastDowItems []item
	type nthItem struct {
		it  item
		nth int
	}
	var nthItems []nthItem

	for _, s := range strings.Split(dom, ",") {
		if s == "?" {
			s = "*"
		}
		switch {
		case s == "l":
			dv.lastDay = true
		case s == "lw":
			dv.lastWeekday = true
		case strings.HasSuffix(s, "w"):
			it, err := parseItem(strings.TrimSuffix(s, "w"), 1, 31, false, nil)
			if err != nil {
				return dv, err
			}
			nearestItems = append(nearestItems, it)
		default:
			it, err := parseItem(s, 1, 31, false, nil)
			if err != nil {
				return dv, err
			}
			dayItems = append(dayItems, it)
		}
	}

	dow = dayRe.ReplaceAllStringFunc(dow, func(m string) string {
		return strconv.Itoa(slices.Index(dayNames, m))
	})
	weekday := func(s string) (item, error) {
		// 7 is Sunday as well as 0.
		return parseItem(s, 0, 6, true, func(n int) int {
			if n == 7 {
				return 0
			}
			return n
		})
	}
	for _, s := range strings.Split(dow, ",") {
		if s == "?" {
			s = "*"
		}
		hash := strings.LastIndex(s, "#")
		switch {
		case strings.HasSuffix(s, "l"):
			it, err := weekday(strings.TrimSuffix(s, "l"))
			if err != nil {
				return dv, err
			}
			lastDowItems = append(lastDowItems, it)
		case hash != -1:
			nth := s[hash+1:]
			if !nthRe.MatchString(nth) {
				return dv, fmt.Errorf("item %q: # takes 1 to 5", s)
			}
			it, err := weekday(s[:hash])
			if err != nil {
				return dv, err
			}
			n, _ := strconv.Atoi(nth)
			nthItems = append(nthItems, nthItem{it, n})
		default:
			it, err := weekday(s)
			if err != nil {
				return dv, err
			}
			dowItems = append(dowItems, it)
		}
	}

	// A lone * in either day field means "no constraint here". Only when both
	// are a lone * is every day selected.
	allDays := !dv.lastDay && !dv.lastWeekday && len(nearestItems) == 0 && len(lastDowItems) == 0 &&
		len(nthItems) == 0 && len(dayItems) == 1 && dayItems[0].any() && len(dowItems) == 1 && dowItems[0].any()
	notAny := func(items []item) []item {
		var out []item
		for _, it := range items {
			if !it.any() {
				out = append(out, it)
			}
		}
		return out
	}
	if allDays {
		dv.days = unionValues([]item{{step: 1}}, 1, 31)
	} else {
		dv.days = unionValues(notAny(dayItems), 1, 31)
	}
	dv.nearestWeekday = unionValues(nearestItems, 1, 31)
	dv.daysOfWeek = unionValues(notAny(dowItems), 0, 6)
	dv.lastDaysOfWeek = unionValues(lastDowItems, 0, 6)
	for _, n := range nthItems {
		for _, d := range n.it.values(0, 6) {
			pair := [2]int{d, n.nth}
			if !slices.Contains(dv.nthDaysOfWeek, pair) {
				dv.nthDaysOfWeek = append(dv.nthDaysOfWeek, pair)
			}
		}
	}
	return dv, nil
}

// daysIn returns the days of a month the expression selects.
func (dv daysValues) daysIn(year, month int) []int {
	days := slices.Clone(dv.days)
	add := func(d int) {
		if !slices.Contains(days, d) {
			days = append(days, d)
		}
	}
	lastDate := time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
	firstDow := int(time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC).Weekday())

	nearestWeekday := func(day int) int {
		if day > lastDate {
			day = lastDate
		}
		dow := (day + firstDow - 1) % 7
		w := day
		switch dow {
		case 0:
			w++
		case 6:
			w--
		}
		// Never into the next or the previous month: the 1st falling on a
		// Saturday moves to Monday the 3rd, not Friday the 31st before it.
		if w < 1 {
			w += 3
		} else if w > lastDate {
			w -= 3
		}
		return w
	}

	if dv.lastDay {
		add(lastDate)
	}
	if dv.lastWeekday {
		add(nearestWeekday(lastDate))
	}
	for _, d := range dv.nearestWeekday {
		add(nearestWeekday(d))
	}
	if len(dv.daysOfWeek) > 0 || len(dv.lastDaysOfWeek) > 0 || len(dv.nthDaysOfWeek) > 0 {
		var byWeekday [7][]int
		for day := 1; day < 36; day++ {
			byWeekday[(day+firstDow-1)%7] = append(byWeekday[(day+firstDow-1)%7], day)
		}
		for _, w := range dv.daysOfWeek {
			for _, d := range byWeekday[w] {
				add(d)
			}
		}
		for _, w := range dv.lastDaysOfWeek {
			for i := len(byWeekday[w]) - 1; i >= 0; i-- {
				if byWeekday[w][i] <= lastDate {
					add(byWeekday[w][i])
					break
				}
			}
		}
		for _, p := range dv.nthDaysOfWeek {
			add(byWeekday[p[0]][p[1]-1])
		}
	}

	out := days[:0:0]
	for _, d := range days {
		if d <= lastDate {
			out = append(out, d)
		}
	}
	slices.Sort(out)
	return out
}

// nextYear returns the first year at or after from that the year field allows.
func (e *Expr) nextYear(from int) (int, bool) {
	best, found := 0, false
	consider := func(y int) {
		if !found || y < best {
			best, found = y, true
		}
	}
	for _, it := range e.years {
		switch {
		case it.any():
			consider(from)
		case it.single():
			if it.from >= from {
				consider(it.from)
			}
		default:
			start := 1970
			if it.hasRange {
				start = it.from
			}
			if start > from {
				consider(start)
				continue
			}
			next := start + (from-start+it.step-1)/it.step*it.step
			end := maxYear
			if it.hasTo {
				end = it.to
			}
			if next <= end {
				consider(next)
			}
		}
	}
	return best, found
}

// wall is a local date and time whose fields may run past their range, the
// way a search steps one second past :59. Turning it into an instant settles
// that.
type wall struct{ year, month, day, hour, minute, second int }

func wallOf(t time.Time, loc *time.Location) wall {
	t = t.In(loc)
	return wall{t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second()}
}

// naive is the wall time read as if it were UTC, which is how cronosjs
// compares two wall times to see whether an hour repeated.
func (w wall) naive() int64 {
	return time.Date(w.year, time.Month(w.month), w.day, w.hour, w.minute, w.second, 0, time.UTC).Unix()
}

// instant resolves a wall time in a zone the way a JavaScript Date does. A
// time that happens twice, in the hour that repeats, is the first of the two.
// A time that never happens, in the hour that goes missing, is read with the
// offset from before the change, which lands it as far past the gap as it was
// into it.
func (w wall) instant(loc *time.Location) time.Time {
	naive := w.naive()
	_, before := time.Unix(naive-86400, 0).In(loc).Zone()
	_, after := time.Unix(naive+86400, 0).In(loc).Zone()
	var valid []int64
	for _, off := range []int{before, after} {
		inst := naive - int64(off)
		if _, o := time.Unix(inst, 0).In(loc).Zone(); o == off && !slices.Contains(valid, inst) {
			valid = append(valid, inst)
		}
	}
	if len(valid) == 0 {
		return time.Unix(naive-int64(before), 0).In(loc)
	}
	return time.Unix(slices.Min(valid), 0).In(loc)
}

// Next returns the first time strictly after after that the expression
// selects, in loc's local time. It reports false when there is none, which
// happens when the year field runs out, or when the date can never exist, like
// the 30th of February. cronosjs searches for that forever; this gives up
// after the four hundred years the calendar takes to repeat itself.
func (e *Expr) Next(after time.Time, loc *time.Location) (time.Time, bool) {
	from := wallOf(after, loc)

	// Inside the repeated hour, its second pass is skipped entirely.
	if from.naive() == wallOf(after.Add(-time.Hour), loc).naive() {
		w := from
		w.minute, w.second = 59, 60
		next, ok := e.search(w)
		if !ok {
			return time.Time{}, false
		}
		return next.instant(loc), true
	}

	w := from
	w.second++
	next, ok := e.search(w)
	if !ok {
		return time.Time{}, false
	}
	at := next.instant(loc)

	// The selected time falls in the hour that went missing: run it at the
	// moment the clocks changed.
	nextHour := next
	nextHour.hour++
	if nextHour.instant(loc).Equal(at) {
		start := next
		start.minute, start.second = 0, 0
		return start.instant(loc), true
	}
	return at, true
}

// errNoMatch ends a search that cannot succeed.
var errNoMatch = errors.New("no match")

// search finds the first wall time at or after from that the expression
// selects, field by field from the year down.
func (e *Expr) search(from wall) (wall, bool) {
	year := from.year
	for range 401 {
		y, ok := e.nextYear(year)
		if !ok || y > maxYear {
			return wall{}, false
		}
		start := wall{year: y, month: 1, day: 1}
		if y == from.year {
			start = from
		}
		if w, err := e.searchMonth(start); err == nil {
			return w, true
		}
		year = y + 1
	}
	return wall{}, false
}

func (e *Expr) searchMonth(from wall) (wall, error) {
	for _, m := range e.months {
		if m < from.month {
			continue
		}
		start := wall{year: from.year, month: m, day: 1}
		if m == from.month {
			start = from
		}
		if w, err := e.searchDay(start); err == nil {
			return w, nil
		}
	}
	return wall{}, errNoMatch
}

func (e *Expr) searchDay(from wall) (wall, error) {
	for _, d := range e.days.daysIn(from.year, from.month) {
		if d < from.day {
			continue
		}
		start := wall{year: from.year, month: from.month, day: d}
		if d == from.day {
			start = from
		}
		if w, err := e.searchHour(start); err == nil {
			return w, nil
		}
	}
	return wall{}, errNoMatch
}

func (e *Expr) searchHour(from wall) (wall, error) {
	for _, h := range e.hours {
		if h < from.hour {
			continue
		}
		start := wall{year: from.year, month: from.month, day: from.day, hour: h}
		if h == from.hour {
			start = from
		}
		if w, err := e.searchMinute(start); err == nil {
			return w, nil
		}
	}
	return wall{}, errNoMatch
}

func (e *Expr) searchMinute(from wall) (wall, error) {
	for _, m := range e.minutes {
		if m < from.minute {
			continue
		}
		start := wall{year: from.year, month: from.month, day: from.day, hour: from.hour, minute: m}
		if m == from.minute {
			start = from
		}
		for _, s := range e.seconds {
			if s >= start.second {
				start.second = s
				return start, nil
			}
		}
	}
	return wall{}, errNoMatch
}
