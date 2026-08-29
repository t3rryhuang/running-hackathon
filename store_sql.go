package main

import (
	"database/sql"
	"fmt"
	"log"
	neturl "net/url"
	"strconv"
	"strings"

	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

// Dialect is the SQL flavour behind the store. Postgres is the persistent
// production backend; SQLite keeps a laptop or a Pi without a database server
// running with no configuration at all.
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

// portable DDL tokens, expanded per dialect. Writing the schema once and
// substituting the three things that actually differ keeps the two backends
// from drifting apart.
var ddlTokens = map[Dialect]*strings.Replacer{
	DialectSQLite: strings.NewReplacer(
		"{{pk}}", "INTEGER PRIMARY KEY AUTOINCREMENT",
		"{{ts}}", "DATETIME",
		"{{text}}", "TEXT",
	),
	DialectPostgres: strings.NewReplacer(
		"{{pk}}", "BIGSERIAL PRIMARY KEY",
		"{{ts}}", "TIMESTAMPTZ",
		"{{text}}", "TEXT",
	),
}

type migration struct {
	Version int
	Name    string
	SQL     []string
}

// migrationList is append-only: every deployment replays the versions it has
// not applied yet inside one transaction each, so a half-applied migration
// never leaves the schema in an undefined state.
var migrationList = []migration{
	{Version: 1, Name: "core", SQL: []string{`
CREATE TABLE IF NOT EXISTS users (
	id {{pk}},
	phone {{text}} UNIQUE NOT NULL,
	name {{text}} NOT NULL DEFAULT '',
	channel {{text}} NOT NULL DEFAULT 'sms' CHECK (channel IN ('call','sms')),
	frequency {{text}} NOT NULL DEFAULT 'daily',
	ics_url {{text}},
	interests {{text}} NOT NULL DEFAULT '',
	onboarded_at {{ts}},
	last_call_sid {{text}},
	last_triggered_at {{ts}},
	created_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP
)`, `
CREATE TABLE IF NOT EXISTS checkins (
	id {{pk}},
	user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	mood INTEGER CHECK (mood IS NULL OR (mood BETWEEN 1 AND 5)),
	summary {{text}} NOT NULL DEFAULT '',
	topics {{text}} NOT NULL DEFAULT '',
	raw {{text}} NOT NULL DEFAULT '',
	created_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP
)`, `
CREATE TABLE IF NOT EXISTS events (
	id {{pk}},
	title {{text}} NOT NULL,
	starts_at {{ts}} NOT NULL,
	city {{text}} NOT NULL DEFAULT 'London',
	url {{text}} NOT NULL DEFAULT '',
	tags {{text}} NOT NULL DEFAULT ''
)`, `
CREATE TABLE IF NOT EXISTS suggestions (
	id {{pk}},
	user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	event_id BIGINT NOT NULL REFERENCES events(id),
	status {{text}} NOT NULL CHECK (status IN ('offered','accepted','declined')),
	created_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP
)`, `
CREATE TABLE IF NOT EXISTS messages (
	id {{pk}},
	user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	role {{text}} NOT NULL CHECK (role IN ('user','assistant')),
	body {{text}} NOT NULL,
	created_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP
)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS events_unique ON events (title, starts_at)`,
		`CREATE INDEX IF NOT EXISTS checkins_user ON checkins (user_id, id DESC)`,
		`CREATE INDEX IF NOT EXISTS messages_user ON messages (user_id, id DESC)`,
		`CREATE INDEX IF NOT EXISTS suggestions_user ON suggestions (user_id, status)`,
	}},

	// Identity: a phone number is only an identity once something proved the
	// person holds it. Names are never an identifier, only a display value.
	{Version: 2, Name: "verified_identity", SQL: []string{
		`ALTER TABLE users ADD COLUMN phone_verified_at {{ts}}`,
		`ALTER TABLE users ADD COLUMN phone_verified_via {{text}}`,
	}},

	// Sessions and the checklist interview.
	{Version: 3, Name: "checklist", SQL: []string{`
CREATE TABLE IF NOT EXISTS sessions (
	id {{pk}},
	user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	channel {{text}} NOT NULL CHECK (channel IN ('call','sms','web')),
	state {{text}} NOT NULL DEFAULT 'open' CHECK (state IN ('open','closed')),
	started_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP,
	ended_at {{ts}}
)`, `
CREATE TABLE IF NOT EXISTS checklist_items (
	id {{pk}},
	user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	session_id BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	item_key {{text}} NOT NULL,
	position INTEGER NOT NULL,
	prompt {{text}} NOT NULL,
	status {{text}} NOT NULL DEFAULT 'unanswered' CHECK (status IN ('unanswered','answered','skipped','declined')),
	answer {{text}} NOT NULL DEFAULT '',
	asked_at {{ts}},
	answered_at {{ts}},
	updated_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP
)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS checklist_unique ON checklist_items (user_id, session_id, item_key)`,
		`CREATE INDEX IF NOT EXISTS sessions_user ON sessions (user_id, state)`,
	}},

	// Consent and ingested signals.
	{Version: 4, Name: "signals", SQL: []string{`
CREATE TABLE IF NOT EXISTS consents (
	id {{pk}},
	user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	scope {{text}} NOT NULL CHECK (scope IN ('heart_rate','location','calendar','commitments')),
	granted_at {{ts}},
	revoked_at {{ts}},
	source {{text}} NOT NULL DEFAULT '',
	updated_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP
)`, `
CREATE TABLE IF NOT EXISTS signals (
	id {{pk}},
	user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	kind {{text}} NOT NULL CHECK (kind IN ('heart_rate','location','calendar','commitments')),
	value {{text}} NOT NULL,
	unit {{text}} NOT NULL DEFAULT '',
	source {{text}} NOT NULL,
	observed_at {{ts}} NOT NULL,
	ingested_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP,
	expires_at {{ts}} NOT NULL
)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS consents_unique ON consents (user_id, scope)`,
		`CREATE INDEX IF NOT EXISTS signals_lookup ON signals (user_id, kind, observed_at DESC)`,
	}},

	// Webhook idempotency: providers retry, and a retried save must not write
	// a second check-in or advance the checklist twice.
	{Version: 5, Name: "idempotency", SQL: []string{`
CREATE TABLE IF NOT EXISTS webhook_events (
	id {{pk}},
	idempotency_key {{text}} NOT NULL,
	endpoint {{text}} NOT NULL,
	user_id BIGINT REFERENCES users(id) ON DELETE CASCADE,
	response {{text}} NOT NULL DEFAULT '',
	created_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP
)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS webhook_events_unique ON webhook_events (endpoint, idempotency_key)`,
	}},

	// Where an answer came from. A value the person typed into the signup form
	// is already known and must not be asked again on the phone, but it is not
	// the same as one they said in this conversation, so it is recorded as
	// such rather than silently presented as something they told the agent.
	{Version: 6, Name: "answer_provenance", SQL: []string{
		`ALTER TABLE checklist_items ADD COLUMN source {{text}} NOT NULL DEFAULT 'conversation'`,
	}},

	// Onboarding and check-ins are different conversations with different
	// question lists, so a session says which one it is. Sessions open under
	// the old single list are closed rather than relabelled: their checklist
	// rows belong to a template that no longer exists, and finishing them
	// would mix the two flows exactly as this migration exists to prevent.
	{Version: 7, Name: "session_flow", SQL: []string{
		`ALTER TABLE sessions ADD COLUMN flow {{text}} NOT NULL DEFAULT 'checkin'`,
		`UPDATE sessions SET state='closed', ended_at=CURRENT_TIMESTAMP WHERE state='open'`,
		`CREATE INDEX IF NOT EXISTS sessions_user_flow ON sessions (user_id, flow, state)`,
	}},

	// Post-call transcripts, delivered by ElevenLabs after the call ends. A
	// transcript belongs to exactly one user, resolved from the number that
	// was dialled; a delivery whose number matches nobody is dropped rather
	// than filed against a guess. conversation_id is unique so a retried
	// delivery updates the same row instead of adding a second copy.
	{Version: 8, Name: "transcripts", SQL: []string{`
CREATE TABLE IF NOT EXISTS transcripts (
	id {{pk}},
	user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	conversation_id {{text}} NOT NULL,
	call_sid {{text}} NOT NULL DEFAULT '',
	direction {{text}} NOT NULL DEFAULT 'outbound',
	status {{text}} NOT NULL DEFAULT '',
	summary {{text}} NOT NULL DEFAULT '',
	body {{text}} NOT NULL DEFAULT '',
	turns INTEGER NOT NULL DEFAULT 0,
	duration_seconds INTEGER NOT NULL DEFAULT 0,
	started_at {{ts}} NOT NULL,
	received_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP
)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS transcripts_conversation ON transcripts (conversation_id)`,
		`CREATE INDEX IF NOT EXISTS transcripts_user ON transcripts (user_id, started_at DESC)`,
	}},

	// Phone + one-time code sign-in. Codes are stored only as a hash, so a
	// dump of this table cannot be replayed, and each row records its own
	// attempt count so a code cannot be brute-forced within its lifetime.
	// Dashboard sessions are the same shape: only the hash of the token is
	// kept, and revoking is a timestamp rather than a delete so the audit
	// trail survives a logout.
	{Version: 9, Name: "auth", SQL: []string{`
CREATE TABLE IF NOT EXISTS login_codes (
	id {{pk}},
	phone {{text}} NOT NULL,
	code_hash {{text}} NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	created_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP,
	expires_at {{ts}} NOT NULL,
	consumed_at {{ts}}
)`, `
CREATE TABLE IF NOT EXISTS auth_sessions (
	id {{pk}},
	user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token_hash {{text}} NOT NULL,
	created_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP,
	expires_at {{ts}} NOT NULL,
	last_seen_at {{ts}},
	revoked_at {{ts}}
)`, `
CREATE TABLE IF NOT EXISTS auth_audit (
	id {{pk}},
	phone {{text}} NOT NULL DEFAULT '',
	user_id BIGINT REFERENCES users(id) ON DELETE SET NULL,
	event {{text}} NOT NULL,
	detail {{text}} NOT NULL DEFAULT '',
	created_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP
)`,
		`CREATE INDEX IF NOT EXISTS login_codes_phone ON login_codes (phone, created_at DESC)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS auth_sessions_token ON auth_sessions (token_hash)`,
		`CREATE INDEX IF NOT EXISTS auth_sessions_user ON auth_sessions (user_id, expires_at DESC)`,
		`CREATE INDEX IF NOT EXISTS auth_audit_lookup ON auth_audit (phone, created_at DESC)`,
	}},

	// Every call has to end up on the dashboard, including the ones that
	// happened before the number signed up. A transcript therefore carries the
	// number it belongs to and may sit without an owner until that number
	// verifies, at which point it is adopted. The table is rebuilt rather than
	// altered because SQLite cannot drop a NOT NULL in place, and `source`
	// records whether the row arrived by webhook or by the provider sync.
	{Version: 10, Name: "unowned_transcripts", SQL: []string{`
CREATE TABLE transcripts_v2 (
	id {{pk}},
	user_id BIGINT REFERENCES users(id) ON DELETE CASCADE,
	phone {{text}} NOT NULL DEFAULT '',
	conversation_id {{text}} NOT NULL,
	call_sid {{text}} NOT NULL DEFAULT '',
	direction {{text}} NOT NULL DEFAULT 'outbound',
	status {{text}} NOT NULL DEFAULT '',
	source {{text}} NOT NULL DEFAULT 'webhook',
	summary {{text}} NOT NULL DEFAULT '',
	body {{text}} NOT NULL DEFAULT '',
	turns INTEGER NOT NULL DEFAULT 0,
	duration_seconds INTEGER NOT NULL DEFAULT 0,
	started_at {{ts}} NOT NULL,
	received_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP
)`, `
INSERT INTO transcripts_v2
	(user_id, phone, conversation_id, call_sid, direction, status, summary, body, turns, duration_seconds, started_at, received_at)
SELECT t.user_id, COALESCE(u.phone, ''), t.conversation_id, t.call_sid, t.direction, t.status,
	t.summary, t.body, t.turns, t.duration_seconds, t.started_at, t.received_at
FROM transcripts t LEFT JOIN users u ON u.id = t.user_id`,
		`DROP TABLE transcripts`,
		`ALTER TABLE transcripts_v2 RENAME TO transcripts`,
		`CREATE UNIQUE INDEX IF NOT EXISTS transcripts_conversation ON transcripts (conversation_id)`,
		`CREATE INDEX IF NOT EXISTS transcripts_user ON transcripts (user_id, started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS transcripts_phone ON transcripts (phone, started_at DESC)`,
	}},
}

// Store is the persistence layer. Every user-scoped query takes a user id and
// filters on it; nothing resolves a person by display name.
type Store struct {
	db      *sql.DB
	dialect Dialect
}

// OpenStore selects the backend from the environment: DATABASE_URL (postgres://)
// wins, otherwise the SQLite file at DATABASE_PATH.
func OpenStore(cfg Config) (*Store, error) {
	if cfg.DatabaseURL != "" {
		return openPostgres(cfg.DatabaseURL)
	}
	return openSQLite(cfg.DatabasePath)
}

// storeTarget describes the configured backend for logs without leaking the
// Postgres password.
func storeTarget(cfg Config) string {
	if cfg.DatabaseURL == "" {
		return "sqlite:" + cfg.DatabasePath
	}
	u, err := neturl.Parse(cfg.DatabaseURL)
	if err != nil {
		return "postgres"
	}
	return "postgres://" + u.Host + u.Path
}

func openSQLite(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, dialect: DialectSQLite}
	return s, s.Migrate()
}

