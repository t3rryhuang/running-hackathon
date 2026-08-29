package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var matchNow = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

func ev(title, city, tags string, days int) Event {
	return Event{
		ID:       int64(len(title)),
		Title:    title,
		City:     city,
		Tags:     tags,
		URL:      "https://example.com/" + url.PathEscape(title),
		StartsAt: matchNow.AddDate(0, 0, days),
	}
}

func userWith(interests string) *User {
	return &User{ID: 1, Phone: "+447700900001", Interests: interests}
}

func TestMatchPicksStatedInterest(t *testing.T) {
	candidates := []Event{
		ev("London Product Meetup", "London", "meetups", 3),
		ev("London AI Hackathon", "London", "non_uni_hackathon", 10),
	}
	m, err := MatchEvent(userWith("hackathons"), candidates, matchNow)
	if err != nil {
		t.Fatalf("want a match: %v", err)
	}
	if m.Event.Title != "London AI Hackathon" {
		t.Fatalf("wrong pick: %q", m.Event.Title)
	}
	if !strings.Contains(m.Why(), "hackathon") || !strings.Contains(m.Why(), "London") {
		t.Fatalf("suggestion should explain itself, got %q", m.Why())
	}
}

// The reported bug: a women-in-business event went to someone whose profile
// says nothing of the sort. Nothing about the user is inferred, so the event is
// simply not eligible.
func TestMatchExcludesAudienceRestrictedEvents(t *testing.T) {
	candidates := []Event{ev("Women in Business Networking Lunch", "London", "meetups", 4)}
	if _, err := MatchEvent(userWith("meetups"), candidates, matchNow); err != ErrNoMatch {
		t.Fatalf("restricted event must not be offered, got %v", err)
	}
	// ...unless the user said themselves that this is their community.
	m, err := MatchEvent(userWith("meetups, women in tech"), candidates, matchNow)
	if err != nil {
		t.Fatalf("explicit opt-in should be offered: %v", err)
	}
	if !strings.Contains(m.Why(), "community") {
		t.Fatalf("opt-in should be explained, got %q", m.Why())
	}
}

func TestMatchExcludesInviteOnlyAndOtherCities(t *testing.T) {
	cases := []struct {
		name  string
		event Event
	}{
		{"invite only", ev("Private Dinner: invite only", "London", "meetups", 5)},
		{"another city", ev("Berlin Dev Meetup", "Berlin", "meetups", 5)},
		{"already happened", ev("Yesterday's Meetup", "London", "meetups", -1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := MatchEvent(userWith("meetups"), []Event{tc.event}, matchNow); err != ErrNoMatch {
				t.Fatalf("want ErrNoMatch, got %v", err)
			}
		})
	}
}

func TestMatchHonoursExplicitDislikes(t *testing.T) {
	candidates := []Event{
		ev("AI Conference London", "London", "conferences", 6),
		ev("AI Builders Meetup", "London", "meetups", 20),
	}
	m, err := MatchEvent(userWith("ai, no conferences"), candidates, matchNow)
	if err != nil {
		t.Fatalf("want a match: %v", err)
	}
	if m.Event.Title != "AI Builders Meetup" {
		t.Fatalf("a stated dislike must exclude the event, got %q", m.Event.Title)
	}
}

