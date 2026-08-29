package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signedUp is a person who has been through the introduction: verified
// number, with the name, interests and frequency they gave persisted.
func signedUp(t *testing.T, store *Store, phone, name, interests, frequency string) *User {
	t.Helper()
	u, err := store.EnsureUser(phone)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	if err := store.SaveOnboarding(u, name, interests, frequency); err != nil {
		t.Fatalf("save onboarding: %v", err)
	}
	if err := store.MarkPhoneVerified(u.ID, "signup"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	u, err = store.UserByPhone(phone)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return u
}

func callerContextFor(t *testing.T, store *Store, u *User) CallerContext {
	t.Helper()
	sess, err := store.EnsureSession(u, "call", FlowFor(u))
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	items, err := store.Checklist(u.ID, sess.ID)
	if err != nil {
		t.Fatalf("checklist: %v", err)
	}
	return buildCallerContext(u.Phone, u, items)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// A complete profile means the agent opens with the name and asks for none of
// what the form already collected.
func TestCallerContextCompleteProfileNeverReAsks(t *testing.T) {
	_, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	u := signedUp(t, store, "+447700900301", "Keanu", "hackathons", "daily")

	c := callerContextFor(t, store, u)
	if c.Greeting != "Hi Keanu, it's CheckIn." {
		t.Fatalf("greeting %q", c.Greeting)
	}
	for _, key := range []string{"name", "frequency", "event_types"} {
		if !c.Known[key].Known {
			t.Errorf("%s should be known", key)
		}
		if contains(c.Missing, key) {
			t.Errorf("%s asked for again", key)
		}
		if !contains(c.DoNotAsk, key) {
			t.Errorf("%s not in do_not_ask", key)
		}
	}
	if c.Known["event_types"].Value != "hackathons" || c.Known["event_types"].Source != SourceProfile {
		t.Errorf("event types not carried from the form: %+v", c.Known["event_types"])
	}
	if !strings.Contains(c.Instruction, "never ask for these again") {
		t.Errorf("instruction missing the do-not-ask rule: %q", c.Instruction)
	}
}

// A half-filled profile is asked only for the holes, in template order.
func TestCallerContextPartialProfileAsksOnlyMissing(t *testing.T) {
	_, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	u, err := store.EnsureUser("+447700900302")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	u.Name = "Jo"
	if err := store.UpsertUser(u); err != nil {
		t.Fatalf("save name: %v", err)
	}

	// Half way through the introduction: the name is on file, nothing else is,
	// so the call resumes at the first question they have not answered.
	c := callerContextFor(t, store, u)
	if contains(c.Missing, "name") {
		t.Error("name is on file but was asked for")
	}
	want := []string{"event_types", "event_time", "event_offer", "checkin_consent"}
	if strings.Join(c.Missing, ",") != strings.Join(want, ",") {
		t.Fatalf("missing = %v, want %v", c.Missing, want)
	}
	if !strings.Contains(c.Instruction, "one at a time") {
		t.Errorf("instruction dropped the one-at-a-time rule: %q", c.Instruction)
	}
}

// An unrecognised number gets no name, no history and nobody else's profile.
func TestCallerContextUnknownCaller(t *testing.T) {
	c := buildCallerContext("+447700900303", nil, nil)
	if c.Resolved || c.PhoneVerified {
		t.Fatal("unknown caller reported as resolved")
	}
	if len(c.Known) != 0 {
		t.Fatalf("unknown caller has known fields: %+v", c.Known)
	}
	if c.Greeting != "Hi, this is CheckIn." {
		t.Fatalf("greeting %q", c.Greeting)
	}
	if !strings.Contains(c.Instruction, "does not match anyone on file") {
		t.Errorf("instruction does not admit the caller is unresolved: %q", c.Instruction)
	}
	vars := c.DynamicVariables()
	if vars["user_name"] != "unknown" || vars["caller_known"] != "no" {
		t.Fatalf("unknown caller variables: %+v", vars)
	}
}

// An unverified number is not addressed by name even though one is stored.
func TestCallerContextUnverifiedPhoneWithholdsName(t *testing.T) {
	_, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	u, err := store.EnsureUser("+447700900304")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	if err := store.SaveOnboarding(u, "Terry", "meetups", "daily"); err != nil {
		t.Fatalf("save onboarding: %v", err)
	}
	u, _ = store.UserByPhone("+447700900304")

	c := callerContextFor(t, store, u)
	if c.PhoneVerified {
		t.Fatal("phone should not be verified")
	}
	if strings.Contains(c.Greeting, "Terry") {
		t.Fatalf("named an unverified caller: %q", c.Greeting)
	}
	if !strings.Contains(c.Instruction, "not been verified") {
		t.Errorf("instruction hides the verification gap: %q", c.Instruction)
	}
}

// Answers given in an earlier session carry into the next one, so a resumed
// caller is not walked through the same interview twice.
func TestCallerContextResumedSessionReusesSettledAnswers(t *testing.T) {
	_, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	u := signedUp(t, store, "+447700900305", "Sam", "hackathons", "daily")

	first, err := store.EnsureSession(u, "call", FlowFor(u))
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if _, err := store.RecordChecklistAnswer(u, first.ID, "evening_availability", StatusAnswered, "yes, after 7"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if _, err := store.RecordChecklistAnswer(u, first.ID, "notify_watch", StatusDeclined, ""); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if err := store.CloseSession(u.ID, first.ID); err != nil {
		t.Fatalf("close: %v", err)
	}

	c := callerContextFor(t, store, u)
	if c.Known["evening_availability"].Value != "yes, after 7" {
		t.Errorf("earlier answer lost: %+v", c.Known["evening_availability"])
	}
	if contains(c.Missing, "evening_availability") {
		t.Error("re-asked a question answered last time")
	}
	// Declined stays settled: no value stated, and never asked again.
	if c.Known["notify_watch"].Known {
		t.Error("declined item presented as a known value")
	}
	if contains(c.Missing, "notify_watch") {
		t.Error("re-asked a question the caller declined")
	}
}

// A time-bound answer ("free at 7 tonight?") is asked again once it goes stale,
// while stable profile facts are not.
func TestCallerContextStaleAnswerIsAskedAgain(t *testing.T) {
	_, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	u := signedUp(t, store, "+447700900306", "Ada", "hackathons", "daily")

	first, err := store.EnsureSession(u, "call", FlowFor(u))
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if _, err := store.RecordChecklistAnswer(u, first.ID, "evening_availability", StatusAnswered, "yes"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if err := store.CloseSession(u.ID, first.ID); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The next call carries the answer while it is still fresh.
	second, err := store.EnsureSession(u, "call", FlowFor(u))
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	freshItems, _ := store.Checklist(u.ID, second.ID)
	if contains(buildCallerContext(u.Phone, u, freshItems).Missing, "evening_availability") {
		t.Error("a fresh answer was treated as stale")
	}
	if err := store.CloseSession(u.ID, second.ID); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Age every copy of the answer past its 20h TTL.
	if _, err := store.exec(`UPDATE checklist_items SET answered_at=? WHERE user_id=? AND item_key=?`,
		time.Now().UTC().Add(-48*time.Hour), u.ID, "evening_availability"); err != nil {
		t.Fatalf("age answer: %v", err)
	}
	third, err := store.EnsureSession(u, "call", FlowFor(u))
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if third.ID == second.ID {
		t.Fatal("expected a new session")
	}
	items, _ := store.Checklist(u.ID, third.ID)
	stale := buildCallerContext(u.Phone, u, items)
	if !contains(stale.Missing, "evening_availability") {
		t.Errorf("stale answer not re-asked: missing=%v", stale.Missing)
	}
	if contains(stale.Missing, "event_types") {
		t.Error("a stable profile field went stale")
	}
}

// Two callers never see each other's context, and the second caller's presence
// does not change the first's.
func TestCallerContextIsolatedBetweenCallers(t *testing.T) {
	_, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	alice := signedUp(t, store, "+447700900307", "Alice", "hackathons", "daily")
	bob := signedUp(t, store, "+447700900308", "Bob", "conferences", "weekdays")

	a := callerContextFor(t, store, alice)
	b := callerContextFor(t, store, bob)

	if strings.Contains(a.Greeting, "Bob") || a.Known["event_types"].Value != "hackathons" || a.Known["frequency"].Value != "daily" {
		t.Fatalf("alice context contaminated: %+v", a)
	}
	if strings.Contains(b.Greeting, "Alice") || b.Known["event_types"].Value != "conferences" || b.Known["frequency"].Value != "weekdays" {
		t.Fatalf("bob context contaminated: %+v", b)
	}
	if a.Phone == b.Phone {
		t.Fatal("contexts share a phone number")
	}
}

// The outbound call carries the caller's own stored profile to the agent, so
// the prompt has no reason to ask for any of it.
func TestPlaceCallSendsStoredProfileAsDynamicVariables(t *testing.T) {
	srv, store, _, voice := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	u := signedUp(t, store, "+447700900309", "Keanu", "hackathons", "daily")

	if err := srv.placeCall(u); err != nil {
		t.Fatalf("place call: %v", err)
	}
	if len(voice.calls) != 1 {
		t.Fatalf("expected one call, got %d", len(voice.calls))
	}
	vars := voice.calls[0].Vars
	if vars["user_name"] != "Keanu" || vars["user_event_types"] != "hackathons" || vars["user_frequency"] != "daily" {
		t.Fatalf("dynamic variables missing stored profile: %+v", vars)
	}
	if vars["phone_verified"] != "yes" || vars["caller_known"] != "yes" {
		t.Fatalf("caller not presented as resolved: %+v", vars)
	}
	for _, key := range []string{"name", "event_types", "frequency"} {
		if strings.Contains(vars["ask_only"], key) {
			t.Errorf("%s listed as something to ask: %q", key, vars["ask_only"])
		}
	}
}

// /tools/get_context is a lookup: an unrecognised number returns an empty
// context and creates nothing.
func TestGetContextUnknownCallerCreatesNoUser(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	signedUp(t, store, "+447700900310", "Keanu", "hackathons", "daily")

	var out struct {
		Caller   CallerContext    `json:"caller"`
		Name     string           `json:"name"`
		Checkins []map[string]any `json:"recent_checkins"`
	}
	toolPost(t, srv, "/tools/get_context", map[string]any{"phone": "+447700900399"}, &out)

	if out.Caller.Resolved || out.Name != "" || len(out.Checkins) != 0 {
		t.Fatalf("unknown caller got a profile: %+v", out)
	}
	if _, err := store.UserByPhone("+447700900399"); err == nil {
		t.Fatal("a lookup created a user record")
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "Keanu") {
		t.Fatalf("another user's data leaked into an unknown caller's context: %s", raw)
	}
}

// A recognised number gets its own stored profile and nobody else's.
func TestGetContextResolvedCallerReturnsOwnProfileOnly(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	signedUp(t, store, "+447700900311", "Alice", "hackathons", "daily")
	signedUp(t, store, "+447700900312", "Bob", "conferences", "weekdays")

	var out struct {
		Caller CallerContext `json:"caller"`
	}
	toolPost(t, srv, "/tools/get_context", map[string]any{"phone": "+447700900311"}, &out)

	if !out.Caller.Resolved || out.Caller.Known["name"].Value != "Alice" {
		t.Fatalf("caller not resolved to themselves: %+v", out.Caller)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "Bob") || strings.Contains(string(raw), "conferences") {
		t.Fatalf("cross-tenant leak: %s", raw)
	}
}

// The webhook secret still guards the lookup.
func TestGetContextRequiresWebhookSecret(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	signedUp(t, store, "+447700900313", "Alice", "hackathons", "daily")

	req := httptest.NewRequest(http.MethodPost, "/tools/get_context",
		strings.NewReader(`{"phone":"+447700900313"}`))
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}
