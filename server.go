package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed events_live.csv
var eventsCSV []byte

//go:embed static
var staticFS embed.FS

var tmpl = template.Must(template.ParseFS(templateFS, "templates/*.html"))

var e164 = regexp.MustCompile(`^\+[1-9]\d{7,14}$`)

// Server wires the HTTP surface to the store, brain and telephony.
type Server struct {
	cfg   Config
	store *Store
	brain *Brain
	tel   Telephony
	voice Voice
	cal   *Calendar
	// hangupGrace is how long the agent gets to say goodbye and hang up itself
	// after onboarding completes, before the service ends the call over Twilio.
	hangupGrace time.Duration
	// afterFunc is time.AfterFunc in production; tests swap it for an
	// immediate call so the backstop is observable without sleeping.
	afterFunc func(time.Duration, func()) *time.Timer
}

// hangupGrace gives the agent a few seconds to deliver its farewell line before
// the Twilio backstop cuts the call.
const defaultHangupGrace = 8 * time.Second

func NewServer(cfg Config, store *Store, brain *Brain, tel Telephony, voice Voice, cal *Calendar) *Server {
	return &Server{
		cfg: cfg, store: store, brain: brain, tel: tel, voice: voice, cal: cal,
		hangupGrace: defaultHangupGrace,
		afterFunc:   time.AfterFunc,
	}
}

// staticHandler serves the embedded brand assets. They are content-stable for a
// given build, so they are cached hard rather than revalidated on every step of
// the wizard.
func staticHandler() http.Handler {
	files := http.FileServer(http.FS(staticFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.Handle("/static/", staticHandler())
	mux.HandleFunc("/signup", s.handleSignup)
	mux.HandleFunc("/call", timed("http.call", s.handleCall))
	mux.HandleFunc("/settings", s.handleSettings)
	mux.HandleFunc("/sms", timed("http.sms", s.handleSMS))
	mux.HandleFunc("/journal", s.handleJournal)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/version", s.handleVersion)
	mux.HandleFunc("/trigger", s.handleTrigger)
	// Tool webhooks land mid-conversation: their server time is dead air the
	// caller hears, so each one is measured separately.
	mux.HandleFunc("/tools/get_context", timed("tool.get_context", s.toolAuth(s.toolGetContext)))
	mux.HandleFunc("/tools/save_onboarding", timed("tool.save_onboarding", s.toolAuth(s.toolSaveOnboarding)))
	mux.HandleFunc("/tools/save_checkin", timed("tool.save_checkin", s.toolAuth(s.toolSaveCheckin)))
	mux.HandleFunc("/tools/suggest_event", timed("tool.suggest_event", s.toolAuth(s.toolSuggestEvent)))
	mux.HandleFunc("/tools/accept_suggestion", timed("tool.accept_suggestion", s.toolAuth(s.toolAcceptSuggestion)))
	// Checklist interview: the agent asks what next_question returns, verbatim,
	// and reports what it heard back through save_answer.
	mux.HandleFunc("/tools/next_question", timed("tool.next_question", s.toolAuth(s.toolNextQuestion)))
	mux.HandleFunc("/tools/save_answer", timed("tool.save_answer", s.toolAuth(s.toolSaveAnswer)))
	// Structured ingestion, consent and erasure.
	mux.HandleFunc("/consent", timed("http.consent", s.toolAuth(s.handleConsent)))
	mux.HandleFunc("/ingest", timed("http.ingest", s.toolAuth(s.handleIngest)))
	mux.HandleFunc("/signals", timed("http.signals", s.toolAuth(s.handleSignals)))
	mux.HandleFunc("/forget", timed("http.forget", s.toolAuth(s.handleForget)))
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "text/plain")
	fmt.Fprint(w, "ok")
}

// handleVersion reports which commit is actually running, so a deploy can be
// verified and drift between the repo and the Pi can be detected from outside
// the box. No auth: it contains nothing sensitive.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, buildInfo(s.cfg))
}

