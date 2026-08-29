# CheckIn

A phone-native journalling companion. You give it your number, pick text or call, and it
checks in with you on a schedule. It remembers what you said last time, knows what was on
your calendar, and when your evening is empty it offers you one real London tech event —
and only signs you up if you say yes.

Single Go binary. Postgres in production with a pure-Go SQLite fallback, no CGO, no
frontend framework, everything embedded. Built to run on a Raspberry Pi behind Caddy.

CheckIn only knows what it was told or what was explicitly connected. People are identified
by verified phone number, preferences come from a checklist state machine rather than from
the model, and a missing or stale signal reads as *unknown* instead of being invented.
[docs/DATA-AND-CONSENT.md](docs/DATA-AND-CONSENT.md) is the contract for identity, tenant
isolation, consent, freshness, retention and deletion.

## Architecture

```mermaid
flowchart LR
    subgraph Phone["User's phone"]
        SMS[SMS]
        VOICE[Voice call]
        WEB[Browser]
    end

    subgraph Providers
        TW[Twilio<br/>+44 7414 145611]
        EL[ElevenLabs Agent<br/>STT / LLM / TTS]
        ANT[Anthropic API<br/>SMS brain]
    end

    subgraph Pi["Raspberry Pi 5 · runhack.keanuc.net"]
        CADDY[Caddy<br/>TLS + CrowdSec]
        APP[checkin<br/>single Go binary]
        PG[(Postgres 16)]
    end

    GCAL[Google Calendar ICS]
    EVENTS[(Hackathon Radar<br/>event export)]

    SMS -->|texts| TW -->|signed webhook /sms| CADDY
    VOICE <-->|PSTN| TW <-->|native integration| EL
    EL -->|tool webhooks /tools/*| CADDY
    EL -->|post-call transcript<br/>HMAC /webhooks/elevenlabs| CADDY
    WEB -->|wizard · OTP · dashboard| CADDY
    CADDY --> APP
    APP <--> PG
    APP -->|conversation| ANT
    APP -->|outbound call API| EL
    APP -.->|5-min cache| GCAL
    EVENTS -->|seed 271 events| PG
```

```mermaid
sequenceDiagram
    participant U as User
    participant W as Web wizard
    participant A as checkin (Go)
    participant T as Twilio
    participant E as ElevenLabs agent

    U->>W: phone number
    W->>A: /login start
    A->>T: send OTP
    T-->>U: SMS code
    U->>W: code + name + channel
    A->>E: outbound call (dynamic variables: greeting, ask_only…)
    E-->>U: rings, interviews one question at a time
    E->>A: save_onboarding / save_checkin (tool webhooks)
    E->>A: post-call transcript (HMAC webhook)
    U->>W: /dashboard — own transcripts, check-in button, settings
```

## Screenshots

| Sign-up | Sign-in |
| --- | --- |
| ![Sign-up wizard](docs/screenshots/home.png) | ![OTP sign-in](docs/screenshots/login.png) |

| Doc | What is in it |
| --- | --- |
| [DEPLOY.md](DEPLOY.md) | How a change actually reaches the Pi, rollback, drift detection |
| [LATENCY.md](LATENCY.md) | Where voice latency goes, measured, and what only the provider can fix |
| [docs/DATA-AND-CONSENT.md](docs/DATA-AND-CONSENT.md) | Identity, isolation, consent, retention, deletion |
| [docs/VOICE-AGENT.md](docs/VOICE-AGENT.md) | The prompt contract the voice agent must be configured with |
| [docs/DESIGN-SYSTEM.md](docs/DESIGN-SYSTEM.md), [docs/BRAND.md](docs/BRAND.md), [docs/UX-DECISIONS.md](docs/UX-DECISIONS.md) | Front-end tokens, branding, and why the UI is shaped the way it is |

## How a conversation works

**Onboarding happens on the phone, not on the website.** The web page collects a number and
a channel and nothing else; the introduction — name, what kind of events you like, when you
like to go — happens over SMS or on the call, one question at a time.

- **Onboarding** and **routine check-ins** are different flows (`FlowOnboarding` /
  `FlowCheckin` in `checklist.go`). A check-in never runs until onboarding is finished, and
  onboarding never asks a check-in question.
- Each question is asked on its own and the answer is written to that user's row before the
  next one is asked. Silence settles nothing; the question is asked again.
