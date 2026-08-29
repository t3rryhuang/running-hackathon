package main

import (
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	phone TEXT UNIQUE NOT NULL,
	name TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT 'sms' CHECK (channel IN ('call','sms')),
	frequency TEXT NOT NULL DEFAULT 'daily',
	ics_url TEXT,
	interests TEXT NOT NULL DEFAULT '',
	onboarded_at DATETIME,
	last_call_sid TEXT,
	last_triggered_at DATETIME,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS checkins (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id),
	mood INTEGER CHECK (mood IS NULL OR (mood BETWEEN 1 AND 5)),
	summary TEXT NOT NULL DEFAULT '',
	topics TEXT NOT NULL DEFAULT '',
	raw TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	title TEXT NOT NULL,
	starts_at DATETIME NOT NULL,
	city TEXT NOT NULL DEFAULT 'London',
	url TEXT NOT NULL DEFAULT '',
	tags TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS suggestions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id),
	event_id INTEGER NOT NULL REFERENCES events(id),
	status TEXT NOT NULL CHECK (status IN ('offered','accepted','declined')),
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id),
	role TEXT NOT NULL,
	body TEXT NOT NULL,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS events_unique ON events (title, starts_at);
`

// migrations are applied after the schema so databases created by earlier
// versions gain new columns instead of needing a wipe.
var migrations = []string{
	`ALTER TABLE users ADD COLUMN interests TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE users ADD COLUMN onboarded_at DATETIME`,
	`ALTER TABLE users ADD COLUMN last_call_sid TEXT`,
}

type User struct {
	ID              int64
	Phone           string
	Name            string
	Channel         string
	Frequency       string
	ICSURL          string
	Interests       string
	OnboardedAt     *time.Time
	LastCallSID     string
	LastTriggeredAt *time.Time
	CreatedAt       time.Time
}

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

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	for _, m := range migrations {
		// Re-running an applied migration errors with "duplicate column";
		// that is the expected steady state, so it is not fatal.
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const userSelect = `SELECT id, phone, name, channel, frequency, COALESCE(ics_url,''), COALESCE(interests,''), onboarded_at, COALESCE(last_call_sid,''), last_triggered_at, created_at FROM users`

type scanner interface{ Scan(dest ...any) error }

func (s *Store) UserByPhone(phone string) (*User, error) {
	return scanUser(s.db.QueryRow(userSelect+` WHERE phone = ?`, phone))
}

func scanUser(row scanner) (*User, error) {
	var u User
	var onboarded, last sql.NullTime
	if err := row.Scan(&u.ID, &u.Phone, &u.Name, &u.Channel, &u.Frequency, &u.ICSURL, &u.Interests, &onboarded, &u.LastCallSID, &last, &u.CreatedAt); err != nil {
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
	return &u, nil
}

func (s *Store) CreateUser(u *User) error {
	res, err := s.db.Exec(`INSERT INTO users (phone, name, channel, frequency, ics_url, interests) VALUES (?,?,?,?,?,?)`,
		u.Phone, u.Name, u.Channel, u.Frequency, nullStr(u.ICSURL), u.Interests)
	if err != nil {
		return err
	}
	u.ID, _ = res.LastInsertId()
	return nil
}

// UpsertUser creates the user if the phone is unknown, otherwise updates the
// mutable onboarding fields so re-signup is idempotent.
func (s *Store) UpsertUser(u *User) error {
	existing, err := s.UserByPhone(u.Phone)
	if err == nil {
		_, err = s.db.Exec(`UPDATE users SET name=?, channel=?, frequency=?, ics_url=? WHERE id=?`,
			u.Name, u.Channel, u.Frequency, nullStr(u.ICSURL), existing.ID)
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
	rows, err := s.db.Query(userSelect + ` ORDER BY id`)
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
// interview and stamps the user as onboarded.
func (s *Store) SaveOnboarding(u *User, name, interests, frequency string) error {
	if name = strings.TrimSpace(name); name != "" {
		u.Name = name
	}
	if interests = strings.TrimSpace(interests); interests != "" {
		u.Interests = interests
	}
	if frequency = strings.TrimSpace(frequency); validFrequency(frequency) {
		u.Frequency = frequency
	}
	now := time.Now().UTC()
	u.OnboardedAt = &now
	_, err := s.db.Exec(`UPDATE users SET name=?, interests=?, frequency=?, onboarded_at=? WHERE id=?`,
		u.Name, u.Interests, u.Frequency, now, u.ID)
	return err
}

// SetCallSID remembers the Twilio call behind the current voice conversation so
// the service can hang it up once onboarding is done.
func (s *Store) SetCallSID(userID int64, sid string) error {
	_, err := s.db.Exec(`UPDATE users SET last_call_sid=? WHERE id=?`, nullStr(sid), userID)
	return err
}

func (s *Store) SetFrequency(userID int64, frequency string) error {
	_, err := s.db.Exec(`UPDATE users SET frequency=? WHERE id=?`, frequency, userID)
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
	_, err := s.db.Exec(`UPDATE users SET last_triggered_at=? WHERE id=?`, at.UTC(), userID)
	return err
}

func (s *Store) AddCheckin(c *Checkin) error {
	res, err := s.db.Exec(`INSERT INTO checkins (user_id, mood, summary, topics, raw) VALUES (?,?,?,?,?)`,
		c.UserID, c.Mood, c.Summary, c.Topics, c.Raw)
	if err != nil {
		return err
	}
	c.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) RecentCheckins(userID int64, limit int) ([]Checkin, error) {
	rows, err := s.db.Query(`SELECT id, user_id, mood, summary, topics, raw, created_at FROM checkins WHERE user_id=? ORDER BY id DESC LIMIT ?`, userID, limit)
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
	_, err := s.db.Exec(`INSERT INTO messages (user_id, role, body) VALUES (?,?,?)`, userID, role, body)
	return err
}

type Message struct {
	Role string
	Body string
}

// RecentMessages returns the last n messages oldest-first, which is the order the
// Anthropic Messages API expects.
func (s *Store) RecentMessages(userID int64, limit int) ([]Message, error) {
	rows, err := s.db.Query(`SELECT role, body FROM (SELECT id, role, body FROM messages WHERE user_id=? ORDER BY id DESC LIMIT ?) ORDER BY id ASC`, userID, limit)
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

// NextEventFor picks the soonest upcoming event that has never been suggested
// to this user. When the user has stated interests it first tries events whose
// tags match one of them, then widens to any event, then (if every upcoming
// event has been offered already) falls back to the soonest event overall.
// Londoners get London events first; the export is London-heavy but not pure.
const eventOrder = `(lower(city) <> 'london'), starts_at ASC`

func (s *Store) NextEventFor(userID int64, interests []string) (*Event, error) {
	now := time.Now().UTC()
	const base = `SELECT id, title, starts_at, city, url, tags FROM events
		WHERE starts_at >= ? AND id NOT IN (SELECT event_id FROM suggestions WHERE user_id=?)`

	if len(interests) > 0 {
		clauses := make([]string, 0, len(interests))
		args := []any{now, userID}
		for _, in := range interests {
			clauses = append(clauses, `lower(tags) LIKE ?`)
			args = append(args, "%"+in+"%")
		}
		q := base + ` AND (` + strings.Join(clauses, " OR ") + `) ORDER BY ` + eventOrder + ` LIMIT 1`
		e, err := scanEvent(s.db.QueryRow(q, args...))
		if err == nil {
			return e, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}

	e, err := scanEvent(s.db.QueryRow(base+` ORDER BY `+eventOrder+` LIMIT 1`, now, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return scanEvent(s.db.QueryRow(`SELECT id, title, starts_at, city, url, tags FROM events ORDER BY ` + eventOrder + ` LIMIT 1`))
	}
	return e, err
}

func scanEvent(row scanner) (*Event, error) {
	var e Event
	if err := row.Scan(&e.ID, &e.Title, &e.StartsAt, &e.City, &e.URL, &e.Tags); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Store) AddSuggestion(userID, eventID int64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO suggestions (user_id, event_id, status) VALUES (?,?, 'offered')`, userID, eventID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// OpenSuggestion returns the most recent still-offered suggestion for a user.
func (s *Store) OpenSuggestion(userID int64) (*Suggestion, error) {
	row := s.db.QueryRow(`SELECT s.id, s.user_id, s.event_id, s.status, s.created_at, e.id, e.title, e.starts_at, e.city, e.url, e.tags
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

func (s *Store) SetSuggestionStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE suggestions SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) AcceptedSuggestions(userID int64) ([]Suggestion, error) {
	rows, err := s.db.Query(`SELECT s.id, s.user_id, s.event_id, s.status, s.created_at, e.id, e.title, e.starts_at, e.city, e.url, e.tags
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
	err := s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
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
	for _, e := range records {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO events (title, starts_at, city, url, tags) VALUES (?,?,?,?,?)`,
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
