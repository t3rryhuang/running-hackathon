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

// Calendar fetches and caches ICS feeds. It is on the voice critical path: the
// agent calls get_context mid-conversation, so a slow Google ICS fetch would be
// silence the caller hears. The cache therefore serves stale data immediately
// and refreshes in the background, and a cold fetch is capped by maxBlock.
type Calendar struct {
	mu       sync.Mutex
	cache    map[string]cacheEntry
	inflight map[string]bool
	ttl      time.Duration
	maxBlock time.Duration
	client   *http.Client
}

func NewCalendar() *Calendar {
	return &Calendar{
		cache:    map[string]cacheEntry{},
		inflight: map[string]bool{},
		ttl:      5 * time.Minute,
		maxBlock: 1500 * time.Millisecond,
		client: &http.Client{
			Timeout: 10 * time.Second,
			// Keep the TLS connection to the calendar host alive between
			// check-ins so a refresh is one round trip, not a full handshake.
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
	}
}

// Today returns today's Europe/London events for the given ICS url. An empty
// url or any fetch error yields an empty list - calendar context is optional,
// and it is never worth making someone wait on the phone for it.
func (c *Calendar) Today(url string) []CalendarEvent {
	if url == "" {
		return nil
	}
	defer track("calendar.today")()

	c.mu.Lock()
	entry, have := c.cache[url]
	fresh := have && time.Since(entry.fetched) <= c.ttl
	c.mu.Unlock()

	if fresh {
		return EventsOn(entry.events, time.Now())
	}
	done := c.refresh(url)
	if have {
		// Stale beats slow: yesterday's copy of a calendar is nearly always
		// right, and the refresh will land before the next turn.
		metrics.Record("calendar.stale_hit", 0)
		return EventsOn(entry.events, time.Now())
	}
	select {
	case <-done:
	case <-time.After(c.maxBlock):
		// Cold cache and the feed is slow: answer without calendar context
		// rather than holding the call open.
		metrics.RecordErr("calendar.today", "cold fetch exceeded "+c.maxBlock.String())
		return nil
	}
	c.mu.Lock()
	entry = c.cache[url]
	c.mu.Unlock()
	return EventsOn(entry.events, time.Now())
}

// Warm pre-fetches a feed off the critical path - called when a call is placed
// so the agent's first get_context hits a warm cache.
func (c *Calendar) Warm(url string) {
	if url == "" {
		return
	}
	c.mu.Lock()
	entry, have := c.cache[url]
	c.mu.Unlock()
	if have && time.Since(entry.fetched) <= c.ttl {
		return
	}
	c.refresh(url)
}

// refresh starts one background fetch per url and returns a channel closed when
// that fetch finishes, so concurrent callers share a single request.
func (c *Calendar) refresh(url string) <-chan struct{} {
	done := make(chan struct{})
	c.mu.Lock()
	if c.inflight[url] {
		c.mu.Unlock()
		close(done)
		return done
	}
	c.inflight[url] = true
	c.mu.Unlock()

	go func() {
		defer close(done)
		start := time.Now()
		events, err := c.fetch(url)
		metrics.Record("calendar.fetch", time.Since(start))
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.inflight, url)
		if err != nil {
			metrics.RecordErr("calendar.fetch", err.Error())
			return
		}
		c.cache[url] = cacheEntry{events: events, fetched: time.Now()}
	}()
	return done
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
