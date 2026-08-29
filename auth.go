package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"math/big"
	"strings"
	"time"
)

// Signing in is the same act as being called: you prove you hold the number.
// A code is sent to it, and only its hash is ever written down, so nothing in
// the database can be replayed against the person who owns that number.
const (
	loginCodeLength   = 6
	loginCodeTTL      = 10 * time.Minute
	loginCodeAttempts = 5
	// loginCodeRate is how many codes one number can request inside
	// loginCodeWindow before it has to wait.
	loginCodeRate   = 3
	loginCodeWindow = 15 * time.Minute
	// loginCodeCooldown is the wait between two codes for the same number. It
	// is short enough to survive a slow SMS and long enough that the resend
	// button cannot be used to spray one number with texts.
	loginCodeCooldown   = 30 * time.Second
	authSessionTTL      = 30 * 24 * time.Hour
	authSessionCookie   = "checkin_session"
	authAuditRetention  = 180 * 24 * time.Hour
	authTokenByteLength = 32
)

var (
	errRateLimited  = errors.New("too many codes requested for this number")
	errCodeInvalid  = errors.New("that code is not right")
	errCodeExpired  = errors.New("that code has expired")
	errCodeAttempts = errors.New("too many attempts on this code")
	errNoSession    = errors.New("not signed in")
)

// AuthSession is a signed-in dashboard session. The token itself exists only
// in the caller's cookie; the row holds a hash of it.
type AuthSession struct {
	ID        int64
	UserID    int64
	ExpiresAt time.Time
	RevokedAt *time.Time
}

func (a AuthSession) Active(now time.Time) bool {
	return a.RevokedAt == nil && now.Before(a.ExpiresAt)
}

// hashSecret is used for both codes and session tokens. Codes are short-lived
// and rate-limited rather than password-like, so a fast hash is appropriate:
// the guessing budget is five attempts in ten minutes, not offline brute force.
func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newLoginCode returns a uniformly random numeric code. It is returned to the
// caller for delivery and never stored, logged, or echoed in a response.
func newLoginCode() (string, error) {
	var b strings.Builder
	for i := 0; i < loginCodeLength; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		b.WriteString(n.String())
	}
	return b.String(), nil
}

