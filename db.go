package main

import (
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"
)

type User struct {
	ID               int64
	Phone            string
	Name             string
	Channel          string
	Frequency        string
	ICSURL           string
	Interests        string
	OnboardedAt      *time.Time
	LastCallSID      string
	LastTriggeredAt  *time.Time
	PhoneVerifiedAt  *time.Time
	PhoneVerifiedVia string
	CreatedAt        time.Time
}

// PhoneVerified reports whether something proved this person holds the number:
// an inbound Twilio SMS with a valid signature, or a tool webhook raised during
// a call the service itself placed to that number.
func (u *User) PhoneVerified() bool { return u.PhoneVerifiedAt != nil }

// DisplayName is the only way a name reaches a prompt or a message. It is empty
// until the person tells us their name, and it is never an identifier.
func (u *User) DisplayName() string { return strings.TrimSpace(u.Name) }

// Onboarded reports whether the voice agent has already interviewed this user.
func (u *User) Onboarded() bool { return u.OnboardedAt != nil }

// InterestList splits the comma-separated interests into trimmed values.
func (u *User) InterestList() []string {
	var out []string
	for _, s := range strings.Split(u.Interests, ",") {
		if s = interestStem(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// interestStem reduces a spoken interest ("Hackathons") to something that
// matches the export's tag vocabulary ("non_uni_hackathon", "meetups").
func interestStem(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(s, ".!?")
	if len(s) > 4 && strings.HasSuffix(s, "s") {
		s = strings.TrimSuffix(s, "s")
	}
	return s
}

type Checkin struct {
	ID        int64
	UserID    int64
	Mood      *int
	Summary   string
	Topics    string
	Raw       string
	CreatedAt time.Time
}

type Event struct {
	ID       int64
	Title    string
	StartsAt time.Time
	City     string
	URL      string
	Tags     string
}

type Suggestion struct {
	ID        int64
	UserID    int64
	EventID   int64
	Status    string
	CreatedAt time.Time
	Event     Event
}

const userSelect = `SELECT id, phone, name, channel, frequency, COALESCE(ics_url,''), COALESCE(interests,''), onboarded_at, COALESCE(last_call_sid,''), last_triggered_at, phone_verified_at, COALESCE(phone_verified_via,''), created_at FROM users`

type scanner interface{ Scan(dest ...any) error }

// UserByPhone is the only lookup that resolves a person. There is deliberately
// no lookup by name: two people can share a name, nobody shares a number.
func (s *Store) UserByPhone(phone string) (*User, error) {
	return scanUser(s.queryRow(userSelect+` WHERE phone = ?`, phone))
}

// UserByID reloads a person the service has already resolved, for paths that
// hold an id from a session rather than a number from a webhook.
func (s *Store) UserByID(id int64) (*User, error) {
	return scanUser(s.queryRow(userSelect+` WHERE id = ?`, id))
}

func scanUser(row scanner) (*User, error) {
	var u User
	var onboarded, last, verified sql.NullTime
	if err := row.Scan(&u.ID, &u.Phone, &u.Name, &u.Channel, &u.Frequency, &u.ICSURL, &u.Interests, &onboarded, &u.LastCallSID, &last, &verified, &u.PhoneVerifiedVia, &u.CreatedAt); err != nil {
		return nil, err
	}
	if onboarded.Valid {
		t := onboarded.Time
		u.OnboardedAt = &t
	}
	if last.Valid {
		t := last.Time
		u.LastTriggeredAt = &t
	}
	if verified.Valid {
		t := verified.Time
		u.PhoneVerifiedAt = &t
	}
	return &u, nil
}

func (s *Store) CreateUser(u *User) error {
	id, err := s.insert(`INSERT INTO users (phone, name, channel, frequency, ics_url, interests) VALUES (?,?,?,?,?,?)`,
		u.Phone, u.Name, u.Channel, u.Frequency, nullStr(u.ICSURL), u.Interests)
	if err != nil {
		return err
	}
	u.ID = id
	return nil
}

// MarkPhoneVerified records that the number was proved, and how. It never
// downgrades an existing verification.
func (s *Store) MarkPhoneVerified(userID int64, via string) error {
	if _, err := s.exec(`UPDATE users SET phone_verified_at=COALESCE(phone_verified_at, ?), phone_verified_via=? WHERE id=?`,
		time.Now().UTC(), via, userID); err != nil {
		return err
	}
	// Calls made before this number signed up were kept unowned. Proving the
	// number is what makes them this person's, so they are attached now.
	n, err := s.AdoptTranscripts(userID)
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("transcripts: attached %d earlier call(s) to user %d", n, userID)
	}
	return nil
}

// UpsertUser creates the user if the phone is unknown, otherwise updates the
// mutable onboarding fields so re-signup is idempotent.
func (s *Store) UpsertUser(u *User) error {
	existing, err := s.UserByPhone(u.Phone)
	if err == nil {
		// Empty interests mean "not answered this time", so a re-signup that
		// skips the question keeps whatever the user already told us.
		if strings.TrimSpace(u.Interests) == "" {
			u.Interests = existing.Interests
		}
		_, err = s.exec(`UPDATE users SET name=?, channel=?, frequency=?, ics_url=?, interests=? WHERE id=?`,
			u.Name, u.Channel, u.Frequency, nullStr(u.ICSURL), u.Interests, existing.ID)
		u.ID = existing.ID
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return s.CreateUser(u)
}

// EnsureUser loads a user by phone, creating a default SMS user when unknown so
// an inbound text from a stranger still works.
func (s *Store) EnsureUser(phone string) (*User, error) {
	u, err := s.UserByPhone(phone)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	nu := &User{Phone: phone, Channel: "sms", Frequency: "daily"}
	if err := s.CreateUser(nu); err != nil {
		return nil, err
	}
	return s.UserByPhone(phone)
}

func (s *Store) AllUsers() ([]User, error) {
	rows, err := s.query(userSelect + ` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// SaveOnboarding records what the voice agent learned during the onboarding
// interview. It returns the fields the introduction still has not established,
// and only stamps the user as onboarded once nothing essential is outstanding:
// an interview that never got a name is not a finished interview, and stamping
// it as one would move the person to check-ins and never ask again.
//
// The name is normalised the same way as an answer given over SMS, so "my name
// is Sam" stores Sam and a whole sentence stores nothing rather than becoming
// the name the agent greets them with.
func (s *Store) SaveOnboarding(u *User, name, interests, frequency string) (missing []string, err error) {
	if name = normaliseName(name); name != "" {
		u.Name = name
	}
	if interests = normaliseInterests(interests); interests != "" {
		u.Interests = interests
	}
	if frequency = strings.TrimSpace(frequency); validFrequency(frequency) {
		u.Frequency = frequency
	}
	if u.DisplayName() == "" {
		missing = append(missing, "name")
	}
	if strings.TrimSpace(u.Interests) == "" {
		missing = append(missing, "event_types")
	}

	now := time.Now().UTC()
	complete := len(missing) == 0
	if complete {
		u.OnboardedAt = &now
		_, err = s.exec(`UPDATE users SET name=?, interests=?, frequency=?, onboarded_at=COALESCE(onboarded_at, ?) WHERE id=?`,
			u.Name, u.Interests, u.Frequency, now, u.ID)
		return nil, err
	}
	// Partial answers are still worth keeping - the next turn should not ask
	// for what was already given - but the profile stays un-onboarded.
	_, err = s.exec(`UPDATE users SET name=?, interests=?, frequency=? WHERE id=?`,
		u.Name, u.Interests, u.Frequency, u.ID)
	return missing, err
}

// SetName stores what someone has just said they are called, on the profile the
// caller already resolved by verified number. An answer that is not a usable
// name is refused rather than stored, and any open name question is settled at
// the same time so the conversation does not go on to ask for what is now on
// file.
func (s *Store) SetName(u *User, name string) error {
	if u == nil || u.ID == 0 {
		return errors.New("no profile to name")
	}
	name = normaliseName(name)
	if name == "" {
		return errUnusableAnswer
	}
	now := time.Now().UTC()
	err := s.tx(func(tx *sql.Tx) error {
		if _, err := s.txExec(tx, `UPDATE users SET name=? WHERE id=?`, name, u.ID); err != nil {
			return err
		}
		_, err := s.txExec(tx, `UPDATE checklist_items SET status=?, answer=?, source=?, answered_at=?
			WHERE user_id=? AND item_key='name' AND status=?`,
			StatusAnswered, name, SourceProfile, now, u.ID, StatusUnanswered)
		return err
	})
	if err != nil {
		return err
	}
	u.Name = name
	return nil
}

// MarkOnboarded stamps that the introduction finished. It writes nothing else:
// the answers were already persisted one at a time as they were given.
func (s *Store) MarkOnboarded(u *User) error {
	if u.Onboarded() {
		return nil
	}
	now := time.Now().UTC()
	if _, err := s.exec(`UPDATE users SET onboarded_at=COALESCE(onboarded_at, ?) WHERE id=?`, now, u.ID); err != nil {
		return err
	}
	u.OnboardedAt = &now
	return nil
}

// SetCallSID remembers the Twilio call behind the current voice conversation so
// the service can hang it up once onboarding is done.
func (s *Store) SetCallSID(userID int64, sid string) error {
	_, err := s.exec(`UPDATE users SET last_call_sid=? WHERE id=?`, nullStr(sid), userID)
	return err
}

func (s *Store) SetFrequency(userID int64, frequency string) error {
	_, err := s.exec(`UPDATE users SET frequency=? WHERE id=?`, frequency, userID)
	return err
}

func validFrequency(f string) bool {
	switch f {
	case "daily", "twice-daily", "weekdays":
		return true
	}
	return false
}

func (s *Store) MarkTriggered(userID int64, at time.Time) error {
	_, err := s.exec(`UPDATE users SET last_triggered_at=? WHERE id=?`, at.UTC(), userID)
	return err
}

func (s *Store) AddCheckin(c *Checkin) error {
	if c.UserID == 0 {
		return errors.New("check-in needs a user")
	}
	id, err := s.insert(`INSERT INTO checkins (user_id, mood, summary, topics, raw) VALUES (?,?,?,?,?)`,
		c.UserID, c.Mood, c.Summary, c.Topics, c.Raw)
	if err != nil {
		return err
	}
	c.ID = id
	return nil
}

func (s *Store) RecentCheckins(userID int64, limit int) ([]Checkin, error) {
	rows, err := s.query(`SELECT id, user_id, mood, summary, topics, raw, created_at FROM checkins WHERE user_id=? ORDER BY id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Checkin
	for rows.Next() {
		var c Checkin
		var mood sql.NullInt64
		if err := rows.Scan(&c.ID, &c.UserID, &mood, &c.Summary, &c.Topics, &c.Raw, &c.CreatedAt); err != nil {
			return nil, err
		}
		if mood.Valid {
			m := int(mood.Int64)
			c.Mood = &m
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AddMessage(userID int64, role, body string) error {
	_, err := s.exec(`INSERT INTO messages (user_id, role, body) VALUES (?,?,?)`, userID, role, body)
	return err
}

type Message struct {
	Role string
	Body string
}

// RecentMessages returns the last n messages oldest-first, which is the order the
// Anthropic Messages API expects.
func (s *Store) RecentMessages(userID int64, limit int) ([]Message, error) {
	rows, err := s.query(`SELECT role, body FROM (SELECT id, role, body FROM messages WHERE user_id=? ORDER BY id DESC LIMIT ?) recent ORDER BY id ASC`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Role, &m.Body); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CandidateEvents returns upcoming events this user has not been offered yet.
// Relevance is decided in matching.go rather than in SQL, so every suggestion
// (and every exclusion) can explain itself.
func (s *Store) CandidateEvents(userID int64, limit int) ([]Event, error) {
	rows, err := s.query(`SELECT id, title, starts_at, city, url, tags FROM events
		WHERE starts_at >= ? AND id NOT IN (SELECT event_id FROM suggestions WHERE user_id=?)
		ORDER BY starts_at ASC LIMIT ?`, time.Now().UTC(), userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Title, &e.StartsAt, &e.City, &e.URL, &e.Tags); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanEvent(row scanner) (*Event, error) {
	var e Event
	if err := row.Scan(&e.ID, &e.Title, &e.StartsAt, &e.City, &e.URL, &e.Tags); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) AddSuggestion(userID, eventID int64) (int64, error) {
	return s.insert(`INSERT INTO suggestions (user_id, event_id, status) VALUES (?,?, 'offered')`, userID, eventID)
}

// OpenSuggestion returns the most recent still-offered suggestion for a user.
func (s *Store) OpenSuggestion(userID int64) (*Suggestion, error) {
	row := s.queryRow(`SELECT s.id, s.user_id, s.event_id, s.status, s.created_at, e.id, e.title, e.starts_at, e.city, e.url, e.tags
		FROM suggestions s JOIN events e ON e.id = s.event_id
		WHERE s.user_id=? AND s.status='offered' ORDER BY s.id DESC LIMIT 1`, userID)
	var sg Suggestion
	err := row.Scan(&sg.ID, &sg.UserID, &sg.EventID, &sg.Status, &sg.CreatedAt,
		&sg.Event.ID, &sg.Event.Title, &sg.Event.StartsAt, &sg.Event.City, &sg.Event.URL, &sg.Event.Tags)
	if err != nil {
		return nil, err
	}
	return &sg, nil
}

// SetSuggestionStatus is tenant-scoped: a suggestion id alone is not enough to
// mutate it, the caller must also own it.
func (s *Store) SetSuggestionStatus(userID, id int64, status string) error {
	res, err := s.exec(`UPDATE suggestions SET status=? WHERE id=? AND user_id=?`, status, id, userID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) AcceptedSuggestions(userID int64) ([]Suggestion, error) {
	rows, err := s.query(`SELECT s.id, s.user_id, s.event_id, s.status, s.created_at, e.id, e.title, e.starts_at, e.city, e.url, e.tags
		FROM suggestions s JOIN events e ON e.id = s.event_id
		WHERE s.user_id=? AND s.status='accepted' ORDER BY e.starts_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Suggestion
	for rows.Next() {
		var sg Suggestion
		if err := rows.Scan(&sg.ID, &sg.UserID, &sg.EventID, &sg.Status, &sg.CreatedAt,
			&sg.Event.ID, &sg.Event.Title, &sg.Event.StartsAt, &sg.Event.City, &sg.Event.URL, &sg.Event.Tags); err != nil {
			return nil, err
		}
		out = append(out, sg)
	}
	return out, rows.Err()
}

func (s *Store) countEvents() (int, error) {
	var n int
	err := s.queryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	return n, err
}

// SeedEvents loads events from src into the events table. It is a no-op when the
// stored count already matches the source, so a redeploy with a fresh export
// picks the new rows up automatically; force wipes unreferenced rows first so a
// shrinking export can drop stale events without orphaning suggestions.
func (s *Store) SeedEvents(src EventSource, force bool) error {
	records, err := src.Events()
	if err != nil {
		return err
	}
	have, err := s.countEvents()
	if err != nil {
		return err
	}
	if have == len(records) && !force {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if force {
		if _, err := tx.Exec(`DELETE FROM events WHERE id NOT IN (SELECT event_id FROM suggestions)`); err != nil {
			return err
		}
	}
	insertEvent := s.rebind(s.insertIgnoreSQL(`INSERT INTO events (title, starts_at, city, url, tags) VALUES (?,?,?,?,?)`))
	for _, e := range records {
		if _, err := tx.Exec(insertEvent,
			e.Title, e.StartsAt.UTC(), e.City, e.URL, e.Tags); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	n, _ := s.countEvents()
	log.Printf("seed: %s -> %d events in db", src.Name(), n)
	return nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
