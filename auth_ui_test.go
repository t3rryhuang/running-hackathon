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

func getPage(t *testing.T, srv *Server, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json %s: %v", rec.Body.String(), err)
	}
	return out
}

// Asking twice in a row is refused for the length of the cooldown, and the
// answer says how long to wait so the page can count it down.
func TestResendCooldownAppliesAndReportsTheWait(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{})
	u, _ := store.EnsureUser("+447700900401")

	if rec := postForm(t, srv, "/auth/request", url.Values{"phone": {u.Phone}}, nil); rec.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", rec.Code, rec.Body.String())
	}
	rec := postForm(t, srv, "/auth/request", url.Values{"phone": {u.Phone}}, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request should be throttled, got %d", rec.Code)
	}
	body := decode(t, rec)
	retry, ok := body["retry_after"].(float64)
	if !ok || retry < 1 || retry > loginCodeCooldown.Seconds() {
		t.Fatalf("unhelpful retry_after: %v", body["retry_after"])
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("throttled response should carry a Retry-After header")
	}
}

// The cooldown is applied before the number is looked up, so a registered and
// an unregistered number are throttled the same way. Otherwise 429-versus-200
// would say who has an account.
func TestThrottlingDoesNotRevealWhoIsRegistered(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{})
	known, _ := store.EnsureUser("+447700900402")
	const unknown = "+447700900403"

	var bodies []string
	for _, phone := range []string{known.Phone, unknown} {
		first := postForm(t, srv, "/auth/request", url.Values{"phone": {phone}}, nil)
		second := postForm(t, srv, "/auth/request", url.Values{"phone": {phone}}, nil)
		if first.Code != http.StatusOK || second.Code != http.StatusTooManyRequests {
			t.Fatalf("%s: got %d then %d", phone, first.Code, second.Code)
		}
		bodies = append(bodies, first.Body.String(), second.Body.String())
	}
	if bodies[0] != bodies[2] || bodies[1] != bodies[3] {
		t.Fatalf("responses differ by registration: %v", bodies)
	}
}

// When the SMS provider fails, the unusable code is thrown away rather than
// left to invalidate the person's next attempt, the failure is audited, and the
// browser is told nothing about the provider.
func TestProviderFailureDiscardsTheCodeAndStaysGeneric(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, &fakeAnthropic{})
	u, _ := store.EnsureUser("+447700900406")
	tel.sendErr = errors.New("twilio 500: upstream unavailable")

	rec := postForm(t, srv, "/auth/request", url.Values{"phone": {u.Phone}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("a provider failure must not be visible per number: %d", rec.Code)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "twilio") {
		t.Fatalf("provider detail leaked to the browser: %s", rec.Body.String())
	}

	// No code survives, so nothing can be verified and the next attempt starts
	// clean rather than racing a code the person never received.
	if err := store.ConsumeLoginCode(u.Phone, "000000", time.Now()); err == nil {
		t.Fatalf("an undelivered code should not be left outstanding")
	}
	var live int
	if err := store.queryRow(`SELECT COUNT(*) FROM login_codes WHERE phone=? AND consumed_at IS NULL`, u.Phone).Scan(&live); err != nil {
		t.Fatalf("count codes: %v", err)
	}
	if live != 0 {
		t.Fatalf("undelivered code was left in the database")
	}

	events, _ := store.AuthEvents(u.Phone, 10)
	var audited bool
	for _, e := range events {
		if e == "code_delivery_failed" {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("delivery failure should be audited, got %v", events)
	}
}

// The code exists in the text message and nowhere else - in particular not in
// the row that validates it.
func TestCodeIsNeverStoredInPlaintext(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, &fakeAnthropic{})
	u, _ := store.EnsureUser("+447700900407")
	if rec := postForm(t, srv, "/auth/request", url.Values{"phone": {u.Phone}}, nil); rec.Code != http.StatusOK {
		t.Fatalf("request: %d", rec.Code)
	}
	code := lastSMSCode(t, tel)

	rows, err := store.query(`SELECT code_hash FROM login_codes WHERE phone=?`, u.Phone)
	if err != nil {
		t.Fatalf("read codes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(stored, code) {
			t.Fatalf("the code itself is in the database")
		}
	}
}

