# CheckIn — design system

Everything here is implemented as CSS custom properties in
`templates/layout.html`. Change a token there, not in a component.

## Colour

| Token | Hex | Role |
|---|---|---|
| `--ink` | `#12151C` | Page background |
| `--raised` | `#1A1F29` | Cards, inputs, chips |
| `--raised-hi` | `#222836` | Hover / selected surface |
| `--line` | `#2C3341` | 1px borders, dividers |
| `--cream` | `#F4EDE4` | Primary text |
| `--muted` | `#9AA3B2` | Secondary text, helper copy |
| `--apricot` | `#FF9A6B` | Primary action, brand, focus ring |
| `--apricot-deep` | `#E97C4B` | Pressed / active |
| `--mint` | `#7BD8B0` | Success, accepted events |
| `--rose` | `#FF8A8A` | Errors |
| `--on-apricot` | `#1A1008` | Text on an apricot fill |

Measured contrast ratios (WCAG 2.1):

| Pair | Ratio | Verdict |
|---|---|---|
| cream on ink | 15.73:1 | AAA |
| cream on raised | 14.22:1 | AAA |
| muted on ink | 7.18:1 | AAA (body), safe for 14px |
| apricot on ink | 8.77:1 | AAA — also fine as a text colour |
| mint on ink | 10.69:1 | AAA |
| rose on ink | 8.05:1 | AAA |
| ink on apricot (button label) | 8.99:1 | AAA |

Rule: **cream text is never placed on apricot** (1.79:1). Apricot fills always
take `--on-apricot`.

`--line` at 1.44:1 is a decorative border only; it never carries meaning on its
own — selected states also change fill and show a check glyph, so the UI does not
rely on colour alone.

## Typography

Two families, loaded from Google Fonts with `preconnect` and `font-display:
swap`, each with a full system fallback stack so the page is fully usable if the
CDN is blocked or slow.

| Token | Family | Used for |
|---|---|---|
| `--font-display` | Fraunces, optical size 9pt, weight 600 | Wordmark, step headings |
| `--font-ui` | Inter Tight, weights 400/500/600 | Everything else |

Fraunces is a soft, slightly quirky serif — it makes the questions read as a
person asking, not a form label. Inter Tight keeps the UI dense and legible at
375px.

Scale (mobile → desktop via `clamp`):

| Token | Size | Line height | Use |
|---|---|---|---|
| `--t-display` | `clamp(28px, 7vw, 40px)` | 1.15 | The one question on a wizard step |
| `--t-title` | `clamp(20px, 4.5vw, 24px)` | 1.25 | Page/section titles |
| `--t-body` | `17px` | 1.5 | Body, inputs, buttons |
| `--t-small` | `14px` | 1.45 | Helper text, meta |
| `--t-micro` | `12.5px` | 1.4 | Labels, timestamps |

Body text never goes below 14px. Inputs are 17px so iOS does not zoom on focus.

## Spacing

4px base, exposed as `--s-1` … `--s-8`: 4, 8, 12, 16, 24, 32, 48, 64.
Card padding is `--s-5` (24px) on mobile, `--s-6` (32px) from 480px up. Vertical
rhythm between blocks is `--s-4` (16px); between sections `--s-6`.

## Radii, elevation, motion

- `--r-sm` 10px (chips, inputs), `--r-md` 16px (cards, buttons), `--r-lg` 24px
  (the wizard shell), `--r-full` 999px (pills).
- One elevation only: `--shadow` `0 24px 60px -30px rgba(0,0,0,.75)`. Depth comes
  from surface colour, not stacked shadows.
- Motion: `--ease` `cubic-bezier(.2,.7,.3,1)`, `--fast` 120ms, `--slow` 220ms.
  Every transition is wrapped by `@media (prefers-reduced-motion: reduce)`, which
  reduces durations to 0.01ms.

## Components

**Button** — `.btn` (apricot fill, ink label, 52px tall, full width on mobile),
`.btn.ghost` (transparent, cream label, 1px line border), `.btn.quiet` (text
only, muted). States: hover lifts 1px and darkens to `--apricot-deep`; active
returns to 0; `:focus-visible` shows the 3px apricot ring; `[disabled]` drops to
50% opacity with `cursor: not-allowed`; `.is-loading` swaps the label for a
spinner and sets `aria-busy`.

**Input** — 56px tall, `--raised` fill, 1px `--line` border, apricot border plus
ring on focus. `aria-invalid="true"` switches the border to `--rose`. Errors are
rendered in a `role="alert"` region below the field, not as a placeholder.

**Choice card** — the big tappable option (call/text, frequency). Left-aligned
title plus one line of muted supporting copy, a 40px round glyph on the left,
and a check that appears when selected. Selected state = apricot border +
`--raised-hi` fill + visible check, so it is not colour-only.

**Chip** — the interests picker. Pill, `--raised` fill, `aria-pressed` toggled by
script; selected chips get an apricot border, an apricot-tinted fill and a
leading `✓`.

**Progress** — a 3px track under the header with an apricot fill, `role="progressbar"`
with `aria-valuenow`/`aria-valuemax`, plus a "Step 3 of 6" label for screen
readers.

**Entry card** — one journal check-in: mood pill, relative time, summary, topic
chips.

**States**

- *Loading*: the primary button becomes `.is-loading` (spinner, `aria-busy="true"`,
  disabled) — the network step is never silent.
- *Error*: inline, `role="alert"`, in `--rose`, keeps whatever the user typed, and
  says what to do next ("Include the country code, like +447700900123").
- *Empty*: journal with no entries shows the mark, one sentence explaining what
  will appear there, and the one useful action (simulate a check-in).

## Layout and responsiveness

Single column throughout, `min(560px, 100% - 32px)`, centred. Designed at 375px
first; the only breakpoint is 480px, where card padding and the display size
step up. Tap targets are ≥ 44px. Long event titles wrap; nothing is truncated
with an ellipsis where the text is the content.

## Accessibility checklist

- All text meets AA and nearly all meets AAA (table above).
- `:focus-visible` ring on every interactive element; never `outline: none`
  without a replacement.
- Wizard steps are `<section aria-hidden>` toggled by script; focus moves to the
  new step's heading on advance, and headings are real `<h1>`/`<h2>`.
- Chips are `<button aria-pressed>`; choice cards are `<button>`, not `<div>`.
- Errors are announced via `role="alert"`; the progress bar exposes its value.
- Colour is never the sole signal for state.
- `prefers-reduced-motion` is honoured.
- The page works with fonts blocked. Sign-up itself needs JavaScript, because
  the number is proved by a texted code before anything else is asked, so
  `<noscript>` says so and points at `/login` rather than offering a form that
  would skip verification.
- The two channel cards sit in a `.choices` grid with a `--s-5` gap and `--s-5`
  padding, so each is a 76px-plus target with clear space between them.
