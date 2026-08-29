package main

import (
	"database/sql"
	"errors"
	"log"
	"sort"
	"strings"
	"time"
)

// Signal kinds. Each one is a separate consent scope: agreeing to share a
// calendar is not agreeing to share a heart rate.
const (
	KindHeartRate   = "heart_rate"
	KindLocation    = "location"
	KindCalendar    = "calendar"
	KindCommitments = "commitments"
)

// signalTTL is how long an observation stays usable. Past it the value is not
// "probably still true", it is unknown, and the agent must say so.
var signalTTL = map[string]time.Duration{
	KindHeartRate:   2 * time.Hour,
	KindLocation:    6 * time.Hour,
	KindCalendar:    12 * time.Hour,
	KindCommitments: 7 * 24 * time.Hour,
}

// signalRetention is how long a stored observation is kept before the sweeper
// deletes it, regardless of freshness. Wellness and location are the most
// sensitive, so they are kept for the shortest time.
var signalRetention = map[string]time.Duration{
	KindHeartRate:   7 * 24 * time.Hour,
	KindLocation:    24 * time.Hour,
	KindCalendar:    7 * 24 * time.Hour,
	KindCommitments: 30 * 24 * time.Hour,
}

func validSignalKind(kind string) bool {
	_, ok := signalTTL[kind]
	return ok
}

var errNoConsent = errors.New("no active consent for this signal")

// Signal is one observation from an authorised source. It always carries who
// said it and when they observed it: a value with no provenance is not stored.
type Signal struct {
	ID         int64
	UserID     int64
	Kind       string
	Value      string
	Unit       string
	Source     string
	ObservedAt time.Time
	IngestedAt time.Time
	ExpiresAt  time.Time
}

// Fresh reports whether the observation is still inside its TTL at now.
func (s Signal) Fresh(now time.Time) bool { return now.Before(s.ExpiresAt) }

// Reading is what the rest of the service sees: either a fresh, consented
// value, or an explicit unknown with the reason why.
type Reading struct {
	Kind    string
	Known   bool
	Value   string
	Unit    string
	Source  string
	AsOf    time.Time
	Unknown string // "no consent", "never received", "stale since ..."
}

// Describe renders the reading for a prompt or a log line. Unknown readings
// render as unknown; there is no default value to fall back on.
func (r Reading) Describe() string {
	if !r.Known {
		return r.Kind + ": unknown (" + r.Unknown + ")"
	}
	v := r.Value
	if r.Unit != "" {
		v += " " + r.Unit
	}
	return r.Kind + ": " + v + " (source " + r.Source + ", observed " + r.AsOf.In(londonLoc).Format("Mon 2 Jan 15:04") + ")"
}

type Consent struct {
	Scope     string
	GrantedAt *time.Time
	RevokedAt *time.Time
	Source    string
}

func (c Consent) Active() bool {
	return c.GrantedAt != nil && (c.RevokedAt == nil || c.RevokedAt.Before(*c.GrantedAt))
}

// SetConsent grants or revokes a scope for one user.
func (s *Store) SetConsent(userID int64, scope string, granted bool, source string) error {
	if !validSignalKind(scope) {
		return errors.New("unknown consent scope " + scope)
	}
	now := time.Now().UTC()
	var grantedAt, revokedAt any
	if granted {
		grantedAt = now
	} else {
		revokedAt = now
	}
	upsert := s.insertIgnoreSQL(`INSERT INTO consents (user_id, scope, granted_at, revoked_at, source) VALUES (?,?,?,?,?)`)
	if _, err := s.exec(upsert, userID, scope, grantedAt, revokedAt, source); err != nil {
		return err
	}
	if granted {
		_, err := s.exec(`UPDATE consents SET granted_at=?, revoked_at=NULL, source=?, updated_at=? WHERE user_id=? AND scope=?`,
			now, source, now, userID, scope)
		return err
	}
	_, err := s.exec(`UPDATE consents SET revoked_at=?, updated_at=? WHERE user_id=? AND scope=?`, now, now, userID, scope)
	return err
}

