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

// The interview asks the template questions in order, one per turn, and never
// moves on until the person has actually said something.
func TestInterviewAsksOneQuestionAtATimeInOrder(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	phone := "+447700900201"

	first := smsBody(t, postSMS(t, srv, phone, "hi"))
	if first != "What type of events do you like to go to?" {
		t.Fatalf("first question wrong: %q", first)
	}

	// An empty reply settles nothing: the same question comes back.
	again := smsBody(t, postSMS(t, srv, phone, "   "))
	if !strings.HasSuffix(again, "What type of events do you like to go to?") {
		t.Fatalf("empty reply advanced the interview: %q", again)
	}
	u, _ := store.UserByPhone(phone)
	if _, ok := store.ChecklistAnswer(u.ID, "event_types"); ok {
		t.Error("an empty message was recorded as an answer")
	}

	second := smsBody(t, postSMS(t, srv, phone, "hackathons and meetups"))
	if second != "What time do you like to go to events?" {
		t.Fatalf("second question wrong: %q", second)
	}
	if got, ok := store.ChecklistAnswer(u.ID, "event_types"); !ok || got != "hackathons and meetups" {
		t.Fatalf("answer not persisted: %q ok=%v", got, ok)
	}
	// The first answer also lands on the profile that matching reads.
	if u, _ = store.UserByPhone(phone); !strings.Contains(u.Interests, "hackathon") {
		t.Errorf("interests not derived from the answer: %q", u.Interests)
	}

	third := smsBody(t, postSMS(t, srv, phone, "evenings after 6"))
	if third != "Are you free for an event with like-minded people at 7 PM?" {
		t.Fatalf("third question wrong: %q", third)
	}
	fourth := smsBody(t, postSMS(t, srv, phone, "yes"))
	if fourth != "What should we keep our eyes out for to notify you?" {
		t.Fatalf("fourth question wrong: %q", fourth)
	}

	// The last answer ends the interview by asking permission, not by acting.
	done := smsBody(t, postSMS(t, srv, phone, "AI hack nights in Shoreditch"))
	if !strings.Contains(done, "Would you like me to text you") {
		t.Fatalf("interview should close by asking permission, got %q", done)
	}
}

// Skipped and declined are real states: they settle the item without ever
// becoming a stated preference.
func TestSkipAndDeclineSettleWithoutAnswering(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	phone := "+447700900202"

	postSMS(t, srv, phone, "hi")
	next := smsBody(t, postSMS(t, srv, phone, "skip"))
	if next != "What time do you like to go to events?" {
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
	sess, _ := store.EnsureSession(u.ID, "sms")
	items, _ := store.Checklist(u.ID, sess.ID)
	if items[0].Status != StatusSkipped || items[1].Status != StatusDeclined {
		t.Fatalf("statuses wrong: %s / %s", items[0].Status, items[1].Status)
	}
}

// The voice agent goes through the same state machine, and cannot answer a
// question that is not the one currently on the table.
func TestChecklistToolWebhooksEnforceOrderAndIdempotency(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
	phone := "+447700900203"

	var first map[string]any
	toolPost(t, srv, "/tools/next_question", map[string]any{"phone": phone, "channel": "call"}, &first)
	if first["question"] != "What type of events do you like to go to?" {
		t.Fatalf("agent got the wrong question: %#v", first)
	}

	// Answering question two first is refused.
	code, _ := toolPostCode(t, srv, "/tools/save_answer", map[string]any{
		"phone": phone, "channel": "call", "key": "event_time", "answer": "evenings",
	})
	if code != 409 {
		t.Fatalf("out-of-order answer should conflict, got %d", code)
	}

	body := map[string]any{
		"phone": phone, "channel": "call", "key": "event_types",
		"answer": "conferences", "idempotency_key": "call-1-q1",
	}
	var saved map[string]any
	toolPost(t, srv, "/tools/save_answer", body, &saved)
	next, _ := saved["next"].(map[string]any)
	if next == nil || next["question"] != "What time do you like to go to events?" {
		t.Fatalf("agent not advanced: %#v", saved)
	}

	// A retried delivery replays rather than writing a second answer.
	var replay map[string]any
	toolPost(t, srv, "/tools/save_answer", body, &replay)
	u, _ := store.UserByPhone(phone)
	sess, _ := store.EnsureSession(u.ID, "call")
	items, _ := store.Checklist(u.ID, sess.ID)
	if items[0].Answer != "conferences" || items[1].Status != StatusUnanswered {
		t.Fatalf("retry mutated state: %#v", items)
	}
}
