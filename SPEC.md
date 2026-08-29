# CheckIn — voice/SMS journalling companion (Running Hackathon build)

Build a single Go binary web service. Deadline-critical: working > pretty. Deliver in ~2.5h of agent time.

## Product
Phone-native journalling companion. User onboards with phone number, picks call or text channel and a check-in frequency. Service does daily check-ins (voice call via ElevenLabs Agents on a Twilio number, or SMS via Twilio webhook + Claude). It remembers past entries, references the user's Google Calendar ("you had a meeting at 3, how did it go?"), and when the user sounds down or their calendar is empty, suggests a real London tech/hackathon event and offers to sign them up.

## Stack (hard requirements)
- Go 1.22+, single module `runhack`, single binary, stdlib `net/http` + `modernc.org/sqlite` (pure-Go, no CGO) or `mattn/go-sqlite3` if CGO fine — target linux/arm64.
- SQLite file at env `DATABASE_PATH` (default ./data.db).
- No frontend framework. One embedded HTML page (html/template + embed).
- All config from env vars: TWILIO_ACCOUNT_SID, TWILIO_AUTH_TOKEN, TWILIO_NUMBER, ELEVENLABS_API_KEY, ANTHROPIC_API_KEY, TAVILY_API_KEY, GCAL_ICS_URL, TOOL_WEBHOOK_SECRET, DATABASE_PATH, PORT (default 8090). Validate presence at startup, warn-don't-crash for optional (GCAL_ICS_URL, TAVILY_API_KEY).
- NO real secrets in code or repo. .env.example only.

## Schema
- users(id, phone TEXT UNIQUE, name, channel TEXT check in ('call','sms'), frequency TEXT, ics_url TEXT nullable, created_at)
- checkins(id, user_id FK, mood INTEGER 1-5 nullable, summary TEXT, topics TEXT, raw TEXT, created_at)
- events(id, title, starts_at, city, url, tags) — seeded from events.csv at boot if table empty (CSV committed to repo; if you cannot find real London tech events, fabricate 30 plausible-but-clearly-sample London hackathons/meetups Sept-Oct 2026 with luma-style URLs)
- suggestions(id, user_id FK, event_id FK, status TEXT check in ('offered','accepted','declined'), created_at)

## HTTP endpoints
1. `GET /` — onboarding page: phone input (E.164), channel radio (call/sms), frequency select (daily/twice-daily/weekdays), optional ICS URL field. POSTs to /signup. Dark theme, Inter font, blue accent, mobile-first. Keep it minimal and clean.
2. `POST /signup` — create user, respond with confirmation page ("expect a text/call"). If channel=sms, immediately send welcome SMS via Twilio REST API ("Hey, I'm CheckIn... reply to start your first check-in").
3. `POST /sms` — Twilio inbound SMS webhook (form-encoded From/Body). Flow: load user by phone (auto-create if unknown), load last 10 checkins + today's calendar events (parse ICS from user's ics_url or GCAL_ICS_URL) + open suggestion state, call Anthropic Messages API (model claude-sonnet-5) with a system prompt making it a warm, concise journalling companion (2-3 sentences max per reply, one question at a time; references calendar items; detects low mood or empty calendar → offers ONE event from events table; on acceptance mark suggestion accepted and confirm "you're on the list"). Use Anthropic tool-use with tools: save_checkin(mood, summary, topics), suggest_event(), accept_suggestion(). After model reply, respond TwiML `<Response><Message>...</Message></Response>`.
4. ElevenLabs agent tool webhooks, all POST, all require header `X-Webhook-Secret: $TOOL_WEBHOOK_SECRET` (401 otherwise):
   - `/tools/get_context` {phone} → {name, last_checkins:[...], todays_calendar:[...], open_suggestion}
   - `/tools/save_checkin` {phone, mood, summary, topics} → {ok}
   - `/tools/suggest_event` {phone} → {event:{title, starts_at, url}} (pick soonest event not yet suggested to user)
   - `/tools/accept_suggestion` {phone} → {ok, confirmation}
5. `GET /journal?phone=...` — simple page listing that user's checkins newest-first + accepted events. Same dark styling. (No auth — hackathon.)
6. `GET /healthz` → ok
7. `POST /trigger?phone=...` — demo button endpoint: for sms user sends opening check-in SMS ("How's your day going?" personalized w/ calendar); for call user initiates outbound call via Twilio REST API Calls endpoint with `Url` pointing at ElevenLabs-provided TwiML/inbound (leave the exact call initiation behind an interface — operator will wire ElevenLabs phone integration separately; implement Twilio call POST with a TODO const for the TwiML URL).
8. Scheduler goroutine: ticker every 60s, fire check-in per user per frequency (daily = 09:00 Europe/London; store last_triggered_at on users to avoid dupes). Reuse /trigger logic.

## ICS parsing
Fetch ics_url, parse VEVENT DTSTART/SUMMARY for today (Europe/London), no external ics lib needed — simple line parser is fine, handle folded lines. Cache 5 min.

## Deliverable & workflow
- Work in /home/ubuntu/runhack (or your workspace). git init, commit early and often.
- README.md: run instructions, env vars, architecture in 10 lines, curl examples for every endpoint.
- `make build` → CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o runhack-arm64 . ; also build native for your own testing.
- Makefile + .env.example committed.
- TEST: unit-test the ICS parser and the SMS webhook flow with a fake Anthropic client (interface). `go vet` + `go build` must pass. Run the server, curl /healthz, /signup, and simulate /sms with a stubbed Anthropic key missing (should degrade gracefully with a canned reply, not 500).
- When done: reply DONE + tree of repo + any deviations. Operator will scp the code from your box, deploy to a Raspberry Pi behind Caddy at https://runhack.keanuc.net, and wire real Twilio/ElevenLabs webhooks.
- You get ANTHROPIC_API_KEY for live-testing the SMS brain: set it in your env only, never commit. All other services: code against env vars, no live calls needed.

## Priorities if time runs short (drop from bottom)
1. /sms conversational loop w/ memory + tools  ← core demo
2. /signup + onboarding page + welcome SMS
3. /tools/* webhooks for ElevenLabs
4. /journal page
5. ICS calendar context
6. Event suggestion flow
7. Scheduler (demo uses /trigger anyway)
