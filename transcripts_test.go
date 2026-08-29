package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func transcriptPayload(conversationID, phone string) string {
	return fmt.Sprintf(`{
	  "type": "post_call_transcription",
	  "data": {
	    "conversation_id": %q,
	    "status": "done",
	    "transcript": [
	      {"role": "agent", "message": "How did the standup go?", "time_in_call_secs": 2},
	      {"role": "user", "message": "Ran long but we shipped the migration.", "time_in_call_secs": 9}
	    ],
	    "metadata": {
	      "start_time_unix_secs": 1739537297,
	      "call_duration_secs": 74,
	      "phone_call": {"direction": "outbound", "external_number": %q, "call_sid": "CAabc"}
	    },
	    "analysis": {"transcript_summary": "Shipped the migration, standup overran."}
	  }
	}`, conversationID, phone)
}

func postTranscript(t *testing.T, srv *Server, secret, body string, at time.Time) *httptest.ResponseRecorder {
	t.Helper()
	ts := fmt.Sprintf("%d", at.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + body))
	sig := fmt.Sprintf("t=%s,v0=%s", ts, hex.EncodeToString(mac.Sum(nil)))
	return postTranscriptSigned(t, srv, body, sig)
}

func postTranscriptSigned(t *testing.T, srv *Server, body, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/elevenlabs", strings.NewReader(body))
	req.Header.Set("elevenlabs-signature", sig)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	srv.WaitAsync()
	return rec
}

func webhookServer(t *testing.T) (*Server, *Store) {
	t.Helper()
	srv, store, _, _ := newTestServer(t, &fakeAnthropic{})
	srv.cfg.ElevenLabsSecret = "whsec_test"
	return srv, store
}

func TestTranscriptWebhookStoresAgainstTheCalledNumber(t *testing.T) {
	srv, store := webhookServer(t)
	u, err := store.EnsureUser("+447700900001")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}

	rec := postTranscript(t, srv, "whsec_test", transcriptPayload("conv_1", u.Phone), time.Now())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	items, total, err := store.Transcripts(u.ID, "", 20, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("want one transcript, got %d", total)
	}
	got := items[0]
	if got.ConversationID != "conv_1" || got.CallSID != "CAabc" {
		t.Fatalf("provider identifiers not kept: %+v", got)
	}
	if got.Turns != 2 || got.Duration != 74*time.Second {
		t.Fatalf("metadata not kept: turns=%d duration=%s", got.Turns, got.Duration)
	}
	if !strings.Contains(got.Body, "shipped the migration") {
		t.Fatalf("transcript body missing what the user said: %q", got.Body)
	}
	if !strings.Contains(got.Body, "[00:09] user:") {
		t.Fatalf("transcript body missing turn timestamps: %q", got.Body)
	}
	if got.Summary == "" {
		t.Fatalf("summary not kept")
	}
}

func TestTranscriptWebhookRejectsBadSignatures(t *testing.T) {
	srv, store := webhookServer(t)
	u, _ := store.EnsureUser("+447700900002")
	body := transcriptPayload("conv_bad", u.Phone)

	cases := []struct {
		name string
		rec  *httptest.ResponseRecorder
	}{
		{"wrong secret", postTranscript(t, srv, "whsec_other", body, time.Now())},
		{"stale timestamp", postTranscript(t, srv, "whsec_test", body, time.Now().Add(-2*time.Hour))},
		{"future timestamp", postTranscript(t, srv, "whsec_test", body, time.Now().Add(2*time.Hour))},
		{"missing header", postTranscriptSigned(t, srv, body, "")},
		{"malformed header", postTranscriptSigned(t, srv, body, "garbage")},
		{"no digest", postTranscriptSigned(t, srv, body, fmt.Sprintf("t=%d", time.Now().Unix()))},
	}
	for _, c := range cases {
		if c.rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %d", c.name, c.rec.Code)
		}
	}

	// A signature over a different body must not validate this one either.
	other := transcriptPayload("conv_other", u.Phone)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte("whsec_test"))
	mac.Write([]byte(ts + "." + other))
	tampered := postTranscriptSigned(t, srv, body, fmt.Sprintf("t=%s,v0=%s", ts, hex.EncodeToString(mac.Sum(nil))))
	if tampered.Code != http.StatusUnauthorized {
		t.Errorf("tampered body: want 401, got %d", tampered.Code)
	}

	if _, total, _ := store.Transcripts(u.ID, "", 20, 0); total != 0 {
		t.Fatalf("rejected deliveries were stored anyway (%d)", total)
	}
}

func TestTranscriptWebhookIsIdempotent(t *testing.T) {
	srv, store := webhookServer(t)
	u, _ := store.EnsureUser("+447700900003")
	body := transcriptPayload("conv_dupe", u.Phone)

	for i := 0; i < 3; i++ {
		if rec := postTranscript(t, srv, "whsec_test", body, time.Now()); rec.Code != http.StatusOK {
			t.Fatalf("delivery %d: %d", i, rec.Code)
		}
	}
	if _, total, _ := store.Transcripts(u.ID, "", 20, 0); total != 1 {
		t.Fatalf("retried delivery duplicated the transcript: %d rows", total)
	}
}

