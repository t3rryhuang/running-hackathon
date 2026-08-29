# CheckIn

A phone-native journalling companion. You sign up with your phone number, pick text or
call, and CheckIn does a daily check-in with you. It remembers what you said last time,
knows what was on your calendar, and when you sound low (or your day is empty) it offers
you one real London tech event and signs you up.

Single Go binary, SQLite, no CGO, no frontend framework. Built to run on a Raspberry Pi
behind Caddy.

## Architecture (10 lines)

1. `main.go` boots config → SQLite store → calendar cache → brain → telephony → voice → HTTP mux + scheduler goroutine.
2. `db.go` owns the schema (`users`, `checkins`, `events`, `suggestions`, `messages`) and seeds events from an `EventSource`.
3. `server.go` is the whole HTTP surface: wizard page, `/signup`, `/call`, `/settings`, `/sms`, `/journal`, `/trigger`, `/healthz`, `/tools/*`.
4. `brain.go` is the SMS brain: it builds a memory + calendar + suggestion-state preamble and runs an Anthropic tool-use loop.
5. Tools exposed to the model: `save_checkin`, `suggest_event`, `accept_suggestion` — the same three operations the voice agent gets over HTTP.
6. `anthropic.go` is a thin Messages API client behind the `AnthropicClient` interface, so tests inject a fake brain.
7. `twilio.go` is `Telephony` (SMS), `voice.go` is `Voice` (ElevenLabs outbound calls), `events.go` is `EventSource` (CSV today, an HTTP feed later) — all three degrade or stub cleanly.
8. `ics.go` is a dependency-free ICS line parser (handles folded lines, `TZID`, UTC and DATE-only) with a 5 minute per-URL cache.
9. `scheduler.go` ticks every 60s and fires `/trigger`'s logic per user per frequency, using `users.last_triggered_at` to avoid duplicates.
10. Templates live in `templates/` and the event export in `events_live.csv`; both are `embed`ed, so the binary is the whole deployment.

Failure posture: every external dependency is optional at runtime. No Anthropic key → `/sms`
answers with a canned reply instead of 500. No Twilio creds → outbound messages are logged.
No calendar → the model just gets an empty calendar block. No ElevenLabs agent/phone id → `/call` logs
the attempt and reports success to the wizard rather than failing the sign-up.

## Onboarding

`GET /` is a stepped wizard (vanilla JS, one question per screen): number → call or text →
frequency → (call users) *Call me now* → done screen with the journal link and a simulate
button. The wizard POSTs the same `/signup` form as before but sends `Accept: application/json`
to get `{ok, phone, channel, frequency, journal}` back and drive the last two steps client-side;
without JS the form still renders the old confirmation page.

For `channel=call`, the interview happens on the phone: the ElevenLabs agent asks what kinds of
events the user likes and how often to check in, then posts the answers to `/tools/save_onboarding`.
`/tools/get_context` reports `onboarded` so the agent knows whether to interview or check in.

## Run

```bash
cp .env.example .env    # fill in your keys
set -a && source .env && set +a
make native && ./runhack       # listens on :8090
```

Cross-compile for the Pi:

```bash
make build      # → runhack-arm64 (CGO_ENABLED=0 GOOS=linux GOARCH=arm64)
```

Tests / checks:

```bash
make test   # unit tests: ICS parser, SMS webhook + tool loop, auth, scheduler
make vet
```

## Environment variables

| Var | Required | Purpose |
| --- | --- | --- |
| `TWILIO_ACCOUNT_SID` | yes | Twilio REST auth |
| `TWILIO_AUTH_TOKEN` | yes | Twilio REST auth |
| `TWILIO_NUMBER` | yes | Sending number (E.164) |
| `TWILIO_REGION` | no | Twilio processing region, e.g. `ie1` (Ireland). Default global (`us1`). Needs region-scoped credentials |
| `TWILIO_EDGE` | no | Edge for the chosen region, e.g. `dublin`. Inferred from `TWILIO_REGION` when omitted |
| `ELEVENLABS_API_KEY` | yes | Outbound voice calls (`xi-api-key`) |
| `ELEVENLABS_API_BASE` | no | Defaults to `https://api.elevenlabs.io` |
| `ELEVENLABS_AGENT_ID` | yes | Conversational agent that runs the call |
| `ELEVENLABS_PHONE_ID` | yes | The agent's Twilio number id in ElevenLabs |
| `ANTHROPIC_API_KEY` | yes | SMS brain |
| `ANTHROPIC_MODEL` | no | Defaults to `claude-sonnet-5` |
| `TOOL_WEBHOOK_SECRET` | yes | Shared secret for `/tools/*` (`X-Webhook-Secret`) |
| `EVENTS_FEED_URL` | no | Live event feed (CSV or JSON, same columns as the export). Falls back to the embedded `events_live.csv` when unreachable |
| `TAVILY_API_KEY` | no | Reserved for live event search; warns if unset |
| `GCAL_ICS_URL` | no | Fallback calendar when a user has no `ics_url`; warns if unset |
| `DATABASE_PATH` | no | Default `./data.db` |
| `PORT` | no | Default `8090` |

