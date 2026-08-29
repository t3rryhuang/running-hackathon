package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The delivery that was lost in production carried the provider's own system
// variables, which are not all strings. Decoding them strictly rejected the
// whole call - transcript, summary and all - and with retries disabled that
// call was gone for good.
func TestTranscriptWebhookAcceptsNonStringDynamicVariables(t *testing.T) {
	srv, store := webhookServer(t)
	u, _ := store.EnsureUser("+447700900021")
	body := fmt.Sprintf(`{
	  "type": "post_call_transcription",
	  "event_timestamp": 1739537297,
	  "data": {
	    "conversation_id": "conv_sysvars",
	    "status": "done",
	    "transcript": [{"role": "user", "message": "Good week actually.", "time_in_call_secs": 4}],
	    "metadata": {
	      "start_time_unix_secs": 1739537297,
	      "call_duration_secs": 61,
	      "phone_call": {"direction": "outbound", "external_number": %q, "call_sid": "CAsys"}
	    },
	    "analysis": {"transcript_summary": "Good week."},
	    "conversation_initiation_client_data": {
	      "dynamic_variables": {
	        "user_name": "Nadia",
	        "system__call_duration_secs": 61,
	        "system__is_agent_transfer": false,
	        "system__conversation_id": "conv_sysvars"
	      }
	    }
	  }
	}`, u.Phone)

	rec := postTranscript(t, srv, "whsec_test", body, time.Now())
	if rec.Code != http.StatusOK {
		t.Fatalf("a genuine delivery was refused: %d %s", rec.Code, rec.Body.String())
	}
	if _, total, _ := store.Transcripts(u.ID, "", 20, 0); total != 1 {
		t.Fatalf("want the call stored, got %d", total)
	}
}

