package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// get fetches a page without following redirects, which is the point of most of
// these tests: where somebody is sent matters as much as what they see.
func get(t *testing.T, srv *Server, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

// onboardedUser is somebody who has already been interviewed: the profile, not
// the browser, is what says so.
func onboardedUser(t *testing.T, srv *Server, store *Store, tel *recordedSMS, phone, name string) *http.Cookie {
	t.Helper()
	cookie := webSignedUp(t, srv, tel, phone, name)
	u, err := store.UserByPhone(phone)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := store.SaveOnboarding(u, name, "hackathons", "daily"); err != nil {
		t.Fatalf("onboard: %v", err)
	}
	return cookie
}

func TestFinishedProfileLandsOnTheDashboard(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	cookie := onboardedUser(t, srv, store, tel, "+447700900300", "Ada")

	rec := get(t, srv, "/", cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("returning user was not sent to the dashboard: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	body := get(t, srv, "/dashboard", cookie).Body.String()
	for _, want := range []string{
		"Ada", `data-checkin="call"`, `data-checkin="sms"`,
		`id="name-form"`, `id="delete-form"`, `id="signout"`, `aria-live="polite"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "+447700900300") {
		t.Error("dashboard shows the whole number")
	}
	if strings.Contains(body, "What's your number?") || strings.Contains(body, "What should I call you?") {
		t.Error("a finished profile was asked to sign up again")
	}
}

// An unfinished profile is not pushed to the dashboard: it still has a step to
// do, and it resumes at that step.
func TestUnfinishedProfileStillFinishesSigningUp(t *testing.T) {
	srv, _, tel, _ := newTestServer(t, nil)

	unnamed := verifiedNumber(t, srv, tel, "+447700900301")
	if body := get(t, srv, "/", unnamed).Body.String(); !strings.Contains(body, "What should I call you?") {
		t.Error("verified but unnamed profile did not resume at the name step")
	}

	named := webSignedUp(t, srv, tel, "+447700900302", "Bo")
	body := get(t, srv, "/", named).Body.String()
	if !strings.Contains(body, `data-channel="call"`) {
		t.Error("named profile did not resume at the channel step")
	}
}

func TestDashboardNeedsASession(t *testing.T) {
	srv, _, _, _ := newTestServer(t, nil)
	for _, path := range []string{"/dashboard", "/api/me", "/api/transcripts"} {
		rec := get(t, srv, path, nil)
		if rec.Code != http.StatusSeeOther && rec.Code != http.StatusUnauthorized {
			t.Errorf("%s served without a session: %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/api/checkin", "/api/name"} {
		if rec := postForm(t, srv, path, url.Values{"channel": {"sms"}, "name": {"Mallory"}}, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s served without a session: %d", path, rec.Code)
		}
	}
}

// Check-ins start because somebody pressed the button, and they go to the
// session's own number.
func TestDashboardChecksInOnTheChannelYouPress(t *testing.T) {
	srv, store, tel, voice := newTestServer(t, nil)
	cookie := onboardedUser(t, srv, store, tel, "+447700900303", "Cleo")
	before := len(tel.sms)

	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("call check-in: %d %s", rec.Code, rec.Body.String())
	}
	if want := []string{"+447700900303"}; !equalStrings(voice.numbers(), want) {
		t.Fatalf("called %#v", voice.numbers())
	}

	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"sms"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("sms check-in: %d %s", rec.Code, rec.Body.String())
	}
	if len(tel.sms) != before+1 || !strings.HasPrefix(tel.sms[len(tel.sms)-1], "+447700900303|") {
		t.Fatalf("check-in text went somewhere else: %#v", tel.sms)
	}

	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"carrier pigeon"}}, cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown channel accepted: %d", rec.Code)
	}
	if rec := get(t, srv, "/api/checkin", cookie); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET started a check-in: %d", rec.Code)
	}
}

// A signed-in browser cannot ring somebody else by naming their number.
func TestASessionCanOnlyRingItsOwnNumber(t *testing.T) {
	srv, store, tel, voice := newTestServer(t, nil)
	cookie := onboardedUser(t, srv, store, tel, "+447700900309", "Ivy")
	if _, err := store.EnsureUser("+447700900310"); err != nil {
		t.Fatalf("other user: %v", err)
	}

	if rec := postForm(t, srv, "/call", url.Values{"phone": {"+447700900310"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("call: %d %s", rec.Code, rec.Body.String())
	}
	if want := []string{"+447700900309"}; !equalStrings(voice.numbers(), want) {
		t.Fatalf("posted number won over the session: %#v", voice.numbers())
	}
}

func TestSettingsChangesYourOwnNameOnly(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	mine := onboardedUser(t, srv, store, tel, "+447700900304", "Dev")
	theirs := onboardedUser(t, srv, store, tel, "+447700900305", "Eve")

	if rec := postForm(t, srv, "/api/name", url.Values{"name": {"Devika"}}, mine); rec.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	if u, _ := store.UserByPhone("+447700900304"); u.Name != "Devika" {
		t.Fatalf("name not persisted: %q", u.Name)
	}
	if u, _ := store.UserByPhone("+447700900305"); u.Name != "Eve" {
		t.Fatalf("renaming one profile changed another: %q", u.Name)
	}

	for _, bad := range []string{"", "   ", "?"} {
		if rec := postForm(t, srv, "/api/name", url.Values{"name": {bad}}, theirs); rec.Code != http.StatusBadRequest {
			t.Errorf("name %q: want 400, got %d", bad, rec.Code)
		}
	}
	if u, _ := store.UserByPhone("+447700900305"); u.Name != "Eve" {
		t.Fatalf("a rejected name still landed: %q", u.Name)
	}
}

func TestDeletingYourDataTakesEverythingWithIt(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	mine := onboardedUser(t, srv, store, tel, "+447700900306", "Fen")
	theirs := onboardedUser(t, srv, store, tel, "+447700900307", "Gus")

	// Nothing goes without the typed confirmation.
	if rec := postForm(t, srv, "/api/forget", url.Values{}, mine); rec.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := store.UserByPhone("+447700900306"); err != nil {
		t.Fatal("unconfirmed delete erased the profile anyway")
	}

	if rec := postForm(t, srv, "/api/forget", url.Values{"confirm": {"DELETE"}}, mine); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := store.UserByPhone("+447700900306"); err == nil {
		t.Fatal("profile survived deletion")
	}
	// The session went with it, and pressing again is safe.
	if rec := get(t, srv, "/dashboard", mine); rec.Code != http.StatusSeeOther {
		t.Fatalf("dead session still opened the dashboard: %d", rec.Code)
	}
	if rec := postForm(t, srv, "/api/forget", url.Values{"confirm": {"DELETE"}}, mine); rec.Code != http.StatusOK {
		t.Fatalf("second delete: %d %s", rec.Code, rec.Body.String())
	}

	// The other account is untouched and still signed in.
	if _, err := store.UserByPhone("+447700900307"); err != nil {
		t.Fatal("deleting one account erased another")
	}
	if rec := get(t, srv, "/dashboard", theirs); rec.Code != http.StatusOK {
		t.Fatalf("the other session was broken by the deletion: %d", rec.Code)
	}
}

func TestSigningOutEndsTheSession(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	cookie := onboardedUser(t, srv, store, tel, "+447700900308", "Hal")

	rec := postForm(t, srv, "/auth/logout", url.Values{}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := get(t, srv, "/dashboard", cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("revoked session still opened the dashboard: %d", rec.Code)
	}
	if rec := postForm(t, srv, "/api/checkin", url.Values{"channel": {"call"}}, cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session still started a check-in: %d", rec.Code)
	}
	// And the page you land on says you are signed out rather than who you were.
	if body := get(t, srv, "/login", cookie).Body.String(); strings.Contains(body, "Hal") {
		t.Error("the signed-out page still names the person")
	}
}
