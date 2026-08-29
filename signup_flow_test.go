package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// startSignup runs step one: the number is posted and a code is texted.
func startSignup(t *testing.T, srv *Server, phone string) *httptest.ResponseRecorder {
	t.Helper()
	return postForm(t, srv, "/signup/start", url.Values{"phone": {phone}}, nil)
}

// verifiedNumber runs steps one and two and hands back the session the code
// bought. It stops there: the name is deliberately not set, so callers can
// assert what a verified-but-unnamed profile is allowed to do.
func verifiedNumber(t *testing.T, srv *Server, tel *recordedSMS, phone string) *http.Cookie {
	t.Helper()
	if rec := startSignup(t, srv, phone); rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	code := lastSMSCode(t, tel)
	rec := postForm(t, srv, "/auth/verify", url.Values{"phone": {phone}, "code": {code}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body.String())
	}
	return sessionCookie(t, rec)
}

// signedUp runs the whole sequence - number, code, name - and returns the
// session, which is what most tests want as a starting point.
func webSignedUp(t *testing.T, srv *Server, tel *recordedSMS, phone, name string) *http.Cookie {
	t.Helper()
	cookie := verifiedNumber(t, srv, tel, phone)
	rec := postForm(t, srv, "/api/name", url.Values{"name": {name}}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("name: %d %s", rec.Code, rec.Body.String())
	}
	return cookie
}

func page(t *testing.T, srv *Server, path string, cookie *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d", path, rec.Code)
	}
	return rec.Body.String()
}

func hasChannelChoice(body string) bool {
	return strings.Contains(body, `data-channel="call"`) || strings.Contains(body, `data-channel="sms"`) ||
		strings.Contains(body, "Call me</strong>") || strings.Contains(body, "Text me</strong>")
}

// The order is the product: number, then the code that proves it, then the
// name, and only then a choice of how to be contacted. Each step is asserted
// both by what the page offers and by what the server will accept.
func TestSignupFollowsPhoneCodeNameChannelInOrder(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	phone := "+447700900301"

	first := page(t, srv, "/", nil)
	if !strings.Contains(first, "What's your number?") {
		t.Fatalf("sign-up did not start with the number: %s", first)
	}
	if hasChannelChoice(first) {
		t.Fatal("the channel choice is on the page before the number is even verified")
	}
	if strings.Contains(first, `id="name"`) {
		t.Fatal("the name is asked for before the number is verified")
	}

	if rec := startSignup(t, srv, phone); rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	if len(tel.sms) != 1 {
		t.Fatalf("one code should have been texted, got %#v", tel.sms)
	}

	// Verified, unnamed: the page asks the name and nothing else.
	code := lastSMSCode(t, tel)
	rec := postForm(t, srv, "/auth/verify", url.Values{"phone": {phone}, "code": {code}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body.String())
	}
	var verified struct {
		SignedIn bool   `json:"signed_in"`
		Step     string `json:"step"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &verified); err != nil {
		t.Fatalf("verify json: %v", err)
	}
	if !verified.SignedIn || verified.Step != "name" {
		t.Fatalf("verification should lead to the name step, got %s", rec.Body.String())
	}
	cookie := sessionCookie(t, rec)

	named := page(t, srv, "/", cookie)
	if !strings.Contains(named, "What should I call you?") {
		t.Fatalf("the name was not asked for: %s", named)
	}
	if hasChannelChoice(named) {
		t.Fatal("the channel choice is on the page before a name is stored")
	}

	if rec := postForm(t, srv, "/api/name", url.Values{"name": {"Rae"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("name: %d %s", rec.Code, rec.Body.String())
	}
	u, err := store.UserByPhone(phone)
	if err != nil || u.Name != "Rae" {
		t.Fatalf("name not persisted to the verified profile: %#v (%v)", u, err)
	}

	chosen := page(t, srv, "/", cookie)
	if !hasChannelChoice(chosen) {
		t.Fatalf("the channel choice never appeared: %s", chosen)
	}
	if rec := postForm(t, srv, "/signup", url.Values{"channel": {"sms"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("channel: %d %s", rec.Code, rec.Body.String())
	}
	u, _ = store.UserByPhone(phone)
	if u.Channel != "sms" {
		t.Fatalf("channel not stored: %q", u.Channel)
	}
}

// The buttons being absent from the page is not enough on its own: the request
// behind them has to be refused too.
func TestChannelIsRefusedBeforeTheNameIsStored(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	phone := "+447700900302"

	// Nobody signed in at all.
	if rec := postForm(t, srv, "/signup", url.Values{"channel": {"call"}, "phone": {phone}}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without a session, got %d: %s", rec.Code, rec.Body.String())
	}

	cookie := verifiedNumber(t, srv, tel, phone)
	rec := postForm(t, srv, "/signup", url.Values{"channel": {"call"}}, cookie)
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 before a name, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"step":"name"`) {
		t.Fatalf("the refusal should say what is missing: %s", rec.Body.String())
	}
	u, _ := store.UserByPhone(phone)
	if u.Channel == "call" {
		t.Fatal("a channel was stored before the name")
	}
	if len(tel.sms) != 1 {
		t.Fatalf("only the code should have been sent, got %#v", tel.sms)
	}
}