func newSessionToken() (string, error) {
	buf := make([]byte, authTokenByteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// RecordAuthEvent appends to the audit trail. It records what happened to
// which number, never the code itself.
func (s *Store) RecordAuthEvent(phone string, userID int64, event, detail string) {
	var owner any
	if userID != 0 {
		owner = userID
	}
	if _, err := s.exec(`INSERT INTO auth_audit (phone, user_id, event, detail) VALUES (?,?,?,?)`,
		phone, owner, event, detail); err != nil {
		log.Printf("auth audit %s: %v", event, err)
	}
}

// AuthEvents returns the audit trail for a number, newest first.
func (s *Store) AuthEvents(phone string, limit int) ([]string, error) {
	rows, err := s.query(`SELECT event FROM auth_audit WHERE phone=? ORDER BY created_at DESC, id DESC LIMIT ?`, phone, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// IssueLoginCode mints a code for a number and returns it for delivery. Any
// code already outstanding for that number is consumed, so the most recent one
// is the only one that works.
func (s *Store) IssueLoginCode(phone string, now time.Time) (string, error) {
	var recent int
	if err := s.queryRow(`SELECT COUNT(*) FROM login_codes WHERE phone=? AND created_at > ?`,
		phone, now.Add(-loginCodeWindow).UTC()).Scan(&recent); err != nil {
		return "", err
	}
	if recent >= loginCodeRate {
		return "", errRateLimited
	}
	code, err := newLoginCode()
	if err != nil {
		return "", err
	}
	err = s.tx(func(tx *sql.Tx) error {
		if _, err := s.txExec(tx, `UPDATE login_codes SET consumed_at=? WHERE phone=? AND consumed_at IS NULL`, now.UTC(), phone); err != nil {
			return err
		}
		_, err := s.txExec(tx, `INSERT INTO login_codes (phone, code_hash, created_at, expires_at) VALUES (?,?,?,?)`,
			phone, hashSecret(code), now.UTC(), now.Add(loginCodeTTL).UTC())
		return err
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

// DiscardLoginCode removes a code that was minted but never reached anybody,
// which is what an SMS provider failure leaves behind. Deleting it rather than
// consuming it also hands the number its rate-limit slot back, so a provider
// outage does not lock somebody out of their own account for fifteen minutes.
func (s *Store) DiscardLoginCode(phone string) error {
	_, err := s.exec(`DELETE FROM login_codes WHERE phone=? AND consumed_at IS NULL`, phone)
	return err
}

// ConsumeLoginCode checks a code and burns it. A correct code works exactly
// once; a wrong one costs an attempt, and the fifth wrong attempt burns the
// code entirely so guessing cannot continue for the rest of its lifetime.
func (s *Store) ConsumeLoginCode(phone, code string, now time.Time) error {
	row := s.queryRow(`SELECT id, code_hash, attempts, expires_at FROM login_codes
		WHERE phone=? AND consumed_at IS NULL ORDER BY created_at DESC, id DESC LIMIT 1`, phone)
	var (
		id       int64
		hash     string
		attempts int
		expires  time.Time
	)
	if err := row.Scan(&id, &hash, &attempts, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errCodeInvalid
		}
		return err
	}
	if now.After(expires) {
		_, _ = s.exec(`UPDATE login_codes SET consumed_at=? WHERE id=?`, now.UTC(), id)
		return errCodeExpired
	}
	if attempts >= loginCodeAttempts {
		_, _ = s.exec(`UPDATE login_codes SET consumed_at=? WHERE id=?`, now.UTC(), id)
		return errCodeAttempts
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashSecret(code))) != 1 {
		if _, err := s.exec(`UPDATE login_codes SET attempts=attempts+1 WHERE id=?`, id); err != nil {
			return err
		}
		if attempts+1 >= loginCodeAttempts {
			_, _ = s.exec(`UPDATE login_codes SET consumed_at=? WHERE id=?`, now.UTC(), id)
		}
		return errCodeInvalid
	}
	_, err := s.exec(`UPDATE login_codes SET consumed_at=? WHERE id=? AND consumed_at IS NULL`, now.UTC(), id)
	return err
}

// StartAuthSession issues a session token for a user and returns it once. Only
// its hash is stored.
func (s *Store) StartAuthSession(userID int64, now time.Time) (string, error) {
	token, err := newSessionToken()
	if err != nil {
		return "", err
	}
	if _, err := s.exec(`INSERT INTO auth_sessions (user_id, token_hash, created_at, expires_at, last_seen_at) VALUES (?,?,?,?,?)`,
		userID, hashSecret(token), now.UTC(), now.Add(authSessionTTL).UTC(), now.UTC()); err != nil {
		return "", err
	}
	return token, nil
}

// AuthSessionUser resolves a token to its owner. An expired or revoked session
// resolves to nobody, and touching it updates last_seen_at so an operator can
// see which sessions are actually in use.
func (s *Store) AuthSessionUser(token string, now time.Time) (*User, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errNoSession
	}
	row := s.queryRow(`SELECT id, user_id, expires_at, revoked_at FROM auth_sessions WHERE token_hash=?`, hashSecret(token))
	var sess AuthSession
	if err := row.Scan(&sess.ID, &sess.UserID, &sess.ExpiresAt, &sess.RevokedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNoSession
		}
		return nil, err
	}
	if !sess.Active(now) {
		return nil, errNoSession
	}
	if _, err := s.exec(`UPDATE auth_sessions SET last_seen_at=? WHERE id=?`, now.UTC(), sess.ID); err != nil {
		return nil, err
	}
	u, err := s.UserByID(sess.UserID)
	if err != nil {
		return nil, err
	}
	// A session only ever means "this number was proved". If the verification
	// is gone from the profile, so is the session: the token cannot outlive the
	// fact it stands for.
	if u == nil || !u.PhoneVerified() {
		return nil, errNoSession
	}
	return u, nil
}

// RevokeAuthSession ends one session. Signing out is a revocation rather than
// a delete so the audit trail still shows the session existed.
func (s *Store) RevokeAuthSession(token string, now time.Time) error {
	_, err := s.exec(`UPDATE auth_sessions SET revoked_at=? WHERE token_hash=? AND revoked_at IS NULL`, now.UTC(), hashSecret(token))
	return err
}

// RevokeAllAuthSessions ends every session a user has, which is what somebody
// who has lost a device needs.
func (s *Store) RevokeAllAuthSessions(userID int64, now time.Time) (int64, error) {
	res, err := s.exec(`UPDATE auth_sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now.UTC(), userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeExpiredAuth clears out spent codes, dead sessions and stale audit rows.
func (s *Store) PurgeExpiredAuth(now time.Time) error {
	for _, stmt := range []struct {
		q   string
		arg time.Time
	}{
		{`DELETE FROM login_codes WHERE expires_at < ?`, now.Add(-loginCodeWindow).UTC()},
		{`DELETE FROM auth_sessions WHERE expires_at < ?`, now.UTC()},
		{`DELETE FROM auth_audit WHERE created_at < ?`, now.Add(-authAuditRetention).UTC()},
	} {
		if _, err := s.exec(stmt.q, stmt.arg); err != nil {
			return err
		}
	}
	return nil
}
