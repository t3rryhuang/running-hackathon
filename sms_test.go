package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAnthropic replays a scripted list of responses and records the requests it
// received, standing in for the real Messages API.
type fakeAnthropic struct {
	responses []AnthropicResponse
	requests  []AnthropicRequest
	err       error
}

func (f *fakeAnthropic) CreateMessage(_ context.Context, req AnthropicRequest) (*AnthropicResponse, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.responses) == 0 {
		return &AnthropicResponse{Content: []ContentBlock{textBlock("(no scripted response)")}}, nil
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	return &resp, nil
}

// recordedSMS is a Telephony stub that captures outbound traffic.
type recordedSMS struct {
	sms   []string
	calls []string
}

func (r *recordedSMS) SendSMS(to, body string) error {
	r.sms = append(r.sms, to+"|"+body)
	return nil
}

func (r *recordedSMS) StartCall(to string) error {
	r.calls = append(r.calls, to)
	return nil
}

func newTestServer(t *testing.T, client AnthropicClient) (*Server, *Store, *recordedSMS) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.SeedEvents(eventsCSV); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := Config{ToolWebhookSecret: "s3cret", AnthropicModel: "claude-sonnet-5"}
	tel := &recordedSMS{}
	brain := NewBrain(store, NewCalendar(), client, cfg.AnthropicModel, "")
	return NewServer(cfg, store, brain, tel, NewCalendar()), store, tel
}

func postSMS(t *testing.T, srv *Server, from, body string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"From": {from}, "Body": {body}}
	req := httptest.NewRequest(http.MethodPost, "/sms", strings.NewReader(form.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

func TestSMSWebhookRepliesWithTwiMLAndRemembersUser(t *testing.T) {
	fake := &fakeAnthropic{responses: []AnthropicResponse{
		{Content: []ContentBlock{textBlock("Glad you made it out. What was the best part?")}},
	}}
	srv, store, _ := newTestServer(t, fake)

	rec := postSMS(t, srv, "+447700900123", "went for a run today")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<Response>") || !strings.Contains(body, "<Message>Glad you made it out. What was the best part?</Message>") {
		t.Fatalf("unexpected TwiML: %s", body)
	}
	if ct := rec.Header().Get("content-type"); ct != "application/xml" {
		t.Errorf("content-type: %q", ct)
	}

	u, err := store.UserByPhone("+447700900123")
	if err != nil {
		t.Fatalf("user should be auto-created: %v", err)
	}
	msgs, _ := store.RecentMessages(u.ID, 10)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("conversation not persisted: %#v", msgs)
	}
	// The context preamble must carry the calendar/memory scaffolding.
	if len(fake.requests) != 1 {
		t.Fatalf("want 1 model call, got %d", len(fake.requests))
	}
	preamble := fake.requests[0].Messages[0].Content[0].Text
	for _, want := range []string{"<recent_checkins>", "<todays_calendar>", "<open_suggestion>"} {
		if !strings.Contains(preamble, want) {
			t.Errorf("preamble missing %s: %s", want, preamble)
		}
	}
}

func TestSMSToolLoopSavesCheckinAndOffersEvent(t *testing.T) {
	fake := &fakeAnthropic{responses: []AnthropicResponse{
		{StopReason: "tool_use", Content: []ContentBlock{
			{Type: "tool_use", ID: "t1", Name: "save_checkin",
				Input: json.RawMessage(`{"mood":2,"summary":"Rough day, deploy broke twice.","topics":"work, stress"}`)},
			{Type: "tool_use", ID: "t2", Name: "suggest_event", Input: json.RawMessage(`{}`)},
		}},
		{Content: []ContentBlock{textBlock("Sounds draining. There's a hack night on Wednesday - want me to put your name down?")}},
	}}
	srv, store, _ := newTestServer(t, fake)

	rec := postSMS(t, srv, "+447700900124", "everything broke today")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}

	u, _ := store.UserByPhone("+447700900124")
	checkins, _ := store.RecentCheckins(u.ID, 5)
	if len(checkins) != 1 {
		t.Fatalf("want 1 checkin, got %d", len(checkins))
	}
	if checkins[0].Mood == nil || *checkins[0].Mood != 2 || checkins[0].Topics != "work, stress" {
		t.Fatalf("checkin not saved correctly: %#v", checkins[0])
	}
	sg, err := store.OpenSuggestion(u.ID)
	if err != nil {
		t.Fatalf("want an open suggestion: %v", err)
	}

	// Second turn: the user accepts, and the suggestion flips to accepted.
	fake.responses = []AnthropicResponse{
		{StopReason: "tool_use", Content: []ContentBlock{
			{Type: "tool_use", ID: "t3", Name: "accept_suggestion", Input: json.RawMessage(`{}`)},
		}},
		{Content: []ContentBlock{textBlock("Done - you're on the list.")}},
	}
	rec = postSMS(t, srv, "+447700900124", "yes please")
	if !strings.Contains(rec.Body.String(), "on the list") {
		t.Fatalf("unexpected reply: %s", rec.Body.String())
	}
	if _, err := store.OpenSuggestion(u.ID); err == nil {
		t.Fatalf("suggestion should no longer be open")
	}
	accepted, _ := store.AcceptedSuggestions(u.ID)
	if len(accepted) != 1 || accepted[0].EventID != sg.EventID {
		t.Fatalf("accepted suggestion not recorded: %#v", accepted)
	}
}

