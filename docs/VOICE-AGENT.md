# Voice agent: caller resolution and prompt guardrails

The ElevenLabs agent prompt lives provider-side, so this file is the contract
between it and the service. The rule it exists to enforce: **the agent never
asks for anything the person already gave the signup form or an earlier
conversation.**

## Where the agent gets its facts

Two paths, both producing the same `CallerContext` (`voice_context.go`):

1. **Outbound call** — `POST /call` resolves the number, builds the context and
   passes it as `conversation_initiation_client_data.dynamic_variables` on the
   ElevenLabs outbound request. The agent has the profile before the first word.
2. **Mid-call lookup** — `POST /tools/get_context` (`X-Webhook-Secret`) returns
   the same structure under `caller`, for inbound calls where no variables were
   injected.

`/tools/get_context` is a **lookup only**: an unrecognised number returns an
unresolved context and does not create a profile.

## Dynamic variables

| Variable | Meaning |
| --- | --- |
| `user_phone` | E.164 number the call is with — the only identity key |
| `caller_known` | `yes` when the number matched a stored profile |
| `phone_verified` | `yes` when something proved they hold the number |
| `user_name` | stored name, or `unknown` |
| `user_frequency` | stored check-in frequency, or `unknown` |
| `user_event_types` | stored event preferences, or `unknown` |
| `user_event_time`, `user_evening_availability`, `user_notify_watch` | checklist answers, or `unknown` |
| `do_not_ask` | comma-separated keys that are settled — answered, skipped or declined |
| `ask_only` | comma-separated keys that are genuinely missing or stale, in order |
| `greeting` | the exact opening line to use |

Every value is either a stored fact or the literal string `unknown`. There are
no blanks for the model to fill in.

## Prompt the operator should configure

```
You are CheckIn, calling {{user_phone}}.

Open with exactly: {{greeting}}

Everything you know about this person is below. Nothing else exists — do not
invent a name, a history, a preference or a past conversation.

  name:        {{user_name}}
  check-ins:   {{user_frequency}}
  event types: {{user_event_types}}

Never ask for anything in this list, it is already on file: {{do_not_ask}}
Ask only for these, one at a time, in this order: {{ask_only}}
If that list is empty, do not interview them at all — just do the check-in.

If caller_known is "no" ({{caller_known}}), say plainly that you do not have
their details yet and ask only for their name.
If phone_verified is "no" ({{phone_verified}}), do not use their name until
they confirm who they are, and say why you are checking.

When the interview is finished, call save_onboarding and then end_call.
```

## Behaviour the service guarantees

| Situation | What the agent is told |
| --- | --- |
| Complete verified profile | greeting with their name; `ask_only` empty; go straight to the check-in |
| Partial profile | known fields stated; `ask_only` lists just the holes, in template order |
| Unknown number | `caller_known=no`, no name, no history, ask for the name only |
| Known number, unverified | name withheld from the greeting, verification gap stated out loud |
| Resumed caller | earlier answers and refusals carried into the new session |
| Stale answer | time-bound answers (e.g. "free at 7 tonight?", 20h TTL) go back into `ask_only`; stable profile facts do not |
| Two callers | context is keyed on user id and normalised phone throughout; nothing crosses between profiles |

Skips are deliberately *not* carried between sessions — "not now" is about that
conversation. Declines are carried: a refusal is a standing preference.

Covered by `voice_context_test.go`.
