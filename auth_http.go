package main

import (
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ipLimiter is a second, coarser rate limit in front of the per-number one. The
// database limit stops one number being flooded with codes; this stops one
// client walking a list of numbers to find out which ones are registered.
type ipLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

func newIPLimiter(limit int, window time.Duration) *ipLimiter {
	return &ipLimiter{hits: map[string][]time.Time{}, limit: limit, window: window}
}

func (l *ipLimiter) allow(key string, now time.Time) bool {
	ok, _ := l.take(key, now)
	return ok
}

// take records a hit and reports whether it was allowed. When it was not, the
// second return is how long the caller has to wait, which the login page shows
// as a countdown instead of leaving somebody pressing a dead button.
func (l *ipLimiter) take(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.hits[key] = kept
	if len(kept) >= l.limit {
		wait := l.window - now.Sub(kept[0])
		if wait < time.Second {
			wait = time.Second
		}
		return false, wait
	}
	l.hits[key] = append(kept, now)
	return true, 0
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// currentUser resolves the signed-in person from the session cookie, or nil.
func (s *Server) currentUser(r *http.Request) *User {
	c, err := r.Cookie(authSessionCookie)
	if err != nil {
		return nil
	}
	u, err := s.store.AuthSessionUser(c.Value, time.Now())
	if err != nil || u == nil {
		return nil
	}
	return u
}

// requireUser guards everything that reads personal data. Handlers behind it
// receive the owner, so no handler has to be trusted to scope its own query by
// a user id taken from the request.
func (s *Server) requireUser(h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in first"})
			return
		}
		h(w, r, u)
	}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     authSessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})
}

// handleAuthRequest sends a login code. The response is deliberately identical
// whether or not the number is registered: a login form should not be a way to
// find out who uses the service. The code is only ever sent to the number
// itself, never returned in the response or written to the log.
func (s *Server) handleAuthRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	phone := normalisePhone(r.FormValue("phone"))
	generic := map[string]any{
		"sent":       true,
		"message":    "If that number is registered, a code is on its way.",
		"resend_in":  int(loginCodeCooldown.Seconds()),
		"expires_in": int(loginCodeTTL.Seconds()),
	}
	if !e164.MatchString(phone) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Use the international format, like +447700900123."})
		return
	}
	if !s.loginLimiter.allow(clientIP(r), time.Now()) {
		s.store.RecordAuthEvent(phone, 0, "code_rate_limited", "ip")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many attempts. Try again in a few minutes."})
		return
	}
	// The per-number throttles are applied before the number is looked up, so
	// a registered number and an unregistered one are throttled identically.
	// Doing it the other way round would turn 429-versus-200 into an oracle for
	// who has an account.
	if ok, wait := s.codeCooldown.take(phone, time.Now()); !ok {
		s.store.RecordAuthEvent(phone, 0, "code_rate_limited", "cooldown")
		s.tooManyCodes(w, "You just asked for a code. Give it a moment before trying again.", wait)
		return
	}
	if ok, wait := s.codeBudget.take(phone, time.Now()); !ok {
		s.store.RecordAuthEvent(phone, 0, "code_rate_limited", "phone")
		s.tooManyCodes(w, "Too many codes requested for that number. Try again shortly.", wait)
		return
	}
	u, err := s.store.UserByPhone(phone)
	if err != nil || u == nil {
		s.store.RecordAuthEvent(phone, 0, "code_requested_unknown", "")
		writeJSON(w, http.StatusOK, generic)
		return
	}
	code, err := s.store.IssueLoginCode(phone, time.Now())
	if errors.Is(err, errRateLimited) {
		s.store.RecordAuthEvent(phone, u.ID, "code_rate_limited", "phone")
		s.tooManyCodes(w, "Too many codes requested for that number. Try again shortly.", loginCodeWindow)
		return
	}
	if err != nil {
		log.Printf("auth: issuing code: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Could not send a code just now."})
		return
	}
	// A code that the provider could not deliver is worse than no code: it
	// silently invalidates the previous one and eats the number's budget. So it
	// is thrown away, and the person can ask again as soon as the cooldown is
	// up. The response stays generic either way - whether Twilio is having a
	// bad day is not something a login form should reveal per number.
	if err := s.tel.SendSMS(phone, "Your CheckIn code is "+code+". It expires in 10 minutes."); err != nil {
		log.Printf("auth: delivering code to a registered number failed: %v", err)
		if derr := s.store.DiscardLoginCode(phone); derr != nil {
			log.Printf("auth: discarding undelivered code: %v", derr)
		}
		s.store.RecordAuthEvent(phone, u.ID, "code_delivery_failed", "")
		writeJSON(w, http.StatusOK, generic)
		return
	}
	s.store.RecordAuthEvent(phone, u.ID, "code_sent", "")
	writeJSON(w, http.StatusOK, generic)
}

// tooManyCodes answers a throttled request with the wait, so the page can show
// a countdown rather than a dead end.
func (s *Server) tooManyCodes(w http.ResponseWriter, msg string, wait time.Duration) {
	secs := int(wait.Seconds() + 0.5)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": msg, "retry_after": secs})
}