func TestSMSDegradesGracefullyWithoutAnthropic(t *testing.T) {
	srv, _, _ := newTestServer(t, nil) // no API key configured
	rec := postSMS(t, srv, "+447700900125", "hello?")
	if rec.Code != http.StatusOK {
		t.Fatalf("must not 500 without a model, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "the one thing that stood out") {
		t.Fatalf("want canned fallback, got %s", rec.Body.String())
	}
}

func TestSMSDegradesGracefullyOnModelError(t *testing.T) {
	srv, _, _ := newTestServer(t, &fakeAnthropic{err: os.ErrDeadlineExceeded})
	rec := postSMS(t, srv, "+447700900126", "hi")
	if rec.Code != http.StatusOK {
		t.Fatalf("must not 500 when the model errors, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<Message>") {
		t.Fatalf("want TwiML fallback, got %s", rec.Body.String())
	}
}

func TestToolWebhooksRequireSecret(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	for _, path := range []string{"/tools/get_context", "/tools/save_checkin", "/tools/suggest_event", "/tools/accept_suggestion"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"phone":"+447700900127"}`))
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without secret: want 401, got %d", path, rec.Code)
		}
	}
}

func TestToolGetContextReturnsMemory(t *testing.T) {
	srv, store, _ := newTestServer(t, nil)
	u, _ := store.EnsureUser("+447700900128")
	mood := 4
	_ = store.AddCheckin(&Checkin{UserID: u.ID, Mood: &mood, Summary: "Good run", Topics: "running"})

	req := httptest.NewRequest(http.MethodPost, "/tools/get_context", strings.NewReader(`{"phone":"+447700900128"}`))
	req.Header.Set("X-Webhook-Secret", "s3cret")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		LastCheckins []struct {
			Summary string `json:"summary"`
		} `json:"last_checkins"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(out.LastCheckins) != 1 || out.LastCheckins[0].Summary != "Good run" {
		t.Fatalf("unexpected context: %s", rec.Body.String())
	}
}

func TestSignupCreatesUserAndSendsWelcomeSMS(t *testing.T) {
	srv, store, tel := newTestServer(t, nil)
	form := url.Values{"phone": {"+44 7700 900129"}, "channel": {"sms"}, "frequency": {"weekdays"}, "name": {"Keanu"}}
	req := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader(form.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	u, err := store.UserByPhone("+447700900129")
	if err != nil {
		t.Fatalf("user not created: %v", err)
	}
	if u.Frequency != "weekdays" || u.Name != "Keanu" {
		t.Fatalf("unexpected user: %#v", u)
	}
	if len(tel.sms) != 1 || !strings.Contains(tel.sms[0], "I'm CheckIn") {
		t.Fatalf("welcome sms not sent: %#v", tel.sms)
	}
}

func TestSignupRejectsNonE164(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	form := url.Values{"phone": {"07700900130"}, "channel": {"sms"}}
	req := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader(form.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestTriggerSendsOpeningMessageOrCall(t *testing.T) {
	srv, store, tel := newTestServer(t, nil)
	_ = store.CreateUser(&User{Phone: "+447700900131", Name: "Sam", Channel: "sms", Frequency: "daily"})
	_ = store.CreateUser(&User{Phone: "+447700900132", Channel: "call", Frequency: "daily"})

	for _, phone := range []string{"+447700900131", "+447700900132"} {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/trigger?phone="+url.QueryEscape(phone), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("trigger %s: %d %s", phone, rec.Code, rec.Body.String())
		}
	}
	if len(tel.sms) != 1 || !strings.Contains(tel.sms[0], "Hey Sam") {
		t.Fatalf("sms trigger: %#v", tel.sms)
	}
	if len(tel.calls) != 1 {
		t.Fatalf("call trigger: %#v", tel.calls)
	}
	u, _ := store.UserByPhone("+447700900131")
	if u.LastTriggeredAt == nil {
		t.Fatalf("last_triggered_at should be set")
	}
}

func TestJournalPageListsCheckins(t *testing.T) {
	srv, store, _ := newTestServer(t, nil)
	u, _ := store.EnsureUser("+447700900133")
	_ = store.AddCheckin(&Checkin{UserID: u.ID, Summary: "Shipped the webhook", Topics: "work"})

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/journal?phone=%2B447700900133", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Shipped the webhook") {
		t.Fatalf("journal missing checkin: %s", rec.Body.String())
	}
}

func TestDueNow(t *testing.T) {
	at := func(day, hour int) time.Time {
		return time.Date(2026, 9, day, hour, 5, 0, 0, londonLoc)
	}
	daily := &User{Frequency: "daily"}
	if !DueNow(daily, at(15, 9)) {
		t.Error("daily user should be due at 09:xx")
	}
	if DueNow(daily, at(15, 11)) {
		t.Error("daily user should not be due at 11:xx")
	}
	last := at(15, 9).Add(-time.Minute)
	fired := &User{Frequency: "daily", LastTriggeredAt: &last}
	if DueNow(fired, at(15, 9)) {
		t.Error("should not double-fire inside the same slot")
	}
	weekend := &User{Frequency: "weekdays"}
	if DueNow(weekend, at(19, 9)) { // 19 Sept 2026 is a Saturday
		t.Error("weekdays user should not fire at the weekend")
	}
	twice := &User{Frequency: "twice-daily"}
	if !DueNow(twice, at(15, 20)) {
		t.Error("twice-daily user should be due at 20:xx")
	}
}