func TestTranscriptWebhookDropsUnresolvableCalls(t *testing.T) {
	srv, store := webhookServer(t)
	known, _ := store.EnsureUser("+447700900004")

	// A number nobody signed up with must not create a profile or be filed
	// against the one user that does exist.
	rec := postTranscript(t, srv, "whsec_test", transcriptPayload("conv_stranger", "+447700900999"), time.Now())
	if rec.Code != http.StatusOK {
		t.Fatalf("want a fast 200 even when unresolvable, got %d", rec.Code)
	}
	if u, _ := store.UserByPhone("+447700900999"); u != nil {
		t.Fatalf("webhook created a profile for an unknown number")
	}
	if _, total, _ := store.Transcripts(known.ID, "", 20, 0); total != 0 {
		t.Fatalf("stranger's call was filed against a known user")
	}

	// So must a delivery carrying no number at all.
	noNumber := `{"type":"post_call_transcription","data":{"conversation_id":"conv_nonum","transcript":[]}}`
	if rec := postTranscript(t, srv, "whsec_test", noNumber, time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if _, total, _ := store.Transcripts(known.ID, "", 20, 0); total != 0 {
		t.Fatalf("numberless call was filed against a known user")
	}
}

func TestTranscriptWebhookIgnoresOtherEvents(t *testing.T) {
	srv, store := webhookServer(t)
	u, _ := store.EnsureUser("+447700900005")
	body := `{"type":"post_call_audio","data":{"conversation_id":"conv_audio"}}`
	rec := postTranscript(t, srv, "whsec_test", body, time.Now())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for an authentic but unhandled event, got %d", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["ignored"] != "post_call_audio" {
		t.Fatalf("want the event reported as ignored, got %v", out)
	}
	if _, total, _ := store.Transcripts(u.ID, "", 20, 0); total != 0 {
		t.Fatalf("unhandled event stored a transcript")
	}
}

func TestTranscriptsArePaginatedSearchableAndOwned(t *testing.T) {
	store := testStore(t)
	alice, _ := store.EnsureUser("+447700900010")
	bob, _ := store.EnsureUser("+447700900011")

	start := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		if err := store.SaveTranscript(&Transcript{
			UserID:         alice.ID,
			ConversationID: fmt.Sprintf("conv_a_%d", i),
			Summary:        fmt.Sprintf("call number %d", i),
			Body:           "we talked about hackathons",
			StartedAt:      start.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if err := store.SaveTranscript(&Transcript{
		UserID: bob.ID, ConversationID: "conv_b_0", Summary: "bob's private call",
		Body: "bob talked about pottery", StartedAt: start,
	}); err != nil {
		t.Fatalf("save bob: %v", err)
	}

	page1, total, err := store.Transcripts(alice.ID, "", 20, 0)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if total != 25 || len(page1) != 20 {
		t.Fatalf("want 25 total and 20 on the page, got %d/%d", total, len(page1))
	}
	if !page1[0].StartedAt.After(page1[1].StartedAt) {
		t.Fatalf("transcripts are not newest first")
	}
	page2, _, _ := store.Transcripts(alice.ID, "", 20, 20)
	if len(page2) != 5 {
		t.Fatalf("want 5 on page 2, got %d", len(page2))
	}

	hits, total, _ := store.Transcripts(alice.ID, "HACKATHONS", 20, 0)
	if total != 25 || len(hits) == 0 {
		t.Fatalf("search should match the body case-insensitively, got %d", total)
	}
	if _, total, _ := store.Transcripts(alice.ID, "pottery", 20, 0); total != 0 {
		t.Fatalf("alice's search reached bob's transcript")
	}

	// Bob's row is invisible to Alice by id as well as by search.
	bobs, _, _ := store.Transcripts(bob.ID, "", 20, 0)
	if len(bobs) != 1 {
		t.Fatalf("bob should see exactly his own call, got %d", len(bobs))
	}
	if got, err := store.Transcript(alice.ID, bobs[0].ID); err != nil || got != nil {
		t.Fatalf("alice fetched bob's transcript by id: %+v (%v)", got, err)
	}
	if ok, err := store.DeleteTranscript(alice.ID, bobs[0].ID); err != nil || ok {
		t.Fatalf("alice deleted bob's transcript")
	}
	if _, total, _ := store.Transcripts(bob.ID, "", 20, 0); total != 1 {
		t.Fatalf("bob's transcript went missing")
	}

	// The owner can delete their own.
	if ok, err := store.DeleteTranscript(bob.ID, bobs[0].ID); err != nil || !ok {
		t.Fatalf("owner could not delete their own transcript: %v", err)
	}
}

func TestTranscriptRetentionPurge(t *testing.T) {
	store := testStore(t)
	u, _ := store.EnsureUser("+447700900012")
	now := time.Now().UTC()
	fresh := &Transcript{UserID: u.ID, ConversationID: "fresh", StartedAt: now.Add(-24 * time.Hour)}
	old := &Transcript{UserID: u.ID, ConversationID: "old", StartedAt: now.Add(-transcriptRetention - time.Hour)}
	for _, tr := range []*Transcript{fresh, old} {
		if err := store.SaveTranscript(tr); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	n, err := store.PurgeExpiredTranscripts(now)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("want one expired transcript purged, got %d", n)
	}
	items, _, _ := store.Transcripts(u.ID, "", 20, 0)
	if len(items) != 1 || items[0].ConversationID != "fresh" {
		t.Fatalf("purge removed the wrong rows: %+v", items)
	}
}