// Signed out, the dashboard is not reachable and the identity endpoint says so
// rather than answering with somebody's profile.
func TestProtectedPagesRequireASession(t *testing.T) {
	srv, _, _, _ := newTestServer(t, &fakeAnthropic{})

	rec := getPage(t, srv, "/dashboard", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("dashboard should send a signed-out visitor to /login, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := getPage(t, srv, "/api/me", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/me should be 401 when signed out, got %d", rec.Code)
	}
}

// An expired or revoked session is not a silent failure: the page says the
// session ended and offers a way back in.
func TestExpiredSessionIsExplainedOnTheLoginPage(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, &fakeAnthropic{})
	u, _ := store.EnsureUser("+447700900408")
	cookie := signIn(t, srv, tel, u.Phone)

	if _, err := store.RevokeAllAuthSessions(u.ID, time.Now()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rec := getPage(t, srv, "/dashboard", cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a revoked session should not open the dashboard, got %d", rec.Code)
	}

	page := getPage(t, srv, "/login?expired=1", nil).Body.String()
	if !strings.Contains(page, "Your session has ended") {
		t.Fatalf("login page should explain the expiry: %s", page)
	}
}

// The header states which of the five states the page is in, and the login page
// carries the states it needs before any JavaScript runs.
func TestPagesCarryAuthenticationStates(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, &fakeAnthropic{})
	u, _ := store.EnsureUser("+447700900409")

	public := getPage(t, srv, "/login", nil).Body.String()
	for _, want := range []string{`id="authbar"`, `data-state="loading"`, `aria-live="polite"`, `id="auth-in"`, `id="auth-out"`} {
		if !strings.Contains(public, want) {
			t.Errorf("signed-out page missing %s", want)
		}
	}
	if !strings.Contains(public, "Sign in") || !strings.Contains(public, "resend") {
		t.Errorf("login page should offer sign-in and a resend control")
	}

	cookie := signIn(t, srv, tel, u.Phone)
	dash := getPage(t, srv, "/dashboard", cookie).Body.String()
	if !strings.Contains(dash, "data-protected") {
		t.Errorf("the dashboard should mark itself as protected so an expired session redirects")
	}
	if !strings.Contains(dash, "Sign out") {
		t.Errorf("an authenticated page needs a visible way out")
	}
}

// The header identifies the account by masked number, and the endpoint behind
// it does not hand out anything more than it needs to.
func TestIdentityShownIsNotSensitive(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, &fakeAnthropic{})
	u, _ := store.EnsureUser("+447700900410")
	cookie := signIn(t, srv, tel, u.Phone)

	body := decode(t, getPage(t, srv, "/api/me", cookie))
	if _, ok := body["phone"]; ok {
		t.Errorf("the full number should not be served to the page: %v", body)
	}
	masked, _ := body["phone_masked"].(string)
	if !strings.HasSuffix(masked, "0410") || strings.Contains(masked, "+447700") {
		t.Errorf("masked number is wrong: %q", masked)
	}
}

// Signing out invalidates the cookie for good; replaying it afterwards is not
// a session.
func TestLogoutRevokesTheCookieForGood(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, &fakeAnthropic{})
	u, _ := store.EnsureUser("+447700900411")
	cookie := signIn(t, srv, tel, u.Phone)

	if rec := postForm(t, srv, "/auth/logout", url.Values{}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := getPage(t, srv, "/api/me", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked cookie should not still work, got %d", rec.Code)
	}
	if rec := getPage(t, srv, "/dashboard", cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("the dashboard should be closed after logout, got %d", rec.Code)
	}
}

// Two people signed in at once see their own account and never each other's.
func TestSessionsDoNotCrossBetweenUsers(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, &fakeAnthropic{})
	ua, _ := store.EnsureUser("+447700900412")
	ub, _ := store.EnsureUser("+447700900413")
	if _, err := store.SaveOnboarding(ua, "Ada", "hackathons", "daily"); err != nil {
		t.Fatalf("name: %v", err)
	}
	if _, err := store.SaveOnboarding(ub, "Bo", "meetups", "daily"); err != nil {
		t.Fatalf("name: %v", err)
	}

	ca := signIn(t, srv, tel, ua.Phone)
	cb := signIn(t, srv, tel, ub.Phone)

	if name := decode(t, getPage(t, srv, "/api/me", ca))["name"]; name != "Ada" {
		t.Fatalf("first session resolved to %v", name)
	}
	if name := decode(t, getPage(t, srv, "/api/me", cb))["name"]; name != "Bo" {
		t.Fatalf("second session resolved to %v", name)
	}
}