Missing *required* vars log a loud warning at startup and disable that feature; the service
still boots. Never commit a real `.env`.

## Endpoints

```bash
# health
curl -s localhost:8090/healthz

# onboarding page
curl -s localhost:8090/

# signup (form-encoded; note --data-urlencode so the leading + survives)
curl -s -X POST localhost:8090/signup \
  --data-urlencode "phone=+447700900123" \
  -d "name=Keanu&channel=sms&frequency=daily" \
  --data-urlencode "ics_url=https://calendar.google.com/calendar/ical/.../basic.ics"

# inbound SMS (what Twilio posts) → TwiML
curl -s -X POST localhost:8090/sms \
  --data-urlencode "From=+447700900123" \
  --data-urlencode "Body=today was rough, the deploy broke twice"

# ring the user now (ElevenLabs outbound agent call)
curl -s -X POST localhost:8090/call -d '{"phone":"+447700900123"}'

# change check-in frequency (daily | twice-daily | weekdays)
curl -s -X POST localhost:8090/settings -d '{"phone":"+447700900123","frequency":"weekdays"}'

# demo trigger: opening check-in SMS, or an outbound call for call users
curl -s -X POST "localhost:8090/trigger?phone=%2B447700900123"

# journal page
curl -s "localhost:8090/journal?phone=%2B447700900123"

# ElevenLabs agent tools (all POST, all need the shared secret)
S="X-Webhook-Secret: $TOOL_WEBHOOK_SECRET"
curl -s -X POST localhost:8090/tools/get_context       -H "$S" -d '{"phone":"+447700900123"}'
curl -s -X POST localhost:8090/tools/save_onboarding   -H "$S" -d '{"phone":"+447700900123","name":"Keanu","interests":"hackathons, meetups","frequency":"daily"}'
curl -s -X POST localhost:8090/tools/save_checkin      -H "$S" -d '{"phone":"+447700900123","mood":4,"summary":"Good run, shipped the webhook","topics":"running, work"}'
curl -s -X POST localhost:8090/tools/suggest_event     -H "$S" -d '{"phone":"+447700900123"}'
curl -s -X POST localhost:8090/tools/accept_suggestion -H "$S" -d '{"phone":"+447700900123"}'
```

`/tools/*` returns `401 {"error":"unauthorized"}` without a matching `X-Webhook-Secret`
(and also when `TOOL_WEBHOOK_SECRET` is unset, so an unconfigured box is closed by default).

## Deployment notes for the operator

- `runhack-arm64` is a static binary; ship it with nothing else. Templates and `events_live.csv` are embedded.
- Twilio: point the number's *A message comes in* webhook at `https://runhack.keanuc.net/sms` (HTTP POST).
- ElevenLabs: register the five `/tools/*` URLs as agent tools with header `X-Webhook-Secret`, and give the agent a prompt that interviews un-onboarded callers (interests + frequency) before calling `save_onboarding`.
- Outbound calls go straight to `POST https://api.elevenlabs.io/v1/convai/twilio/outbound-call` with `agent_id` / `agent_phone_number_id` / `to_number`; the old Twilio TwiML path is gone. Nothing on the build box ever dialled that API — the request shape is covered by `TestElevenLabsOutboundRequestShape` only.
- Scheduler slots are Europe/London: `daily` 09:00, `twice-daily` 09:00 + 20:00, `weekdays` 09:00 Mon–Fri.
- `events_live.csv` is the Hackathon Radar export: 500 rows, of which 271 unique rows have a registration URL and get seeded (228 have no URL — there is nothing to sign anyone up to — and one is a duplicate `(title, starts_at)`).
- Seeding is incremental and keyed on `(title, starts_at)`: on boot the binary inserts anything new when the stored count differs from the export. `./runhack -reseed` additionally drops stale events, keeping any event a user was already offered so journals don't lose their history.
- Event suggestions prefer London and, when the user has stated interests, tags matching them (interests are stemmed, so "hackathons" matches the `non_uni_hackathon` tag).
