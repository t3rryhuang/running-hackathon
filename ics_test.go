package main

import (
	"testing"
	"time"
)

const sampleICS = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"BEGIN:VEVENT\r\n" +
	"DTSTART:20260915T140000Z\r\n" +
	"SUMMARY:Design review with the \r\n platform team\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"DTSTART;TZID=Europe/London:20260915T090000\r\n" +
	"SUMMARY:Standup\\, daily\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"DTSTART;VALUE=DATE:20260916\r\n" +
	"SUMMARY:Offsite\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"SUMMARY:No start time\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestUnfoldICSJoinsContinuationLines(t *testing.T) {
	lines := UnfoldICS("SUMMARY:Design review with the \r\n platform team\r\nDTSTART:20260915T140000Z")
	if len(lines) != 2 {
		t.Fatalf("want 2 unfolded lines, got %d: %#v", len(lines), lines)
	}
	if lines[0] != "SUMMARY:Design review with the platform team" {
		t.Fatalf("unexpected unfold: %q", lines[0])
	}
}

func TestParseICS(t *testing.T) {
	events := ParseICS(sampleICS)
	if len(events) != 3 {
		t.Fatalf("want 3 events (the one without DTSTART is dropped), got %d", len(events))
	}

	if got := events[0].Summary; got != "Design review with the platform team" {
		t.Errorf("summary: got %q", got)
	}
	// 14:00Z in September is 15:00 in London (BST).
	if got := events[0].Start.In(londonLoc).Format("2006-01-02 15:04"); got != "2026-09-15 15:00" {
		t.Errorf("utc dtstart: got %q", got)
	}
	if events[0].When != "15:00" {
		t.Errorf("when: got %q", events[0].When)
	}

	if got := events[1].Summary; got != "Standup, daily" {
		t.Errorf("escaped comma: got %q", got)
	}
	if got := events[1].Start.In(londonLoc).Format("15:04"); got != "09:00" {
		t.Errorf("tzid dtstart: got %q", got)
	}

	if !events[2].AllDay || events[2].When != "all day" {
		t.Errorf("date-only event should be all day: %#v", events[2])
	}
}

func TestEventsOnFiltersToTheDay(t *testing.T) {
	events := ParseICS(sampleICS)
	day := time.Date(2026, 9, 15, 12, 0, 0, 0, londonLoc)
	today := EventsOn(events, day)
	if len(today) != 2 {
		t.Fatalf("want 2 events on 15 Sept, got %d", len(today))
	}
	if len(EventsOn(events, day.AddDate(0, 0, 5))) != 0 {
		t.Fatalf("want no events five days later")
	}
}

func TestCalendarTodayEmptyURL(t *testing.T) {
	if got := NewCalendar().Today(""); got != nil {
		t.Fatalf("empty url should yield no events, got %#v", got)
	}
}
