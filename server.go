package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed events.csv
var eventsCSV []byte

var tmpl = template.Must(template.ParseFS(templateFS, "templates/*.html"))

var e164 = regexp.MustCompile(`^\+[1-9]\d{7,14}$`)

// Server wires the HTTP surface to the store, brain and telephony.
type Server struct {
	cfg   Config
	store *Store
	brain *Brain
	tel   Telephony
	cal   *Calendar
}

func NewServer(cfg Config, store *Store, brain *Brain, tel Telephony, cal *Calendar) *Server {
	return &Server{cfg: cfg, store: store, brain: brain, tel: tel, cal: cal}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/signup", s.handleSignup)
	mux.HandleFunc("/sms", s.handleSMS)
	mux.HandleFunc("/journal", s.handleJournal)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/trigger", s.handleTrigger)
	mux.HandleFunc("/tools/get_context", s.toolAuth(s.toolGetContext))
	mux.HandleFunc("/tools/save_checkin", s.toolAuth(s.toolSaveCheckin))
	mux.HandleFunc("/tools/suggest_event", s.toolAuth(s.toolSuggestEvent))
	mux.HandleFunc("/tools/accept_suggestion", s.toolAuth(s.toolAcceptSuggestion))
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "text/plain")
	fmt.Fprint(w, "ok")
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
	phone := normalisePhone(r.FormValue("phone"))
	channel := r.FormValue("channel")
	frequency := r.FormValue("frequency")

	if !e164.MatchString(phone) {
		s.renderStatus(w, http.StatusBadRequest, "index.html", map[string]any{
			"Error": "Phone must be in E.164 format, e.g. +447700900123",
			"Form":  r.Form,
		})
		return
	}
	if channel != "call" && channel != "sms" {
		channel = "sms"
	}
	switch frequency {
	case "daily", "twice-daily", "weekdays":
	default:
		frequency = "daily"
	}

	u := &User{
		Phone:     phone,
		Name:      strings.TrimSpace(r.FormValue("name")),
		Channel:   channel,
		Frequency: frequency,
		ICSURL:    strings.TrimSpace(r.FormValue("ics_url")),
	}
	if err := s.store.UpsertUser(u); err != nil {
		log.Printf("signup: %v", err)
		http.Error(w, "could not save signup", http.StatusInternalServerError)
		return
	}

	if channel == "sms" {
		body := "Hey, I'm CheckIn - your daily journalling companion. Reply to this message to start your first check-in."
		if err := s.tel.SendSMS(phone, body); err != nil {
			log.Printf("signup: welcome sms failed: %v", err)
		} else {
			_ = s.store.AddMessage(u.ID, "assistant", body)
		}
	}

	s.render(w, "confirm.html", map[string]any{
		"Phone":     phone,
		"Channel":   channel,
		"Frequency": frequency,
	})
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

	u, err := s.store.EnsureUser(from)
	if err != nil {
		log.Printf("sms: ensure user: %v", err)
		writeTwiML(w, fallbackReply)
		return
	}
	if body == "" {
		body = "(empty message)"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	writeTwiML(w, s.brain.Reply(ctx, u, body))
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
func (s *Server) TriggerCheckin(u *User) error {
	defer func() { _ = s.store.MarkTriggered(u.ID, time.Now()) }()
	if u.Channel == "call" {
		return s.tel.StartCall(u.Phone)
	}
	body := s.openingMessage(u)
	if err := s.tel.SendSMS(u.Phone, body); err != nil {
		return err
	}
	return s.store.AddMessage(u.ID, "assistant", body)
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
	Phone   string `json:"phone"`
	Mood    *int   `json:"mood"`
	Summary string `json:"summary"`
	Topics  string `json:"topics"`
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

func (s *Server) toolGetContext(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.toolUser(w, r)
	if !ok {
		return
	}
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
		"name":            u.Name,
		"last_checkins":   out,
		"todays_calendar": calendar,
		"open_suggestion": open,
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
	ev, err := s.brain.SuggestEvent(u)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no events available"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": map[string]any{
		"title":     ev.Title,
		"starts_at": ev.StartsAt.In(londonLoc).Format(time.RFC3339),
		"url":       ev.URL,
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
