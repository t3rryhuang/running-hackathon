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
	ID             int64
	UserID         int64
	ConversationID string
	CallSID        string
	Direction      string
	Status         string
	Summary        string
	Body           string
	Turns          int
	Duration       time.Duration
	StartedAt      time.Time
	ReceivedAt     time.Time
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
func (s *Store) SaveTranscript(t *Transcript) error {
	if t.UserID == 0 {
		return errors.New("transcript needs an owner")
	}
	if strings.TrimSpace(t.ConversationID) == "" {
		return errors.New("transcript needs a conversation id")
	}
	if t.StartedAt.IsZero() {
		t.StartedAt = time.Now().UTC()
	}
	return s.tx(func(tx *sql.Tx) error {
		res, err := s.txExec(tx, `UPDATE transcripts
			SET user_id=?, call_sid=?, direction=?, status=?, summary=?, body=?, turns=?, duration_seconds=?, started_at=?, received_at=?
			WHERE conversation_id=?`,
			t.UserID, t.CallSID, t.Direction, t.Status, t.Summary, t.Body, t.Turns,
			int64(t.Duration.Seconds()), t.StartedAt.UTC(), time.Now().UTC(), t.ConversationID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			return nil
		}
		id, err := s.txInsert(tx, `INSERT INTO transcripts
			(user_id, conversation_id, call_sid, direction, status, summary, body, turns, duration_seconds, started_at, received_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			t.UserID, t.ConversationID, t.CallSID, t.Direction, t.Status, t.Summary, t.Body, t.Turns,
			int64(t.Duration.Seconds()), t.StartedAt.UTC(), time.Now().UTC())
		if err != nil {
			return err
		}
		t.ID = id
		return nil
	})
}

func scanTranscript(rows interface{ Scan(...any) error }) (Transcript, error) {
	var t Transcript
	var secs int64
	err := rows.Scan(&t.ID, &t.UserID, &t.ConversationID, &t.CallSID, &t.Direction, &t.Status,
		&t.Summary, &t.Body, &t.Turns, &secs, &t.StartedAt, &t.ReceivedAt)
	t.Duration = time.Duration(secs) * time.Second
	return t, err
}

const transcriptColumns = `id, user_id, conversation_id, call_sid, direction, status, summary, body, turns, duration_seconds, started_at, received_at`

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
	where := `WHERE user_id=?`
	args := []any{userID}
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
	row := s.queryRow(`SELECT `+transcriptColumns+` FROM transcripts WHERE id=? AND user_id=?`, id, userID)
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
	res, err := s.exec(`DELETE FROM transcripts WHERE id=? AND user_id=?`, id, userID)
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

// The subset of the post_call_transcription payload this service reads.
type elevenLabsWebhook struct {
	Type string `json:"type"`
	Data struct {
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
			DynamicVariables map[string]string `json:"dynamic_variables"`
		} `json:"conversation_initiation_client_data"`
	} `json:"data"`
}

// phone is the number the call was placed to. The dynamic variables are only a
// fallback: they are whatever this service sent when it dialled, so they are
// as trustworthy as the call record itself, but the provider's own view of the
// external number is preferred.
func (w elevenLabsWebhook) phone() string {
	if n := strings.TrimSpace(w.Data.Metadata.PhoneCall.ExternalNumber); n != "" {
		return n
	}
	return strings.TrimSpace(w.Data.ClientData.DynamicVariables["user_phone"])
}

func (w elevenLabsWebhook) transcript() (body string, turns int) {
	var b strings.Builder
	for _, line := range w.Data.Transcript {
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
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if err := verifyElevenLabsSignature(s.cfg.ElevenLabsSecret, r.Header.Get("elevenlabs-signature"), body, time.Now()); err != nil {
		log.Printf("elevenlabs webhook: rejected: %v", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var payload elevenLabsWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if payload.Type != "post_call_transcription" {
		// Authentic, but not an event this service handles. Accepting it stops
		// the provider retrying something we will never process.
		writeJSON(w, http.StatusOK, map[string]any{"ignored": payload.Type})
		return
	}
	if payload.Data.ConversationID == "" {
		http.Error(w, "missing conversation id", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
	s.async.Add(1)
	go func() {
		defer s.async.Done()
		s.storeTranscript(payload)
	}()
}

func (s *Server) storeTranscript(payload elevenLabsWebhook) {
	phone := normalisePhone(payload.phone())
	if !e164.MatchString(phone) {
		log.Printf("elevenlabs webhook: %s has no usable number, dropped", payload.Data.ConversationID)
		return
	}
	// Lookup, never create: a transcript is not proof that somebody signed up,
	// and inventing a profile from one would file the call against a stranger.
	u, err := s.store.UserByPhone(phone)
	if err != nil || u == nil {
		log.Printf("elevenlabs webhook: %s matches no user, dropped", payload.Data.ConversationID)
		return
	}
	body, turns := payload.transcript()
	started := time.Now().UTC()
	if payload.Data.Metadata.StartUnix > 0 {
		started = time.Unix(payload.Data.Metadata.StartUnix, 0).UTC()
	}
	t := &Transcript{
		UserID:         u.ID,
		ConversationID: payload.Data.ConversationID,
		CallSID:        payload.Data.Metadata.PhoneCall.CallSID,
		Direction:      payload.Data.Metadata.PhoneCall.Direction,
		Status:         payload.Data.Status,
		Summary:        strings.TrimSpace(payload.Data.Analysis.Summary),
		Body:           body,
		Turns:          turns,
		Duration:       time.Duration(payload.Data.Metadata.DurationSec) * time.Second,
		StartedAt:      started,
	}
	if err := s.store.SaveTranscript(t); err != nil {
		log.Printf("elevenlabs webhook: saving %s: %v", payload.Data.ConversationID, err)
	}
}
