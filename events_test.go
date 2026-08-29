package main

import (
	"path/filepath"
	"testing"
)

const exportSample = `title,starts_at,city,url,tags
London A.I. Networking Lunch,2026-09-30 11:30:00+00,London,https://example.com/ai-lunch,meetups
Executive Lunch: No Link Here,2026-09-16 11:00:00+00,London,,conferences
"Hack, Sleep, Repeat",2026-10-01T18:30:00+01:00,London,https://example.com/hack,hackathons
Broken Date,not-a-date,London,https://example.com/broken,meetups
`

func TestCSVEventSourceSkipsUnusableRows(t *testing.T) {
	events, err := NewCSVEventSource("test.csv", []byte(exportSample)).Events()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 usable events, got %d: %#v", len(events), events)
	}
	if events[0].Title != "London A.I. Networking Lunch" || events[0].StartsAt.UTC().Hour() != 11 {
		t.Errorf("postgres-style timestamp not parsed: %#v", events[0])
	}
	// Quoted commas must survive, and BST offsets must normalise to UTC.
	if events[1].Title != "Hack, Sleep, Repeat" || events[1].StartsAt.UTC().Hour() != 17 {
		t.Errorf("quoted/offset row wrong: %#v", events[1])
	}
}

func TestSeedEventsIsIdempotentAndReseeds(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	src := NewCSVEventSource("test.csv", []byte(exportSample))
	if err := store.SeedEvents(src, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	n, _ := store.countEvents()
	if n != 2 {
		t.Fatalf("want 2 events, got %d", n)
	}
	if err := store.SeedEvents(src, false); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	if n2, _ := store.countEvents(); n2 != 2 {
		t.Fatalf("re-running the seed must not duplicate rows: %d", n2)
	}

	// A fresh export with an extra row is picked up without -reseed.
	grown := exportSample + "New Event,2026-10-05 18:00:00+00,London,https://example.com/new,meetups\n"
	if err := store.SeedEvents(NewCSVEventSource("test.csv", []byte(grown)), false); err != nil {
		t.Fatalf("grown seed: %v", err)
	}
	if n3, _ := store.countEvents(); n3 != 3 {
		t.Fatalf("want 3 events after a bigger export, got %d", n3)
	}
}

// The committed export is what production seeds from, so it must parse.
func TestLiveExportParses(t *testing.T) {
	events, err := NewCSVEventSource("events_live.csv", eventsCSV).Events()
	if err != nil {
		t.Fatalf("events_live.csv: %v", err)
	}
	if len(events) < 100 {
		t.Fatalf("expected the full export, got %d events", len(events))
	}
	for _, e := range events {
		if e.URL == "" || e.Title == "" || e.StartsAt.IsZero() {
			t.Fatalf("incomplete event survived parsing: %#v", e)
		}
	}
}