// Consents lists every scope decision this user has made.
func (s *Store) Consents(userID int64) ([]Consent, error) {
	rows, err := s.query(`SELECT scope, granted_at, revoked_at, source FROM consents WHERE user_id=? ORDER BY scope`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Consent
	for rows.Next() {
		var c Consent
		var granted, revoked sql.NullTime
		if err := rows.Scan(&c.Scope, &granted, &revoked, &c.Source); err != nil {
			return nil, err
		}
		if granted.Valid {
			t := granted.Time
			c.GrantedAt = &t
		}
		if revoked.Valid {
			t := revoked.Time
			c.RevokedAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// HasConsent reports whether the scope is currently granted for this user.
func (s *Store) HasConsent(userID int64, scope string) bool {
	consents, err := s.Consents(userID)
	if err != nil {
		return false
	}
	for _, c := range consents {
		if c.Scope == scope {
			return c.Active()
		}
	}
	return false
}

// IngestSignal stores one observation. It refuses without active consent, and
// refuses an observation with no source or no observation time: a value we
// cannot attribute is worse than no value.
func (s *Store) IngestSignal(userID int64, sig Signal) (*Signal, error) {
	if !validSignalKind(sig.Kind) {
		return nil, errors.New("unknown signal kind " + sig.Kind)
	}
	if !s.HasConsent(userID, sig.Kind) {
		return nil, errNoConsent
	}
	if strings.TrimSpace(sig.Source) == "" {
		return nil, errors.New("signal needs a source")
	}
	if strings.TrimSpace(sig.Value) == "" {
		return nil, errors.New("signal needs a value")
	}
	if sig.ObservedAt.IsZero() {
		return nil, errors.New("signal needs an observed_at timestamp")
	}
	sig.UserID = userID
	sig.IngestedAt = time.Now().UTC()
	sig.ExpiresAt = sig.ObservedAt.Add(signalTTL[sig.Kind]).UTC()
	id, err := s.insert(`INSERT INTO signals (user_id, kind, value, unit, source, observed_at, ingested_at, expires_at) VALUES (?,?,?,?,?,?,?,?)`,
		userID, sig.Kind, sig.Value, sig.Unit, sig.Source, sig.ObservedAt.UTC(), sig.IngestedAt, sig.ExpiresAt)
	if err != nil {
		return nil, err
	}
	sig.ID = id
	return &sig, nil
}

// LatestSignal returns the most recent observation of a kind for this user,
// as a Reading that is explicit about not knowing.
func (s *Store) LatestSignal(userID int64, kind string, now time.Time) Reading {
	if !s.HasConsent(userID, kind) {
		return Reading{Kind: kind, Unknown: "no consent on file"}
	}
	row := s.queryRow(`SELECT id, user_id, kind, value, unit, source, observed_at, ingested_at, expires_at
		FROM signals WHERE user_id=? AND kind=? ORDER BY observed_at DESC LIMIT 1`, userID, kind)
	var sig Signal
	if err := row.Scan(&sig.ID, &sig.UserID, &sig.Kind, &sig.Value, &sig.Unit, &sig.Source,
		&sig.ObservedAt, &sig.IngestedAt, &sig.ExpiresAt); err != nil {
		return Reading{Kind: kind, Unknown: "no data received"}
	}
	if !sig.Fresh(now) {
		return Reading{Kind: kind, Unknown: "stale, last observed " + sig.ObservedAt.In(londonLoc).Format("Mon 2 Jan 15:04")}
	}
	return Reading{
		Kind: kind, Known: true, Value: sig.Value, Unit: sig.Unit,
		Source: sig.Source, AsOf: sig.ObservedAt,
	}
}

// Readings returns every signal kind for a user, in a stable order.
func (s *Store) Readings(userID int64, now time.Time) []Reading {
	kinds := make([]string, 0, len(signalTTL))
	for k := range signalTTL {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	out := make([]Reading, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, s.LatestSignal(userID, k, now))
	}
	return out
}

// PurgeExpiredSignals deletes observations past their retention window. It is
// the data-minimisation half of the consent story: consent controls what comes
// in, retention controls how long it stays.
func (s *Store) PurgeExpiredSignals(now time.Time) (int64, error) {
	var total int64
	for kind, keep := range signalRetention {
		res, err := s.exec(`DELETE FROM signals WHERE kind=? AND observed_at < ?`, kind, now.Add(-keep).UTC())
		if err != nil {
			return total, err
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	return total, nil
}

// sweep is one pass of every retention rule: sensitive observations past their
// window, transcripts past ninety days, and spent codes, dead sessions and old
// audit rows. Each is independent, so one failure must not skip the rest.
func (s *Store) sweep(now time.Time) {
	if n, err := s.PurgeExpiredSignals(now); err != nil {
		log.Printf("retention: signals: %v", err)
	} else if n > 0 {
		log.Printf("retention: deleted %d expired signals", n)
	}
	if n, err := s.PurgeExpiredTranscripts(now); err != nil {
		log.Printf("retention: transcripts: %v", err)
	} else if n > 0 {
		log.Printf("retention: deleted %d expired transcripts", n)
	}
	if err := s.PurgeExpiredAuth(now); err != nil {
		log.Printf("retention: auth: %v", err)
	}
}

// RunRetention sweeps expired data hourly until stop closes.
func (s *Store) RunRetention(stop <-chan struct{}) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.sweep(time.Now())
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// ForgetUser deletes everything held about one person. Foreign keys cascade
// from users, so this is a single statement and leaves nothing behind.
func (s *Store) ForgetUser(userID int64) error {
	return s.tx(func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`DELETE FROM signals WHERE user_id=?`,
			`DELETE FROM consents WHERE user_id=?`,
			`DELETE FROM checklist_items WHERE user_id=?`,
			`DELETE FROM sessions WHERE user_id=?`,
			`DELETE FROM messages WHERE user_id=?`,
			`DELETE FROM checkins WHERE user_id=?`,
			`DELETE FROM suggestions WHERE user_id=?`,
			`DELETE FROM webhook_events WHERE user_id=?`,
			`DELETE FROM transcripts WHERE user_id=?`,
			// Including any call still held against the number but never
			// attached to the profile: forgetting has to mean the number too.
			`DELETE FROM transcripts WHERE user_id IS NULL AND phone IN (SELECT phone FROM users WHERE id=?)`,
			`DELETE FROM auth_sessions WHERE user_id=?`,
			`DELETE FROM auth_audit WHERE user_id=?`,
			// Login codes are keyed by number rather than by user, so they are
			// cleared through the profile before it goes.
			`DELETE FROM login_codes WHERE phone IN (SELECT phone FROM users WHERE id=?)`,
			`DELETE FROM auth_audit WHERE phone IN (SELECT phone FROM users WHERE id=?)`,
			`DELETE FROM users WHERE id=?`,
		} {
			if _, err := s.txExec(tx, stmt, userID); err != nil {
				return err
			}
		}
		return nil
	})
}

// RememberWebhook records an idempotency key for an endpoint and reports
// whether this delivery is the first one. Providers retry; a retry must not
// write a second check-in or advance the checklist twice.
func (s *Store) RememberWebhook(endpoint, key string, userID int64, response string) (first bool, cached string, err error) {
	row := s.queryRow(`SELECT response FROM webhook_events WHERE endpoint=? AND idempotency_key=?`, endpoint, key)
	if err := row.Scan(&cached); err == nil {
		return false, cached, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, "", err
	}
	insert := s.insertIgnoreSQL(`INSERT INTO webhook_events (endpoint, idempotency_key, user_id, response) VALUES (?,?,?,?)`)
	res, err := s.exec(insert, endpoint, key, userID, response)
	if err != nil {
		return false, "", err
	}
	// A concurrent duplicate loses the insert race and must be treated as a
	// repeat, not as the first delivery.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		row := s.queryRow(`SELECT response FROM webhook_events WHERE endpoint=? AND idempotency_key=?`, endpoint, key)
		_ = row.Scan(&cached)
		return false, cached, nil
	}
	return true, "", nil
}
