package main

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ChecklistItem statuses. An item is only ever "answered" because the person
// said something; nothing infers an answer from silence, tone, a calendar or
// another user.
const (
	StatusUnanswered = "unanswered"
	StatusAnswered   = "answered"
	StatusSkipped    = "skipped"
	StatusDeclined   = "declined"
)

// ChecklistQuestion is one interview item. The wording is fixed so every user
// is asked the same thing in the same order, on the phone and over SMS.
type ChecklistQuestion struct {
	Key    string
	Prompt string
	// Persists names the user field this answer updates, if any.
	Persists string
}

// checklistTemplate is the standard interview, asked strictly in this order,
// one question per turn.
var checklistTemplate = []ChecklistQuestion{
	{Key: "event_types", Prompt: "What type of events do you like to go to?", Persists: "interests"},
	// Only the event types feed a denormalised user column, because matching
	// reads it on every suggestion. The rest stay in checklist_items, where
	// the answer keeps its status: a skipped question must never read back as
	// a stated preference.
	{Key: "event_time", Prompt: "What time do you like to go to events?"},
	{Key: "evening_availability", Prompt: "Are you free for an event with like-minded people at 7 PM?"},
	{Key: "notify_watch", Prompt: "What should we keep our eyes out for to notify you?"},
}

var errNotCurrentItem = errors.New("that is not the question currently on the table")

type Session struct {
	ID        int64
	UserID    int64
	Channel   string
	State     string
	StartedAt time.Time
}

type ChecklistItem struct {
	ID         int64
	UserID     int64
	SessionID  int64
	Key        string
	Position   int
	Prompt     string
	Status     string
	Answer     string
	AskedAt    *time.Time
	AnsweredAt *time.Time
}

// Answered reports whether this item has reached a terminal state. Skipped and
// declined are terminal too: the person responded, just not with an answer.
func (c ChecklistItem) Settled() bool { return c.Status != StatusUnanswered }