// handleMetrics exposes the in-process latency summary. No auth: it is timings
// only, no user data, and the operator needs it from the Pi during a demo.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ops": metrics.Stats()})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.render(w, "index.html", map[string]any{})
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// The web form exists only to reach the person: a number and how they want
	// to be contacted. Everything about them - their name, what they are into,
	// how often to check in - is asked in the conversation itself, so nothing
	// here is a substitute for them telling us.
	phone := normalisePhone(r.FormValue("phone"))
	channel := r.FormValue("channel")

	if !e164.MatchString(phone) {
		if wantsJSON(r) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Phone must be in E.164 format, e.g. +447700900123"})
			return
		}
		s.renderStatus(w, http.StatusBadRequest, "index.html", map[string]any{
			"Error": "Phone must be in E.164 format, e.g. +447700900123",
			"Form":  r.Form,
		})
		return
	}
	if channel != "call" && channel != "sms" {
		channel = "sms"
	}

	u, err := s.store.EnsureUser(phone)
	if err == nil {
		u.Channel = channel
		if ics := strings.TrimSpace(r.FormValue("ics_url")); ics != "" {
			u.ICSURL = ics
		}
		err = s.store.UpsertUser(u)
	}
	if err != nil {
		log.Printf("signup: %v", err)
		if wantsJSON(r) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "could not save signup"})
			return
		}
		http.Error(w, "could not save signup", http.StatusInternalServerError)
		return
	}

	if channel == "sms" {
		if err := s.startSMSOnboarding(u); err != nil {
			log.Printf("signup: opening sms failed: %v", err)
		}
	}

	if wantsJSON(r) {
		// The wizard hands over to the conversation once the number is
		// stored, so it only needs the record back rather than a page.
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"phone":     phone,
			"name":      u.Name,
			"channel":   channel,
			"onboarded": u.Onboarded(),
			"journal":   "/journal?phone=" + url.QueryEscape(phone),
		})
		return
	}
	s.render(w, "confirm.html", map[string]any{
		"Phone":     phone,
		"Channel":   channel,
		"Frequency": u.Frequency,
	})
}

// handleCall rings the user through the ElevenLabs agent. The wizard uses it for
// the "Call me now" onboarding interview; /trigger uses it for check-ins.
func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	fields := requestFields(r)
	phone := normalisePhone(fields["phone"])
	if !e164.MatchString(phone) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "phone must be E.164, e.g. +447700900123"})
		return
	}
	u, err := s.store.EnsureUser(phone)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "user lookup failed"})
		return
	}
	if err := s.placeCall(u); err != nil {
		log.Printf("call: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "could not place the call"})
		return
	}
	_ = s.store.MarkTriggered(u.ID, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "phone": u.Phone, "name": u.Name, "onboarded": u.Onboarded()})
}

// handleSettings changes the check-in frequency from the journal control panel.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	fields := requestFields(r)
	if !validFrequency(fields["frequency"]) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "frequency must be daily, twice-daily or weekdays"})
		return
	}
	u, err := s.store.UserByPhone(normalisePhone(fields["phone"]))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown user"})
		return
	}
	if err := s.store.SetFrequency(u.ID, fields["frequency"]); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "could not save"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "frequency": fields["frequency"]})
}

// handleSMS is the Twilio inbound SMS webhook. It always answers with TwiML,
// even when the model is unavailable.
func (s *Server) handleSMS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	from := normalisePhone(r.FormValue("From"))
	body := strings.TrimSpace(r.FormValue("Body"))
	if from == "" {
		writeTwiML(w, "Sorry, I couldn't work out who this is from.")
		return
	}

	if !s.verifyTwilio(r) {
		log.Printf("sms: rejected an inbound with a bad Twilio signature")
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}

	u, err := s.store.EnsureUser(from)
	if err != nil {
		log.Printf("sms: ensure user: %v", err)
		writeTwiML(w, fallbackReply)
		return
	}
	// A signed inbound message from this number is proof the person holds it.
	if s.cfg.TwilioAuthToken != "" && !u.PhoneVerified() {
		if err := s.store.MarkPhoneVerified(u.ID, "twilio_inbound_sms"); err == nil {
			u, _ = s.store.UserByPhone(from)
		}
	}
	// Twilio retries a webhook it thinks failed; MessageSid makes the retry
	// replay the same answer instead of taking another conversation turn.
	sid := r.FormValue("MessageSid")
	if sid != "" {
		first, cached, err := s.store.RememberWebhook("/sms", sid, u.ID, "")
		if err == nil && !first {
			writeTwiML(w, cached)
			return
		}
	}
	// An empty body stays empty: the checklist must see that nothing was said
	// rather than a placeholder it could mistake for an answer.
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	reply := s.brain.Reply(ctx, u, body)
	if sid != "" {
		_ = s.store.StoreWebhookResponse("/sms", sid, reply)
	}
	writeTwiML(w, reply)
}

