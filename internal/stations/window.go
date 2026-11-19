package stations

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window is a time of day in which a push may restart loggers without being
// asked: "02:00-04:00". It may run past midnight ("22:00-02:00").
type Window struct {
	start, end int // minutes into the day
	loc        *time.Location
	text       string
}

// ParseWindow reads "HH:MM-HH:MM" in the zone named (an IANA name, or "" for
// UTC). An empty text is no window.
func ParseWindow(text, zone string) (*Window, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	loc := time.UTC
	if zone != "" {
		l, err := time.LoadLocation(zone)
		if err != nil {
			return nil, fmt.Errorf("window zone %q: %w", zone, err)
		}
		loc = l
	}
	a, b, ok := strings.Cut(text, "-")
	if !ok {
		return nil, fmt.Errorf("window %q: want HH:MM-HH:MM", text)
	}
	s, err1 := clock(a)
	e, err2 := clock(b)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("window %q: want HH:MM-HH:MM", text)
	}
	if s == e {
		return nil, fmt.Errorf("window %q is empty", text)
	}
	return &Window{start: s, end: e, loc: loc, text: text + " " + loc.String()}, nil
}

func clock(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, fmt.Errorf("no colon")
	}
	hh, err := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("not a time")
	}
	return hh*60 + mm, nil
}

// Contains says whether now falls inside the window.
func (w *Window) Contains(now time.Time) bool {
	if w == nil {
		return false
	}
	t := now.In(w.loc)
	m := t.Hour()*60 + t.Minute()
	if w.start < w.end {
		return m >= w.start && m < w.end
	}
	return m >= w.start || m < w.end
}

func (w *Window) String() string {
	if w == nil {
		return ""
	}
	return w.text
}