- Every item is `unanswered`, `answered`, `skipped` or `declined`. Skipped and declined are
  settled but not known — they never read back as a stated preference.
- Answers already on file are never asked for again, over either channel.
- An event is only ever suggested from explicitly stated interests, and is only acted on
  after an explicit yes.

## Architecture

1. `main.go` boots config → store (Postgres if `DATABASE_URL`, else SQLite) → calendar cache
   → brain → telephony → voice → HTTP mux + scheduler + retention sweeper.
2. `store_sql.go` is the backend: one migration list rendered per dialect (`?`→`$n`,
   portable primary keys, `RETURNING id`), run in a transaction at boot. `db.go` owns the
   domain queries, all scoped by `user_id`.
3. `checklist.go` is the interview state machine; `signals.go` is the consented signal store
   (heart rate, location, calendar, commitments) with freshness, retention and deletion.
4. `server.go` is the whole HTTP surface: landing page, `/signup/start`, `/signup`, `/call`, `/settings`,
   `/sms`, `/journal`, `/login`, `/dashboard`, `/healthz`, `/metrics`, `/version`, `/tools/*`,
   `/api/*`, `/webhooks/elevenlabs`.
5. `auth.go` / `auth_http.go` are phone + one-time-code sign-in and the session layer the
   dashboard sits on. `transcripts.go` ingests post-call transcripts and serves them back to
   their owner only.
6. `matching.go` decides which event a user is offered: scored on stated interests, event
   tags, city and timing, with the reasons attached. Audience-restricted events are only
   offered to people who said they are in that audience, and when nothing clears the bar the
   tools return `no_match` rather than a weak suggestion.
7. `brain.go` is the SMS brain: it builds a memory + calendar + suggestion-state preamble and
   runs an Anthropic tool-use loop over `save_checkin`, `suggest_event`, `accept_suggestion`
   — the same operations the voice agent gets over HTTP.
8. `anthropic.go`, `twilio.go` (`Telephony`), `voice.go` (`Voice`, ElevenLabs outbound calls)
   and `events.go` (`EventSource`) are all interfaces, so tests never touch a provider and an
   unconfigured box still boots.
9. `ics.go` is a dependency-free ICS parser (folded lines, `TZID`, UTC, DATE-only) that
   serves stale-while-revalidate so a slow calendar host is never dead air on a call.
10. `scheduler.go` ticks every 60s and fires each user's check-in for their frequency, using
    `users.last_triggered_at` to avoid duplicates. `timing.go` records p50/p95/max per
    operation for `/metrics`.

Templates, static assets and `events_live.csv` are `embed`ed: the binary is the whole
deployment.

**Failure posture** — every external dependency is optional at runtime. No Anthropic key →
`/sms` answers with a canned reply instead of a 500. No Twilio creds → outbound messages are
logged. No calendar → the model gets an empty calendar block. No ElevenLabs agent/phone id →
`/call` logs the attempt instead of failing the sign-up. No `ELEVENLABS_WEBHOOK_SECRET` →
transcript deliveries are rejected rather than trusted. No `ELEVENLABS_API_KEY` → transcripts
arrive by webhook only, with no backfill of calls that predate it.

## Local development

Requires Go 1.22+. Nothing else — no CGO, no node, no database needed to start.

```bash
cp .env.example .env       # fill in what you have; missing keys just disable that feature
set -a && source .env && set +a
make native && ./runhack   # listens on :8090, SQLite at ./data.db
```

Then open `http://localhost:8090`. With no Twilio and no ElevenLabs configured you can still
click through onboarding, drive the SMS flow by POSTing to `/sms`, and read `/journal`.

```bash
make build     # → runhack-arm64 (CGO_ENABLED=0 GOOS=linux GOARCH=arm64), stamped with the commit
make test      # go test ./...
make vet
gofmt -l .     # must print nothing
```

### Testing

`make test` covers the ICS parser, the SMS webhook and tool loop, the checklist state
machine, event matching (positive, excluded, ineligible, no-match), signal consent and
freshness, cross-user isolation, Twilio signature verification, webhook idempotency,
transcript ingestion and access control, sign-in and session handling, and per-endpoint
latency budgets (`latency_test.go`).

The Postgres integration test is skipped unless you point it at a throwaway database:

