package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func postForm(t *testing.T, srv *Server, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == authSessionCookie && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no session cookie in response: %s", rec.Body.String())
	return nil
}

// lastSMSCode pulls the code out of the text the service just sent, which is
// the only place it exists after issuing.
func lastSMSCode(t *testing.T, tel *recordedSMS) string {
	t.Helper()
	if len(tel.sms) == 0 {
		t.Fatalf("no code was texted")
	}
	msg := tel.sms[len(tel.sms)-1]
	fields := strings.Fields(msg)
	for _, f := range fields {
		f = strings.TrimSuffix(f, ".")
		if len(f) == loginCodeLength && strings.Trim(f, "0123456789") == "" {
			return f
		}
	}
	t.Fatalf("no code in %q", msg)
	return ""
}

func signIn(t *testing.T, srv *Server, tel *recordedSMS, phone string) *http.Cookie {
	t.Helper()
	if rec := postForm(t, srv, "/auth/request", url.Values{"phone": {phone}}, nil); rec.Code != http.StatusOK {
		t.Fatalf("request code: %d %s", rec.Code, rec.Body.String())
	}
	code := lastSMSCode(t, tel)
	rec := postForm(t, srv, "/auth/verify", url.Values{"phone": {phone}, "code": {code}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body.String())
	}
	return sessionCookie(t, rec)
}

func TestLoginCodesAreHashedSingleUseAndExpiring(t *testing.T) {
	store := testStore(t)
	u, _ := store.EnsureUser("+447700900101")
	now := time.Now()

	code, err := store.IssueLoginCode(u.Phone, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if len(code) != loginCodeLength {
		t.Fatalf("want a %d digit code, got %q", loginCodeLength, code)
	}

	// Nothing readable as the code itself may sit in the database.
	var stored string
	if err := store.queryRow(`SELECT code_hash FROM login_codes WHERE phone=?`, u.Phone).Scan(&stored); err != nil {
		t.Fatalf("read code row: %v", err)
	}
	if stored == code || strings.Contains(stored, code) {
		t.Fatalf("login code was persisted in the clear")
	}
	if stored != hashSecret(code) {
		t.Fatalf("stored value is not the code hash")
	}

	if err := store.ConsumeLoginCode(u.Phone, code, now); err != nil {
		t.Fatalf("first use should succeed: %v", err)
	}
	if err := store.ConsumeLoginCode(u.Phone, code, now); err == nil {
		t.Fatalf("a spent code was accepted a second time")
	}

	expired, _ := store.IssueLoginCode(u.Phone, now)
	if err := store.ConsumeLoginCode(u.Phone, expired, now.Add(loginCodeTTL+time.Minute)); err != errCodeExpired {
		t.Fatalf("want expiry, got %v", err)
	}
}

func TestLoginCodeAttemptsAndRateLimits(t *testing.T) {
	store := testStore(t)
	u, _ := store.EnsureUser("+447700900102")
	now := time.Now()

	code, _ := store.IssueLoginCode(u.Phone, now)
	for i := 0; i < loginCodeAttempts; i++ {
		if err := store.ConsumeLoginCode(u.Phone, "000000", now); err == nil {
			t.Fatalf("a wrong code was accepted")
		}
	}
	// Burnt through the attempt budget: the real code no longer works either.
	if err := store.ConsumeLoginCode(u.Phone, code, now); err == nil {
		t.Fatalf("code still usable after exhausting attempts")
	}

	fresh := testStore(t)
	v, _ := fresh.EnsureUser("+447700900103")
	for i := 0; i < loginCodeRate; i++ {
		if _, err := fresh.IssueLoginCode(v.Phone, now); err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
	}
	if _, err := fresh.IssueLoginCode(v.Phone, now); err != errRateLimited {
		t.Fatalf("want rate limiting, got %v", err)
	}
	// The window moves: later requests are allowed again.
	if _, err := fresh.IssueLoginCode(v.Phone, now.Add(loginCodeWindow+time.Minute)); err != nil {
		t.Fatalf("request after the window: %v", err)
	}
}

func TestSessionsResolveExpireAndRevoke(t *testing.T) {
	store := testStore(t)
	u, _ := store.EnsureUser("+447700900104")
	now := time.Now()

	token, err := store.StartAuthSession(u.ID, now)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	var storedHash string
	if err := store.queryRow(`SELECT token_hash FROM auth_sessions WHERE user_id=?`, u.ID).Scan(&storedHash); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if storedHash == token {
		t.Fatalf("session token was persisted in the clear")
	}

	got, err := store.AuthSessionUser(token, now)
	if err != nil || got == nil || got.ID != u.ID {
		t.Fatalf("session did not resolve to its owner: %v", err)
	}
	if _, err := store.AuthSessionUser(token, now.Add(authSessionTTL+time.Hour)); err != errNoSession {
		t.Fatalf("expired session still resolved: %v", err)
	}
	if err := store.RevokeAuthSession(token, now); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := store.AuthSessionUser(token, now); err != errNoSession {
		t.Fatalf("revoked session still resolved: %v", err)
	}

	a, _ := store.StartAuthSession(u.ID, now)
	b, _ := store.StartAuthSession(u.ID, now)
	if n, err := store.RevokeAllAuthSessions(u.ID, now); err != nil || n != 2 {
		t.Fatalf("want both sessions revoked, got %d (%v)", n, err)
	}
	for _, tok := range []string{a, b} {
		if _, err := store.AuthSessionUser(tok, now); err != errNoSession {
			t.Fatalf("session survived a global revocation")
		}
	}
}

func TestSignInFlowResumesTheExistingProfile(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, &fakeAnthropic{})
	u := settleChecklist(t, store, "+447700900105")
	if err := store.SaveOnboarding(u, "Keanu", "hackathons", "daily"); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	cookie := signIn(t, srv, tel, u.Phone)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: %d", rec.Code)
	}
	var me map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me["name"] != "Keanu" || me["onboarded"] != true {
		t.Fatalf("signing in did not resume the stored profile: %v", me)
	}

	// Signing in proves the number, the same fact a completed call establishes.
	after, _ := store.UserByPhone(u.Phone)
	if !after.PhoneVerified() || after.PhoneVerifiedVia == "" {
		t.Fatalf("sign-in did not record the number as verified")
	}
	if !after.Onboarded() {
		t.Fatalf("signing in reset onboarding")
	}
}

