package main

import (
	"context"
	"encoding/json"
	"io"
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

// recordedSMS is a Telephony stub that captures outbound texts.
type recordedSMS struct {
	sms       []string
	hangups   []string
	hangupErr error
	sendErr   error
}

func (r *recordedSMS) SendSMS(to, body string) error {
	if r.sendErr != nil {
		return r.sendErr
	}
	r.sms = append(r.sms, to+"|"+body)
	return nil
}

func (r *recordedSMS) HangUp(callSID string) error {
	r.hangups = append(r.hangups, callSID)
	return r.hangupErr
}

// recordedVoice is a Voice stub that captures outbound agent calls.
type recordedVoice struct {
	calls []CallRequest
	sid   string
	err   error
}

func (r *recordedVoice) Call(cr CallRequest) (CallResult, error) {
	if r.err != nil {
		return CallResult{}, r.err
	}
	r.calls = append(r.calls, cr)
	return CallResult{CallSID: r.sid, ConversationID: "conv_test"}, nil
}

func (r *recordedVoice) numbers() []string {
	var out []string
	for _, c := range r.calls {
		out = append(out, c.To)
	}
	return out
}

func newTestServer(t *testing.T, client AnthropicClient) (*Server, *Store, *recordedSMS, *recordedVoice) {
	t.Helper()
	store, err := OpenStore(Config{DatabasePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.SeedEvents(NewCSVEventSource("events_live.csv", eventsCSV), false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := Config{ToolWebhookSecret: "s3cret", AnthropicModel: "claude-sonnet-5"}
	tel := &recordedSMS{}
	voice := &recordedVoice{sid: "CAtest123"}
	brain := NewBrain(store, NewCalendar(), client, cfg.AnthropicModel, "")
	srv := NewServer(cfg, store, brain, tel, voice, NewCalendar())
	srv.afterFunc = func(_ time.Duration, f func()) *time.Timer {
		f()
		return time.NewTimer(time.Hour)
	}
	return srv, store, tel, voice
}

// settleChecklist walks both interviews to the end for a user and marks them
// onboarded, so a test can exercise the journalling loop rather than the
// introduction and the check-in checklist that precede it.
func settleChecklist(t *testing.T, store *Store, phone string) *User {
	t.Helper()
	u, err := store.EnsureUser(phone)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	for _, flow := range []string{FlowOnboarding, FlowCheckin} {
		sess, err := store.EnsureSession(u, "sms", flow)
		if err != nil {
			t.Fatalf("ensure %s session: %v", flow, err)
		}
		for {
			item, err := store.NextChecklistItem(u.ID, sess.ID)
			if err != nil {
				t.Fatalf("next item: %v", err)
			}
			if item == nil {
				break
			}
			if _, err := store.RecordChecklistAnswer(u, sess.ID, item.Key, StatusSkipped, ""); err != nil {
				t.Fatalf("settle %s: %v", item.Key, err)
			}
		}
		if flow == FlowOnboarding {
			if err := store.MarkOnboarded(u); err != nil {
				t.Fatalf("mark onboarded: %v", err)
			}
			if err := store.CloseSession(u.ID, sess.ID); err != nil {
				t.Fatalf("close onboarding session: %v", err)
			}
		}
	}
	return u
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
	srv, store, _, _ := newTestServer(t, fake)
	settleChecklist(t, store, "+447700900123")

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
	srv, store, _, _ := newTestServer(t, fake)
	settleChecklist(t, store, "+447700900124")

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
	srv, store, _, _ := newTestServer(t, nil) // no API key configured
	settleChecklist(t, store, "+447700900125")
	rec := postSMS(t, srv, "+447700900125", "hello?")
	if rec.Code != http.StatusOK {
		t.Fatalf("must not 500 without a model, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "the one thing that stood out") {
		t.Fatalf("want canned fallback, got %s", rec.Body.String())
	}
}

func TestSMSDegradesGracefullyOnModelError(t *testing.T) {
	srv, _, _, _ := newTestServer(t, &fakeAnthropic{err: os.ErrDeadlineExceeded})
	rec := postSMS(t, srv, "+447700900126", "hi")
	if rec.Code != http.StatusOK {
		t.Fatalf("must not 500 when the model errors, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<Message>") {
		t.Fatalf("want TwiML fallback, got %s", rec.Body.String())
	}
}

func TestToolWebhooksRequireSecret(t *testing.T) {
	srv, _, _, _ := newTestServer(t, nil)
	for _, path := range []string{"/tools/get_context", "/tools/save_onboarding", "/tools/save_checkin", "/tools/suggest_event", "/tools/accept_suggestion"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"phone":"+447700900127"}`))
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without secret: want 401, got %d", path, rec.Code)
		}
	}
}

func TestToolGetContextReturnsMemory(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
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
		Onboarded    bool   `json:"onboarded"`
		Interests    string `json:"interests"`
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
	if out.Onboarded {
		t.Errorf("a user the agent has never interviewed must report onboarded=false")
	}
}

// The voice agent interviews new callers, then posts what it learned; the next
// get_context must show the user as onboarded so it skips the interview.
func TestToolSaveOnboardingMarksUserOnboarded(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/tools/save_onboarding",
		strings.NewReader(`{"phone":"+447700900140","name":"Joseph","interests":"hackathons, meetups","frequency":"weekdays"}`))
	req.Header.Set("X-Webhook-Secret", "s3cret")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	u, err := store.UserByPhone("+447700900140")
	if err != nil {
		t.Fatalf("user not created: %v", err)
	}
	if u.Name != "Joseph" || u.Interests != "hackathons, meetups" || u.Frequency != "weekdays" {
		t.Fatalf("onboarding not stored: %#v", u)
	}
	if !u.Onboarded() {
		t.Fatalf("onboarded_at should be stamped")
	}

	req = httptest.NewRequest(http.MethodPost, "/tools/get_context", strings.NewReader(`{"phone":"+447700900140"}`))
	req.Header.Set("X-Webhook-Secret", "s3cret")
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	var out struct {
		Onboarded bool   `json:"onboarded"`
		Interests string `json:"interests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if !out.Onboarded || out.Interests != "hackathons, meetups" {
		t.Fatalf("context should reflect the interview: %s", rec.Body.String())
	}
}

func TestSuggestEventPrefersStatedInterests(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
	u, _ := store.EnsureUser("+447700900141")
	if _, err := store.SaveOnboarding(u, "", "hackathons", ""); err != nil {
		t.Fatalf("save onboarding: %v", err)
	}
	// The committed export has no London hackathon with a registration URL, so
	// seed one rather than depending on whatever the live data happens to hold.
	extra := "title,starts_at,city,url,tags\nLondon Autumn Hackathon,2099-01-02 09:00:00+00,London,https://example.com/hack,non_uni_hackathon\n"
	if err := store.SeedEvents(NewCSVEventSource("extra.csv", []byte(extra)), false); err != nil {
		t.Fatalf("seed extra: %v", err)
	}

	match, err := srv.brain.SuggestEvent(u)
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	if !strings.Contains(strings.ToLower(match.Event.Tags), "hackathon") {
		t.Fatalf("want a hackathon-tagged event, got %q tagged %q", match.Event.Title, match.Event.Tags)
	}
	if match.Why() == "" {
		t.Fatalf("suggestion should explain itself")
	}
}

func TestCallEndpointRingsThroughVoice(t *testing.T) {
	srv, _, _, voice := newTestServer(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/call", strings.NewReader(`{"phone":"+447700900142"}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(voice.calls) != 1 || voice.calls[0].To != "+447700900142" {
		t.Fatalf("call not placed: %#v", voice.calls)
	}
	// Nobody has said their name yet, so the agent is told to ask for it
	// rather than being handed one by the website.
	if voice.calls[0].Name != "" || voice.calls[0].Vars["user_name"] != "unknown" {
		t.Fatalf("call invented a name: %#v", voice.calls[0])
	}
	if !strings.Contains(voice.calls[0].Vars["ask_only"], "name") {
		t.Fatalf("agent not asked to introduce itself: %#v", voice.calls[0].Vars)
	}

	rec = httptest.NewRecorder()
	bad := httptest.NewRequest(http.MethodPost, "/call", strings.NewReader(`{"phone":"07700900142"}`))
	bad.Header.Set("content-type", "application/json")
	srv.Routes().ServeHTTP(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-E.164 should 400, got %d", rec.Code)
	}
}

// The ElevenLabs request is asserted rather than sent: no key on this box.
func TestElevenLabsOutboundRequestShape(t *testing.T) {
	v := NewVoice(Config{ElevenLabsAPIKey: "key-123", ElevenLabsAgentID: "agent_abc", ElevenLabsPhoneID: "phnum_xyz"})
	el, ok := v.(*elevenLabsVoice)
	if !ok {
		t.Fatalf("want a live ElevenLabs caller, got %T", v)
	}
	req, err := el.newRequest(CallRequest{To: "+447700900143", Name: "Keanu"})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if req.URL.String() != "https://api.elevenlabs.io"+elevenLabsOutboundPath {
		t.Errorf("url: %s", req.URL)
	}
	if got := req.Header.Get("xi-api-key"); got != "key-123" {
		t.Errorf("xi-api-key: %q", got)
	}
	body, _ := io.ReadAll(req.Body)
	var sent outboundCallRequest
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("bad body %s: %v", body, err)
	}
	if sent.AgentID != "agent_abc" || sent.AgentPhoneNumberID != "phnum_xyz" || sent.ToNumber != "+447700900143" {
		t.Fatalf("body = %#v", sent)
	}
	if sent.ClientData == nil || sent.ClientData.DynamicVariables["user_name"] != "Keanu" {
		t.Fatalf("agent should get the name as a dynamic variable: %s", body)
	}

	if _, ok := NewVoice(Config{ElevenLabsAPIKey: "key-123"}).(logVoice); !ok {
		t.Errorf("missing agent/phone ids should fall back to the logging stub")
	}
}

func TestSettingsChangesFrequency(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
	_, _ = store.EnsureUser("+447700900144")

	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(`{"phone":"+447700900144","frequency":"twice-daily"}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	u, _ := store.UserByPhone("+447700900144")
	if u.Frequency != "twice-daily" {
		t.Fatalf("frequency not saved: %q", u.Frequency)
	}

	rec = httptest.NewRecorder()
	bad := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(`{"phone":"+447700900144","frequency":"hourly"}`))
	bad.Header.Set("content-type", "application/json")
	srv.Routes().ServeHTTP(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown frequency should 400, got %d", rec.Code)
	}
}

// The wizard posts the same form but wants JSON back so it can drive the last
// two steps client-side.
func TestSignupReturnsJSONForWizard(t *testing.T) {
	srv, _, _, _ := newTestServer(t, nil)
	form := url.Values{"phone": {"+447700900145"}, "channel": {"call"}, "frequency": {"daily"}}
	req := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader(form.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	var out struct {
		OK      bool   `json:"ok"`
		Channel string `json:"channel"`
		Journal string `json:"journal"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json %s: %v", rec.Body.String(), err)
	}
	if !out.OK || out.Channel != "call" || !strings.Contains(out.Journal, "%2B447700900145") {
		t.Fatalf("unexpected signup json: %s", rec.Body.String())
	}
}

// The website captures a number and a channel and nothing else: the first
// question is put to them in the conversation itself.
func TestSignupCreatesUserAndOpensTheConversation(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	form := url.Values{"phone": {"+44 7700 900129"}, "channel": {"sms"}}
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
	if u.Name != "" || u.Interests != "" || u.Onboarded() {
		t.Fatalf("website assumed something about them: %#v", u)
	}
	if len(tel.sms) != 1 || !strings.Contains(tel.sms[0], onboardingTemplate[0].Prompt) {
		t.Fatalf("introduction not opened: %#v", tel.sms)
	}
}

func TestSignupRejectsNonE164(t *testing.T) {
	srv, _, _, _ := newTestServer(t, nil)
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
	srv, store, tel, voice := newTestServer(t, nil)
	_ = store.CreateUser(&User{Phone: "+447700900131", Name: "Sam", Channel: "sms", Frequency: "daily"})
	_ = store.CreateUser(&User{Phone: "+447700900132", Channel: "call", Frequency: "daily"})
	// A check-in only goes to someone who has already been introduced.
	sam, _ := store.UserByPhone("+447700900131")
	if err := store.MarkOnboarded(sam); err != nil {
		t.Fatalf("mark onboarded: %v", err)
	}

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
	if got := voice.numbers(); len(got) != 1 || got[0] != "+447700900132" {
		t.Fatalf("call trigger should go through ElevenLabs: %#v", got)
	}
	u, _ := store.UserByPhone("+447700900131")
	if u.LastTriggeredAt == nil {
		t.Fatalf("last_triggered_at should be set")
	}
}

func TestJournalPageListsCheckins(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
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
	onboarded := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	daily := &User{Frequency: "daily", OnboardedAt: &onboarded}
	if !DueNow(daily, at(15, 9)) {
		t.Error("daily user should be due at 09:xx")
	}
	if DueNow(daily, at(15, 11)) {
		t.Error("daily user should not be due at 11:xx")
	}
	last := at(15, 9).Add(-time.Minute)
	fired := &User{Frequency: "daily", LastTriggeredAt: &last, OnboardedAt: &onboarded}
	if DueNow(fired, at(15, 9)) {
		t.Error("should not double-fire inside the same slot")
	}
	weekend := &User{Frequency: "weekdays", OnboardedAt: &onboarded}
	if DueNow(weekend, at(19, 9)) { // 19 Sept 2026 is a Saturday
		t.Error("weekdays user should not fire at the weekend")
	}
	twice := &User{Frequency: "twice-daily", OnboardedAt: &onboarded}
	if !DueNow(twice, at(15, 20)) {
		t.Error("twice-daily user should be due at 20:xx")
	}
	if DueNow(&User{Frequency: "daily"}, at(15, 9)) {
		t.Error("somebody who has not been introduced was put on a schedule")
	}
}

func TestOnboardingCompletionHangsUpTheCall(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)

	rec := httptest.NewRecorder()
	call := httptest.NewRequest(http.MethodPost, "/call", strings.NewReader(`{"phone":"+447700900150","name":"Keanu"}`))
	call.Header.Set("content-type", "application/json")
	srv.Routes().ServeHTTP(rec, call)
	if rec.Code != http.StatusOK {
		t.Fatalf("call: %d %s", rec.Code, rec.Body.String())
	}
	u, err := store.UserByPhone("+447700900150")
	if err != nil || u.LastCallSID != "CAtest123" {
		t.Fatalf("call sid not remembered: %#v (%v)", u, err)
	}

	rec = httptest.NewRecorder()
	done := httptest.NewRequest(http.MethodPost, "/tools/save_onboarding",
		strings.NewReader(`{"phone":"+447700900150","name":"Keanu","interests":"hackathons","frequency":"daily"}`))
	done.Header.Set("X-Webhook-Secret", "s3cret")
	srv.Routes().ServeHTTP(rec, done)
	if rec.Code != http.StatusOK {
		t.Fatalf("save_onboarding: %d %s", rec.Code, rec.Body.String())
	}

	var out struct {
		EndCall bool `json:"end_call"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if !out.EndCall {
		t.Errorf("agent should be told to end the call: %s", rec.Body.String())
	}
	if len(tel.hangups) != 1 || tel.hangups[0] != "CAtest123" {
		t.Fatalf("call should be hung up as a backstop: %#v", tel.hangups)
	}
	if u, _ := store.UserByPhone("+447700900150"); u.LastCallSID != "" {
		t.Errorf("sid should be cleared after hangup, got %q", u.LastCallSID)
	}
}

func TestTwilioAPIBaseRegionSelection(t *testing.T) {
	cases := []struct{ edge, region, want string }{
		{"", "", "https://api.twilio.com"},
		{"", "us1", "https://api.twilio.com"},
		{"", "ie1", "https://api.dublin.ie1.twilio.com"},
		{"dublin", "ie1", "https://api.dublin.ie1.twilio.com"},
		{"", "xx9", "https://api.twilio.com"},
	}
	for _, c := range cases {
		if got := twilioAPIBase(c.edge, c.region); got != c.want {
			t.Errorf("twilioAPIBase(%q,%q) = %s, want %s", c.edge, c.region, got, c.want)
		}
	}
}

// The website is a way to reach someone, not an interview: a number, a
// channel, and the rest happens in the conversation.
func TestWebFlowOnlyCollectsContactDetails(t *testing.T) {
	srv, _, _, _ := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "What's your number?") || !strings.Contains(body, `data-channel="sms"`) {
		t.Fatalf("web flow lost the contact step: %s", body)
	}
	for _, gone := range []string{"data-interest=", "data-frequency=", `id="name"`, "interests-other"} {
		if strings.Contains(body, gone) {
			t.Errorf("web flow still interviews the user: found %q", gone)
		}
	}
}