// A blank or unusable answer leaves the question standing rather than writing
// an empty name and moving on.
func TestNameStepRefusesAnEmptyAnswer(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	phone := "+447700900303"
	cookie := verifiedNumber(t, srv, tel, phone)

	for _, answer := range []string{"", "   ", "?"} {
		rec := postForm(t, srv, "/api/name", url.Values{"name": {answer}}, cookie)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("answer %q: want 400, got %d %s", answer, rec.Code, rec.Body.String())
		}
	}
	u, _ := store.UserByPhone(phone)
	if u.DisplayName() != "" {
		t.Fatalf("a non-answer was stored as a name: %q", u.Name)
	}
	if hasChannelChoice(page(t, srv, "/", cookie)) {
		t.Fatal("the channel choice appeared after a refused name")
	}
}

// An invalid code, a spent one, an expired one and a throttled one all have to
// leave the browser exactly where it was: unverified, unnamed, no channel.
func TestBadCodesNeverReachTheNameOrChannelStep(t *testing.T) {
	t.Run("invalid", func(t *testing.T) {
		srv, _, _, _ := newTestServer(t, nil)
		phone := "+447700900304"
		startSignup(t, srv, phone)
		rec := postForm(t, srv, "/auth/verify", url.Values{"phone": {phone}, "code": {"000000"}}, nil)
		if rec.Code == http.StatusOK {
			t.Fatalf("a wrong code signed someone in: %s", rec.Body.String())
		}
		if !strings.Contains(page(t, srv, "/", nil), "What's your number?") {
			t.Fatal("the page moved on after a wrong code")
		}
	})

	t.Run("reused", func(t *testing.T) {
		srv, _, tel, _ := newTestServer(t, nil)
		phone := "+447700900305"
		startSignup(t, srv, phone)
		code := lastSMSCode(t, tel)
		if rec := postForm(t, srv, "/auth/verify", url.Values{"phone": {phone}, "code": {code}}, nil); rec.Code != http.StatusOK {
			t.Fatalf("first use: %d", rec.Code)
		}
		rec := postForm(t, srv, "/auth/verify", url.Values{"phone": {phone}, "code": {code}}, nil)
		if rec.Code == http.StatusOK {
			t.Fatalf("a spent code was accepted again: %s", rec.Body.String())
		}
	})

	t.Run("expired", func(t *testing.T) {
		srv, store, _, _ := newTestServer(t, nil)
		phone := "+447700900306"
		u, _ := store.EnsureUser(phone)
		code, err := store.IssueLoginCode(phone, time.Now().Add(-loginCodeTTL-time.Minute))
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		rec := postForm(t, srv, "/auth/verify", url.Values{"phone": {phone}, "code": {code}}, nil)
		if rec.Code == http.StatusOK {
			t.Fatalf("an expired code was accepted: %s", rec.Body.String())
		}
		if fresh, _ := store.UserByPhone(phone); fresh.ID != u.ID || fresh.DisplayName() != "" {
			t.Fatal("an expired code changed the profile")
		}
	})

	t.Run("throttled", func(t *testing.T) {
		srv, _, tel, _ := newTestServer(t, nil)
		phone := "+447700900307"
		if rec := startSignup(t, srv, phone); rec.Code != http.StatusOK {
			t.Fatalf("first request: %d", rec.Code)
		}
		rec := startSignup(t, srv, phone)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("a second code straight away should be throttled, got %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "retry_after") {
			t.Fatalf("a throttled request should say how long to wait: %s", rec.Body.String())
		}
		if len(tel.sms) != 1 {
			t.Fatalf("the throttled request still sent a text: %#v", tel.sms)
		}
	})
}