```bash
docker run -d --name runhack-pg-test -p 55432:5432 \
  -e POSTGRES_USER=runhack -e POSTGRES_PASSWORD=test -e POSTGRES_DB=runhack postgres:16
RUNHACK_TEST_DATABASE_URL='postgres://runhack:test@127.0.0.1:55432/runhack?sslmode=disable' \
  go test -run TestPostgresBackend .
```

No test ever contacts Twilio, ElevenLabs or Anthropic. The outbound-call request shape is
asserted against the built request, never against the live API.

## Environment variables

| Var | Required | Purpose |
| --- | --- | --- |
| `TWILIO_ACCOUNT_SID` | yes | Twilio REST auth |
| `TWILIO_AUTH_TOKEN` | yes | Twilio REST auth; also turns on inbound `/sms` signature verification |
| `TWILIO_NUMBER` | yes | Sending number (E.164) |
| `TWILIO_REGION` | no | Twilio processing region, e.g. `ie1` (Ireland). Default global (`us1`). Needs region-scoped credentials |
| `TWILIO_EDGE` | no | Edge for the chosen region, e.g. `dublin`. Inferred from `TWILIO_REGION` when omitted |
| `ELEVENLABS_API_KEY` | yes | Outbound voice calls and the transcript backfill/sync (`xi-api-key`) |
| `ELEVENLABS_API_BASE` | no | Defaults to `https://api.elevenlabs.io` |
| `ELEVENLABS_AGENT_ID` | yes | Conversational agent that runs the call |
| `ELEVENLABS_PHONE_ID` | yes | The agent's Twilio number id in ElevenLabs |
| `ELEVENLABS_WEBHOOK_SECRET` | yes | Verifies the `elevenlabs-signature` HMAC on post-call transcript deliveries. Without it every delivery is rejected |
| `ANTHROPIC_API_KEY` | yes | SMS brain |
| `ANTHROPIC_MODEL` | no | Defaults to `claude-sonnet-5` |
| `TOOL_WEBHOOK_SECRET` | yes | Shared secret for `/tools/*` (`X-Webhook-Secret`) |
| `EVENTS_FEED_URL` | no | Live event feed (CSV or JSON, same columns as the export). Falls back to the embedded `events_live.csv` when unreachable |
| `TAVILY_API_KEY` | no | Reserved for live event search; warns if unset |
| `GCAL_ICS_URL` | no | Fallback calendar when a user has no `ics_url`; warns if unset |
| `DATABASE_URL` | no | Postgres DSN. When set (and **exported** to the process), Postgres is used and `DATABASE_PATH` is ignored |
| `DATABASE_PATH` | no | SQLite fallback, default `./data.db` |
| `PORT` | no | Default `8090` |

Missing *required* vars log a loud warning at startup and disable that feature; the service
still boots. Never commit a real `.env` — `.gitignore` covers it, and nothing in this repo,
including the scripts, contains a credential.

## Database

One schema, two backends. `DATABASE_URL` selects Postgres; otherwise it is SQLite at
`DATABASE_PATH`. The boot log states which was chosen, and `/version` reports it as
`backend` — worth checking, because a `DATABASE_URL` that is present in a file but not
exported to the process silently means SQLite.

- Migrations are a numbered list in `store_sql.go`, run in order inside a transaction at
  boot, and are idempotent. A deploy needs no manual DDL; the role only needs `CREATE` in
  its own database.
- Tables: `users`, `sessions`, `messages`, `checkins`, `checklist_items`, `consents`,
  `signals`, `events`, `suggestions`, `webhook_events`, `transcripts`, `login_codes`,
  `auth_sessions`, `auth_audit`.
- Every user-scoped read and write carries `user_id`. Cross-user leakage is covered by
  `isolation_test.go`, which fails if a query forgets its scope.
- Retried provider webhooks are idempotent: SMS on Twilio's `MessageSid`, transcripts on
  ElevenLabs' `conversation_id` — the same key the periodic sync upserts on.
- `transcripts.user_id` is nullable (migration 10): a call from a number that has not signed
  up is kept, unattached, alongside the number it came from, and attaches on verification.
  Erasing an account removes the unattached calls for its number too.
- `users.call_started_at` (migration 11) is the server's record of a call being live. It is
  claimed by a conditional `UPDATE` before the provider is called, so two clicks cannot place
  two calls, and it is cleared on hang-up, on a call the provider refused, and when the
  post-call transcript arrives. A call nobody ever reported the end of expires after ten
  minutes so the dashboard button cannot lock permanently.
