# CheckIn — Revision 2 (from team feedback)

Feedback source: WhatsApp (Keanu + Joseph). App deployed at https://runhack.keanuc.net but "kindof borked" UX-wise. Fix list, priority order:

## 1. Interactive wizard onboarding (Jack & Jill pattern)
Replace the single form with a stepped wizard — one question per screen, big friendly type, progress dots, instant advance on selection (no submit-per-step where avoidable). Steps:
  1. "What's your number?" — phone input, E.164, big input
  2. "Call or text?" — two large tappable cards (call = voice check-ins, text = SMS)
  3. "How often should I check in?" — cards: daily / twice daily / weekdays only
  4. If channel=call: "I'll ring you now to get to know you" → button **"Call me now"** → POST /call (onboarding interview happens on the phone). If channel=sms: send welcome SMS immediately.
  5. Done screen: link to /journal?phone=..., and a **"Simulate check-in"** button (see §4).
Vanilla JS + the existing dark theme (Inter, blue accent, mobile-first 375px). No framework.

## 2. Outbound voice calls (the app calls the user)
New endpoint `POST /call {phone}`:
  - Calls ElevenLabs outbound API: POST https://api.elevenlabs.io/v1/convai/twilio/outbound-call
    body: {"agent_id": $ELEVENLABS_AGENT_ID, "agent_phone_number_id": $ELEVENLABS_PHONE_ID, "to_number": phone}
    header: xi-api-key: $ELEVENLABS_API_KEY
  - New env vars: ELEVENLABS_AGENT_ID, ELEVENLABS_PHONE_ID (operator supplies).
  - Replaces the Twilio TwiML TODO — delete elevenLabsTwiMLURL path, outbound goes through ElevenLabs directly.
  - Scheduler + /trigger for channel=call users now use /call logic.

## 3. Voice onboarding interview support
The phone agent (operator configures its prompt separately) will interview new users: what kinds of events they like (hackathons/meetups/conferences/social), preferred check-in frequency. Backend support:
  - users table: add `interests TEXT` (comma-separated), keep frequency.
  - New tool webhook `POST /tools/save_onboarding` {phone, name, interests, frequency} → upsert user, mark onboarded_at. Same X-Webhook-Secret auth.
  - /tools/get_context response: add "onboarded": bool and "interests" so the agent knows to run interview vs normal check-in.
  - suggest_event: filter events by user interests when set (match against tags column).

## 4. Frequency change + simulate
  - /journal page: add frequency dropdown (posts to `POST /settings {phone, frequency}`) and a **"Simulate check-in now"** button hitting existing /trigger. This is the live-demo control panel.

## 5. Live event database
  - events_live.csv committed to repo: 500 REAL upcoming events (London-heavy) exported from the Hackathon Radar production database (titles, start dates, cities, registration URLs, category tags). Replace events.csv seeding with this file.
  - Skip rows with empty url. Add `-reseed` flag (or reseed automatically when events table row count differs from CSV) so redeploys pick up fresh data.
  - Keep the EventSource behind a small interface so a live HTTP feed can replace CSV later.

## Constraints (unchanged)
Single Go binary, stdlib + modernc sqlite, arm64 target, no secrets in repo, graceful degradation, tests still pass, commit early, README updated with new endpoints. Push to origin main when done and report DONE with deviations.
