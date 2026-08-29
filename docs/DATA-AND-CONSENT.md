# Data, identity, consent and retention

CheckIn ingests what a person tells it and what they have explicitly connected.
It does not infer, guess or remember anything else. This document is the
contract: schema, who can read what, how long it lives, and what happens when
data is missing.

## Identity

A person **is** a verified phone number. `users.phone` is unique and is the only
lookup that resolves a person (`Store.UserByPhone`); there is deliberately no
lookup by name, because two people share a name and nobody shares a number.

- `users.name` is a display label. It is empty until the person gives it, and
  the model is told `name: (unknown)` until then. No name is ever hard-coded.
- `phone_verified_at` / `phone_verified_via` record how the number was proved.
  Today the only trusted proof is a Twilio-signed inbound SMS
  (`twilio_inbound_sms`): the signature is checked against `TWILIO_AUTH_TOKEN`
  over the public URL plus the sorted form body, so an unsigned POST cannot
  claim a number. Web signup and tool webhooks create an *unverified* user.
- A new user starts with no name, no history, no signals and no assumed
  context. The prompt states this explicitly rather than leaving it implied.

### Tenant isolation

Every user-scoped read and write takes a `user_id` and includes it in the
`WHERE` clause — including mutations that already have a primary key
(`SetSuggestionStatus(userID, id, status)` returns `sql.ErrNoRows` rather than
touching another tenant's row). Checklist items are scoped by `user_id` *and*
`session_id`. `isolation_test.go` proves two users cannot read or mutate each
other's profile, journal, messages, sessions, checklist, signals or
recommendations.

## Storage backends

| Config | Backend |
| --- | --- |
| `DATABASE_URL` set | Postgres (production; the Pi runs Postgres 16 on `127.0.0.1:5433`) |
| otherwise | SQLite file at `DATABASE_PATH` (local dev, demo fallback) |

One SQL dialect is written once and rendered per backend (`store_sql.go`):
`?` placeholders are rebound to `$n`, `{{pk}}` becomes `INTEGER PRIMARY KEY
AUTOINCREMENT` or `BIGSERIAL PRIMARY KEY`, and inserts use `RETURNING id` on
both. Migrations run at boot, in order, inside a transaction, and are
idempotent — re-opening an existing database is a no-op. Constraints are in the
schema, not in Go: foreign keys with `ON DELETE CASCADE`, `CHECK` constraints on
every status/enum column, unique constraints on `users.phone`,
`(user_id, session_id, key)` and `(endpoint, key)`, and indexes on every foreign
key used for lookup.

`DATABASE_URL` must be **exported** to the service process; a value that merely
sits in the env file silently falls back to SQLite. The log line at boot states
which backend was selected (host and database only — never the password).

## Structured signals

Instead of inferring how someone is doing, CheckIn reads signals that were
explicitly connected. Four kinds are modelled:

| Kind | `kind` | Freshness (treated unknown after) | Retention (deleted after) |
| --- | --- | --- | --- |
| Heart rate / wellness | `heart_rate` | 2 h | 7 days |
| Current or recent geography | `location` | 6 h | 24 h |
| Calendar availability / free times | `calendar` | 12 h | 7 days |
| Commitments made earlier in the week | `commitments` | 7 days | 30 days |

Every row carries provenance: `user_id`, `kind`, `value`, `unit`, `source`
(the provider), `observed_at` (when the *provider* measured it), `ingested_at`
(when we received it) and `expires_at` (retention deadline). A reading is only
returned as known when consent is currently granted, the row exists, and it is
inside the freshness window. Otherwise the context block says
`unknown (no consent on file)` / `unknown (stale: last seen …)` — never a
plausible number.

### Consent

`consents(user_id, scope, granted, source, updated_at)` gates every kind.
`IngestSignal` refuses to write without a granted consent for that kind, and
`LatestSignal` refuses to read when consent is revoked, even if rows still
exist. Consent is recorded with its source (e.g. `sms:yes`) so it can be
audited. Revocation is immediate and does not require deletion.

### Deletion

- `POST /forget {phone}` erases the user and everything referencing them
  (profile, journal, messages, sessions, checklist, consents, signals,
  suggestions, call transcripts, sign-in sessions, outstanding login codes and
  their audit rows) in one transaction. Only the user id is logged.
- A retention sweeper runs hourly and deletes signals past `expires_at`, so
  sensitive observations expire without anyone asking. The same sweep drops
  transcripts older than 90 days, expired or revoked sign-in sessions, spent
  login codes, and audit rows older than 180 days.
- A signed-in user can delete any single transcript themselves from the
  dashboard (`DELETE /api/transcripts/{id}`), scoped to their own id.

### Ingestion API

`POST /ingest` (shared secret, same header as `/tools/*`):

```json
{"phone":"+447700900123","kind":"heart_rate","value":"58","unit":"bpm",
 "source":"whoop","observed_at":"2026-08-29T09:12:00Z","idempotency_key":"whoop-1"}
```

`observed_at` is required and must be RFC3339 — a signal with no source
timestamp is not a fact, it is a rumour. `source` is required. Unknown phone
numbers are rejected rather than auto-created. `idempotency_key` makes retried
deliveries safe. `POST /signals {phone}` returns every kind with `known`,
`value`, `observed_at`, `expires_at` and `unknown_because`.

**Provider prerequisites / limitations (operator side).** No wellness or
location provider is connected in this deployment, so heart rate and location
read as unknown in production today. `/ingest` is the authorized intake: a
Whoop/Fitbit/Apple Health export job, or the phone itself, posts to it with the
shared secret. Calendar has a second, already-wired path — an ICS URL per user
(`users.ics_url`, else `GCAL_ICS_URL`), cached 5 minutes, parsed for today in
`Europe/London`. Commitments are derived only from what the person actually said
in a check-in; nothing scrapes them. Signals are stored in plaintext in the
database; for anything beyond a demo, put them in a Postgres instance with
disk encryption and restrict the role to the application.

## Checklist interview

Preferences are collected by a state machine, not by the model. The template, in
order:

1. `event_types` — "What type of events do you like to go to?"
2. `event_time` — "What time do you like to go to events?"
3. `evening_availability` — "Are you free for an event with like-minded people at 7 PM?"
4. `notify_watch` — "What should we keep our eyes out for to notify you?"

Rules enforced in code (`checklist.go`):

- One question per turn. The next item is only asked once the current one is
  settled.
- Four states: `unanswered`, `answered`, `skipped`, `declined`. Skipped and
  declined settle the item **without** becoming a stated preference — a skipped
  question never reads back as an answer, and never writes `users.interests`.
- An empty or silent reply settles nothing; the question is re-asked. Answers
  are never inferred from tone, silence, calendar, location or another user.
- `RecordChecklistAnswer` refuses to write an item that is not the current one,
  so the voice agent cannot answer question three while question one is open.
- State is keyed by `(user_id, session_id)` and survives across webhook calls,
  so SMS and voice resume where they left off.

Only `event_types` is denormalised onto `users.interests`, because matching
reads it on every suggestion. Everything else stays in `checklist_items` with
its status attached.

Voice uses the same machine over `POST /tools/next_question` and
`POST /tools/save_answer` (which returns the next question, or a conflict if the
agent answers out of order).

## Call transcripts and sign-in

| Data | Kept for | Stored as |
| --- | --- | --- |
| Call transcript (text, summary, metadata) | 90 days | Plain text, owned by one `user_id` |
| Sign-in session | 30 days, or until logout | SHA-256 of the token, never the token |
| One-time login code | 10 minutes, single use | SHA-256 of the code, never the code |
| Auth audit row (requested, failed, signed in, signed out, deleted) | 180 days | Phone + event, no code, no token |

- A transcript is filed against the number the provider actually dialled. A
  delivery whose number matches no user is dropped: a transcript never creates a
  profile, and is never attached to a guess.
- Transcripts are visible only to the signed-in owner. Every read, search and
  delete carries `user_id` in its `WHERE` clause, and another user's transcript
  returns "not found" rather than "forbidden", so its existence is not
  disclosed.
- A code is generated with `crypto/rand`, exists in the clear only in the SMS,
  and is never logged or included in an HTTP response. Requesting a code for an
  unregistered number returns exactly what a registered one returns.
- Signing in marks the number verified (`phone_verified_via=otp`) and resumes
  the existing profile and checklist state — a returning user is never asked to
  onboard twice.

## Webhook idempotency

`webhook_events(endpoint, key, user_id, response, created_at)` with a unique
`(endpoint, key)`. Inbound SMS keys on Twilio's `MessageSid`; tool and ingest
calls key on an optional `idempotency_key`. A retried delivery replays the
stored response instead of taking a second conversation turn or writing a second
signal.

## Acting on the user's behalf

Model output alone never triggers an external action. Signing someone up for an
event requires an explicit accepted suggestion (`suggestions.status`), and
notifications require the person to have said yes. The system prompt states the
rule, and the state machine — not the prompt — is what actually gates it.
