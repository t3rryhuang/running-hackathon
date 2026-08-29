package main

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CalendarEvent is one calendar entry, already resolved to Europe/London.
type CalendarEvent struct {
	Summary string    `json:"summary"`
	Start   time.Time `json:"-"`
	When    string    `json:"when"`
	AllDay  bool      `json:"all_day"`
}

// londonLoc is Europe/London, falling back to UTC when tzdata is unavailable.
var londonLoc = func() *time.Location {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		return time.UTC
	}
	return loc
}()

// UnfoldICS joins RFC 5545 folded lines (continuation lines start with a space
// or tab) and normalises CRLF.
func UnfoldICS(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(out) > 0 {
			out[len(out)-1] += line[1:]
			continue
		}
		out = append(out, line)
	}
	return out
}

// ParseICS extracts VEVENTs with their SUMMARY and DTSTART. Times are converted
// to Europe/London; DATE-only values are treated as all-day.
func ParseICS(raw string) []CalendarEvent {
	var out []CalendarEvent
	var cur *CalendarEvent
	for _, line := range UnfoldICS(raw) {
		upper := strings.ToUpper(line)
		switch {
		case upper == "BEGIN:VEVENT":
			cur = &CalendarEvent{}
		case upper == "END:VEVENT":
			if cur != nil && !cur.Start.IsZero() {
				cur.When = formatWhen(*cur)
				out = append(out, *cur)
			}
			cur = nil
		case cur == nil:
			continue
		case strings.HasPrefix(upper, "SUMMARY"):
			cur.Summary = unescapeICSText(valueOf(line))
		case strings.HasPrefix(upper, "DTSTART"):
			name, value := splitProp(line)
			t, allDay, ok := parseICSTime(name, value)
			if ok {
				cur.Start = t
				cur.AllDay = allDay
			}
		}
	}
	return out
}

func splitProp(line string) (name, value string) {
	i := strings.Index(line, ":")
	if i < 0 {
		return line, ""
	}
	return line[:i], line[i+1:]
}

func valueOf(line string) string {
	_, v := splitProp(line)
	return v
}

func unescapeICSText(s string) string {
	r := strings.NewReplacer(`\n`, " ", `\N`, " ", `\,`, ",", `\;`, ";", `\\`, `\`)
	return strings.TrimSpace(r.Replace(s))
}

// parseICSTime handles the three DTSTART shapes that matter in practice:
// UTC (...Z), floating/zoned local time with a TZID param, and DATE-only.
func parseICSTime(name, value string) (time.Time, bool, bool) {
	value = strings.TrimSpace(value)
	loc := londonLoc
	for _, param := range strings.Split(name, ";")[1:] {
		if strings.HasPrefix(strings.ToUpper(param), "TZID=") {
			if l, err := time.LoadLocation(strings.Trim(param[5:], `"`)); err == nil {
				loc = l
			}
		}
	}
	if strings.HasSuffix(value, "Z") {
		if t, err := time.ParseInLocation("20060102T150405Z", value, time.UTC); err == nil {
			return t.In(londonLoc), false, true
		}
	}
	if len(value) == 15 {
		if t, err := time.ParseInLocation("20060102T150405", value, loc); err == nil {
			return t.In(londonLoc), false, true
		}
	}
	if len(value) == 8 {
		if t, err := time.ParseInLocation("20060102", value, londonLoc); err == nil {
			return t, true, true
		}
	}
	return time.Time{}, false, false
}

func formatWhen(e CalendarEvent) string {
	if e.AllDay {
		return "all day"
	}
	return e.Start.Format("15:04")
}

// EventsOn filters events to those starting on the given day in Europe/London.
func EventsOn(events []CalendarEvent, day time.Time) []CalendarEvent {
	day = day.In(londonLoc)
	y, m, d := day.Date()
	var out []CalendarEvent
	for _, e := range events {
		ey, em, ed := e.Start.In(londonLoc).Date()
		if ey == y && em == m && ed == d {
			out = append(out, e)
		}
	}
	return out
}

type cacheEntry struct {
	events  []CalendarEvent
	fetched time.Time
}

// Calendar fetches and caches ICS feeds for 5 minutes per URL.
type Calendar struct {
	mu     sync.Mutex
	cache  map[string]cacheEntry
	ttl    time.Duration
	client *http.Client
}

func NewCalendar() *Calendar {
	return &Calendar{
		cache:  map[string]cacheEntry{},
		ttl:    5 * time.Minute,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Today returns today's Europe/London events for the given ICS url. An empty
// url or any fetch error yields an empty list - calendar context is optional.
func (c *Calendar) Today(url string) []CalendarEvent {
	if url == "" {
		return nil
	}
	c.mu.Lock()
	entry, ok := c.cache[url]
	c.mu.Unlock()
	if !ok || time.Since(entry.fetched) > c.ttl {
		events, err := c.fetch(url)
		if err != nil {
			return nil
		}
		entry = cacheEntry{events: events, fetched: time.Now()}
		c.mu.Lock()
		c.cache[url] = entry
		c.mu.Unlock()
	}
	return EventsOn(entry.events, time.Now())
}

func (c *Calendar) fetch(url string) ([]CalendarEvent, error) {
	resp, err := c.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return ParseICS(string(body)), nil
}
