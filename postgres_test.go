package main

import (
	"os"
	"testing"
	"time"
)

// Postgres is the production store; SQLite is the fallback. This test runs the
// same identity, checklist, signal and idempotency paths against a real server
// so the dialect rewriting and migrations are proven, not assumed. Set
// RUNHACK_TEST_DATABASE_URL to run it (see README).
func TestPostgresBackend(t *testing.T) {
	url := os.Getenv("RUNHACK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set RUNHACK_TEST_DATABASE_URL to run the Postgres integration test")
	}
	store, err := OpenStore(Config{DatabaseURL: url})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer store.Close()

	// Migrations are idempotent: opening the same database twice must not fail.
	again, err := OpenStore(Config{DatabaseURL: url})
	if err != nil {
		t.Fatalf("re-open postgres: %v", err)
	}
	again.Close()

	phone := "+4477009009" + time.Now().Format("05") + "1"
	u, err := store.EnsureUser(phone)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	t.Cleanup(func() { _ = store.ForgetUser(u.ID) })

	if same, err := store.EnsureUser(phone); err != nil || same.ID != u.ID {
		t.Fatalf("the same number produced a second user: %v %#v", err, same)
	}

	sess, err := store.EnsureSession(u.ID, "sms")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	item, err := store.NextChecklistItem(u.ID, sess.ID)
	if err != nil || item == nil {
		t.Fatalf("next item: %v %#v", err, item)
	}
	if item.Key != checklistTemplate[0].Key {
		t.Fatalf("checklist out of order on postgres: %s", item.Key)
	}
	if _, err := store.RecordChecklistAnswer(u, sess.ID, item.Key, StatusAnswered, "hackathons"); err != nil {
		t.Fatalf("record answer: %v", err)
	}
	if got, ok := store.ChecklistAnswer(u.ID, item.Key); !ok || got != "hackathons" {
		t.Fatalf("answer not persisted: %q %v", got, ok)
	}

	if err := store.SetConsent(u.ID, KindHeartRate, true, "test"); err != nil {
		t.Fatalf("consent: %v", err)
	}
	if _, err := store.IngestSignal(u.ID, Signal{
		Kind: KindHeartRate, Value: "60", Unit: "bpm", Source: "test", ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if r := store.LatestSignal(u.ID, KindHeartRate, time.Now()); !r.Known || r.Value != "60" {
		t.Fatalf("signal round trip failed: %#v", r)
	}

	key := "pg-test-" + phone
	if first, _, err := store.RememberWebhook("/sms", key, u.ID, ""); err != nil || !first {
		t.Fatalf("first delivery: %v %v", first, err)
	}
	if err := store.StoreWebhookResponse("/sms", key, "hello"); err != nil {
		t.Fatalf("store response: %v", err)
	}
	first, cached, err := store.RememberWebhook("/sms", key, u.ID, "")
	if err != nil || first || cached != "hello" {
		t.Fatalf("retry not replayed: first=%v cached=%q err=%v", first, cached, err)
	}

	// Exercise the query paths the HTTP surface uses, so a SQLite-only bit of
	// SQL cannot hide in seeding, matching or the journal.
	if err := store.SeedEvents(NewCSVEventSource("events_live.csv", eventsCSV), false); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	if _, err := store.RecentCheckins(u.ID, 10); err != nil {
		t.Fatalf("recent checkins: %v", err)
	}
	if _, err := store.RecentMessages(u.ID, 10); err != nil {
		t.Fatalf("recent messages: %v", err)
	}
	candidates, err := store.CandidateEvents(u.ID, candidateLimit)
	if err != nil {
		t.Fatalf("candidate events: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("no candidate events on postgres")
	}
	sgID, err := store.AddSuggestion(u.ID, candidates[0].ID)
	if err != nil {
		t.Fatalf("add suggestion: %v", err)
	}
	if _, err := store.OpenSuggestion(u.ID); err != nil {
		t.Fatalf("open suggestion: %v", err)
	}
	if err := store.SetSuggestionStatus(u.ID, sgID, "accepted"); err != nil {
		t.Fatalf("accept suggestion: %v", err)
	}
	if _, err := store.AcceptedSuggestions(u.ID); err != nil {
		t.Fatalf("accepted suggestions: %v", err)
	}

	if err := store.ForgetUser(u.ID); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, err := store.UserByPhone(phone); err == nil {
		t.Error("forgotten user still resolvable")
	}
}
