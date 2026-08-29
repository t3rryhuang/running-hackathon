package main

import (
	"errors"
	"log"
	"net/http"
	"time"
)

// handleCheckinNow starts a check-in on demand from the dashboard. Nothing here
// happens on its own: the person picks the channel and presses the button, and
// the check-in goes to the number the session resolves to rather than to
// anything the browser names.
func (s *Server) handleCheckinNow(w http.ResponseWriter, r *http.Request, u *User) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	channel := requestFields(r)["channel"]
	if channel != "call" && channel != "sms" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Pick a call or a text."})
		return
	}
	if err := s.checkinOn(u, channel); err != nil {
		// A duplicate is not a failure: the caller asked for a call and a call
		// is happening. Saying so keeps the button disabled instead of
		// bouncing it back to "try again" while the phone is ringing.
		if errors.Is(err, errCallInProgress) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok": false, "channel": channel, "call": s.callState(u),
				"error": "A call is already in progress.",
			})
			return
		}
		log.Printf("checkin: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "channel": channel, "call": s.callState(u), "error": "Could not reach you just now. Try again in a minute."})
		return
	}
	_ = s.store.MarkTriggered(u.ID, time.Now())
	msg := "Texting you now."
	if channel == "call" {
		msg = "Pick up - I'm calling you now."
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channel": channel, "message": msg, "call": s.callState(u)})
}

// callState is the server's answer to "is a call happening right now?". The
// dashboard polls it, so a refresh, a second tab or a reconnect all agree with
// each other instead of each running their own timer.
func (s *Server) callState(u *User) map[string]any {
	fresh, err := s.store.UserByID(u.ID)
	if err != nil || fresh == nil {
		fresh = u
	}
	now := time.Now()
	state := map[string]any{"in_progress": fresh.CallInProgress(now)}
	if fresh.CallInProgress(now) {
		state["started_at"] = fresh.CallStartedAt.UTC().Format(time.RFC3339)
		// The browser needs an upper bound so it can stop polling a call the
		// server will itself give up on.
		state["expires_in_seconds"] = int((callMaxDuration - now.Sub(*fresh.CallStartedAt)).Seconds())
	}
	return state
}

// handleCallState reports live-call state for the signed-in person only. It is
// read-only and scoped to the session's own profile: no number is accepted from
// the browser, so it cannot be used to probe whether anyone else is on a call.
func (s *Server) handleCallState(w http.ResponseWriter, r *http.Request, u *User) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "call": s.callState(u)})
}

// handleForgetMe erases the signed-in person's own data. It is deliberately not
// behind requireUser: erasing takes the session with it, so a retry after the
// first one succeeded has no session to present, and answering that the same
// way is what makes the button safe to press twice. It can only ever act on the
// session's own profile, so there is no number to pass and nobody else's data
// to reach.
func (s *Server) handleForgetMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	// Typed confirmation, so a stray click or a prefetch cannot erase an
	// account.
	if requestFields(r)["confirm"] != "DELETE" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Type DELETE to confirm."})
		return
	}
	u := s.currentUser(r)
	if u == nil {
		s.setSessionCookie(w, r, "", time.Unix(0, 0))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "erased": true})
		return
	}
	if err := s.store.ForgetUser(u.ID); err != nil {
		log.Printf("forget: erasing user %d: %v", u.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Could not delete your data just now."})
		return
	}
	// No audit row is written for this one: it would keep the number the person
	// just asked us to forget.
	log.Printf("forget: erased user %d from the dashboard", u.ID)
	s.setSessionCookie(w, r, "", time.Unix(0, 0))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "erased": true})
}
