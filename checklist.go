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
	// KnownFrom reads the answer off the profile when the person already gave
	// it somewhere else, such as the signup form. Nil when only the interview
	// can supply it.
	KnownFrom func(*User) string
	// TTL is how long a previous answer stays usable. Zero means a stable
	// preference that is never re-asked; a short TTL means the answer is about
	// right now and goes stale, so it is asked again.
	TTL time.Duration
	// PerSession marks a question whose answer belongs to the conversation it
	// was asked in - a yes to one specific event says nothing about the next
	// one - so it is never carried into a later session.
	PerSession bool
	// Dynamic marks a question whose wording is written when it is asked,
	// because it names something chosen from the answers so far.
	Dynamic bool
}

// A conversation is one of two things, never both at once. Onboarding is the
// introduction: who they are, what they are into, one event worth their time,
// and permission to come back. Check-ins are the daily conversation that only
// happens after that permission was given.
const (
	FlowOnboarding = "onboarding"
	FlowCheckin    = "checkin"
)

// onboardingTemplate is the introduction, asked strictly in this order, one
// question per turn. It ends by offering something concrete and asking whether
// to come back at all - it never slides into a check-in.
var onboardingTemplate = []ChecklistQuestion{
	{Key: "name", Prompt: "Hi, I'm CheckIn. Before anything else - what should I call you?", Persists: "name",
		KnownFrom: func(u *User) string { return u.DisplayName() }},
	{Key: "event_types", Prompt: "Good to meet you. What kind of events are you actually into - hackathons, meetups, conferences, something more social?",
		Persists: "interests", KnownFrom: func(u *User) string { return u.Interests }},
	// Only the event types feed a denormalised user column, because matching
	// reads it on every suggestion. The rest stay in checklist_items, where
	// the answer keeps its status: a skipped question must never read back as
	// a stated preference.
	{Key: "event_time", Prompt: "And when do you usually have time for them - weekday evenings, weekends, lunchtimes?"},
	// The prompt for this one is written when it is asked, because it names a
	// real event chosen from what they just told us. Empty here so a stale
	// suggestion can never be read back out of the table.
	{Key: "event_offer", Prompt: "", PerSession: true, Dynamic: true},
	{Key: "checkin_consent", Persists: "frequency",
		Prompt: "Last thing, and it's a separate question: want me to check in with you now and then to see how you're doing? Reply daily, twice daily, weekdays, or no thanks."},
}

// checkinTemplate is the daily conversation. It assumes onboarding already
// happened and asks nothing that onboarding covered.
var checkinTemplate = []ChecklistQuestion{
	// Availability is about tonight, not about them, so last week's yes proves
	// nothing and the question comes back.
	{Key: "evening_availability", Prompt: "Are you free for an event with like-minded people at 7 PM?", TTL: 20 * time.Hour},
	{Key: "notify_watch", Prompt: "What should we keep our eyes out for to notify you?"},
}

// templateFor is the question list for a flow. An unknown flow gets the
// check-in list rather than an empty interview.
func templateFor(flow string) []ChecklistQuestion {
	if flow == FlowOnboarding {
		return onboardingTemplate
	}
	return checkinTemplate
}

// FlowFor decides which conversation this person is in. Onboarding runs once,
// and only the onboarded get check-ins.
func FlowFor(u *User) string {
	if u == nil || !u.Onboarded() {
		return FlowOnboarding
	}
	return FlowCheckin
}

// SourceConversation is an answer the person gave in this interview;
// SourceProfile is one they already gave on the signup form or an earlier
// session, carried forward so it is never asked twice.
const (
	SourceConversation = "conversation"
	SourceProfile      = "profile"
)

var errNotCurrentItem = errors.New("that is not the question currently on the table")

type Session struct {
	ID        int64
	UserID    int64
	Channel   string
	Flow      string
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
	Source     string
	AskedAt    *time.Time
	AnsweredAt *time.Time
}

// Answered reports whether this item has reached a terminal state. Skipped and
// declined are terminal too: the person responded, just not with an answer.
func (c ChecklistItem) Settled() bool { return c.Status != StatusUnanswered }

