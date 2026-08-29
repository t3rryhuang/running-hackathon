package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(Config{DatabasePath: filepath.Join(t.TempDir(), "iso.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func mustUser(t *testing.T, store *Store, phone string) *User {
	t.Helper()
	u, err := store.EnsureUser(phone)
	if err != nil {
		t.Fatalf("ensure user %s: %v", phone, err)
	}
	return u
}

// Two people must never see each other's journal, conversation, sessions,
// preferences or recommendations, and the number is the only identity.
func TestTwoUsersCannotSeeEachOther(t *testing.T) {
	store := testStore(t)
	alice := mustUser(t, store, "+447700900001")
	bob := mustUser(t, store, "+447700900002")
	if alice.ID == bob.ID {
		t.Fatal("two numbers collapsed into one user")
	}

	mood := 4
	if err := store.AddCheckin(&Checkin{UserID: alice.ID, Mood: &mood, Summary: "Ran by the canal.", Topics: "running"}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	if err := store.AddMessage(alice.ID, "user", "alice private message"); err != nil {
		t.Fatalf("add message: %v", err)
	}

	if got, _ := store.RecentCheckins(bob.ID, 10); len(got) != 0 {
		t.Errorf("bob can read alice's check-ins: %#v", got)
	}
	if got, _ := store.RecentMessages(bob.ID, 10); len(got) != 0 {
		t.Errorf("bob can read alice's messages: %#v", got)
	}

	// Sessions and checklists are per user, and neither can be read across.
	aSess, err := store.EnsureSession(alice, "sms", FlowFor(alice))
	if err != nil {
		t.Fatalf("alice session: %v", err)
	}
	bSess, err := store.EnsureSession(bob, "sms", FlowFor(bob))
	if err != nil {
		t.Fatalf("bob session: %v", err)
	}
	if aSess.ID == bSess.ID {
		t.Fatal("two users share one session")
	}
	if items, _ := store.Checklist(bob.ID, aSess.ID); len(items) != 0 {
		t.Errorf("bob read alice's checklist through her session id: %#v", items)
	}

	// Alice answers; nothing of hers appears on bob's profile or checklist.
	if _, err := store.RecordChecklistAnswer(alice, aSess.ID, "name", StatusAnswered, "Alice"); err != nil {
		t.Fatalf("alice name: %v", err)
	}
	if _, err := store.RecordChecklistAnswer(alice, aSess.ID, "event_types", StatusAnswered, "hackathons"); err != nil {
		t.Fatalf("alice answer: %v", err)
	}
	if got, ok := store.ChecklistAnswer(bob.ID, "event_types"); ok {
		t.Errorf("bob inherited alice's answer: %q", got)
	}
	freshBob, _ := store.UserByPhone(bob.Phone)
	if freshBob.Interests != "" {
		t.Errorf("bob's interests were written by alice's answer: %q", freshBob.Interests)
	}

	// Bob cannot answer an item in alice's session even with her session id.
	if _, err := store.RecordChecklistAnswer(bob, aSess.ID, "event_time", StatusAnswered, "evenings"); err == nil {
		t.Error("bob wrote into alice's session")
	}
}

// A suggestion belongs to the user it was offered to: knowing the id is not
// enough to accept or decline it.
func TestSuggestionStatusIsTenantScoped(t *testing.T) {
	store := testStore(t)
	if err := store.SeedEvents(NewCSVEventSource("events_live.csv", eventsCSV), false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	alice := mustUser(t, store, "+447700900011")
	bob := mustUser(t, store, "+447700900012")

	brain := NewBrain(store, NewCalendar(), nil, "", "")
	if _, err := brain.SuggestEvent(alice); err != nil {
		t.Fatalf("suggest: %v", err)
	}
	sg, err := store.OpenSuggestion(alice.ID)
	if err != nil {
		t.Fatalf("open suggestion: %v", err)
	}
	if err := store.SetSuggestionStatus(bob.ID, sg.ID, "accepted"); err == nil {
		t.Fatal("bob was allowed to accept a suggestion he does not own")
	}
	if _, err := store.OpenSuggestion(alice.ID); err != nil {
		t.Error("bob accepted alice's suggestion")
	}
	if accepted, _ := store.AcceptedSuggestions(bob.ID); len(accepted) != 0 {
		t.Errorf("bob acquired an acceptance he never made: %#v", accepted)
	}
}

// A brand new user has no history, and nothing puts a name on them.
func TestNewUserStartsWithNoHistoryOrName(t *testing.T) {
	store := testStore(t)
	u := mustUser(t, store, "+447700900013")
	if u.Name != "" {
		t.Errorf("new user arrived with a name: %q", u.Name)
	}
	if u.PhoneVerified() {
		t.Error("new user counted as verified before anything proved the number")
	}
	if got, _ := store.RecentCheckins(u.ID, 10); len(got) != 0 {
		t.Errorf("new user has history: %#v", got)
	}

	brain := NewBrain(store, NewCalendar(), nil, "", "")
	ctxBlock := brain.contextBlock(u)
	if strings.Contains(strings.ToLower(ctxBlock), "keanu") {
		t.Errorf("context leaked a hard-coded name:\n%s", ctxBlock)
	}
	for _, want := range []string{"name: (unknown)", "phone_verified: no", "this is their first conversation"} {
		if !strings.Contains(ctxBlock, want) {
			t.Errorf("context missing %q:\n%s", want, ctxBlock)
		}
	}
}

// Erasure removes everything, including the sensitive signals.
func TestForgetUserErasesEverything(t *testing.T) {
	store := testStore(t)
	u := mustUser(t, store, "+447700900014")
	if err := store.SetConsent(u.ID, KindHeartRate, true, "test"); err != nil {
		t.Fatalf("consent: %v", err)
	}
	if _, err := store.IngestSignal(u.ID, Signal{
		Kind: KindHeartRate, Value: "58", Unit: "bpm", Source: "test", ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := store.EnsureSession(u, "sms", FlowFor(u)); err != nil {
		t.Fatalf("session: %v", err)
	}
	if err := store.SaveTranscript(&Transcript{
		UserID: u.ID, ConversationID: "conv_forget", Body: "call text", StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("transcript: %v", err)
	}
	code, err := store.IssueLoginCode(u.Phone, time.Now())
	if err != nil {
		t.Fatalf("login code: %v", err)
	}
	token, err := store.StartAuthSession(u.ID, time.Now())
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	store.RecordAuthEvent(u.Phone, u.ID, "login_ok", "")

	if err := store.ForgetUser(u.ID); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, err := store.UserByPhone("+447700900014"); err == nil {
		t.Error("user survived erasure")
	}
	if r := store.LatestSignal(u.ID, KindHeartRate, time.Now()); r.Known {
		t.Error("signal survived erasure")
	}
	if _, total, err := store.Transcripts(u.ID, "", 10, 0); err != nil || total != 0 {
		t.Errorf("transcript survived erasure: %d (%v)", total, err)
	}
	if _, err := store.AuthSessionUser(token, time.Now()); err == nil {
		t.Error("sign-in session survived erasure")
	}
	// The code was keyed by phone rather than user id, so it needs its own
	// clause in ForgetUser - erasure must not leave a usable way back in.
	if err := store.ConsumeLoginCode(u.Phone, code, time.Now()); err == nil {
		t.Error("login code survived erasure")
	}
	if events, err := store.AuthEvents(u.Phone, 10); err != nil || len(events) != 0 {
		t.Errorf("audit rows survived erasure: %v (%v)", events, err)
	}
}

// The hourly sweep is where retention actually happens; each rule has its own
// window and one failing must not stop the others.
func TestRetentionSweepClearsTranscriptsAndDeadSessions(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	u := mustUser(t, store, "+447700900015")
	if err := store.SaveTranscript(&Transcript{
		UserID: u.ID, ConversationID: "conv_old", Body: "ancient", StartedAt: now.Add(-transcriptRetention - time.Hour),
	}); err != nil {
		t.Fatalf("old transcript: %v", err)
	}
	if err := store.SaveTranscript(&Transcript{
		UserID: u.ID, ConversationID: "conv_recent", Body: "recent", StartedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("recent transcript: %v", err)
	}
	token, err := store.StartAuthSession(u.ID, now.Add(-authSessionTTL-time.Hour))
	if err != nil {
		t.Fatalf("stale session: %v", err)
	}

	store.sweep(now)

	items, total, err := store.Transcripts(u.ID, "", 10, 0)
	if err != nil || total != 1 || items[0].ConversationID != "conv_recent" {
		t.Fatalf("sweep kept the wrong transcripts: %d %v (%v)", total, items, err)
	}
	if _, err := store.AuthSessionUser(token, now); err == nil {
		t.Error("expired session still resolves after the sweep")
	}
}
