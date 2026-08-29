package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var errModelUnavailable = errors.New("model unavailable in this test")

// smsBody pulls the message text out of a TwiML response.
func smsBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("sms status %d: %s", rec.Code, rec.Body.String())
	}
	var out twiMLMessage
	if err := xml.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad twiml %q: %v", rec.Body.String(), err)
	}
	return out.Message
}

func toolPostCode(t *testing.T, srv *Server, path string, body map[string]any) (int, []byte) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("X-Webhook-Secret", "s3cret")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func toolPost(t *testing.T, srv *Server, path string, body map[string]any, out any) {
	t.Helper()
	code, raw := toolPostCode(t, srv, path, body)
	if code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", path, code, raw)
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: bad json %s: %v", path, raw, err)
	}
}

// The introduction asks the onboarding questions in order, one per turn, and
// never moves on until the person has actually said something.
func TestOnboardingAsksOneQuestionAtATimeInOrder(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	phone := "+447700900201"

	first := smsBody(t, postSMS(t, srv, phone, "hi"))
	if first != onboardingTemplate[0].Prompt {
		t.Fatalf("first question wrong: %q", first)
	}

	// An empty reply settles nothing: the same question comes back.
	again := smsBody(t, postSMS(t, srv, phone, "   "))
	if !strings.HasSuffix(again, onboardingTemplate[0].Prompt) {
		t.Fatalf("empty reply advanced the introduction: %q", again)
	}
	u, _ := store.UserByPhone(phone)
	if _, ok := store.ChecklistAnswer(u.ID, "name"); ok {
		t.Error("an empty message was recorded as an answer")
	}

	second := smsBody(t, postSMS(t, srv, phone, "Keanu"))
	if second != onboardingTemplate[1].Prompt {
		t.Fatalf("second question wrong: %q", second)
	}
	if u, _ = store.UserByPhone(phone); u.Name != "Keanu" {
		t.Fatalf("name not persisted to the profile: %q", u.Name)
	}

	third := smsBody(t, postSMS(t, srv, phone, "hackathons and meetups"))
	if third != onboardingTemplate[2].Prompt {
		t.Fatalf("third question wrong: %q", third)
	}
	if got, ok := store.ChecklistAnswer(u.ID, "event_types"); !ok || got != "hackathons and meetups" {
		t.Fatalf("answer not persisted: %q ok=%v", got, ok)
	}
	// That answer also lands on the profile that matching reads.
	if u, _ = store.UserByPhone(phone); !strings.Contains(u.Interests, "hackathon") {
		t.Errorf("interests not derived from the answer: %q", u.Interests)
	}

	// Fourth is the event offer, whose wording is written from what they just
	// said rather than being fixed in the template.
	offer := smsBody(t, postSMS(t, srv, phone, "weekday evenings"))
	if offer == "" || offer == onboardingTemplate[4].Prompt {
		t.Fatalf("expected an event offer, got %q", offer)
	}

	// Check-ins are a separate ask, and the introduction ends there rather
	// than rolling into one.
	consent := smsBody(t, postSMS(t, srv, phone, "no thanks"))
	if !strings.Contains(consent, onboardingTemplate[4].Prompt) {
		t.Fatalf("consent question not asked separately: %q", consent)
	}
	done := smsBody(t, postSMS(t, srv, phone, "daily"))
	if done != onboardingDoneLine {
		t.Fatalf("introduction should close, got %q", done)
	}
	u, _ = store.UserByPhone(phone)
	if !u.Onboarded() {
		t.Fatal("finished introduction did not mark the profile onboarded")
	}
	if u.Frequency != "daily" {
		t.Errorf("consent answer did not set the cadence: %q", u.Frequency)
	}
}

