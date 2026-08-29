package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Transcripts are what the person actually said, so they are held to the same
// rules as the rest of the profile: one owner, resolved from the number that
// was dialled, and never shown to anybody else.
type Transcript struct {
	ID int64
	// UserID is zero while the call belongs to a number that has not signed up
	// yet. The row is kept anyway and adopted when that number verifies, so a
	// call placed before onboarding is not lost.
	UserID         int64
	Phone          string
	ConversationID string
	CallSID        string
	Direction      string
	Status         string
	// Source is how the row arrived: the provider's webhook, or the sync that
	// reads the provider's own list of conversations.
	Source     string
	Summary    string
	Body       string
	Turns      int
	Duration   time.Duration
	StartedAt  time.Time
	ReceivedAt time.Time
}

// transcriptRetention is how long a call transcript is kept before the
// retention sweep removes it. Ninety days is long enough to look back over a
// season of check-ins and short enough that nothing accumulates forever.
const transcriptRetention = 90 * 24 * time.Hour

// elevenLabsSignatureTolerance is how far a delivery's timestamp may be from
// our clock. ElevenLabs signs the timestamp along with the body, so a narrow
// window stops a captured request being replayed later.
const elevenLabsSignatureTolerance = 30 * time.Minute

// SaveTranscript writes a delivered transcript, keyed on the provider's
// conversation id so a retried delivery overwrites rather than duplicates.
// Whichever of the webhook and the sync arrives first creates the row and the
// other updates it, so the two paths can never produce two copies of one call.
func (s *Store) SaveTranscript(t *Transcript) error {
	if t.UserID == 0 && strings.TrimSpace(t.Phone) == "" {
		return errors.New("transcript needs an owner or a number")
	}
	if strings.TrimSpace(t.ConversationID) == "" {
		return errors.New("transcript needs a conversation id")
	}
	if t.StartedAt.IsZero() {
		t.StartedAt = time.Now().UTC()
	}
	if t.Source == "" {
		t.Source = transcriptFromWebhook
	}
	owner := nullableID(t.UserID)
	return s.tx(func(tx *sql.Tx) error {
		// COALESCE on the owner keeps an already-adopted row attached: a later
		// delivery for a number that has since signed up must not orphan it.
		res, err := s.txExec(tx, `UPDATE transcripts
			SET user_id=COALESCE(?, user_id), phone=?, call_sid=?, direction=?, status=?, source=?,
				summary=?, body=?, turns=?, duration_seconds=?, started_at=?, received_at=?
			WHERE conversation_id=?`,
			owner, t.Phone, t.CallSID, t.Direction, t.Status, t.Source, t.Summary, t.Body, t.Turns,
			int64(t.Duration.Seconds()), t.StartedAt.UTC(), time.Now().UTC(), t.ConversationID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			return nil
		}
		id, err := s.txInsert(tx, `INSERT INTO transcripts
			(user_id, phone, conversation_id, call_sid, direction, status, source, summary, body, turns, duration_seconds, started_at, received_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			owner, t.Phone, t.ConversationID, t.CallSID, t.Direction, t.Status, t.Source, t.Summary, t.Body, t.Turns,
			int64(t.Duration.Seconds()), t.StartedAt.UTC(), time.Now().UTC())
		if err != nil {
			return err
		}
		t.ID = id
		return nil
	})
}

// nullableID renders "nobody owns this yet" as SQL NULL rather than user 0,
// which no foreign key would accept.
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// AdoptTranscripts attaches the calls already held for a number to the profile
// that has just proved it owns that number.
func (s *Store) AdoptTranscripts(userID int64) (int64, error) {
	res, err := s.exec(`UPDATE transcripts SET user_id=?
		WHERE user_id IS NULL AND phone = (SELECT phone FROM users WHERE id=?)`, userID, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanTranscript(rows interface{ Scan(...any) error }) (Transcript, error) {
	var t Transcript
	var secs int64
	var owner sql.NullInt64
	err := rows.Scan(&t.ID, &owner, &t.Phone, &t.ConversationID, &t.CallSID, &t.Direction, &t.Status,
		&t.Source, &t.Summary, &t.Body, &t.Turns, &secs, &t.StartedAt, &t.ReceivedAt)
	t.UserID = owner.Int64
	t.Duration = time.Duration(secs) * time.Second
	return t, err
}

const transcriptColumns = `id, user_id, phone, conversation_id, call_sid, direction, status, source, summary, body, turns, duration_seconds, started_at, received_at`

// transcriptOwned matches every call that belongs to one profile: the ones
// already attached to it, and the ones held against its verified number from
// before it signed up. The number comes from the users table, never from the
// browser, so this cannot reach across accounts.
const transcriptOwned = `(user_id=? OR (user_id IS NULL AND phone <> '' AND phone = (SELECT phone FROM users WHERE id=?)))`

// Transcripts lists one user's calls, newest first. query filters on the
// summary and the transcript body; limit and offset paginate. Every query is
// scoped by user id — there is no way to ask this for somebody else's calls.
func (s *Store) Transcripts(userID int64, query string, limit, offset int) ([]Transcript, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	where := `WHERE ` + transcriptOwned
	args := []any{userID, userID}
	if q := strings.TrimSpace(query); q != "" {
		where += ` AND (LOWER(body) LIKE ? OR LOWER(summary) LIKE ?)`
		like := "%" + strings.ToLower(q) + "%"
		args = append(args, like, like)
	}

	var total int
	if err := s.queryRow(`SELECT COUNT(*) FROM transcripts `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.query(`SELECT `+transcriptColumns+` FROM transcripts `+where+
		` ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Transcript
	for rows.Next() {
		t, err := scanTranscript(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// Transcript returns one call, or nothing at all if it belongs to somebody
// else: an id from another account is indistinguishable from an id that does
// not exist.
func (s *Store) Transcript(userID, id int64) (*Transcript, error) {
	row := s.queryRow(`SELECT `+transcriptColumns+` FROM transcripts WHERE id=? AND `+transcriptOwned, id, userID, userID)
	t, err := scanTranscript(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// DeleteTranscript removes one call for its owner.
func (s *Store) DeleteTranscript(userID, id int64) (bool, error) {
	res, err := s.exec(`DELETE FROM transcripts WHERE id=? AND `+transcriptOwned, id, userID, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PurgeExpiredTranscripts drops transcripts past the retention window.
func (s *Store) PurgeExpiredTranscripts(now time.Time) (int64, error) {
	res, err := s.exec(`DELETE FROM transcripts WHERE started_at < ?`, now.Add(-transcriptRetention).UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// elevenLabsConversation is the subset of a conversation this service reads.
// The webhook wraps it in an envelope and the provider's own conversation API
// returns it bare, so both paths decode into this one shape.
type elevenLabsConversation struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Transcript     []struct {
		Role    string  `json:"role"`
		Message string  `json:"message"`
		AtSecs  float64 `json:"time_in_call_secs"`
	} `json:"transcript"`
	Metadata struct {
		StartUnix   int64 `json:"start_time_unix_secs"`
		DurationSec int64 `json:"call_duration_secs"`
		PhoneCall   struct {
			Direction      string `json:"direction"`
			ExternalNumber string `json:"external_number"`
			CallSID        string `json:"call_sid"`
		} `json:"phone_call"`
	} `json:"metadata"`
	Analysis struct {
		Summary string `json:"transcript_summary"`
	} `json:"analysis"`
	ClientData struct {
		// Dynamic variables are not all strings: the provider mixes in its own
		// numeric and boolean system variables. Decoding them as strings threw
		// the whole delivery away, transcript and all, so they are read loosely
		// and converted only for the one key this service looks at.
		DynamicVariables map[string]any `json:"dynamic_variables"`
	} `json:"conversation_initiation_client_data"`
}

// The post_call_transcription envelope.
type elevenLabsWebhook struct {
	Type string                 `json:"type"`
	Data elevenLabsConversation `json:"data"`
}

// phone is the number the call was placed to. The dynamic variables are only a
// fallback: they are whatever this service sent when it dialled, so they are
// as trustworthy as the call record itself, but the provider's own view of the
// external number is preferred.
func (w elevenLabsConversation) phone() string {
	if n := strings.TrimSpace(w.Metadata.PhoneCall.ExternalNumber); n != "" {
		return n
	}
	if v, ok := w.ClientData.DynamicVariables["user_phone"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func (w elevenLabsConversation) transcript() (body string, turns int) {
	var b strings.Builder
	for _, line := range w.Transcript {
		msg := strings.TrimSpace(line.Message)
		if msg == "" {
			continue
		}
		role := line.Role
		if role == "" {
			role = "unknown"
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", formatCallOffset(line.AtSecs), role, msg)
		turns++
	}
	return strings.TrimRight(b.String(), "\n"), turns
}

func formatCallOffset(secs float64) string {
	d := time.Duration(secs) * time.Second
	return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// verifyElevenLabsSignature checks the `elevenlabs-signature` header, which is
// `t=<unix>,v0=<hex hmac-sha256 of "<t>.<raw body>">`. Both halves matter: the
// digest proves the body came from ElevenLabs, and the timestamp stops a
// captured delivery being replayed days later.
func verifyElevenLabsSignature(secret, header string, body []byte, now time.Time) error {
	if secret == "" {
		return errors.New("no webhook secret configured")
	}
	var ts, digest string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v0":
			digest = v
		}
	}
	if ts == "" || digest == "" {
		return errors.New("malformed signature header")
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("malformed signature timestamp")
	}
	if age := now.Sub(time.Unix(secs, 0)); age > elevenLabsSignatureTolerance || age < -elevenLabsSignatureTolerance {
		return errors.New("signature timestamp outside the tolerance window")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(strings.TrimPrefix(digest, "v0="))) {
		return errors.New("signature mismatch")
	}
	return nil
}

// handleElevenLabsWebhook accepts post-call transcripts. It answers as soon as
// the delivery is authentic and well-formed, and files the transcript on a
// separate goroutine: the provider times out fast, and a slow database write
// here shows up as a retry storm rather than an error.
func (s *Server) handleElevenLabsWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		rejectDelivery(w, http.StatusBadRequest, "unreadable body", err.Error(), nil)
		return
	}
	if err := verifyElevenLabsSignature(s.cfg.ElevenLabsSecret, r.Header.Get("elevenlabs-signature"), body, time.Now()); err != nil {
		rejectDelivery(w, http.StatusUnauthorized, "invalid signature", err.Error(), body)
		return
	}
	var payload elevenLabsWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		rejectDelivery(w, http.StatusBadRequest, "invalid json", err.Error(), body)
		return
	}
	if payload.Type == "" && payload.Data.ConversationID == "" {
		// Some deliveries arrive as the conversation itself rather than the
		// envelope. It is signed with our secret and it is a call, so it is
		// filed rather than argued with.
		var bare elevenLabsConversation
		if err := json.Unmarshal(body, &bare); err == nil && bare.ConversationID != "" {
			payload.Type, payload.Data = "post_call_transcription", bare
		}
	}
	if payload.Type != "post_call_transcription" {
		// Authentic, but not an event this service handles. Accepting it stops
		// the provider retrying something we will never process.
		writeJSON(w, http.StatusOK, map[string]any{"ignored": payload.Type})
		return
	}
	if payload.Data.ConversationID == "" {
		rejectDelivery(w, http.StatusBadRequest, "missing conversation id", "", body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
	s.async.Add(1)
	go func() {
		defer s.async.Done()
		s.storeConversation(payload.Data, transcriptFromWebhook)
	}()
}

// deliveryPeekBytes is how much of a rejected body is logged. Retries are
// disabled provider-side, so a refusal is final and has to be diagnosable
// afterwards; the envelope head carries the type and the ids, and stops well
// short of the conversation itself.
const deliveryPeekBytes = 220

// rejectDelivery answers the provider and records why, because a delivery this
// service turns away is a call that will never appear on anybody's dashboard.
func rejectDelivery(w http.ResponseWriter, status int, reason, detail string, body []byte) {
	peek := body
	if len(peek) > deliveryPeekBytes {
		peek = peek[:deliveryPeekBytes]
	}
	log.Printf("elevenlabs webhook: rejected %d %s (%s), %d bytes, head: %s",
		status, reason, detail, len(body), strconv.Quote(string(peek)))
	metrics.RecordErr("http.transcript", reason)
	http.Error(w, reason, status)
}

// How a stored transcript reached us.
const (
	transcriptFromWebhook = "webhook"
	transcriptFromSync    = "sync"
)

// storeConversation files one call. A number nobody has signed up with is kept
// unowned rather than dropped: the call happened, and it is handed to that
// profile the moment the number verifies. Lookup never creates a profile — a
// transcript is not consent to sign somebody up.
func (s *Server) storeConversation(conv elevenLabsConversation, source string) {
	phone := normalisePhone(conv.phone())
	if !e164.MatchString(phone) {
		log.Printf("elevenlabs %s: %s has no usable number, dropped", source, conv.ConversationID)
		return
	}
	var ownerID int64
	if u, err := s.store.UserByPhone(phone); err == nil && u != nil {
		ownerID = u.ID
	}
	body, turns := conv.transcript()
	started := time.Now().UTC()
	if conv.Metadata.StartUnix > 0 {
		started = time.Unix(conv.Metadata.StartUnix, 0).UTC()
	}
	t := &Transcript{
		UserID:         ownerID,
		Phone:          phone,
		ConversationID: conv.ConversationID,
		CallSID:        conv.Metadata.PhoneCall.CallSID,
		Direction:      conv.Metadata.PhoneCall.Direction,
		Status:         conv.Status,
		Source:         source,
		Summary:        strings.TrimSpace(conv.Analysis.Summary),
		Body:           body,
		Turns:          turns,
		Duration:       time.Duration(conv.Metadata.DurationSec) * time.Second,
		StartedAt:      started,
	}
	if err := s.store.SaveTranscript(t); err != nil {
		log.Printf("elevenlabs %s: saving %s: %v", source, conv.ConversationID, err)
	}
	// A transcript is the provider confirming the call is over, however it
	// ended - answered, dropped, failed. That is the definitive signal that
	// frees the dashboard's call button, so it beats waiting for the stale-call
	// timeout. Replays are harmless: clearing an already-clear slot is a no-op.
	if ownerID != 0 {
		if err := s.store.EndCall(ownerID); err != nil {
			log.Printf("elevenlabs %s: clearing call state for %d: %v", source, ownerID, err)
		}
	}
}
