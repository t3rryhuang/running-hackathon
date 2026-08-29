package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// verifyTwilio checks the X-Twilio-Signature on an inbound webhook. Without it
// anyone could POST /sms claiming to be any number, which would make "identify
// the user by their verified phone number" meaningless.
//
// It fails open only when TWILIO_AUTH_TOKEN is unset, which is the documented
// unconfigured/demo mode; with a token set, an unsigned request is rejected.
func (s *Server) verifyTwilio(r *http.Request) bool {
	if s.cfg.TwilioAuthToken == "" {
		return true
	}
	sig := r.Header.Get("X-Twilio-Signature")
	if sig == "" {
		return false
	}
	want := twilioSignature(s.cfg.TwilioAuthToken, s.publicURL(r), r.PostForm)
	return hmac.Equal([]byte(sig), []byte(want))
}

// publicURL rebuilds the URL Twilio signed, which is the public one behind
// Caddy rather than the address the Go server sees.
func (s *Server) publicURL(r *http.Request) string {
	scheme := "https"
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		scheme = fwd
	} else if r.TLS == nil && strings.HasPrefix(r.Host, "localhost") {
		scheme = "http"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}
	return scheme + "://" + host + r.URL.RequestURI()
}

// twilioSignature is Twilio's scheme: the URL followed by every POST parameter
// sorted by name and concatenated, HMAC-SHA1'd with the auth token.
func twilioSignature(token, url string, form map[string][]string) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString(url)
	for _, k := range keys {
		sb.WriteString(k)
		for _, v := range form[k] {
			sb.WriteString(v)
		}
	}
	mac := hmac.New(sha1.New, []byte(token))
	mac.Write([]byte(sb.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// StoreWebhookResponse caches the reply that a webhook delivery produced, so a
// provider retry replays it instead of re-running the side effects.
func (s *Store) StoreWebhookResponse(endpoint, key, response string) error {
	_, err := s.exec(`UPDATE webhook_events SET response=? WHERE endpoint=? AND idempotency_key=?`, response, endpoint, key)
	return err
}

// --- checklist tool webhooks ---

type checklistRequest struct {
	Phone          string `json:"phone"`
	Key            string `json:"key"`
	Status         string `json:"status"`
	Answer         string `json:"answer"`
	Channel        string `json:"channel"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (s *Server) checklistUser(w http.ResponseWriter, r *http.Request) (*User, *Session, checklistRequest, bool) {
	var req checklistRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return nil, nil, req, false
	}
	phone := normalisePhone(req.Phone)
	if !e164.MatchString(phone) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "phone must be E.164"})
		return nil, nil, req, false
	}
	u, err := s.store.EnsureUser(phone)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "user lookup failed"})
		return nil, nil, req, false
	}
	channel := req.Channel
	if channel == "" {
		channel = "call"
	}
	sess, err := s.store.EnsureSession(u, channel)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return nil, nil, req, false
	}
	return u, sess, req, true
}

// toolNextQuestion tells the agent the one question it may ask next. When the
// checklist is settled it returns complete:true and no question, so the agent
// has nothing to improvise from.
func (s *Server) toolNextQuestion(w http.ResponseWriter, r *http.Request) {
	u, sess, _, ok := s.checklistUser(w, r)
	if !ok {
		return
	}
	item, err := s.store.NextChecklistItem(u.ID, sess.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "checklist unavailable"})
		return
	}
	if item == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"complete":             true,
			"session_id":           sess.ID,
			"instruction_to_agent": "The checklist is complete. Do not ask further preference questions and do not act on the answers without asking permission first.",
		})
		return
	}
	_ = s.store.MarkAsked(u.ID, item.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"complete":             false,
		"session_id":           sess.ID,
		"key":                  item.Key,
		"question":             item.Prompt,
		"position":             item.Position + 1,
		"total":                len(checklistTemplate),
		"instruction_to_agent": "Ask this question exactly as written, then wait for their answer. Do not ask anything else and do not answer it for them.",
	})
}

// toolSaveAnswer records the user's actual response to the current question.
// It refuses anything that is not the current item, so the agent cannot skip
// ahead or backfill.
func (s *Server) toolSaveAnswer(w http.ResponseWriter, r *http.Request) {
	u, sess, req, ok := s.checklistUser(w, r)
	if !ok {
		return
	}
	if req.IdempotencyKey != "" {
		first, cached, err := s.store.RememberWebhook("/tools/save_answer", req.IdempotencyKey, u.ID, "")
		if err == nil && !first {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusOK)
			if cached == "" {
				cached = `{"ok":true,"replayed":true}`
			}
			_, _ = w.Write([]byte(cached))
			return
		}
	}
	status := req.Status
	if status == "" {
		status = StatusAnswered
	}
	item, err := s.store.RecordChecklistAnswer(u, sess.ID, req.Key, status, req.Answer)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errNotCurrentItem) {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	next, _ := s.store.NextChecklistItem(u.ID, sess.ID)
	out := map[string]any{"ok": true, "saved": map[string]any{"key": item.Key, "status": item.Status, "answer": item.Answer}}
	if next == nil {
		out["complete"] = true
		out["instruction_to_agent"] = "That was the last question. Ask whether they want to be notified before you promise anything."
	} else {
		_ = s.store.MarkAsked(u.ID, next.ID)
		out["next"] = map[string]any{"key": next.Key, "question": next.Prompt, "position": next.Position + 1}
		out["instruction_to_agent"] = "Ask the next question exactly as written and wait for the answer."
	}
	if req.IdempotencyKey != "" {
		_ = s.store.StoreWebhookResponse("/tools/save_answer", req.IdempotencyKey, jsonStr(out))
	}
	writeJSON(w, http.StatusOK, out)
}

// --- consent, ingestion, erasure ---

type consentRequest struct {
	Phone   string `json:"phone"`
	Scope   string `json:"scope"`
	Granted *bool  `json:"granted"`
	Source  string `json:"source"`
}

// handleConsent grants or revokes one signal scope for one person. Consent is
// per scope and per user; there is no blanket "share everything".
func (s *Server) handleConsent(w http.ResponseWriter, r *http.Request) {
	var req consentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	u, err := s.store.UserByPhone(normalisePhone(req.Phone))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown user"})
		return
	}
	if req.Granted == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "granted must be true or false"})
		return
	}
	source := req.Source
	if source == "" {
		source = "operator"
	}
	if err := s.store.SetConsent(u.ID, req.Scope, *req.Granted, source); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("consent: user %d scope %s granted=%v via %s", u.ID, req.Scope, *req.Granted, source)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scope": req.Scope, "granted": *req.Granted})
}

type ingestRequest struct {
	Phone          string `json:"phone"`
	Kind           string `json:"kind"`
	Value          string `json:"value"`
	Unit           string `json:"unit"`
	Source         string `json:"source"`
	ObservedAt     string `json:"observed_at"`
	IdempotencyKey string `json:"idempotency_key"`
}

// handleIngest takes one observation from an authorised source. Every field
// that makes the value trustworthy - who observed it and when - is required,
// and consent is checked before anything is written.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	var req ingestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	u, err := s.store.UserByPhone(normalisePhone(req.Phone))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown user"})
		return
	}
	observed, err := time.Parse(time.RFC3339, req.ObservedAt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "observed_at must be RFC3339"})
		return
	}
	if req.IdempotencyKey != "" {
		first, _, err := s.store.RememberWebhook("/ingest", req.IdempotencyKey, u.ID, "")
		if err == nil && !first {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "replayed": true})
			return
		}
	}
	sig, err := s.store.IngestSignal(u.ID, Signal{
		Kind: req.Kind, Value: req.Value, Unit: req.Unit,
		Source: req.Source, ObservedAt: observed,
	})
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errNoConsent) {
			code = http.StatusForbidden
		}
		writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	log.Printf("ingest: user %d kind %s from %s observed %s", u.ID, sig.Kind, sig.Source, sig.ObservedAt.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"kind":        sig.Kind,
		"observed_at": sig.ObservedAt.Format(time.RFC3339),
		"expires_at":  sig.ExpiresAt.Format(time.RFC3339),
	})
}

// handleSignals returns what the service currently knows, with unknowns spelled
// out. It is the same view the agent gets, so an operator can see exactly what
// the model was told.
func (s *Server) handleSignals(w http.ResponseWriter, r *http.Request) {
	fields := requestFields(r)
	u, err := s.store.UserByPhone(normalisePhone(fields["phone"]))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown user"})
		return
	}
	out := []map[string]any{}
	for _, reading := range s.store.Readings(u.ID, time.Now()) {
		row := map[string]any{"kind": reading.Kind, "known": reading.Known}
		if reading.Known {
			row["value"] = reading.Value
			row["unit"] = reading.Unit
			row["source"] = reading.Source
			row["observed_at"] = reading.AsOf.Format(time.RFC3339)
		} else {
			row["unknown_because"] = reading.Unknown
		}
		out = append(out, row)
	}
	consents := []map[string]any{}
	list, _ := s.store.Consents(u.ID)
	for _, c := range list {
		consents = append(consents, map[string]any{"scope": c.Scope, "active": c.Active(), "source": c.Source})
	}
	writeJSON(w, http.StatusOK, map[string]any{"signals": out, "consents": consents})
}

// handleForget deletes everything held about a person.
func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	fields := requestFields(r)
	u, err := s.store.UserByPhone(normalisePhone(fields["phone"]))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown user"})
		return
	}
	if err := s.store.ForgetUser(u.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	log.Printf("forget: erased user %d", u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "erased": true})
}