type twiMLMessage struct {
	XMLName xml.Name `xml:"Response"`
	Message string   `xml:"Message"`
}

func writeTwiML(w http.ResponseWriter, body string) {
	w.Header().Set("content-type", "application/xml")
	out, err := xml.Marshal(twiMLMessage{Message: body})
	if err != nil {
		http.Error(w, "twiml error", http.StatusInternalServerError)
		return
	}
	fmt.Fprint(w, xml.Header)
	w.Write(out)
}

func (s *Server) handleJournal(w http.ResponseWriter, r *http.Request) {
	phone := normalisePhone(r.URL.Query().Get("phone"))
	if phone == "" {
		s.renderStatus(w, http.StatusBadRequest, "journal.html", map[string]any{"Error": "Pass ?phone=+44..."})
		return
	}
	u, err := s.store.UserByPhone(phone)
	if err != nil {
		s.renderStatus(w, http.StatusNotFound, "journal.html", map[string]any{"Error": "No journal yet for " + phone})
		return
	}
	checkins, _ := s.store.RecentCheckins(u.ID, 200)
	accepted, _ := s.store.AcceptedSuggestions(u.ID)

	type row struct {
		When    string
		Mood    string
		Summary string
		Topics  string
	}
	var rows []row
	for _, c := range checkins {
		mood := "-"
		if c.Mood != nil {
			mood = strings.Repeat("*", *c.Mood)
		}
		rows = append(rows, row{
			When:    c.CreatedAt.In(londonLoc).Format("Mon 2 Jan 2006, 15:04"),
			Mood:    mood,
			Summary: c.Summary,
			Topics:  c.Topics,
		})
	}
	type ev struct{ Title, When, URL string }
	var events []ev
	for _, a := range accepted {
		events = append(events, ev{
			Title: a.Event.Title,
			When:  a.Event.StartsAt.In(londonLoc).Format("Mon 2 Jan 2006, 15:04"),
			URL:   a.Event.URL,
		})
	}
	s.render(w, "journal.html", map[string]any{
		"User":     u,
		"Checkins": rows,
		"Events":   events,
	})
}

// handleTrigger kicks off a check-in on demand (demo button and scheduler).
func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	phone := normalisePhone(r.URL.Query().Get("phone"))
	if phone == "" {
		http.Error(w, "phone required", http.StatusBadRequest)
		return
	}
	u, err := s.store.UserByPhone(phone)
	if err != nil {
		http.Error(w, "unknown user", http.StatusNotFound)
		return
	}
	if err := s.TriggerCheckin(u); err != nil {
		log.Printf("trigger: %v", err)
		http.Error(w, "trigger failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channel": u.Channel, "phone": u.Phone})
}

// TriggerCheckin sends the opening check-in over the user's chosen channel.
// Somebody who has not been introduced yet gets the introduction instead: a
// check-in question would assume a relationship that does not exist.
func (s *Server) TriggerCheckin(u *User) error {
	defer func() { _ = s.store.MarkTriggered(u.ID, time.Now()) }()
	if u.Channel == "call" {
		return s.placeCall(u)
	}
	if !u.Onboarded() {
		return s.startSMSOnboarding(u)
	}
	body := s.openingMessage(u)
	if err := s.tel.SendSMS(u.Phone, body); err != nil {
		return err
	}
	return s.store.AddMessage(u.ID, "assistant", body)
}

// startSMSOnboarding texts the first onboarding question and records that it
// was asked, so the reply is matched to the question they were actually sent.
func (s *Server) startSMSOnboarding(u *User) error {
	body := s.brain.OnboardingTurn(u, "sms", "")
	if err := s.tel.SendSMS(u.Phone, body); err != nil {
		return err
	}
	return s.store.AddMessage(u.ID, "assistant", body)
}

// callerContext resolves what is already on file for this person, so both the
// outbound call variables and /tools/get_context describe the same closed
// world.
func (s *Server) callerContext(u *User) CallerContext {
	var items []ChecklistItem
	if sess, err := s.store.EnsureSession(u, "call", FlowFor(u)); err == nil {
		items, _ = s.store.Checklist(u.ID, sess.ID)
	}
	return buildCallerContext(u.Phone, u, items)
}

