package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// EventSource supplies the events the suggestion engine offers. The CSV export
// is the current implementation; a live HTTP feed can drop in behind the same
// interface without touching the store or the brain.
type EventSource interface {
	Name() string
	Events() ([]Event, error)
}

// csvEventSource reads the Hackathon Radar export format:
// title,starts_at,city,url,tags
type csvEventSource struct {
	name string
	data []byte
}

func NewCSVEventSource(name string, data []byte) EventSource {
	return &csvEventSource{name: name, data: data}
}

func (c *csvEventSource) Name() string { return c.name }

func (c *csvEventSource) Events() ([]Event, error) {
	r := csv.NewReader(bytes.NewReader(c.data))
	r.FieldsPerRecord = -1
	var out []Event
	var skipped int
	for line := 1; ; line++ {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", c.name, line, err)
		}
		if len(rec) < 5 {
			skipped++
			continue
		}
		if line == 1 && strings.EqualFold(strings.TrimSpace(rec[0]), "title") {
			continue
		}
		url := strings.TrimSpace(rec[3])
		if url == "" {
			// No registration link means nothing to offer the user.
			skipped++
			continue
		}
		start, err := parseEventTime(rec[1])
		if err != nil {
			skipped++
			continue
		}
		out = append(out, Event{
			Title:    strings.TrimSpace(rec[0]),
			StartsAt: start.UTC(),
			City:     strings.TrimSpace(rec[2]),
			URL:      url,
			Tags:     strings.TrimSpace(rec[4]),
		})
	}
	if skipped > 0 {
		log.Printf("events: %s: skipped %d rows (no url / unparseable date)", c.name, skipped)
	}
	return out, nil
}

// eventTimeLayouts covers RFC3339 plus the Postgres-style timestamps the
// Hackathon Radar export produces ("2026-09-30 11:30:00+00").
var eventTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02 15:04:05-07",
	"2006-01-02 15:04:05-07:00",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

func parseEventTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range eventTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, londonLoc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised starts_at %q", s)
}

// httpEventSource pulls the same schema from a URL the operator controls, so a
// live feed can replace the committed export without a rebuild. It accepts the
// CSV export format or a JSON array of the same fields, and falls back to the
// embedded CSV when the feed is unreachable.
type httpEventSource struct {
	url      string
	fallback EventSource
	client   *http.Client
}

func NewHTTPEventSource(url string, fallback EventSource) EventSource {
	return &httpEventSource{url: url, fallback: fallback, client: &http.Client{Timeout: 20 * time.Second}}
}

func (h *httpEventSource) Name() string { return h.url }

func (h *httpEventSource) Events() ([]Event, error) {
	events, err := h.fetch()
	if err == nil {
		return events, nil
	}
	log.Printf("events: feed %s unavailable (%v), using %s", h.url, err, h.fallback.Name())
	return h.fallback.Events()
}

func (h *httpEventSource) fetch() ([]Event, error) {
	resp, err := h.client.Get(h.url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
		return parseJSONEvents(body)
	}
	return NewCSVEventSource(h.url, body).Events()
}

func parseJSONEvents(body []byte) ([]Event, error) {
	var rows []struct {
		Title    string `json:"title"`
		StartsAt string `json:"starts_at"`
		City     string `json:"city"`
		URL      string `json:"url"`
		Tags     string `json:"tags"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, err
	}
	var out []Event
	for _, r := range rows {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		start, err := parseEventTime(r.StartsAt)
		if err != nil {
			continue
		}
		out = append(out, Event{
			Title:    strings.TrimSpace(r.Title),
			StartsAt: start.UTC(),
			City:     strings.TrimSpace(r.City),
			URL:      strings.TrimSpace(r.URL),
			Tags:     strings.TrimSpace(r.Tags),
		})
	}
	return out, nil
}