- There is **no** automatic SQLite→Postgres data migration. The first Postgres boot starts
  empty.

## Sign-in and the dashboard

`/login` takes a phone number, texts a six-digit code, and exchanges it for a session
cookie. A profile that has already been through the interview goes straight to `/dashboard`
rather than back through sign-up, decided from the phone-identified profile rather than
anything the browser says. The dashboard lists that user's own call transcripts with search,
pagination and per-transcript deletion, offers a deliberate call or text check-in, and carries
a settings widget for renaming, signing out and erasing the account. The rules the
implementation holds to:

- Codes are random (`crypto/rand`), single-use, expire after 10 minutes, allow 5 wrong
  attempts, and are limited to 3 requests per number per 15 minutes plus an IP limit.
- **Only the hash of a code or a session token is ever stored**, and a code is never logged
  or returned in a response — the SMS is the only place it exists in the clear.
- An unknown number gets the same answer as a known one, so the endpoint cannot be used to
  test whether somebody is registered.
- Signing in marks the number verified and resumes the existing profile; a returning user
  never repeats onboarding.
- Every transcript query is scoped to the session's user id. Another user's transcript is
  `404`, not `403`, so its existence is not disclosed.
- Sessions are revoked on logout, and expire after 30 days.
- Transcripts are kept 90 days, sessions 30 days, the audit trail 180 days; `/forget`
  removes transcripts, sessions, codes and audit rows along with the profile.

Transcripts arrive at `/webhooks/elevenlabs` (`post_call_transcription`). The handler
verifies the HMAC and the timestamp freshness, answers `200` immediately, and stores
asynchronously, keyed on the provider's `conversation_id` so a retried delivery updates one
row. A delivery for a number that matches no user is kept unattached rather than filed
against a guess — a transcript never creates a profile — and attaches when that number
verifies. A transcript is also the provider confirming the call is over, so it clears the
live-call state behind the dashboard's call button.

## Endpoints