// EnsureSession returns the user's open session for this channel and flow,
// creating it (and its checklist) when there is none. A session belongs to
// exactly one user and is never shared, and an onboarding session is never
// handed back to a check-in: the two conversations keep separate state.
func (s *Store) EnsureSession(u *User, channel, flow string) (*Session, error) {
	if u == nil || u.ID == 0 {
		return nil, errors.New("session needs a user")
	}
	userID := u.ID
	switch channel {
	case "call", "sms", "web":
	default:
		return nil, errors.New("unknown channel " + channel)
	}
	switch flow {
	case FlowOnboarding, FlowCheckin:
	default:
		return nil, errors.New("unknown flow " + flow)
	}
	template := templateFor(flow)
	row := s.queryRow(`SELECT id, user_id, channel, flow, state, started_at FROM sessions
		WHERE user_id=? AND channel=? AND flow=? AND state='open' ORDER BY id DESC LIMIT 1`, userID, channel, flow)
	var sess Session
	err := row.Scan(&sess.ID, &sess.UserID, &sess.Channel, &sess.Flow, &sess.State, &sess.StartedAt)
	if err == nil {
		return &sess, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	now := time.Now().UTC()
	// Resolved before the transaction opens: the SQLite pool is a single
	// connection, so a read issued from inside the transaction would deadlock
	// against it.
	prefill := make([]carriedState, len(template))
	for i, q := range template {
		prefill[i] = s.carryForward(q, u, now)
	}

	var created Session
	err = s.tx(func(tx *sql.Tx) error {
		id, err := s.txInsert(tx, `INSERT INTO sessions (user_id, channel, flow, state) VALUES (?,?,?,'open')`, userID, channel, flow)
		if err != nil {
			return err
		}
		for i, q := range template {
			// Anything already on file - typed into the signup form, or said in
			// an earlier session and still fresh - starts the session settled,
			// which is what stops the agent asking for it a second time.
			p := prefill[i]
			if _, err := s.txExec(tx,
				`INSERT INTO checklist_items (user_id, session_id, item_key, position, prompt, status, answer, source, answered_at)
				VALUES (?,?,?,?,?,?,?,?,?)`,
				userID, id, q.Key, i, q.Prompt, p.status, p.answer, p.source,
				nullTimeIf(p.status != StatusUnanswered, now)); err != nil {
				return err
			}
		}
		created = Session{ID: id, UserID: userID, Channel: channel, Flow: flow, State: "open", StartedAt: time.Now().UTC()}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// carriedState is how a question starts a new session.
type carriedState struct {
	status string
	answer string
	source string
}

// carryForward decides what a new session already knows about a question: the
// profile field the person filled in, their own answer from an earlier session
// while it is still fresh, or a refusal they have already made. Anything else
// starts unanswered and gets asked. Skips are deliberately not carried - "not
// now" is about that conversation, not a standing preference.
func (s *Store) carryForward(q ChecklistQuestion, u *User, now time.Time) carriedState {
	if q.PerSession {
		return carriedState{StatusUnanswered, "", SourceConversation}
	}
	if q.KnownFrom != nil {
		if v := strings.TrimSpace(q.KnownFrom(u)); v != "" {
			return carriedState{StatusAnswered, v, SourceProfile}
		}
	}
	status, answer, answeredAt, ok := s.lastSettled(u.ID, q.Key)
	if !ok {
		return carriedState{StatusUnanswered, "", SourceConversation}
	}
	if q.TTL > 0 && now.Sub(answeredAt) > q.TTL {
		return carriedState{StatusUnanswered, "", SourceConversation}
	}
	return carriedState{status, answer, SourceConversation}
}

// lastSettled is the most recent answer or refusal for a question, across this
// user's sessions only.
func (s *Store) lastSettled(userID int64, key string) (string, string, time.Time, bool) {
	row := s.queryRow(`SELECT status, answer, answered_at FROM checklist_items
		WHERE user_id=? AND item_key=? AND (status='declined' OR (status='answered' AND answer<>''))
		ORDER BY answered_at DESC LIMIT 1`, userID, key)
	var status, answer string
	var at sql.NullTime
	if err := row.Scan(&status, &answer, &at); err != nil {
		return "", "", time.Time{}, false
	}
	return status, answer, at.Time, true
}

func nullTimeIf(cond bool, t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: cond}
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
	rows, err := s.query(`SELECT id, user_id, session_id, item_key, position, prompt, status, answer, source, asked_at, answered_at
		FROM checklist_items WHERE user_id=? AND session_id=? ORDER BY position ASC`, userID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChecklistItem
	for rows.Next() {
		var c ChecklistItem
		var asked, answered sql.NullTime
		if err := rows.Scan(&c.ID, &c.UserID, &c.SessionID, &c.Key, &c.Position, &c.Prompt, &c.Status, &c.Answer, &c.Source, &asked, &answered); err != nil {
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

// SetItemPrompt writes the wording of a dynamic question at the moment it is
// asked, so what the person was actually asked is what the table records.
func (s *Store) SetItemPrompt(userID, itemID int64, prompt string) error {
	_, err := s.exec(`UPDATE checklist_items SET prompt=?, updated_at=? WHERE id=? AND user_id=?`,
		prompt, time.Now().UTC(), itemID, userID)
	return err
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
			`UPDATE checklist_items SET status=?, answer=?, source=?, answered_at=?, updated_at=? WHERE id=? AND user_id=? AND session_id=?`,
			status, answer, SourceConversation, now, now, next.ID, u.ID, sessionID); err != nil {
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
		case "name":
			name := normaliseName(answer)
			if name == "" {
				return nil
			}
			if _, err := s.txExec(tx, `UPDATE users SET name=? WHERE id=?`, name, u.ID); err != nil {
				return err
			}
			u.Name = name
		case "frequency":
			// Only a recognised cadence is written. "Sure, go on then" is a
			// yes to being contacted, not a statement about how often, so it
			// leaves the existing schedule alone rather than inventing one.
			freq := normaliseFrequency(answer)
			if freq == "" {
				return nil
			}
			if _, err := s.txExec(tx, `UPDATE users SET frequency=? WHERE id=?`, freq, u.ID); err != nil {
				return err
			}
			u.Frequency = freq
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	next.Status = status
	next.Answer = answer
	next.Source = SourceConversation
	next.AnsweredAt = &now
	return next, nil
}

func questionByKey(key string) ChecklistQuestion {
	for _, q := range append(append([]ChecklistQuestion{}, onboardingTemplate...), checkinTemplate...) {
		if q.Key == key {
			return q
		}
	}
	return ChecklistQuestion{}
}

// SettledStatus reports how a question was last left, including refusals, so
// callers can tell "they said no" from "they never said".
func (s *Store) SettledStatus(userID int64, key string) (status, answer string, ok bool) {
	status, answer, _, ok = s.lastSettled(userID, key)
	return status, answer, ok
}

// normaliseName keeps the name people actually give ("it's Sam", "Sam here")
// down to something usable, and refuses anything that looks like a sentence
// rather than a name.
func normaliseName(answer string) string {
	name := strings.TrimSpace(answer)
	for _, prefix := range []string{"my name is ", "my name's ", "i'm ", "im ", "i am ", "it's ", "its ", "this is ", "call me "} {
		if len(name) > len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
			name = strings.TrimSpace(name[len(prefix):])
			break
		}
	}
	name = strings.Trim(name, ".!,")
	if name == "" || len(strings.Fields(name)) > 4 || len(name) > 60 {
		return ""
	}
	return name
}

// normaliseFrequency maps what someone says to one of the cadences the
// scheduler understands, and returns empty for anything else.
func normaliseFrequency(answer string) string {
	lower := strings.ToLower(strings.TrimSpace(answer))
	switch {
	case strings.Contains(lower, "twice"), strings.Contains(lower, "two times"), strings.Contains(lower, "2x"):
		return "twice-daily"
	case strings.Contains(lower, "weekday"), strings.Contains(lower, "week day"), strings.Contains(lower, "work day"):
		return "weekdays"
	case strings.Contains(lower, "daily"), strings.Contains(lower, "every day"), strings.Contains(lower, "each day"):
		return "daily"
	}
	return ""
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

// isYes reports whether an answer is a clear acceptance. Anything ambiguous is
// not a yes: consent to an external action has to be explicit.
func isYes(answer string) bool {
	lower := strings.ToLower(strings.Trim(strings.TrimSpace(answer), ".!? "))
	switch lower {
	case "y", "yes", "yeah", "yep", "yes please", "go on", "go on then", "sure", "ok", "okay", "please do", "sign me up", "put me down", "do it":
		return true
	}
	return strings.HasPrefix(lower, "yes ") || strings.HasPrefix(lower, "yeah ") || strings.HasPrefix(lower, "sign me up")
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
