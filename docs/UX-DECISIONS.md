# UX decisions and implementation plan

Decisions, with the reasoning, so the next person can disagree with the reason
rather than guess the intent.

## Decisions

**One question per screen, in a wizard.** Kept from v2. A phone-first onboarding
where every step is a single large question tests better than a form, and it
mirrors how the product itself behaves — one question at a time.

**Name first, number second.** Asking for a name first makes the following
number request feel like a conversation rather than a lead-capture form, and it
gives the voice agent something to open with.

**Interests are asked explicitly, and can be declined.** The recommendation
engine only acts on what the user typed or tapped (see `matching.go`). That makes
the interests step load-bearing, not decoration: skipping it means the user gets
broad picks and is told so. Nothing is inferred from anything else about them.

**"I'd rather not say" is a first-class option**, not a small grey link. A
journalling product that pressures you on screen one has already lost.

**Chips plus free text**, not a multi-select listbox. Chips make the vocabulary
visible (people don't know we key off tags like `founder_vc`), and the free-text
field catches everything we didn't list.

**No progress dots — a bar.** Six dots at 375px are ambiguous and tiny; a bar
plus "Step 3 of 6" is readable and announceable.

**Warm, not blue.** See [BRAND.md](BRAND.md).

**The journal doubles as the control panel.** Frequency select and "Simulate
check-in" live there because that is the only page a user returns to, and the
demo needs both within one tap.

**No auth.** Hackathon scope, unchanged. Documented rather than hidden.

## Implementation plan

Phased, each phase independently shippable:

1. **Tokens and shell** — replace the v2 CSS variables in `layout.html` with the
   full token set, wire the fonts with a fallback stack, add the brand mark,
   header, and the `static/` route. No markup changes elsewhere. ✅
2. **Components** — buttons (incl. loading/disabled), inputs (incl. invalid),
   choice cards, chips, progress bar, alerts. Restyle in place so existing
   selectors and the JS keep working. ✅
3. **Wizard** — apply the components to the six steps, add focus management,
   loading state on submit, inline errors, and the `<noscript>` fallback. ✅
4. **Journal** — entry cards, mood pills, accepted-event list, empty state,
   control panel. ✅
5. **Verification** — `go vet`, `go test`, arm64 build, markup assertions in
   `sms_test.go`/`matching_test.go`, and a manual pass at 375px and 1280px. ✅

## Verification steps (manual, per release)

- 375px: every step fits without horizontal scroll; tap targets ≥44px.
- Keyboard only: tab through a whole signup; the focus ring is always visible;
  the ring never lands on an invisible step.
- Submit with an invalid number: the error is announced, the value is kept.
- Journal for a phone with no entries: empty state, not a blank page.
- Block fonts.googleapis.com in devtools: layout is unchanged in the fallback
  stack.
- `prefers-reduced-motion: reduce`: no transitions.

## Known limitations

- No auth on `/journal`; anyone with a phone number can read it.
- The design system is documented and implemented but not enforced — there is no
  visual-regression test, so drift is possible.
- Fonts come from a CDN. Self-hosting would be better for privacy and for a Pi
  behind Caddy, but adds ~200KB of binary assets to the repo.