```bash
# operations
curl -s localhost:8090/healthz    # liveness
curl -s localhost:8090/metrics    # p50/p95/max per operation, see LATENCY.md
curl -s localhost:8090/version    # commit, build time, backend, uptime — drift check

# landing page, sign-up, and the demo control panel
curl -s localhost:8090/
# sign-up is number → code → name → channel; each step needs the one before it
curl -s -X POST localhost:8090/signup/start --data-urlencode "phone=+447700900123"   # texts a code
curl -s -X POST localhost:8090/auth/verify  --data-urlencode "phone=+447700900123" -d "code=123456" -c jar
curl -s -X POST localhost:8090/api/name -b jar -d "name=Rae"      # session-scoped, no phone accepted
curl -s -X POST localhost:8090/signup   -b jar -d "channel=sms"   # refused until the name is stored

# dashboard, for a profile that has already been through the interview
curl -s -X POST localhost:8090/api/checkin -b jar -d "channel=call"    # check in now, on purpose
curl -s localhost:8090/api/call-state -b jar                           # is a call live? (the button reads this)
curl -s -X POST localhost:8090/api/name    -b jar -d "name=Rae"        # change your name
curl -s -X POST localhost:8090/api/forget  -b jar -d "confirm=DELETE"  # erase your own account
curl -s -X POST localhost:8090/call     -d '{"phone":"+447700900123"}'     # ring them now
curl -s -X POST localhost:8090/settings -d '{"phone":"+447700900123","frequency":"weekdays"}'
curl -s -X POST "localhost:8090/trigger?phone=%2B447700900123"             # simulate a check-in
curl -s "localhost:8090/journal?phone=%2B447700900123"

# inbound SMS (what Twilio posts) → TwiML
curl -s -X POST localhost:8090/sms \
  --data-urlencode "From=+447700900123" \
  --data-urlencode "Body=today was rough, the deploy broke twice"

# voice agent tools (all POST, all need the shared secret)
S="X-Webhook-Secret: $TOOL_WEBHOOK_SECRET"
curl -s -X POST localhost:8090/tools/get_context       -H "$S" -d '{"phone":"+447700900123"}'
curl -s -X POST localhost:8090/tools/save_onboarding   -H "$S" -d '{"phone":"+447700900123","name":"Ada","interests":"hackathons, meetups","frequency":"daily"}'
curl -s -X POST localhost:8090/tools/save_checkin      -H "$S" -d '{"phone":"+447700900123","mood":4,"summary":"Shipped the webhook","topics":"work"}'
curl -s -X POST localhost:8090/tools/suggest_event     -H "$S" -d '{"phone":"+447700900123"}'
curl -s -X POST localhost:8090/tools/accept_suggestion -H "$S" -d '{"phone":"+447700900123"}'

# the interview, one question at a time, answered in order (409 if answered out of order)
curl -s -X POST localhost:8090/tools/next_question -H "$S" -d '{"phone":"+447700900123","channel":"call"}'
curl -s -X POST localhost:8090/tools/save_answer   -H "$S" \
  -d '{"phone":"+447700900123","channel":"call","key":"event_types","status":"answered","answer":"hackathons","idempotency_key":"conv1-q1"}'

# post-call transcripts from ElevenLabs (HMAC-signed, not the shared secret)
# elevenlabs-signature: t=<unix>,v0=<hex hmac-sha256 of "<t>.<raw body>">
curl -s -X POST localhost:8090/webhooks/elevenlabs \
  -H "elevenlabs-signature: t=1756480000,v0=<digest>" \
  -d '{"type":"post_call_transcription","data":{}}'

# phone + one-time code sign-in (the code is sent by SMS, never returned here)
curl -s -X POST localhost:8090/auth/request -d '{"phone":"+447700900123"}'
curl -s -X POST localhost:8090/auth/verify  -d '{"phone":"+447700900123","code":"123456"}' -c jar
curl -s -X POST localhost:8090/auth/logout  -b jar

# the signed-in user's own data (session cookie only — no phone in the query)
curl -s -b jar "localhost:8090/api/me"
curl -s -b jar "localhost:8090/api/transcripts?q=hackathon&page=1"
curl -s -b jar "localhost:8090/api/transcripts/42"
curl -s -b jar -X DELETE "localhost:8090/api/transcripts/42"

# consent, structured ingestion, inspection and erasure (same shared secret)
curl -s -X POST localhost:8090/consent -H "$S" -d '{"phone":"+447700900123","scope":"heart_rate","granted":true,"source":"sms:yes"}'
curl -s -X POST localhost:8090/ingest  -H "$S" \
  -d '{"phone":"+447700900123","kind":"heart_rate","value":"58","unit":"bpm","source":"whoop","observed_at":"2026-08-29T09:12:00Z","idempotency_key":"whoop-1"}'
curl -s -X POST localhost:8090/signals -H "$S" -d '{"phone":"+447700900123"}'
curl -s -X POST localhost:8090/forget  -H "$S" -d '{"phone":"+447700900123"}'
```

`/tools/*` returns `401 {"error":"unauthorized"}` without a matching `X-Webhook-Secret`, and
also when `TOOL_WEBHOOK_SECRET` is unset — an unconfigured box is closed by default.

## Deployment

The full runbook, including rollback and drift detection, is [DEPLOY.md](DEPLOY.md). In
short: the binary is built and pushed to the Pi by the operator, who owns the host, the
secrets and the provider consoles.

```
push to origin main  →  operator builds arm64, copies to the Pi, restarts systemd
                     →  operator verifies /healthz and /version
```

**There is no Tailscale or SSH path from a developer machine to the Pi, by design.** Deploy
credentials stay with the operator, so nothing in this repository, in CI, or in a contributor's
environment can reach the host. Drift is still checkable over plain HTTPS without any access
to the box: the binary stamps its commit at build time, serves it at `/version` (commit, build
time, backend, uptime — no DSN, no secrets), and `scripts/check-deploy.sh` diffs that against
`origin/main`, exiting `0` in sync, `1` on drift, `2` unreachable.

The Pi itself: binary `/opt/runhack/runhack-arm64`, environment `/opt/runhack/.env` via
systemd `EnvironmentFile=`, service `runhack`, Caddy vhost → `127.0.0.1:8090`, Postgres 16 in
docker on `127.0.0.1:5433`.

Provider configuration, all operator-side:

- **Twilio** — point the number's *A message comes in* webhook at `https://<host>/sms` (POST).
  Signature verification is computed over the public HTTPS URL, so the reverse proxy must
  forward `Host` and `X-Forwarded-Proto` or legitimate traffic 403s.
