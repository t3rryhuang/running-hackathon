package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The wizard is the whole product for anyone who has not been called yet, so
// the accessibility affordances the design system promises are asserted rather
// than eyeballed: an announced progress bar, real alert regions, pressed state
// on the choice cards, and a form that still works without JavaScript.
func TestWizardAccessibleMarkup(t *testing.T) {
	srv, _, _, _ := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	for _, want := range []string{
		`role="progressbar"`,
		`aria-valuenow="1"`,
		`id="err" role="alert"`,
		`aria-live="polite"`,
		`<label for="phone">`,
		`<noscript>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("wizard markup missing %q", want)
		}
	}
}

// The choice cards keep their pressed state and their accessible label once
// they are reachable, which is only after a name is stored.
func TestChoiceCardsAnnounceTheirPressedState(t *testing.T) {
	srv, _, tel, _ := newTestServer(t, nil)
	cookie := webSignedUp(t, srv, tel, "+447700900160", "Priya")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `data-channel="call" aria-pressed="false"`) {
		t.Errorf("choice cards missing pressed state: %s", rec.Body.String())
	}
}

// A placeholder in the name field must never look like a real person's name -
// people copy them, and the agent then greets everyone as that person.
func TestWizardDoesNotSuggestAName(t *testing.T) {
	srv, _, _, _ := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), `placeholder="Keanu"`) {
		t.Error("name field placeholder is a real name; use neutral copy")
	}
}

func TestBrandAssetsAreServed(t *testing.T) {
	srv, _, _, _ := newTestServer(t, nil)
	for _, path := range []string{
		"/static/brand/checkin-mark.png",
		"/static/brand/checkin-mark-64.png",
		"/static/brand/favicon-32.png",
	} {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("%s: content type %q, want image/png", path, ct)
		}
	}
}

// An empty journal is the first thing a new user sees after signing up; it has
// to explain itself rather than render a bare heading.
func TestJournalEmptyState(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
	if err := store.UpsertUser(&User{Phone: "+447700900555", Channel: "sms", Frequency: "daily"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/journal?phone=%2B447700900555", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "No check-ins yet") {
		t.Error("journal with no entries should render the empty state")
	}
	if !strings.Contains(body, `class="empty"`) {
		t.Error("empty state should use the design system's empty component")
	}
}