// handleAuthVerify exchanges a code for a session. Verifying also records the
// number as proved, which is the same fact a completed call establishes, so a
// returning user lands on their existing profile rather than onboarding again.
func (s *Server) handleAuthVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	phone := normalisePhone(r.FormValue("phone"))
	code := strings.TrimSpace(r.FormValue("code"))
	if !e164.MatchString(phone) || code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Enter your number and the code you were sent."})
		return
	}
	if !s.loginLimiter.allow(clientIP(r), time.Now()) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many attempts. Try again in a few minutes."})
		return
	}
	u, err := s.store.UserByPhone(phone)
	if err != nil || u == nil {
		// Same wording as a wrong code, for the same reason as above.
		s.store.RecordAuthEvent(phone, 0, "login_failed", "unknown number")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "That code is not right or has expired."})
		return
	}
	if err := s.store.ConsumeLoginCode(phone, code, time.Now()); err != nil {
		s.store.RecordAuthEvent(phone, u.ID, "login_failed", err.Error())
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "That code is not right or has expired."})
		return
	}
	token, err := s.store.StartAuthSession(u.ID, time.Now())
	if err != nil {
		log.Printf("auth: starting session: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Could not sign you in just now."})
		return
	}
	_ = s.store.MarkPhoneVerified(u.ID, "otp")
	s.store.RecordAuthEvent(phone, u.ID, "login_ok", "")
	s.setSessionCookie(w, r, token, time.Now().Add(authSessionTTL))
	writeJSON(w, http.StatusOK, map[string]any{
		"signed_in": true,
		"name":      u.DisplayName(),
		"onboarded": u.Onboarded(),
	})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(authSessionCookie); err == nil {
		if u := s.currentUser(r); u != nil {
			s.store.RecordAuthEvent(u.Phone, u.ID, "logout", "")
		}
		_ = s.store.RevokeAuthSession(c.Value, time.Now())
	}
	s.setSessionCookie(w, r, "", time.Unix(0, 0))
	if r.Header.Get("accept") == "application/json" || r.Method == http.MethodPost {
		writeJSON(w, http.StatusOK, map[string]any{"signed_out": true})
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// maskPhone keeps the last four digits, which is enough for somebody to
// recognise their own number and not enough to be worth shoulder-surfing.
func maskPhone(phone string) string {
	digits := strings.TrimSpace(phone)
	if len(digits) <= 4 {
		return digits
	}
	return "\u2022\u2022\u2022\u2022 " + digits[len(digits)-4:]
}

// handleMe is what the header uses to show who is signed in. It returns the
// masked number rather than the number itself: the page only needs enough for
// somebody to recognise their own account.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, u *User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":         u.DisplayName(),
		"phone_masked": maskPhone(u.Phone),
		"onboarded":    u.Onboarded(),
		"interests":    u.InterestList(),
	})
}

const transcriptPageSize = 20

// handleTranscriptList returns a page of the signed-in person's own calls.
func (s *Server) handleTranscriptList(w http.ResponseWriter, r *http.Request, u *User) {
	q := r.URL.Query().Get("q")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	items, total, err := s.store.Transcripts(u.ID, q, transcriptPageSize, (page-1)*transcriptPageSize)
	if err != nil {
		log.Printf("transcripts: listing: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Could not load your calls."})
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, t := range items {
		out = append(out, map[string]any{
			"id":         t.ID,
			"started_at": t.StartedAt.In(londonLoc).Format(time.RFC3339),
			"when":       t.StartedAt.In(londonLoc).Format("Mon 2 Jan 2006, 15:04"),
			"direction":  t.Direction,
			"status":     t.Status,
			"summary":    t.Summary,
			"turns":      t.Turns,
			"seconds":    int(t.Duration.Seconds()),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"transcripts": out,
		"total":       total,
		"page":        page,
		"page_size":   transcriptPageSize,
		"pages":       (total + transcriptPageSize - 1) / transcriptPageSize,
		"query":       q,
		"retain_days": int(transcriptRetention.Hours() / 24),
	})
}

// handleTranscriptItem serves or deletes one call. An id belonging to somebody
// else reads as missing rather than forbidden, so the endpoint cannot be used
// to confirm that a given transcript exists.
func (s *Server) handleTranscriptItem(w http.ResponseWriter, r *http.Request, u *User) {
	raw := strings.TrimPrefix(r.URL.Path, "/api/transcripts/")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "No such call."})
		return
	}
	switch r.Method {
	case http.MethodGet:
		t, err := s.store.Transcript(u.ID, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Could not load that call."})
			return
		}
		if t == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "No such call."})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":         t.ID,
			"started_at": t.StartedAt.In(londonLoc).Format(time.RFC3339),
			"when":       t.StartedAt.In(londonLoc).Format("Mon 2 Jan 2006, 15:04"),
			"direction":  t.Direction,
			"status":     t.Status,
			"summary":    t.Summary,
			"body":       t.Body,
			"turns":      t.Turns,
			"seconds":    int(t.Duration.Seconds()),
		})
	case http.MethodDelete:
		ok, err := s.store.DeleteTranscript(u.ID, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Could not delete that call."})
			return
		}
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "No such call."})
			return
		}
		s.store.RecordAuthEvent(u.Phone, u.ID, "transcript_deleted", strconv.FormatInt(id, 10))
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if u := s.currentUser(r); u != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.render(w, "login.html", map[string]any{
		"Expired":  r.URL.Query().Get("expired") != "",
		"ResendIn": int(loginCodeCooldown.Seconds()),
	})
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, "dashboard.html", map[string]any{
		"User":       u,
		"RetainDays": int(transcriptRetention.Hours() / 24),
		"Protected":  true,
	})
}
