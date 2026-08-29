package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// With an auth token configured, an inbound /sms must carry a valid Twilio
// signature - otherwise anyone could post as any number, and "identified by
// verified phone number" would mean nothing.
func TestInboundSMSRequiresTwilioSignature(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	srv.cfg.TwilioAuthToken = "token123"

	form := url.Values{"From": {"+447700900401"}, "Body": {"hello"}}
	req := httptest.NewRequest(http.MethodPost, "https://runhack.keanuc.net/sms", strings.NewReader(form.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unsigned webhook accepted: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := store.UserByPhone("+447700900401"); err == nil {
		t.Error("an unsigned webhook created a user")
	}

	signed := httptest.NewRequest(http.MethodPost, "https://runhack.keanuc.net/sms", strings.NewReader(form.Encode()))
	signed.Header.Set("content-type", "application/x-www-form-urlencoded")
	signed.Header.Set("X-Forwarded-Proto", "https")
	if err := signed.ParseForm(); err != nil {
		t.Fatalf("parse: %v", err)
	}
	signed.Header.Set("X-Twilio-Signature", twilioSignature("token123", "https://runhack.keanuc.net/sms", signed.PostForm))
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, signed)
	if rec.Code != http.StatusOK {
		t.Fatalf("signed webhook rejected: %d %s", rec.Code, rec.Body.String())
	}

	// A signed message proves the number, which is what marks it verified.
	u, err := store.UserByPhone("+447700900401")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if !u.PhoneVerified() || u.PhoneVerifiedVia != "twilio_inbound_sms" {
		t.Errorf("phone not marked verified: %#v", u)
	}
}

// Twilio retries a delivery it thinks failed. The retry must replay the same
// answer, not take a second conversation turn.
func TestRetriedSMSDeliveryIsIdempotent(t *testing.T) {
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{err: errModelUnavailable})
	phone := "+447700900402"

	post := func() string {
		form := url.Values{"From": {phone}, "Body": {"hackathons"}, "MessageSid": {"SM123"}}
		req := httptest.NewRequest(http.MethodPost, "/sms", strings.NewReader(form.Encode()))
		req.Header.Set("content-type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		return smsBody(t, rec)
	}
	first := post()
	second := post()
	if first != second {
		t.Fatalf("retry produced a different reply: %q vs %q", first, second)
	}
	u, _ := store.UserByPhone(phone)
	msgs, _ := store.RecentMessages(u.ID, 10)
	if len(msgs) != 2 {
		t.Fatalf("retry took a second conversation turn: %#v", msgs)
	}
}