// placeCall rings the user and remembers the Twilio call SID ElevenLabs reports
// back, which is what lets the service hang up when onboarding finishes.
func (s *Server) placeCall(u *User) error {
	// The agent asks for context within a second of the caller picking up, so
	// fetch the calendar while the phone is still ringing.
	go s.brain.WarmCalendar(u)
	defer track("call.place")()
	res, err := s.voice.Call(CallRequest{
		To: u.Phone, Name: u.Name, Onboarded: u.Onboarded(),
		Vars: s.callerContext(u).DynamicVariables(),
	})
	if err != nil {
		return err
	}
	u.LastCallSID = res.CallSID
	if err := s.store.SetCallSID(u.ID, res.CallSID); err != nil {
		log.Printf("call: remember sid: %v", err)
	}
	return nil
}

// endCall closes out a live call. The agent's own end_call tool is the primary
// mechanism - the ask in the tool response below triggers it - and this Twilio
// backstop covers agents that keep the line open after the interview is done.
func (s *Server) endCall(u *User) {
	sid := u.LastCallSID
	if sid == "" {
		return
	}
	s.afterFunc(s.hangupGrace, func() {
		if err := s.tel.HangUp(sid); err != nil {
			log.Printf("hangup %s: %v", sid, err)
		}
		_ = s.store.SetCallSID(u.ID, "")
	})
}

// openingMessage personalises the opening text with today's calendar.
func (s *Server) openingMessage(u *User) string {
	greeting := "Hey"
	if u.Name != "" {
		greeting = "Hey " + u.Name
	}
	events := s.brain.CalendarFor(u)
	if len(events) > 0 {
		e := events[0]
		return fmt.Sprintf("%s - how's your day going? I see you had %s at %s, how did that go?", greeting, e.Summary, e.When)
	}
	return greeting + " - how's your day going? Anything worth writing down?"
}

// --- ElevenLabs agent tool webhooks ---

func (s *Server) toolAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		got := r.Header.Get("X-Webhook-Secret")
		want := s.cfg.ToolWebhookSecret
		if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

type toolRequest struct {
	Phone     string `json:"phone"`
	Mood      *int   `json:"mood"`
	Summary   string `json:"summary"`
	Topics    string `json:"topics"`
	Name      string `json:"name"`
	Interests string `json:"interests"`
	Frequency string `json:"frequency"`
}

// toolUser decodes the request body and resolves the caller's user record.
func (s *Server) toolUser(w http.ResponseWriter, r *http.Request) (*User, toolRequest, bool) {
	var req toolRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return nil, req, false
	}
	phone := normalisePhone(req.Phone)
	if phone == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "phone required"})
		return nil, req, false
	}
	u, err := s.store.EnsureUser(phone)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "user lookup failed"})
		return nil, req, false
	}
	return u, req, true
}

// toolLookupUser resolves the caller strictly by phone number and does not
// create anything. An unrecognised number returns a nil user, which the caller
// must report as unresolved rather than fill in.
func (s *Server) toolLookupUser(w http.ResponseWriter, r *http.Request) (*User, toolRequest, bool) {
	var req toolRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return nil, req, false
	}
	phone := normalisePhone(req.Phone)
	if phone == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "phone required"})
		return nil, req, false
	}
	u, err := s.store.UserByPhone(phone)
	if err != nil {
		return nil, req, true
	}
	return u, req, true
}

func (s *Server) toolGetContext(w http.ResponseWriter, r *http.Request) {
	u, req, ok := s.toolLookupUser(w, r)
	if !ok {
		return
	}
	// A number with no profile behind it is reported as unresolved. Nothing is
	// invented, and no other user's record is ever offered in its place.
	if u == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"caller":          buildCallerContext(normalisePhone(req.Phone), nil, nil),
			"name":            "",
			"onboarded":       false,
			"interests":       "",
			"frequency":       "",
			"last_checkins":   []map[string]any{},
			"todays_calendar": []map[string]any{},
			"open_suggestion": nil,
		})
		return
	}
	caller := s.callerContext(u)
	checkins, _ := s.store.RecentCheckins(u.ID, 10)
	out := []map[string]any{}
	for _, c := range checkins {
		out = append(out, map[string]any{
			"created_at": c.CreatedAt.In(londonLoc).Format(time.RFC3339),
			"mood":       c.Mood,
			"summary":    c.Summary,
			"topics":     c.Topics,
		})
	}
	calendar := []map[string]any{}
	for _, e := range s.brain.CalendarFor(u) {
		calendar = append(calendar, map[string]any{"summary": e.Summary, "when": e.When, "all_day": e.AllDay})
	}
	var open any
	if sg, err := s.store.OpenSuggestion(u.ID); err == nil {
		open = map[string]any{
			"title":     sg.Event.Title,
			"starts_at": sg.Event.StartsAt.In(londonLoc).Format(time.RFC3339),
			"url":       sg.Event.URL,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"caller":          caller,
		"name":            u.Name,
		"onboarded":       u.Onboarded(),
		"interests":       u.Interests,
		"frequency":       u.Frequency,
		"last_checkins":   out,
		"todays_calendar": calendar,
		"open_suggestion": open,
	})
}

