package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// twiml pulls the reply text out of the TwiML the SMS webhook returns.
func twiml(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := rec.Body.String()
	start := strings.Index(body, "<Message>")
	end := strings.Index(body, "</Message>")
	if start < 0 || end < 0 {
		t.Fatalf("no message in reply: %s", body)
	}
	return body[start+len("<Message>") : end]
}

func namePrompt() string { return questionByKey("name").Prompt }

// A number nobody has introduced themselves from is asked for a name, in those
// words, before anything else.
func TestSMSOnboardingAsksForNameWhenMissing(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)

	reply := twiml(t, postSMS(t, srv, "+447700900301", "hello"))
	if !strings.Contains(reply, "what should I call you") {
		t.Fatalf("first onboarding turn should ask for a name, got %q", reply)
	}

	u, err := store.UserByPhone("+447700900301")
	if err != nil {
		t.Fatalf("user should exist: %v", err)
	}
	if u.DisplayName() != "" {
		t.Fatalf("no name should be assumed before it is given, got %q", u.DisplayName())
	}
	if u.Onboarded() {
		t.Fatalf("a nameless profile must not count as onboarded")
	}
}

// The answer lands on the row the number owns, and the question is not asked
// again - neither in the rest of the conversation nor after a resume.
func TestSMSOnboardingPersistsNameAndNeverReasks(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
	const phone = "+447700900302"

	postSMS(t, srv, phone, "hi")
	reply := twiml(t, postSMS(t, srv, phone, "my name is Sam"))

	u, err := store.UserByPhone(phone)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if u.Name != "Sam" {
		t.Fatalf("name should be persisted as Sam, got %q", u.Name)
	}
	if strings.Contains(reply, "what should I call you") {
		t.Fatalf("the name was just given and must not be asked again: %q", reply)
	}
	if !strings.Contains(strings.ToLower(reply), "events") {
		t.Fatalf("the interview should move on to interests, got %q", reply)
	}

	// A resumed session - a new session row for the same user - carries the
	// stored name forward rather than starting the interview over.
	sess, err := store.EnsureSession(u, "sms", FlowOnboarding)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	next, err := store.NextChecklistItem(u.ID, sess.ID)
	if err != nil {
		t.Fatalf("next item: %v", err)
	}
	if next != nil && next.Key == "name" {
		t.Fatalf("a stored name must not be asked for again on resume")
	}
}

// An answer that is a sentence rather than a name is not silently accepted as
// one: nothing is stored and the question stands.
func TestOnboardingRejectsNonAnswerAsName(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
	const phone = "+447700900303"

	postSMS(t, srv, phone, "hi")
	reply := twiml(t, postSMS(t, srv, phone, "why do you need to know that, this is quite a long non-answer"))

	u, _ := store.UserByPhone(phone)
	if u.DisplayName() != "" {
		t.Fatalf("a non-answer must not become somebody's name, got %q", u.DisplayName())
	}
	if !strings.Contains(reply, "what should I call you") {
		t.Fatalf("the name question should stand, got %q", reply)
	}
}

// Two people onboarding at once keep their own names, and neither inherits the
// other's answers or progress.
func TestOnboardingNamesDoNotLeakBetweenUsers(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
	const a, b = "+447700900304", "+447700900305"

	postSMS(t, srv, a, "hi")
	postSMS(t, srv, b, "hello")
	postSMS(t, srv, a, "Ada")
	// B has still not given a name, so B is still being asked for one.
	reply := twiml(t, postSMS(t, srv, b, ""))
	if !strings.Contains(reply, "what should I call you") {
		t.Fatalf("the second user should still be asked their name, got %q", reply)
	}
	postSMS(t, srv, b, "Bo")

	ua, _ := store.UserByPhone(a)
	ub, _ := store.UserByPhone(b)
	if ua.Name != "Ada" || ub.Name != "Bo" {
		t.Fatalf("names crossed over: %q / %q", ua.Name, ub.Name)
	}
	if ua.ID == ub.ID {
		t.Fatalf("two numbers must not share a profile")
	}
}

// The voice agent saving an interview with no name has not finished it. The
// call is not ended and the profile is not stamped, or the person would sit in
// check-ins forever with nothing to be called.
func TestSaveOnboardingWithoutNameStaysUnfinished(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	const phone = "+447700900306"

	var out struct {
		Complete bool     `json:"onboarding_complete"`
		EndCall  bool     `json:"end_call"`
		Missing  []string `json:"missing"`
		AskNext  string   `json:"ask_next"`
		Question string   `json:"question"`
	}
	toolPost(t, srv, "/tools/save_onboarding",
		map[string]any{"phone": phone, "interests": "hackathons", "frequency": "daily"}, &out)
	if out.Complete || out.EndCall {
		t.Fatalf("a nameless interview is not complete: %+v", out)
	}
	if out.AskNext != "name" || out.Question != namePrompt() {
		t.Fatalf("the agent should be sent back to ask for the name: %+v", out)
	}
	if len(tel.hangups) != 0 {
		t.Fatalf("the call must not be hung up mid-interview: %v", tel.hangups)
	}

	u, _ := store.UserByPhone(phone)
	if u.Onboarded() {
		t.Fatalf("profile must not be stamped as onboarded without a name")
	}
	if u.Interests != "hackathons" {
		t.Fatalf("the part that was answered should still be kept, got %q", u.Interests)
	}

	// Saving again with the name finishes it.
	toolPost(t, srv, "/tools/save_onboarding", map[string]any{"phone": phone, "name": "Sam"}, &out)
	if !out.Complete || !out.EndCall {
		t.Fatalf("with a name and interests it is complete: %+v", out)
	}
	u, _ = store.UserByPhone(phone)
	if u.Name != "Sam" || !u.Onboarded() {
		t.Fatalf("name should be stored and the profile stamped: %#v", u)
	}
}

// The caller context for a nameless profile lists the name as missing and does
// not greet anybody by name.
func TestCallerContextAsksForMissingName(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
	const phone = "+447700900307"
	u, _ := store.EnsureUser(phone)
	if err := store.MarkPhoneVerified(u.ID, "sms"); err != nil {
		t.Fatalf("verify: %v", err)
	}

	var out struct {
		Caller CallerContext `json:"caller"`
	}
	toolPost(t, srv, "/tools/get_context", map[string]any{"phone": phone}, &out)
	var missingName bool
	for _, m := range out.Caller.Missing {
		if m == "name" {
			missingName = true
		}
	}
	if !missingName {
		t.Fatalf("name should be listed as missing: %+v", out.Caller)
	}
	if out.Caller.Greeting != "Hi, this is CheckIn." {
		t.Fatalf("no name to greet with, got %q", out.Caller.Greeting)
	}
}
