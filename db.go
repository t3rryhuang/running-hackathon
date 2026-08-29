package main

import (
	"database/sql"
	"encoding/csv"
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
`

type User struct {
	ID              int64
	Phone           string
	Name            string
	Channel         string
	Frequency       string
	ICSURL          string
	LastTriggeredAt *time.Time
	CreatedAt       time.Time
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
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) UserByPhone(phone string) (*User, error) {
	row := s.db.QueryRow(`SELECT id, phone, name, channel, frequency, COALESCE(ics_url,''), last_triggered_at, created_at FROM users WHERE phone = ?`, phone)
	return scanUser(row)
}

func scanUser(row *sql.Row) (*User, error) {
	var u User
	var last sql.NullTime
	if err := row.Scan(&u.ID, &u.Phone, &u.Name, &u.Channel, &u.Frequency, &u.ICSURL, &last, &u.CreatedAt); err != nil {
		return nil, err
	}
	if last.Valid {
		t := last.Time
		u.LastTriggeredAt = &t
	}
	return &u, nil
}

func (s *Store) CreateUser(u *User) error {
	res, err := s.db.Exec(`INSERT INTO users (phone, name, channel, frequency, ics_url) VALUES (?,?,?,?,?)`,
		u.Phone, u.Name, u.Channel, u.Frequency, nullStr(u.ICSURL))
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
	rows, err := s.db.Query(`SELECT id, phone, name, channel, frequency, COALESCE(ics_url,''), last_triggered_at, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var last sql.NullTime
		if err := rows.Scan(&u.ID, &u.Phone, &u.Name, &u.Channel, &u.Frequency, &u.ICSURL, &last, &u.CreatedAt); err != nil {
			return nil, err
		}
		if last.Valid {
			t := last.Time
			u.LastTriggeredAt = &t
		}
		out = append(out, u)
	}
	return out, rows.Err()
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

// NextEventFor picks the soonest upcoming event that has never been suggested to
// this user, falling back to the soonest event overall when all have been used.
func (s *Store) NextEventFor(userID int64) (*Event, error) {
	now := time.Now().UTC()
	row := s.db.QueryRow(`SELECT id, title, starts_at, city, url, tags FROM events
		WHERE starts_at >= ? AND id NOT IN (SELECT event_id FROM suggestions WHERE user_id=?)
		ORDER BY starts_at ASC LIMIT 1`, now, userID)
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		row = s.db.QueryRow(`SELECT id, title, starts_at, city, url, tags FROM events ORDER BY starts_at ASC LIMIT 1`)
		return scanEvent(row)
	}
	return e, err
}

func scanEvent(row *sql.Row) (*Event, error) {
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

// SeedEvents loads events.csv into the events table when the table is empty.
// CSV columns: title,starts_at (RFC3339),city,url,tags
func (s *Store) SeedEvents(csvBytes []byte) error {
	n, err := s.countEvents()
	if err != nil || n > 0 {
		return err
	}
	r := csv.NewReader(strings.NewReader(string(csvBytes)))
	records, err := r.ReadAll()
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, rec := range records {
		if i == 0 && strings.EqualFold(strings.TrimSpace(rec[0]), "title") {
			continue
		}
		if len(rec) < 5 {
			continue
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(rec[1]))
		if err != nil {
			log.Printf("seed: skipping %q: bad starts_at %q", rec[0], rec[1])
			continue
		}
		if _, err := tx.Exec(`INSERT INTO events (title, starts_at, city, url, tags) VALUES (?,?,?,?,?)`,
			rec[0], t.UTC(), rec[2], rec[3], rec[4]); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	n, _ = s.countEvents()
	log.Printf("seed: loaded %d events", n)
	return nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