// EnsureSession returns the user's open session on this channel, creating it
// (and its checklist) when there is none. A session belongs to exactly one
// user and is never shared.
func (s *Store) EnsureSession(userID int64, channel string) (*Session, error) {
	if userID == 0 {
		return nil, errors.New("session needs a user")
	}
	switch channel {
	case "call", "sms", "web":
	default:
		return nil, errors.New("unknown channel " + channel)
	}
	row := s.queryRow(`SELECT id, user_id, channel, state, started_at FROM sessions
		WHERE user_id=? AND channel=? AND state='open' ORDER BY id DESC LIMIT 1`, userID, channel)
	var sess Session
	err := row.Scan(&sess.ID, &sess.UserID, &sess.Channel, &sess.State, &sess.StartedAt)
	if err == nil {
		return &sess, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	var created Session
	err = s.tx(func(tx *sql.Tx) error {
		id, err := s.txInsert(tx, `INSERT INTO sessions (user_id, channel, state) VALUES (?,?,'open')`, userID, channel)
		if err != nil {
			return err
		}
		for i, q := range checklistTemplate {
			if _, err := s.txExec(tx,
				`INSERT INTO checklist_items (user_id, session_id, item_key, position, prompt, status) VALUES (?,?,?,?,?,?)`,
				userID, id, q.Key, i, q.Prompt, StatusUnanswered); err != nil {
				return err
			}
		}
		created = Session{ID: id, UserID: userID, Channel: channel, State: "open", StartedAt: time.Now().UTC()}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// CloseSession ends a session so the next conversation starts a fresh
// checklist. Tenant-scoped: the caller must own the session.
func (s *Store) CloseSession(userID, sessionID int64) error {
	_, err := s.exec(`UPDATE sessions SET state='closed', ended_at=? WHERE id=? AND user_id=?`,
		time.Now().UTC(), sessionID, userID)
	return err
}

// Checklist returns every item for this user's session, in order.
func (s *Store) Checklist(userID, sessionID int64) ([]ChecklistItem, error) {
	rows, err := s.query(`SELECT id, user_id, session_id, item_key, position, prompt, status, answer, asked_at, answered_at
		FROM checklist_items WHERE user_id=? AND session_id=? ORDER BY position ASC`, userID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChecklistItem
	for rows.Next() {
		var c ChecklistItem
		var asked, answered sql.NullTime
		if err := rows.Scan(&c.ID, &c.UserID, &c.SessionID, &c.Key, &c.Position, &c.Prompt, &c.Status, &c.Answer, &asked, &answered); err != nil {
			return nil, err
		}
		if asked.Valid {
			t := asked.Time
			c.AskedAt = &t
		}
		if answered.Valid {
			t := answered.Time
			c.AnsweredAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NextChecklistItem returns the first item that is still unanswered, or nil
// when the interview is complete. This is the only thing that decides what to
// ask next, so question two can never appear before question one is settled.
func (s *Store) NextChecklistItem(userID, sessionID int64) (*ChecklistItem, error) {
	items, err := s.Checklist(userID, sessionID)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if !it.Settled() {
			c := it
			return &c, nil
		}
	}
	return nil, nil
}

// MarkAsked stamps when the question was put to the user. It does not change
// the answer state.
func (s *Store) MarkAsked(userID, itemID int64) error {
	_, err := s.exec(`UPDATE checklist_items SET asked_at=COALESCE(asked_at, ?), updated_at=? WHERE id=? AND user_id=?`,
		time.Now().UTC(), time.Now().UTC(), itemID, userID)
	return err
}

// RecordChecklistAnswer settles one item. It refuses to write to any item other
// than the current one, so an agent cannot answer question three on the user's
// behalf, and it writes the derived profile field in the same transaction.
func (s *Store) RecordChecklistAnswer(u *User, sessionID int64, key, status, answer string) (*ChecklistItem, error) {
	switch status {
	case StatusAnswered, StatusSkipped, StatusDeclined:
	default:
		return nil, errors.New("status must be answered, skipped or declined")
	}
	answer = strings.TrimSpace(answer)
	if status == StatusAnswered && answer == "" {
		return nil, errors.New("an answered item needs the user's actual answer")
	}
	next, err := s.NextChecklistItem(u.ID, sessionID)
	if err != nil {
		return nil, err
	}
	if next == nil || next.Key != key {
		return nil, errNotCurrentItem
	}

	now := time.Now().UTC()
	err = s.tx(func(tx *sql.Tx) error {
		if _, err := s.txExec(tx,
			`UPDATE checklist_items SET status=?, answer=?, answered_at=?, updated_at=? WHERE id=? AND user_id=? AND session_id=?`,
			status, answer, now, now, next.ID, u.ID, sessionID); err != nil {
			return err
		}
		if status != StatusAnswered {
			return nil
		}
		switch questionByKey(key).Persists {
		case "interests":
			interests := normaliseInterests(answer)
			if interests == "" {
				return nil
			}
			if _, err := s.txExec(tx, `UPDATE users SET interests=? WHERE id=?`, interests, u.ID); err != nil {
				return err
			}
			u.Interests = interests
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	next.Status = status
	next.Answer = answer
	next.AnsweredAt = &now
	return next, nil
}

func questionByKey(key string) ChecklistQuestion {
	for _, q := range checklistTemplate {
		if q.Key == key {
			return q
		}
	}
	return ChecklistQuestion{}
}

// ChecklistAnswer returns a settled answer by key, and whether it is usable
// (answered rather than skipped or declined). Recommendations and
// notifications read preferences through here, so a skipped question can never
// masquerade as a preference.
func (s *Store) ChecklistAnswer(userID int64, key string) (string, bool) {
	row := s.queryRow(`SELECT status, answer FROM checklist_items
		WHERE user_id=? AND item_key=? AND status='answered' ORDER BY answered_at DESC LIMIT 1`, userID, key)
	var status, answer string
	if err := row.Scan(&status, &answer); err != nil {
		return "", false
	}
	return answer, true
}

// classifyReply turns an inbound message into a checklist status. Only explicit
// words settle an item as skipped or declined; anything else is the answer, and
// an empty message settles nothing.
func classifyReply(body string) (status string, answer string) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return StatusUnanswered, ""
	}
	switch strings.ToLower(strings.Trim(trimmed, ".!? ")) {
	case "skip", "pass", "next", "later":
		return StatusSkipped, ""
	case "stop", "decline", "no thanks", "don't ask", "dont ask", "rather not", "prefer not to say":
		return StatusDeclined, ""
	}
	return StatusAnswered, trimmed
}