func TestMatchOffersBroadPickWhenInterestsUnknown(t *testing.T) {
	candidates := []Event{ev("London Tech Meetup", "London", "meetups", 7)}
	m, err := MatchEvent(userWith(""), candidates, matchNow)
	if err != nil {
		t.Fatalf("want a broad pick: %v", err)
	}
	if !strings.Contains(m.Why(), "haven't told me") {
		t.Fatalf("low-confidence pick should say why, got %q", m.Why())
	}
	// Even without interests, restricted events stay off the table.
	restricted := []Event{ev("Women in Business Breakfast", "London", "meetups", 7)}
	if _, err := MatchEvent(userWith(""), restricted, matchNow); err != ErrNoMatch {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
}

func TestMatchNoMatchWhenNothingFitsInterests(t *testing.T) {
	candidates := []Event{ev("Knitting Circle", "London", "social", 5)}
	if _, err := MatchEvent(userWith("hardware"), candidates, matchNow); err != ErrNoMatch {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
}

func TestMatchPrefersSoonerAndOnlineIsAllowed(t *testing.T) {
	candidates := []Event{
		ev("Far Off AI Meetup", "London", "ai_ml", 200),
		ev("Next Week AI Meetup", "London", "ai_ml", 7),
		ev("Online AI Meetup", "Online", "ai_ml", 7),
	}
	m, err := MatchEvent(userWith("ai"), candidates, matchNow)
	if err != nil {
		t.Fatalf("want a match: %v", err)
	}
	if m.Event.Title != "Next Week AI Meetup" {
		t.Fatalf("soonest London event should win, got %q", m.Event.Title)
	}
	if _, err := MatchEvent(userWith("ai"), candidates[2:], matchNow); err != nil {
		t.Fatalf("online events are fine: %v", err)
	}
}

func TestNormaliseInterests(t *testing.T) {
	got := normaliseInterests("  Hackathons , meetups,, HACKATHONS , rust! ")
	if got != "hackathons, meetups, rust" {
		t.Fatalf("got %q", got)
	}
}

// End to end: the wizard's interests reach the database, the agent context, and
// the recommendation engine.
func TestSignupCapturesInterestsThroughToRecommendation(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)

	form := url.Values{
		"name":      {"Keanu"},
		"phone":     {"+447700900150"},
		"channel":   {"sms"},
		"frequency": {"daily"},
		"interests": {"Hackathons, AI"},
	}
	req := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader(form.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("signup status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Interests string `json:"interests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out.Interests != "hackathons, ai" {
		t.Fatalf("signup should echo stored interests, got %q", out.Interests)
	}

	u, err := store.UserByPhone("+447700900150")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if u.Interests != "hackathons, ai" {
		t.Fatalf("interests not persisted: %q", u.Interests)
	}
	if want := []string{"hackathon", "ai"}; !equalStrings(u.InterestList(), want) {
		t.Fatalf("recommendation view of interests = %#v", u.InterestList())
	}

	// A later signup that skips the question keeps the stored answer.
	form.Del("interests")
	req = httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader(form.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")
	srv.Routes().ServeHTTP(httptest.NewRecorder(), req)
	u, _ = store.UserByPhone("+447700900150")
	if u.Interests != "hackathons, ai" {
		t.Fatalf("re-signup wiped interests: %q", u.Interests)
	}

	// And the agent context exposes them.
	req = httptest.NewRequest(http.MethodPost, "/tools/get_context", strings.NewReader(`{"phone":"+447700900150"}`))
	req.Header.Set("X-Webhook-Secret", "s3cret")
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "hackathons, ai") {
		t.Fatalf("context missing interests: %s", rec.Body.String())
	}
}

func TestSuggestEventToolReportsNoMatchHonestly(t *testing.T) {
	store, err := OpenStore(Config{DatabasePath: filepath.Join(t.TempDir(), "nomatch.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	// Only an event this user has no stated interest in.
	src := NewCSVEventSource("t.csv", []byte("title,starts_at,city,url,tags\nKnitting Circle,2099-01-01 18:00:00+00,London,https://example.com/k,social\n"))
	if err := store.SeedEvents(src, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := Config{ToolWebhookSecret: "s3cret"}
	brain := NewBrain(store, NewCalendar(), nil, "", "")
	srv := NewServer(cfg, store, brain, &recordedSMS{}, &recordedVoice{}, NewCalendar())

	u, _ := store.EnsureUser("+447700900151")
	if err := store.SaveOnboarding(u, "Keanu", "hardware", ""); err != nil {
		t.Fatalf("onboarding: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/tools/suggest_event", strings.NewReader(`{"phone":"+447700900151"}`))
	req.Header.Set("X-Webhook-Secret", "s3cret")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	var out struct {
		Event   any    `json:"event"`
		NoMatch bool   `json:"no_match"`
		Say     string `json:"say"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if !out.NoMatch || out.Event != nil || out.Say == "" {
		t.Fatalf("want an honest no-match, got %s", rec.Body.String())
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