// toolSaveOnboarding stores what the voice agent learned in its intro interview.
func (s *Server) toolSaveOnboarding(w http.ResponseWriter, r *http.Request) {
	u, req, ok := s.toolUser(w, r)
	if !ok {
		return
	}
	if err := s.store.SaveOnboarding(u, req.Name, req.Interests, req.Frequency); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// The interview is the whole point of the onboarding call, so finishing it
	// ends the call: the agent is told to invoke end_call, and endCall hangs the
	// line up over Twilio if it doesn't.
	s.endCall(u)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                   true,
		"name":                 u.Name,
		"interests":            u.Interests,
		"frequency":            u.Frequency,
		"onboarding_complete":  true,
		"end_call":             true,
		"instruction_to_agent": "Onboarding is complete. Say a short goodbye and call the end_call tool now - do not ask another question.",
	})
}

func (s *Server) toolSaveCheckin(w http.ResponseWriter, r *http.Request) {
	u, req, ok := s.toolUser(w, r)
	if !ok {
		return
	}
	if err := s.brain.SaveCheckin(u, req.Mood, req.Summary, req.Topics, jsonStr(req)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) toolSuggestEvent(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.toolUser(w, r)
	if !ok {
		return
	}
	match, err := s.brain.SuggestEvent(u)
	if err != nil {
		if errors.Is(err, ErrNoMatch) {
			// Explicitly not an error the agent should paper over: it is told
			// what to say instead of being left to improvise an event.
			writeJSON(w, http.StatusOK, map[string]any{
				"event":                nil,
				"no_match":             true,
				"say":                  noMatchLine(u),
				"instruction_to_agent": "There is no suitable event. Say so honestly and ask what they would want to go to. Do not invent an event.",
			})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no events available"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"why": match.Why(), "event": map[string]any{
		"title":     match.Event.Title,
		"starts_at": match.Event.StartsAt.In(londonLoc).Format(time.RFC3339),
		"city":      match.Event.City,
		"url":       match.Event.URL,
	}})
}

func (s *Server) toolAcceptSuggestion(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.toolUser(w, r)
	if !ok {
		return
	}
	confirmation, err := s.brain.AcceptSuggestion(u)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "no open suggestion"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "confirmation": confirmation})
}

// --- helpers ---

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	s.renderStatus(w, http.StatusOK, name, data)
}

func (s *Server) renderStatus(w http.ResponseWriter, status int, name string, data map[string]any) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// wantsJSON reports whether the caller (the onboarding wizard) asked for JSON
// rather than a rendered page.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// requestFields accepts JSON, a form post or the query string for the small
// {phone, frequency} bodies, sniffing the payload rather than trusting the
// content-type so plain `curl -d '{...}'` works too.
func requestFields(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return out
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '{' {
		var fields map[string]any
		if err := json.Unmarshal(trimmed, &fields); err == nil {
			for k, v := range fields {
				if s, ok := v.(string); ok {
					out[k] = s
				}
			}
		}
		return out
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return out
	}
	for k, v := range form {
		if len(v) > 0 && v[0] != "" {
			out[k] = v[0]
		}
	}
	return out
}

// normalisePhone strips spaces and common punctuation and converts a leading
// 00 international prefix to +.
func normalisePhone(p string) string {
	p = strings.TrimSpace(p)
	r := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", ".", "")
	p = r.Replace(p)
	if strings.HasPrefix(p, "00") {
		p = "+" + p[2:]
	}
	return p
}
