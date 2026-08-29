package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// callState reads the server's own answer to "is a call running?" the same way
// the dashboard does, over HTTP with a session rather than out of the store.
func callStateOf(t *testing.T, srv *Server, cookie *http.Cookie) (bool, *httptest.ResponseRecorder) {
	t.Helper()
	rec := get(t, srv, "/api/call-state", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("call state: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Call struct {
			InProgress bool `json:"in_progress"`
		} `json:"call"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("call state json: %v", err)
	}
	return out.Call.InProgress, rec
}

// Pressing the button twice in a row must ring the phone once. The second press
// is answered with the live-call state rather than an error, because from the
// caller's point of view the thing they asked for is happening.
func TestASecondCallRequestDoesNotRingTwice(t *testing.T) {
	srv, store, tel, voice := newTestServer(t, nil)
	cookie := onboardedUser(t, srv, store, tel, "+447700900401", "Ada")

	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("first call: %d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 4; i++ {
		rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, cookie)
		if rec.Code != http.StatusConflict {
			t.Fatalf("duplicate press %d was accepted: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if got := voice.numbers(); len(got) != 1 {
		t.Fatalf("rapid presses placed %d calls: %#v", len(got), got)
	}
	if live, _ := callStateOf(t, srv, cookie); !live {
		t.Fatal("server does not report the call it just placed as in progress")
	}
}

// A refresh must not free the button: the state lives on the server, so a fresh
// page load with the same session sees the call that is still running.
func TestCallStateSurvivesARefresh(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	cookie := onboardedUser(t, srv, store, tel, "+447700900402", "Bo")

	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("call: %d", rec.Code)
	}
	// A brand-new request, as a reloaded tab or a reconnected one would make.
	if live, _ := callStateOf(t, srv, cookie); !live {
		t.Fatal("a reloaded dashboard would have re-enabled the button mid-call")
	}
	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, cookie); rec.Code != http.StatusConflict {
		t.Fatalf("the reloaded tab was allowed to place a second call: %d", rec.Code)
	}
}

// A call that lasts longer than the 47 seconds we were losing calls at is still
// running: nothing in the service ends a call on a timer of its own.
func TestALongCallIsStillRunningAfterFortySevenSeconds(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	cookie := onboardedUser(t, srv, store, tel, "+447700900403", "Cleo")
	u, err := store.UserByPhone("+447700900403")
	if err != nil {
		t.Fatalf("user: %v", err)
	}

	for _, age := range []time.Duration{47 * time.Second, 3 * time.Minute, callMaxDuration - time.Minute} {
		if err := store.EndCall(u.ID); err != nil {
			t.Fatalf("reset: %v", err)
		}
		started, err := store.StartCall(u.ID, time.Now().Add(-age))
		if err != nil || !started {
			t.Fatalf("start call: %v %v", started, err)
		}
		if live, _ := callStateOf(t, srv, cookie); !live {
			t.Fatalf("a call %s old was treated as over", age)
		}
		if len(tel.hangups) != 0 {
			t.Fatalf("something hung the call up on its own: %v", tel.hangups)
		}
	}
}

// The other half of that: a call nothing ever reported the end of cannot hold
// the button down forever.
func TestAnAbandonedCallStopsBlockingTheButton(t *testing.T) {
	srv, store, tel, voice := newTestServer(t, nil)
	cookie := onboardedUser(t, srv, store, tel, "+447700900404", "Dae")
	u, _ := store.UserByPhone("+447700900404")

	if _, err := store.StartCall(u.ID, time.Now().Add(-callMaxDuration-time.Minute)); err != nil {
		t.Fatalf("start: %v", err)
	}
	if live, _ := callStateOf(t, srv, cookie); live {
		t.Fatal("a call older than the maximum is still reported as live")
	}
	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("a new call after an abandoned one: %d %s", rec.Code, rec.Body.String())
	}
	if len(voice.numbers()) != 1 {
		t.Fatalf("the new call was not placed: %#v", voice.numbers())
	}
}

// A provider that refuses the call frees the button immediately: there is no
// call to wait for, and making somebody wait out the stale-call timeout for a
// failure would look like the service ignoring them.
func TestAFailedCallReleasesTheButton(t *testing.T) {
	srv, store, tel, voice := newTestServer(t, nil)
	cookie := onboardedUser(t, srv, store, tel, "+447700900405", "Eun")
	voice.err = errors.New("provider is down")

	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, cookie); rec.Code != http.StatusBadGateway {
		t.Fatalf("failed call: %d %s", rec.Code, rec.Body.String())
	}
	if live, _ := callStateOf(t, srv, cookie); live {
		t.Fatal("a call that was never placed is being reported as in progress")
	}

	voice.err = nil
	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("retry after a failure: %d %s", rec.Code, rec.Body.String())
	}
}

// The transcript is the provider saying the call is over, and that is what
// definitively re-enables the button - including for a call that dropped.
func TestATranscriptEndsTheCall(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	srv.cfg.ElevenLabsSecret = "whsec_test"
	const phone = "+447700900406"
	cookie := onboardedUser(t, srv, store, tel, phone, "Fen")

	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("call: %d", rec.Code)
	}
	if live, _ := callStateOf(t, srv, cookie); !live {
		t.Fatal("call not marked live")
	}

	if rec := postTranscript(t, srv, "whsec_test", transcriptPayload("conv_end", phone), time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("transcript: %d %s", rec.Code, rec.Body.String())
	}
	if live, _ := callStateOf(t, srv, cookie); live {
		t.Fatal("the button is still locked after the call ended")
	}

	// A retried delivery of the same transcript must not disturb anything, and
	// the next call must be allowed.
	if rec := postTranscript(t, srv, "whsec_test", transcriptPayload("conv_end", phone), time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("retried transcript: %d", rec.Code)
	}
	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("call after the previous one ended: %d %s", rec.Code, rec.Body.String())
	}
}

// One person's call must not disable anybody else's button, and a call state is
// only ever read from the session's own profile.
func TestTwoPeopleCanBeOnCallsAtOnce(t *testing.T) {
	srv, store, tel, voice := newTestServer(t, nil)
	ada := onboardedUser(t, srv, store, tel, "+447700900407", "Ada")
	bo := onboardedUser(t, srv, store, tel, "+447700900408", "Bo")

	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, ada); rec.Code != http.StatusOK {
		t.Fatalf("ada: %d", rec.Code)
	}
	if live, _ := callStateOf(t, srv, bo); live {
		t.Fatal("one person's call blocked another person's button")
	}
	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, bo); rec.Code != http.StatusOK {
		t.Fatalf("bo was refused while somebody else was on a call: %d", rec.Code)
	}
	if want := []string{"+447700900407", "+447700900408"}; !equalStrings(voice.numbers(), want) {
		t.Fatalf("wrong numbers called: %#v", voice.numbers())
	}
	// Ada's call ending must not free Bo's button.
	u, _ := store.UserByPhone("+447700900407")
	if err := store.EndCall(u.ID); err != nil {
		t.Fatalf("end: %v", err)
	}
	if live, _ := callStateOf(t, srv, bo); !live {
		t.Fatal("ending one call ended somebody else's too")
	}
}

// A text check-in is not a call and must not be gated by one: somebody on a
// call that is going badly can still ask for a text.
func TestATextIsNotBlockedByACall(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	cookie := onboardedUser(t, srv, store, tel, "+447700900409", "Gwen")

	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("call: %d", rec.Code)
	}
	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"sms"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("text during a call: %d %s", rec.Code, rec.Body.String())
	}
}

// The call-state endpoint is a signed-in read of your own profile only.
func TestCallStateNeedsASession(t *testing.T) {
	srv, _, _, _ := newTestServer(t, nil)
	if rec := get(t, srv, "/api/call-state", nil); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusSeeOther {
		t.Fatalf("call state served without a session: %d", rec.Code)
	}
}

// The dashboard has to be able to show all of this: a busy state screen readers
// announce, and a server state to read it from.
func TestDashboardShowsAnAccessibleCallState(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	cookie := onboardedUser(t, srv, store, tel, "+447700900412", "Ines")

	body := get(t, srv, "/dashboard", cookie).Body.String()
	for _, want := range []string{
		`id="call-btn"`, // the button the gating is applied to
		`aria-busy`,     // announced while a call is running
		`aria-describedby="checkin-status"`,
		`role="status"`,    // the live region the message lands in
		"/api/call-state",  // state comes from the server, not a timer
		"visibilitychange", // a tab that was hidden re-syncs
		"online",           // so does one that was offline
		"holdUntil",        // a poll repaint does not wipe a just-asked-for answer
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard is missing call-state handling: %q", want)
		}
	}
}

// The greeting and the context handed to the agent are built from the same
// profile, so they cannot contradict each other - no greeting somebody by name
// while telling the agent nothing is known about them.
func TestTheGreetingAndTheContextAgree(t *testing.T) {
	srv, store, tel, voice := newTestServer(t, nil)
	const phone = "+447700900413"
	cookie := onboardedUser(t, srv, store, tel, phone, "Jae")

	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("call: %d", rec.Code)
	}
	if len(voice.calls) != 1 {
		t.Fatalf("expected one call, got %d", len(voice.calls))
	}
	vars := voice.calls[0].Vars
	greeting := vars["greeting"]
	if vars["user_name"] != "Jae" || !strings.Contains(greeting, "Jae") {
		t.Fatalf("a known profile was not used for the opening line: %#v", vars)
	}
	if vars["caller_known"] != "yes" || vars["phone_verified"] != "yes" {
		t.Fatalf("a stored, verified profile was described as unknown: %#v", vars)
	}
	if strings.Contains(greeting, "unknown") {
		t.Fatalf("greeting leaked a placeholder: %q", greeting)
	}

	// And the tool the agent calls mid-conversation says the same thing.
	var ctx struct {
		Caller CallerContext `json:"caller"`
	}
	toolPost(t, srv, "/tools/get_context", map[string]any{"phone": phone}, &ctx)
	if ctx.Caller.Greeting != greeting {
		t.Fatalf("get_context disagrees with the opening line: %q vs %q", ctx.Caller.Greeting, greeting)
	}
	if !ctx.Caller.Known["name"].Known || ctx.Caller.Known["name"].Value != "Jae" {
		t.Fatalf("get_context claims the name is unknown: %#v", ctx.Caller.Known)
	}

	// An unknown number gets neither a name nor somebody else's context.
	var stranger struct {
		Caller CallerContext `json:"caller"`
	}
	toolPost(t, srv, "/tools/get_context", map[string]any{"phone": "+447700900414"}, &stranger)
	if strings.Contains(stranger.Caller.Greeting, "Jae") || stranger.Caller.Known["name"].Known {
		t.Fatalf("an unknown caller was handed a known profile: %#v", stranger.Caller)
	}
}

// Re-confirming a profile that is already complete is not the end of an
// interview: hanging up there is what cut routine check-ins short.
func TestSavingAKnownProfileDoesNotHangUpTheCheckin(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	const phone = "+447700900410"
	u, err := store.EnsureUser(phone)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := store.MarkPhoneVerified(u.ID, "sms"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := store.SaveOnboarding(u, "Hana", "hackathons", "daily"); err != nil {
		t.Fatalf("onboard: %v", err)
	}
	if err := store.SetCallSID(u.ID, "CAlive"); err != nil {
		t.Fatalf("sid: %v", err)
	}

	var out struct {
		Complete bool   `json:"onboarding_complete"`
		EndCall  bool   `json:"end_call"`
		Says     string `json:"instruction_to_agent"`
	}
	toolPost(t, srv, "/tools/save_onboarding",
		map[string]any{"phone": phone, "name": "Hana", "interests": "hackathons", "frequency": "daily"}, &out)
	if !out.Complete {
		t.Fatalf("a complete profile should report complete: %+v", out)
	}
	if out.EndCall {
		t.Fatalf("a check-in was told to hang up because the profile was already full: %+v", out)
	}
	if len(tel.hangups) != 0 {
		t.Fatalf("the check-in call was hung up: %v", tel.hangups)
	}

	// The first-time path still ends the call, so the introduction does not run on.
	newPhone := "+447700900411"
	nu, _ := store.EnsureUser(newPhone)
	if err := store.MarkPhoneVerified(nu.ID, "sms"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := store.SetCallSID(nu.ID, "CAintro"); err != nil {
		t.Fatalf("sid: %v", err)
	}
	toolPost(t, srv, "/tools/save_onboarding",
		map[string]any{"phone": newPhone, "name": "Ivo", "interests": "hackathons", "frequency": "daily"}, &out)
	if !out.Complete || !out.EndCall {
		t.Fatalf("a finished introduction should end the call: %+v", out)
	}
	if len(tel.hangups) != 1 || tel.hangups[0] != "CAintro" {
		t.Fatalf("the introduction call was not hung up: %v", tel.hangups)
	}
}
