# CheckIn

A phone-native journalling companion. You sign up with your phone number, pick text or
call, and CheckIn does a daily check-in with you. It remembers what you said last time,
knows what was on your calendar, and when you sound low (or your day is empty) it offers
you one real London tech event and signs you up.

Single Go binary, SQLite, no CGO, no frontend framework. Built to run on a Raspberry Pi
behind Caddy.

## Architecture (10 lines)

1. `main.go` boots config → SQLite store → calendar cache → brain → telephony → HTTP mux + scheduler goroutine.
2. `db.go` owns the schema (`users`, `checkins`, `events`, `suggestions`, `messages`) and seeds `events.csv` on first boot.
3. `server.go` is the whole HTTP surface: onboarding page, `/signup`, `/sms`, `/journal`, `/trigger`, `/healthz`, `/tools/*`.
4. `brain.go` is the SMS brain: it builds a memory + calendar + suggestion-state preamble and runs an Anthropic tool-use loop.
5. Tools exposed to the model: `save_checkin`, `suggest_event`, `accept_suggestion` — the same three operations the voice agent gets over HTTP.
6. `anthropic.go` is a thin Messages API client behind the `AnthropicClient` interface, so tests inject a fake brain.
7. `twilio.go` is the `Telephony` interface (SendSMS / StartCall); with no Twilio creds it degrades to a logging stub.
8. `ics.go` is a dependency-free ICS line parser (handles folded lines, `TZID`, UTC and DATE-only) with a 5 minute per-URL cache.
9. `scheduler.go` ticks every 60s and fires `/trigger`'s logic per user per frequency, using `users.last_triggered_at` to avoid duplicates.
10. Templates live in `templates/` and the events CSV in `events.csv`; both are `embed`ed, so the binary is the whole deployment.

Failure posture: every external dependency is optional at runtime. No Anthropic key → `/sms`
answers with a canned reply instead of 500. No Twilio creds → outbound messages are logged.
No calendar → the model just gets an empty calendar block.

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
| `ELEVENLABS_API_KEY` | yes | Voice agent (operator wires the phone integration) |
| `ANTHROPIC_API_KEY` | yes | SMS brain |
| `ANTHROPIC_MODEL` | no | Defaults to `claude-sonnet-5` |
| `TOOL_WEBHOOK_SECRET` | yes | Shared secret for `/tools/*` (`X-Webhook-Secret`) |
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

# demo trigger: opening check-in SMS, or an outbound call for call users
curl -s -X POST "localhost:8090/trigger?phone=%2B447700900123"

# journal page
curl -s "localhost:8090/journal?phone=%2B447700900123"

# ElevenLabs agent tools (all POST, all need the shared secret)
S="X-Webhook-Secret: $TOOL_WEBHOOK_SECRET"
curl -s -X POST localhost:8090/tools/get_context       -H "$S" -d '{"phone":"+447700900123"}'
curl -s -X POST localhost:8090/tools/save_checkin      -H "$S" -d '{"phone":"+447700900123","mood":4,"summary":"Good run, shipped the webhook","topics":"running, work"}'
curl -s -X POST localhost:8090/tools/suggest_event     -H "$S" -d '{"phone":"+447700900123"}'
curl -s -X POST localhost:8090/tools/accept_suggestion -H "$S" -d '{"phone":"+447700900123"}'
```

`/tools/*` returns `401 {"error":"unauthorized"}` without a matching `X-Webhook-Secret`
(and also when `TOOL_WEBHOOK_SECRET` is unset, so an unconfigured box is closed by default).

## Deployment notes for the operator

- `runhack-arm64` is a static binary; ship it with nothing else. Templates and `events.csv` are embedded.
- Twilio: point the number's *A message comes in* webhook at `https://runhack.keanuc.net/sms` (HTTP POST).
- ElevenLabs: register the four `/tools/*` URLs as agent tools with header `X-Webhook-Secret`.
- Outbound calls use `elevenLabsTwiMLURL` in `twilio.go` — a `TODO(operator)` const to point at the ElevenLabs inbound/TwiML URL for the agent.
- Scheduler slots are Europe/London: `daily` 09:00, `twice-daily` 09:00 + 20:00, `weekdays` 09:00 Mon–Fri.
- `events.csv` is clearly-labelled sample data (30 plausible London meetups/hackathons, Sept–Oct 2026). Replace it, or clear the `events` table, before showing it to anyone who might believe it.