// A check-in never repeats the introduction, and the introduction never asks a
// check-in question.
func TestOnboardingAndCheckinAreSeparateFlows(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	phone := "+447700900204"

	for _, reply := range []string{"hi", "Ada", "hackathons", "weekends", "no thanks", "no thanks"} {
		body := smsBody(t, postSMS(t, srv, phone, reply))
		for _, q := range checkinTemplate {
			if strings.Contains(body, q.Prompt) {
				t.Fatalf("check-in question %q asked during the introduction", q.Prompt)
			}
		}
	}

	u, _ := store.UserByPhone(phone)
	if !u.Onboarded() {
		t.Fatal("introduction did not finish")
	}
	// The next message is a check-in, and it starts with the check-in
	// checklist rather than asking who they are again.
	next := smsBody(t, postSMS(t, srv, phone, "hello again"))
	if next != checkinTemplate[0].Prompt {
		t.Fatalf("check-in did not start its own flow: %q", next)
	}
	for _, q := range onboardingTemplate {
		if q.Prompt != "" && strings.Contains(next, q.Prompt) {
			t.Fatalf("introduction question repeated after onboarding: %q", next)
		}
	}
}

// Skipped and declined are real states: they settle the item without ever
// becoming a stated preference.
func TestSkipAndDeclineSettleWithoutAnswering(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	phone := "+447700900202"

	postSMS(t, srv, phone, "hi") // asks for the name
	postSMS(t, srv, phone, "Jo") // names them, asks what they are into
	next := smsBody(t, postSMS(t, srv, phone, "skip"))
	if next != onboardingTemplate[2].Prompt {
		t.Fatalf("skip did not advance: %q", next)
	}
	u, _ := store.UserByPhone(phone)
	if got, ok := store.ChecklistAnswer(u.ID, "event_types"); ok {
		t.Errorf("a skipped item read back as a preference: %q", got)
	}
	if u.Interests != "" {
		t.Errorf("a skipped item wrote interests: %q", u.Interests)
	}

	postSMS(t, srv, phone, "prefer not to say")
	sess, _ := store.EnsureSession(u, "sms", FlowOnboarding)
	items, _ := store.Checklist(u.ID, sess.ID)
	if items[1].Status != StatusSkipped || items[2].Status != StatusDeclined {
		t.Fatalf("statuses wrong: %s / %s", items[1].Status, items[2].Status)
	}
}

// The voice agent goes through the same state machine, and cannot answer a
// question that is not the one currently on the table.
func TestChecklistToolWebhooksEnforceOrderAndIdempotency(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
	phone := "+447700900203"

	var first map[string]any
	toolPost(t, srv, "/tools/next_question", map[string]any{"phone": phone, "channel": "call"}, &first)
	if first["question"] != onboardingTemplate[0].Prompt || first["flow"] != FlowOnboarding {
		t.Fatalf("agent got the wrong question: %#v", first)
	}

	// Answering question two first is refused.
	code, _ := toolPostCode(t, srv, "/tools/save_answer", map[string]any{
		"phone": phone, "channel": "call", "key": "event_types", "answer": "conferences",
	})
	if code != 409 {
		t.Fatalf("out-of-order answer should conflict, got %d", code)
	}

	body := map[string]any{
		"phone": phone, "channel": "call", "key": "name",
		"answer": "Sam", "idempotency_key": "call-1-q1",
	}
	var saved map[string]any
	toolPost(t, srv, "/tools/save_answer", body, &saved)
	next, _ := saved["next"].(map[string]any)
	if next == nil || next["question"] != onboardingTemplate[1].Prompt {
		t.Fatalf("agent not advanced: %#v", saved)
	}

	// A retried delivery replays rather than writing a second answer.
	var replay map[string]any
	toolPost(t, srv, "/tools/save_answer", body, &replay)
	u, _ := store.UserByPhone(phone)
	sess, _ := store.EnsureSession(u, "call", FlowOnboarding)
	items, _ := store.Checklist(u.ID, sess.ID)
	if items[0].Answer != "Sam" || items[1].Status != StatusUnanswered {
		t.Fatalf("retry mutated state: %#v", items)
	}
}