func openPostgres(url string) (*Store, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	s := &Store{db: db, dialect: DialectPostgres}
	return s, s.Migrate()
}

func (s *Store) Close() error { return s.db.Close() }

// Migrate applies every unapplied migration, one transaction per version.
func (s *Store) Migrate() error {
	if _, err := s.db.Exec(s.ddl(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name {{text}} NOT NULL,
		applied_at {{ts}} NOT NULL DEFAULT CURRENT_TIMESTAMP)`)); err != nil {
		return err
	}
	applied := map[int]bool{}
	rows, err := s.db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, m := range migrationList {
		if applied[m.Version] {
			continue
		}
		if err := s.applyMigration(m); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.Version, m.Name, err)
		}
		log.Printf("db: applied migration %d %s", m.Version, m.Name)
	}
	return nil
}

func (s *Store) applyMigration(m migration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range m.SQL {
		if _, err := tx.Exec(s.ddl(stmt)); err != nil {
			// A database created before migrations existed already has the
			// v1/v2 columns; adding them again is the expected steady state.
			if isAlreadyExists(err) {
				continue
			}
			return err
		}
	}
	if _, err := tx.Exec(s.rebind(`INSERT INTO schema_migrations (version, name) VALUES (?,?)`), m.Version, m.Name); err != nil {
		return err
	}
	return tx.Commit()
}

func isAlreadyExists(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") || strings.Contains(msg, "already exists")
}

func (s *Store) ddl(stmt string) string { return ddlTokens[s.dialect].Replace(stmt) }

// rebind turns the portable ? placeholders into $1..$n for Postgres.
func (s *Store) rebind(q string) string {
	if s.dialect != DialectPostgres {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Store) exec(q string, args ...any) (sql.Result, error) {
	return s.db.Exec(s.rebind(q), args...)
}

func (s *Store) query(q string, args ...any) (*sql.Rows, error) {
	return s.db.Query(s.rebind(q), args...)
}

func (s *Store) queryRow(q string, args ...any) *sql.Row {
	return s.db.QueryRow(s.rebind(q), args...)
}

// insert runs an INSERT ... RETURNING id, which both backends support and
// which avoids LastInsertId (unimplemented by the Postgres driver).
func (s *Store) insert(q string, args ...any) (int64, error) {
	var id int64
	err := s.db.QueryRow(s.rebind(q+" RETURNING id"), args...).Scan(&id)
	return id, err
}

// tx runs fn in a transaction, rolling back on any error. Multi-row state
// changes (recording an answer and advancing the checklist, accepting a
// suggestion) go through it so a crash cannot leave half of the change.
func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) txExec(tx *sql.Tx, q string, args ...any) (sql.Result, error) {
	return tx.Exec(s.rebind(q), args...)
}

func (s *Store) txInsert(tx *sql.Tx, q string, args ...any) (int64, error) {
	var id int64
	err := tx.QueryRow(s.rebind(q+" RETURNING id"), args...).Scan(&id)
	return id, err
}

// insertIgnore is INSERT OR IGNORE / ON CONFLICT DO NOTHING.
func (s *Store) insertIgnoreSQL(q string) string {
	if s.dialect == DialectPostgres {
		return q + " ON CONFLICT DO NOTHING"
	}
	return strings.Replace(q, "INSERT INTO", "INSERT OR IGNORE INTO", 1)
}