// A call made before the number signed up still belongs to whoever proves that
// number, and shows up on their dashboard the moment they do.
func TestEarlierCallsAreAdoptedWhenTheNumberVerifies(t *testing.T) {
	srv, store := webhookServer(t)
	phone := "+447700900022"
	if rec := postTranscript(t, srv, "whsec_test", transcriptPayload("conv_before", phone), time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("delivery: %d", rec.Code)
	}

	u, _ := store.EnsureUser(phone)
	// Even unattached, the call is already theirs to read: the number is the
	// same one the session proved.
	if _, total, _ := store.Transcripts(u.ID, "", 20, 0); total != 1 {
		t.Fatalf("call held for this number is not visible to it: %d", total)
	}
	if err := store.MarkPhoneVerified(u.ID, "otp"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	var owner int64
	if err := store.queryRow(`SELECT user_id FROM transcripts WHERE conversation_id=?`, "conv_before").Scan(&owner); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	if owner != u.ID {
		t.Fatalf("verifying the number did not attach the call: owner=%d want %d", owner, u.ID)
	}

	// And it is nobody else's, before or after adoption.
	other, _ := store.EnsureUser("+447700900023")
	if _, total, _ := store.Transcripts(other.ID, "", 20, 0); total != 0 {
		t.Fatalf("another number can see this call: %d", total)
	}
}

// stubConversations stands in for the provider's conversation history.
type stubConversations struct {
	ids   []string
	convs map[string]elevenLabsConversation
	err   error
	fetch int
}

func (s *stubConversations) Conversations(time.Time) ([]string, error) { return s.ids, s.err }

func (s *stubConversations) Conversation(id string) (elevenLabsConversation, error) {
	s.fetch++
	c, ok := s.convs[id]
	if !ok {
		return elevenLabsConversation{}, errors.New("no such conversation")
	}
	return c, nil
}

func conversation(id, phone, said string) elevenLabsConversation {
	var c elevenLabsConversation
	c.ConversationID = id
	c.Status = "done"
	c.Metadata.StartUnix = time.Now().Add(-time.Hour).Unix()
	c.Metadata.DurationSec = 42
	c.Metadata.PhoneCall.ExternalNumber = phone
	c.Metadata.PhoneCall.CallSID = "CA" + id
	c.Analysis.Summary = said
	c.Transcript = append(c.Transcript, struct {
		Role    string  `json:"role"`
		Message string  `json:"message"`
		AtSecs  float64 `json:"time_in_call_secs"`
	}{Role: "user", Message: said, AtSecs: 3})
	return c
}

// The sync is what makes "every call is on the dashboard" true: it reads the
// provider's own record, so calls that predate the webhook, or whose delivery
// was lost, still arrive.
func TestSyncBackfillsCallsTheWebhookNeverDelivered(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
	u, _ := store.EnsureUser("+447700900024")
	api := &stubConversations{
		ids: []string{"conv_old", "conv_older"},
		convs: map[string]elevenLabsConversation{
			"conv_old":   conversation("conv_old", u.Phone, "Talked about the hackathon."),
			"conv_older": conversation("conv_older", u.Phone, "First ever call."),
		},
	}

	if _, err := srv.syncTranscripts(api, time.Time{}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	items, total, err := store.Transcripts(u.ID, "", 20, 0)
	if err != nil || total != 2 {
		t.Fatalf("want both calls backfilled, got %d (%v)", total, err)
	}
	if items[0].Source != transcriptFromSync {
		t.Fatalf("backfilled call not marked as such: %q", items[0].Source)
	}
	if !strings.Contains(items[0].Body, "user:") {
		t.Fatalf("backfilled call has no transcript body: %q", items[0].Body)
	}

	// Running again reconciles rather than duplicating.
	if _, err := srv.syncTranscripts(api, time.Time{}); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if _, total, _ := store.Transcripts(u.ID, "", 20, 0); total != 2 {
		t.Fatalf("re-running the sync duplicated calls: %d", total)
	}
}

// Whichever path sees a call first records it; the other updates the same row.
func TestSyncAndWebhookAgreeOnOneRow(t *testing.T) {
	srv, store := webhookServer(t)
	u, _ := store.EnsureUser("+447700900025")
	api := &stubConversations{
		ids:   []string{"conv_both"},
		convs: map[string]elevenLabsConversation{"conv_both": conversation("conv_both", u.Phone, "Synced copy.")},
	}

	if rec := postTranscript(t, srv, "whsec_test", transcriptPayload("conv_both", u.Phone), time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("delivery: %d", rec.Code)
	}
	if _, err := srv.syncTranscripts(api, time.Time{}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	items, total, _ := store.Transcripts(u.ID, "", 20, 0)
	if total != 1 {
		t.Fatalf("webhook and sync produced %d rows for one call", total)
	}
	if items[0].Summary != "Synced copy." {
		t.Fatalf("the later view of the call did not win: %q", items[0].Summary)
	}
}

// A sync run keeps the calls it managed to read even when the provider fails
// part way through, and never invents a profile for an unknown number.
func TestSyncSurvivesProviderFailures(t *testing.T) {
	srv, store, _, _ := newTestServer(t, nil)
	u, _ := store.EnsureUser("+447700900026")
	api := &stubConversations{
		ids: []string{"conv_ok", "conv_missing", "conv_stranger"},
		convs: map[string]elevenLabsConversation{
			"conv_ok":       conversation("conv_ok", u.Phone, "Fine."),
			"conv_stranger": conversation("conv_stranger", "+447700900927", "Not a member."),
		},
	}
	if _, err := srv.syncTranscripts(api, time.Time{}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, total, _ := store.Transcripts(u.ID, "", 20, 0); total != 1 {
		t.Fatalf("a failed fetch lost the calls around it: %d", total)
	}
	if other, _ := store.UserByPhone("+447700900927"); other != nil {
		t.Fatalf("sync signed up an unknown number")
	}
	var held int
	store.queryRow(`SELECT COUNT(*) FROM transcripts WHERE user_id IS NULL`).Scan(&held)
	if held != 1 {
		t.Fatalf("want the stranger's call held unowned, got %d", held)
	}

	// An unconfigured provider is not an error, just no backfill.
	if n, err := srv.syncTranscripts(nil, time.Time{}); n != 0 || err != nil {
		t.Fatalf("sync without a provider: %d %v", n, err)
	}
}

// Erasing an account takes the calls held against its number with it, attached
// or not.
func TestForgettingTakesUnattachedCallsToo(t *testing.T) {
	srv, store := webhookServer(t)
	phone := "+447700900027"
	if rec := postTranscript(t, srv, "whsec_test", transcriptPayload("conv_unowned", phone), time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("delivery: %d", rec.Code)
	}
	u, _ := store.EnsureUser(phone)
	if err := store.ForgetUser(u.ID); err != nil {
		t.Fatalf("forget: %v", err)
	}
	var left int
	store.queryRow(`SELECT COUNT(*) FROM transcripts WHERE phone=?`, phone).Scan(&left)
	if left != 0 {
		t.Fatalf("erasure left %d call(s) behind", left)
	}
}
