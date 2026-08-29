# Voice latency: where the time actually goes

Scope: the delay between a caller speaking (or the "Call me now" tap) and the
agent responding. No live calls were placed from the build box — every number
below is either measured locally, measured against a provider's TLS endpoint
(handshake only, no API request), or explicitly marked as operator-side.

## The chain

```
tap "Call me now"
  └─ POST /call                       (us)
       └─ ElevenLabs outbound-call    (provider, HTTP)
            └─ Twilio dials the PSTN  (provider, seconds — carrier setup)
                 └─ caller answers
                      └─ agent turn:  STT → LLM → TTS   (provider config)
                           └─ tool webhook → our HTTP   (us)
```

Only two of those hops are ours: `POST /call` and the `/tools/*` webhooks the
agent hits mid-conversation. Everything else is provider configuration.

## Measured

Our own handlers, measured through the new `/metrics` endpoint after 30 requests
each against the built binary (SQLite on local disk):

| Operation             | p50      | p95      | max      |
|-----------------------|----------|----------|----------|
| `tool.get_context`    | 0.22 ms  | 0.24 ms  | 0.28 ms  |
| `tool.save_checkin`   | 0.16 ms  | 0.17 ms  | 0.19 ms  |
| `tool.suggest_event`  | 1.86 ms  | —        | 1.86 ms  |

Conclusion: **our request handling is not the problem** — it is three orders of
magnitude below anything a caller can hear. Expect the Pi to be slower than the
build box, but not by 100x; `/metrics` on the Pi will confirm.

Connection setup to the providers (DNS + TCP + TLS, mean of 3, from the build
box in the US — the Pi in London will differ, particularly for ElevenLabs):

| Host                        | DNS | TCP  | TLS  |
|-----------------------------|-----|------|------|
| `api.elevenlabs.io`         | 1ms | 33ms | 37ms |
| `api.twilio.com`            | 1ms | 8ms  | 9ms  |
| `api.dublin.ie1.twilio.com` | 6ms | 8ms  | 8ms  |
| `api.anthropic.com`         | 1ms | 8ms  | 9ms  |

So a *cold* connection to ElevenLabs costs ~70ms before the request is even
sent, every time, if connections are not reused.

The two real risks in our own code were both latent rather than visible in the
table above:

1. **The ICS fetch was on the critical path.** `/tools/get_context` fetched the
   user's calendar synchronously with a 10s client timeout and a 5-minute cache,
   so the first tool call of a conversation (and one call every 5 minutes after)
   could stall for as long as the calendar host took. That is dead air on a live
   call. `TestCalendarDoesNotBlockOnSlowFeed` reproduces it.
2. **Every provider request paid a fresh handshake**, because the default
   `http.Client` was used with no configured transport and connections went idle
   between check-ins.

## Fixed in code

- `/metrics` — in-process p50/p95/max per operation, no external dependency
  (`timing.go`). Every `/tools/*` webhook, `/call`, `/sms`, the
  Anthropic request, the Twilio requests and the ElevenLabs outbound call are
  recorded, and anything over 400ms logs a `slow:` line.
- **Calendar off the critical path** (`ics.go`): a stale cache entry is served
  immediately while a refresh runs in the background, a cold fetch is capped at
  1.5s, and concurrent callers share one in-flight request. Worst case for the
  caller went from "as long as the ICS host takes, up to 10s" to 1.5s once, and
  0ms thereafter.
- **Calendar pre-warm on dial** (`server.go`, `brain.go`): `placeCall` warms the
  feed while the phone is still ringing, so the agent's first `get_context` hits
  a warm cache.
- **Connection reuse** for ElevenLabs, Twilio and Anthropic: explicit transports
  with `MaxIdleConnsPerHost`, a 5-minute idle timeout and HTTP/2 attempted,
  saving the ~70ms handshake on every request after the first.
- **TLS pre-warm at boot** (`voice.go`): one TLS handshake to the ElevenLabs
  host when the service starts, so the first real call does not pay for it. It
  is a handshake only — no API request, nothing billable, no call.
- **Per-phase attribution** on the outbound call: `httptrace` records DNS, TCP,
  TLS, TTFB and whether the connection was reused, logged per call and recorded
  as `voice.outbound_call.ttfb`. Next time call setup feels slow there will be a
  number for it instead of a guess.

## Requires operator / provider configuration

These dominate perceived latency and cannot be fixed from the Go service:

1. **Agent turn latency (the big one).** Response time is set by the ElevenLabs
   agent's STT → LLM → TTS pipeline. Levers, in order of impact: a smaller/faster
   LLM for the agent, a low-latency TTS model (Flash/Turbo rather than the
   highest-quality multilingual), and turn-detection/interruption settings. A
   heavyweight model here costs seconds per turn; nothing we do in Go is visible
   next to it.
2. **Keep the agent's tool list short and its webhook timeouts tight.** Each tool
   call is an extra round trip mid-turn. `get_context` at conversation start plus
   `save_checkin` at the end is enough; avoid per-turn tools.
3. **Number and agent region.** A UK Twilio number with the agent's media in a
   European region avoids transatlantic media legs. This is set on the ElevenLabs
   phone-number/agent config, not in our code.
4. **PSTN call setup** (ring to answer) is carrier time — seconds, unavoidable.
   The wizard's "I'll ring you now" copy sets that expectation deliberately.
5. **Twilio processing region** is available but was declined: `TWILIO_REGION=ie1`
   points the REST calls at `api.dublin.ie1.twilio.com`, which needs IE1-scoped
   credentials. The measurement above shows ~1ms difference in handshake from
   this box, and our Twilio calls (welcome SMS, hang-up backstop) are not in the
   conversational path anyway — so this is not worth the credential churn.

## Tradeoffs

- Serving a stale calendar can mention an event that was deleted in the last few
  minutes. Judged clearly better than silence on a call.
- The 1.5s cold-fetch cap means a genuinely slow calendar host yields no calendar
  context on the first check-in of the day rather than delaying it.
- The boot-time TLS handshake makes one outbound connection at startup even when
  no call is ever placed.
- `/metrics` is unauthenticated. It contains timings only — no phone numbers, no
  journal content — and the operator needs it reachable during a demo.

## How to verify on the Pi

```bash
curl -s https://runhack.keanuc.net/metrics | jq
journalctl -u runhack | grep -E 'slow:|outbound-call timing'
```

`slow:` lines and the per-phase `outbound-call timing` line are the two things to
look at first if the agent feels sluggish; if neither shows anything, the latency
is in the ElevenLabs agent pipeline (§1 above).
