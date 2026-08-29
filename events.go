package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"log"
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