func TestSignInDoesNotRevealWhoIsRegistered(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, &fakeAnthropic{})
	known, _ := store.EnsureUser("+447700900106")

	a := postForm(t, srv, "/auth/request", url.Values{"phone": {known.Phone}}, nil)
	b := postForm(t, srv, "/auth/request", url.Values{"phone": {"+447700900107"}}, nil)
	if a.Code != b.Code || a.Body.String() != b.Body.String() {
		t.Fatalf("registered and unknown numbers answer differently:\n%s\n%s", a.Body.String(), b.Body.String())
	}
	for _, msg := range tel.sms {
		if strings.Contains(msg, "+447700900107") {
			t.Fatalf("a code was texted to an unregistered number")
		}
	}

	// A wrong code and an unknown number are indistinguishable too.
	wrong := postForm(t, srv, "/auth/verify", url.Values{"phone": {known.Phone}, "code": {"000000"}}, nil)
	unknown := postForm(t, srv, "/auth/verify", url.Values{"phone": {"+447700900107"}, "code": {"000000"}}, nil)
	if wrong.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for both, got %d and %d", wrong.Code, unknown.Code)
	}
	if wrong.Body.String() != unknown.Body.String() {
		t.Fatalf("failure wording differs between known and unknown numbers")
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, &fakeAnthropic{})
	u, _ := store.EnsureUser("+447700900108")
	cookie := signIn(t, srv, tel, u.Phone)

	if rec := postForm(t, srv, "/auth/logout", url.Values{}, cookie); rec.Code != http.StatusOK {
		t.Fatalf("logout: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/transcripts", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("the cookie still worked after signing out: %d", rec.Code)
	}
}

func TestDashboardAPIsAreOwnerOnly(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, &fakeAnthropic{})
	alice, _ := store.EnsureUser("+447700900109")
	bob, _ := store.EnsureUser("+447700900110")
	if err := store.SaveTranscript(&Transcript{
		UserID: bob.ID, ConversationID: "conv_bob", Summary: "bob's call",
		Body: "bob talked about pottery", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	bobs, _, _ := store.Transcripts(bob.ID, "", 10, 0)

	// Signed out, nothing is readable.
	for _, path := range []string{"/api/transcripts", "/api/me"} {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a session: want 401, got %d", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("dashboard without a session should redirect to sign-in, got %d", rec.Code)
	}

	cookie := signIn(t, srv, tel, alice.Phone)

	list := httptest.NewRequest(http.MethodGet, "/api/transcripts", nil)
	list.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, list)
	var page struct {
		Transcripts []map[string]any `json:"transcripts"`
		Total       int              `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if page.Total != 0 || len(page.Transcripts) != 0 {
		t.Fatalf("alice sees calls that are not hers: %v", page)
	}

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/transcripts/"+strconv.FormatInt(bobs[0].ID, 10), nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s of bob's transcript by alice: want 404, got %d", method, rec.Code)
		}
	}
	if _, total, _ := store.Transcripts(bob.ID, "", 10, 0); total != 1 {
		t.Fatalf("bob's transcript was affected by alice's requests")
	}
}

func TestSignInIsAudited(t *testing.T) {
	srv, store, tel, _ := newTestServer(t, &fakeAnthropic{})
	u, _ := store.EnsureUser("+447700900111")
	cookie := signIn(t, srv, tel, u.Phone)
	postForm(t, srv, "/auth/logout", url.Values{}, cookie)

	events, err := store.AuthEvents(u.Phone, 10)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	want := map[string]bool{"code_sent": false, "login_ok": false, "logout": false}
	for _, e := range events {
		if _, ok := want[e]; ok {
			want[e] = true
		}
	}
	for e, seen := range want {
		if !seen {
			t.Errorf("audit trail missing %q (got %v)", e, events)
		}
	}
}