- **ElevenLabs** — register the `/tools/*` URLs as agent tools with the `X-Webhook-Secret`
  header, enable the `end_call` system tool, set `first_message` to `{{greeting}}`, and paste
  the prompt from [docs/VOICE-AGENT.md](docs/VOICE-AGENT.md) verbatim. The service cannot stop
  a prompt from asking a question it has already been given the answer to.
- **Post-call transcripts** — deliver `post_call_transcription` (JSON, no audio) to
  `https://<host>/webhooks/elevenlabs` and export the signing secret as
  `ELEVENLABS_WEBHOOK_SECRET`. Deliveries are one-shot (retries are off provider-side), so the
  service does not rely on them alone: at boot and every 10 minutes it reconciles against
  `GET /v1/convai/conversations` with `ELEVENLABS_API_KEY`, upserting on `conversation_id`.
  Whichever source sees a call first creates the row; the other updates it. That is what makes
  every call — inbound or outbound, onboarding or check-in, older than the webhook — appear on
  the dashboard.
- A call from a number with no profile is kept unattached (`transcripts.user_id` is nullable)
  and attaches to the profile when that number is verified. Until then it belongs to nobody,
  and it is never filed against a guess.
- Outbound calls go straight to `POST https://api.elevenlabs.io/v1/convai/twilio/outbound-call`
  with `agent_id` / `agent_phone_number_id` / `to_number`; there is no TwiML path.

## Operations

```bash
curl -s https://<host>/healthz
curl -s https://<host>/version | jq            # commit + backend actually running
curl -s https://<host>/metrics | jq            # per-operation latency
scripts/check-deploy.sh                        # 0 in sync, 1 drift, 2 unreachable
journalctl -u runhack -f | grep -E 'slow:|outbound-call timing'
```

- Scheduler slots are Europe/London: `daily` 09:00, `twice-daily` 09:00 + 20:00, `weekdays`
  09:00 Mon–Fri.
- A retention sweep runs hourly: expired signals, transcripts past 90 days, dead sessions,
  spent codes, audit rows past 180 days.
- `events_live.csv` is the Hackathon Radar export: 500 rows, of which 271 unique rows have a
  registration URL and get seeded (228 have no URL, so there is nothing to sign anyone up to,
  and one is a duplicate `(title, starts_at)`). Seeding is incremental and keyed on
  `(title, starts_at)`; `./runhack -reseed` also drops stale events, keeping any event a user
  was already offered so journals don't lose their history.
- Event suggestions prefer London and, when the user has stated interests, tags matching them
  (interests are stemmed, so "hackathons" matches the `non_uni_hackathon` tag).

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| `/version` says `backend: sqlite` in production | `DATABASE_URL` is in the env file but not exported to the process. Fix the unit, restart, re-check |
| Inbound SMS 403s | `TWILIO_AUTH_TOKEN` is set and the proxy is not forwarding `Host` / `X-Forwarded-Proto`, so the signature is computed over the wrong URL |
| Dashboard is empty after a real call | Check `journalctl -u runhack \| grep -E 'transcript\|elevenlabs webhook'`. Rejected deliveries log the reason and the first bytes of the body; if the sync is also silent, `ELEVENLABS_API_KEY` is missing and only the webhook path is live |
| `/login` never texts a code | Twilio is unconfigured on that box; the code is generated and has nowhere to go |
| Agent asks for something already on file | Provider-side prompt, not the service. Re-apply [docs/VOICE-AGENT.md](docs/VOICE-AGENT.md); `/tools/get_context` shows what the agent was actually told |
| Calls cut off at a consistent short duration (we saw ~47s) | Provider-side, not the service: an ElevenLabs workspace out of credits fails the conversation mid-call (`status=failed`). Check the workspace balance and the conversation status before looking at the code |
| "Call me" stays disabled | The server still believes a call is live. `GET /api/call-state` says what it thinks; it clears on the post-call transcript, or after ten minutes if that never arrives |
| Agent keeps the line open after the interview | `end_call` is not enabled on the agent. The Twilio hang-up backstop fires a few seconds later |
| Calls feel slow to respond | Check `/metrics` and `slow:` log lines first. If the service is fast, it is the agent's STT/LLM/TTS configuration — see [LATENCY.md](LATENCY.md) |
| Everything answers but nothing is remembered | Fresh Postgres starts empty; there is no SQLite→Postgres migration |
| `check-deploy.sh` exits 2 | The host is unreachable, or the build predates the `/version` endpoint |