// Someone who has already been through this gets their profile back, not the
// sign-up again.
func TestReturningVerifiedUserSkipsOnboarding(t *testing.T) {
	srv, _, tel, _ := newTestServer(t, nil)
	phone := "+447700900308"
	cookie := webSignedUp(t, srv, tel, phone, "Wren")

	body := page(t, srv, "/", cookie)
	if strings.Contains(body, "What's your number?") || strings.Contains(body, "What should I call you?") {
		t.Fatalf("a verified, named user was asked to onboard again: %s", body)
	}
	if !strings.Contains(body, "Wren") || !hasChannelChoice(body) {
		t.Fatalf("the page did not resume where they left off: %s", body)
	}

	// And posting their number again is a no-op that resumes rather than a
	// fresh code.
	before := len(tel.sms)
	rec := postForm(t, srv, "/signup/start", url.Values{"phone": {phone}}, cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"verified":true`) {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body.String())
	}
	if len(tel.sms) != before {
		t.Fatalf("an already-verified number was texted again: %#v", tel.sms)
	}
}

// A refresh mid-flow resumes at the step the profile is actually at, because
// the step comes from the session rather than from anything the page kept.
func TestRefreshResumesTheRealStep(t *testing.T) {
	srv, _, tel, _ := newTestServer(t, nil)
	cookie := verifiedNumber(t, srv, tel, "+447700900309")

	for i := 0; i < 2; i++ {
		body := page(t, srv, "/", cookie)
		if !strings.Contains(body, "What should I call you?") {
			t.Fatalf("refresh %d did not resume at the name: %s", i, body)
		}
	}
	if rec := postForm(t, srv, "/api/name", url.Values{"name": {"Nadia"}}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("name: %d", rec.Code)
	}
	for i := 0; i < 2; i++ {
		if !hasChannelChoice(page(t, srv, "/", cookie)) {
			t.Fatalf("refresh %d lost the channel step", i)
		}
	}
}

// Two people signing up at once stay entirely separate, and neither session can
// write to the other's profile.
func TestSignupKeepsTwoPeopleApart(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, nil)
	one := webSignedUp(t, srv, tel, "+447700900310", "Ada")
	two := verifiedNumber(t, srv, tel, "+447700900311")

	// The second session names itself, posting the first one's number as well
	// - which must be ignored entirely.
	rec := postForm(t, srv, "/api/name", url.Values{"name": {"Bea"}, "phone": {"+447700900310"}}, two)
	if rec.Code != http.StatusOK {
		t.Fatalf("name: %d %s", rec.Code, rec.Body.String())
	}
	a, _ := store.UserByPhone("+447700900310")
	b, _ := store.UserByPhone("+447700900311")
	if a.Name != "Ada" || b.Name != "Bea" {
		t.Fatalf("names crossed over: %q / %q", a.Name, b.Name)
	}

	// Likewise the channel: it lands on the session's own profile whatever the
	// form says.
	if rec := postForm(t, srv, "/signup", url.Values{"channel": {"call"}, "phone": {"+447700900311"}}, one); rec.Code != http.StatusOK {
		t.Fatalf("channel: %d %s", rec.Code, rec.Body.String())
	}
	a, _ = store.UserByPhone("+447700900310")
	b, _ = store.UserByPhone("+447700900311")
	if a.Channel != "call" || b.Channel == "call" {
		t.Fatalf("channel written to the wrong profile: %q / %q", a.Channel, b.Channel)
	}
}

// The page a signed-in person sees says who they are without ever putting the
// whole number on screen.
func TestSignupPagesNeverShowTheWholeNumber(t *testing.T) {
	srv, _, tel, _ := newTestServer(t, nil)
	phone := "+447700900312"
	cookie := verifiedNumber(t, srv, tel, phone)

	for _, body := range []string{page(t, srv, "/", cookie), page(t, srv, "/dashboard", cookie)} {
		if strings.Contains(body, phone) {
			t.Fatalf("the full number is on the page: %s", body)
		}
	}
	if !strings.Contains(page(t, srv, "/", cookie), maskPhone(phone)) {
		t.Fatal("the masked number should be shown so they know which one was verified")
	}
}

// The two cards are spaced and big enough to tap apart on a phone, and keep
// their focus ring.
func TestChannelButtonsAreSpacedAndTappable(t *testing.T) {
	srv, _, tel, _ := newTestServer(t, nil)
	cookie := webSignedUp(t, srv, tel, "+447700900313", "Ola")

	body := page(t, srv, "/", cookie)
	if !strings.Contains(body, `class="choices"`) {
		t.Fatal("the choice cards are not in a spaced group")
	}
	css := page(t, srv, "/", cookie)
	for _, want := range []string{
		".choices { display: grid; gap: var(--s-5);",
		"min-height: 76px; padding: var(--s-5);",
		"button.choice:hover, button.choice:focus-visible",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("channel button styling missing %q", want)
		}
	}
}

// Nothing in the sign-up may carry the code itself.
func TestSignupNeverEchoesTheCode(t *testing.T) {
	srv, _, tel, _ := newTestServer(t, nil)
	phone := "+447700900314"
	rec := startSignup(t, srv, phone)
	code := lastSMSCode(t, tel)
	if strings.Contains(rec.Body.String(), code) {
		t.Fatalf("the response carried the code: %s", rec.Body.String())
	}
	if strings.Contains(page(t, srv, "/", nil), code) {
		t.Fatal("the page carried the code")
	}
}
